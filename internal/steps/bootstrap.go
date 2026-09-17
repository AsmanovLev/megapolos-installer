package steps

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"megapolos/installer/internal/sys"
)

// =============================================================================
// bootstrap — платформенная оркестрация через GraphQL API core
// (замена вызова ts-node install.ts: та же семантика, но без node-зависимости
// и с параллелизмом: цепочка ноды ∥ DBMS/preset).
//
// Цепочка (как install.ts): нода → INIT → PREPARE FOR CORE → INSTALL REGISTRY
// → DBMS + системное приложение → (опц.) деплой GUI-приложения.
// Контракт — GraphQL-мутации, которыми пользуется GUI платформы
// (initNode/prepareNodeForCore/installRegistryToNode и др.) — самая стабильная
// поверхность core (резолверы месяцами не меняются).
// =============================================================================

// dockerImages — образы, нужные платформе и сборке приложений оффлайн:
// registry (INSTALL REGISTRY), nginx (контейнер ноды INIT),
// node:18 + busybox:1.35 (FROM в Dockerfile'ах core/gui).
var dockerImages = []string{"registry:2", "nginx:latest", "node:18", "busybox:1.35"}

func imageTarName(image string) string {
	return strings.NewReplacer(":", "-", "/", "-").Replace(image) + ".tar.gz"
}

// dockerImagesStep: предзакладка docker-образов.
// Онлайн — docker pull; оффлайн — docker load из бандла (bundle/docker/).
func dockerImagesStep() Step {
	has := func(c *Ctx, img string) bool {
		return outOK(c, "docker image inspect "+img+" >/dev/null 2>&1")
	}
	return StepFunc{
		N: "docker-images", D: []string{"packages"},
		DetectF: func(c *Ctx) (bool, string) {
			for _, img := range dockerImages {
				if !has(c, img) {
					return false, ""
				}
			}
			return true, "все образы на месте (" + strings.Join(dockerImages, ", ") + ")"
		},
		RunF: func(c *Ctx, w io.Writer) error {
			for _, img := range dockerImages {
				if has(c, img) {
					fmt.Fprintf(w, "%s: уже есть\n", img)
					continue
				}
				tar := filepath.Join(c.O.BundleDir, "docker", imageTarName(img))
				if c.O.BundleDir != "" && sys.FileExists(c, c.Ex, tar) {
					if err := sh(c, w, "zcat "+tar+" | docker load"); err != nil {
						return err
					}
					continue
				}
				if err := sh(c, w, "docker pull "+img); err != nil {
					return fmt.Errorf("docker pull %s: %w", img, err)
				}
			}
			return nil
		},
	}
}

// swarmStep: docker swarm init (нужен платформе для деплоя приложений/registry).
func swarmStep() Step {
	return StepFunc{
		N: "swarm", D: []string{"packages"},
		DetectF: func(c *Ctx) (bool, string) {
			if s, _ := out(c, "docker info --format '{{.Swarm.LocalNodeState}}' 2>/dev/null"); s == "active" {
				return true, "swarm уже активен"
			}
			return false, ""
		},
		RunF: func(c *Ctx, w io.Writer) error {
			// advertise-addr: IP исходящего интерфейса; оффлайн — первый адрес хоста
			return sh(c, w, `IP=$(ip -4 route get 1.1.1.1 2>/dev/null | sed -n 's/.* src \([0-9.]*\).*/\1/p')
[ -z "$IP" ] && IP=$(hostname -I 2>/dev/null | awk '{print $1}')
docker swarm init ${IP:+--advertise-addr "$IP"}`)
		},
	}
}

// ---------------------------------------------------------------------------

// gql — вызов GraphQL с ожиданием API (ретрай сетевых ошибок, до ~2 мин).
func gql(c *Ctx, w io.Writer, query string, vars map[string]any) (json.RawMessage, error) {
	return apiQueryWait(c, w, c.O.Token, query, vars)
}

// waitNodeRunning — ждём lifeStatus 'running': стадии (init/prepare/registry)
// запускают ansible в фоне и сразу возвращаются. Как install.ts: сначала пауза,
// чтобы не поймать прошлый 'running', затем polling до ~7.5 мин.
func waitNodeRunning(c *Ctx, w io.Writer, nodeID, label string) error {
	// ждём старта ansible (нода уходит в updating); иначе поймаем прошлый 'running'
	// и ложно решим, что стадия прошла (так маскировался сбой коллекций на jammy)
	seenUpdating := false
	for i := 0; i < 20; i++ {
		data, err := apiQuery(c, c.O.Token, "query($id: String!) { getNode(id: $id) { lifeStatus } }",
			map[string]any{"id": nodeID})
		if err == nil && strings.Contains(string(data), `"updating"`) {
			seenUpdating = true
			break
		}
		time.Sleep(1500 * time.Millisecond)
	}
	if !seenUpdating {
		return fmt.Errorf("%s: ansible не стартовал (нода не ушла в updating)", label)
	}
	for i := 0; i < 150; i++ {
		data, err := apiQuery(c, c.O.Token, "query($id: String!) { getNode(id: $id) { lifeStatus } }",
			map[string]any{"id": nodeID})
		if err == nil {
			var parsed struct {
				GetNode struct {
					LifeStatus string `json:"lifeStatus"`
				} `json:"getNode"`
			}
			if json.Unmarshal(data, &parsed) == nil && parsed.GetNode.LifeStatus == "running" {
				fmt.Fprintf(w, "%s: нода running\n", label)
				return nil
			}
		}
		time.Sleep(3 * time.Second)
	}
	return fmt.Errorf("%s: нода не вернулась в running за отведённое время", label)
}

// ensureNode — get-or-create нода 'localhost' (host=localhost: node-nginx
// ожидает серт <host>.crt, а registry.conf — localhost.crt).
func ensureNode(c *Ctx, w io.Writer) (string, error) {
	data, err := gql(c, w, "{ getAllNode { id name host user } }", nil)
	if err != nil {
		return "", err
	}
	var parsed struct {
		GetAllNode []struct {
			ID, Name, Host, User string
		} `json:"getAllNode"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return "", err
	}
	for _, n := range parsed.GetAllNode {
		if n.Name == "localhost" {
			if n.Host != "localhost" || n.User != "root" {
				// host мог остаться от прошлого прогона — чиним (иначе nginx upstream не резолвится)
				_, err := gql(c, w, "mutation($id: String!, $values: NodeUpdateInput!) { editNode(id: $id, values: $values) }",
					map[string]any{"id": n.ID, "values": map[string]any{
						"host": "localhost", "user": "root", "password": c.O.NodeRootPassword,
					}})
				if err != nil {
					return "", fmt.Errorf("editNode: %w", err)
				}
				fmt.Fprintln(w, "нода localhost обновлена (host → localhost)")
			}
			return n.ID, nil
		}
	}
	res, err := gql(c, w, "mutation($values: NodeInput!) { createNode(values: $values) { id } }",
		map[string]any{"values": map[string]any{
			"name": "localhost", "host": "localhost", "user": "root",
			"password": c.O.NodeRootPassword, "autoCreateInstances": true,
		}})
	if err != nil {
		return "", fmt.Errorf("createNode: %w", err)
	}
	var created struct {
		CreateNode struct {
			ID string `json:"id"`
		} `json:"createNode"`
	}
	if err := json.Unmarshal(res, &created); err != nil {
		return "", err
	}
	fmt.Fprintln(w, "нода localhost создана (root@localhost:22)")
	return created.CreateNode.ID, nil
}

// ensureRegistry — дефолтный docker registry (localhost, megapolos/megapolos).
func ensureRegistry(c *Ctx, w io.Writer) error {
	data, err := gql(c, w, "{ getAllDockerRegistry { id name isDefault } }", nil)
	if err != nil {
		return err
	}
	var parsed struct {
		GetAllDockerRegistry []struct {
			Name      string `json:"name"`
			IsDefault bool   `json:"isDefault"`
		} `json:"getAllDockerRegistry"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return err
	}
	for _, r := range parsed.GetAllDockerRegistry {
		if r.IsDefault {
			return nil
		}
	}
	_, err = gql(c, w, "mutation($values: DockerRegistryInput!) { createDockerRegistry(values: $values) { id } }",
		map[string]any{"values": map[string]any{
			"name": "default", "host": "localhost", "user": "megapolos",
			"password": "megapolos", "isDefault": true,
		}})
	if err != nil {
		return fmt.Errorf("createDockerRegistry: %w", err)
	}
	fmt.Fprintln(w, "docker registry 'default' (localhost) создан")
	return nil
}

// stageKey → каноничный ключ стадии (для маркера, --retry-stage, --info).
const (
	stageInit          = "init"
	stagePrepareForCore = "prepare-for-core"
	stageInstallRegistry = "install-registry"
)

func stageMarkerPath(key string) string {
	return "/var/lib/megapolos/stage-" + key + ".done"
}

func writeStageMarker(c *Ctx, key string) {
	if err := os.MkdirAll("/var/lib/megapolos", 0o755); err != nil {
		return
	}
	_ = os.WriteFile(stageMarkerPath(key), []byte("done"), 0o644)
}

// stageSpec — декларативное описание стадии ноды (label, мутация, артефакт).
type stageSpec struct {
	Key      string
	Label    string
	Mutation string
	// hasArtifact: возвращает true если стадия уже выполнена (артефакт на месте)
	HasArtifact func(c *Ctx) bool
	// onFailureHint: подсказка пользователю при провале стадии (ansible log)
	OnFailureHint string
}

func nodeStages() []stageSpec {
	return []stageSpec{
		{
			Key:      stageInit,
			Label:    "INIT (nginx, единый CA)",
			Mutation: "initNode",
			HasArtifact: func(c *Ctx) bool {
				return outOK(c, "docker ps --format '{{.Names}}' | grep -qx nginx")
			},
			OnFailureHint: "docker exec nginx nginx -t; journalctl -u megapolos-core -n 200 --no-pager",
		},
		{
			Key:      stagePrepareForCore,
			Label:    "PREPARE FOR CORE",
			Mutation: "prepareNodeForCore",
			HasArtifact: func(c *Ctx) bool {
				return sys.FileExists(c, c.Ex, "/data/nginx/conf/core.conf")
			},
			OnFailureHint: "docker exec nginx nginx -t; ls -la /data/nginx/conf/",
		},
		{
			Key:      stageInstallRegistry,
			Label:    "INSTALL REGISTRY",
			Mutation: "installRegistryToNode",
			HasArtifact: func(c *Ctx) bool {
				return sys.FileExists(c, c.Ex, "/data/registry/docker-compose.yml")
			},
			OnFailureHint: "ls -la /data/registry/; docker logs registry 2>&1 | tail -50",
		},
	}
}

// nodeChain — INIT → PREPARE FOR CORE → INSTALL REGISTRY (строго последовательно:
// стадии меняют state machine ноды). При --resume уже завершённые стадии
// (по маркеру И артефакту) пропускаются; при --retry-stage — запускается
// только указанная стадия (если её ещё нет). Перед каждой стадией — предикт
// артефакта: если его нет и маркер есть — пишем предупреждение (артефакт
// могли удалить руками) и перезапускаем.
func nodeChain(c *Ctx, w io.Writer, nodeID string) error {
	stages := nodeStages()
	// --retry-stage: фильтруем до одной стадии
	if c.O.RetryStage != "" {
		var filtered []stageSpec
		for _, s := range stages {
			if s.Key == c.O.RetryStage {
				filtered = append(filtered, s)
			}
		}
		if len(filtered) == 0 {
			return fmt.Errorf("--retry-stage=%s: неизвестная стадия (допустимо: %s, %s, %s)",
				c.O.RetryStage, stageInit, stagePrepareForCore, stageInstallRegistry)
		}
		stages = filtered
	}

	for _, stage := range stages {
		markerExists := sys.FileExists(c, c.Ex, stageMarkerPath(stage.Key))
		artifactOK := stage.HasArtifact(c)
		if artifactOK && (markerExists || c.O.Resume) {
			fmt.Fprintf(w, "%s: артефакт на месте — пропускаю%s\n",
				stage.Label, func() string { if markerExists { return " (по маркеру)" }; return " (--resume)" }())
			writeStageMarker(c, stage.Key)
			continue
		}
		if markerExists && !artifactOK {
			fmt.Fprintf(w, "WARN: %s: маркер есть, но артефакта нет — перезапускаю\n", stage.Label)
			_ = os.Remove(stageMarkerPath(stage.Key))
		}
		fmt.Fprintf(w, "%s...\n", stage.Label)
		if _, err := gql(c, w, fmt.Sprintf("mutation($id: String!) { %s(id: $id) }", stage.Mutation),
			map[string]any{"id": nodeID}); err != nil {
			return fmt.Errorf("%s: %w\nПодсказка: %s", stage.Label, err, stage.OnFailureHint)
		}
		if err := waitNodeRunning(c, w, nodeID, stage.Label); err != nil {
			return err
		}
		// post-check: артефакт должен появиться (ansible смог перезаписать)
		if !stage.HasArtifact(c) {
			return fmt.Errorf("%s: ansible завершился (нода running), но артефакт не создан\n"+
				"Это значит ansible вернул exit 0, но задача не выполнилась — смотри:\n  %s\n"+
				"Либо покажи последний лог: installer --info",
				stage.Label, stage.OnFailureHint)
		}
		writeStageMarker(c, stage.Key)
	}
	return nil
}

// dbmsChain — DBMS из connectionString + системное приложение
// (infrastructureType DatabaseSystem), как setupDbms в install.ts.
func dbmsChain(c *Ctx, w io.Writer) error {
	// существующие DBMS/приложения
	dbmsData, err := gql(c, w, "{ getDbmss { id name } }", nil)
	if err != nil {
		return err
	}
	var dbmsList struct {
		GetDbmss []struct {
			Name string `json:"name"`
		} `json:"getDbmss"`
	}
	if err := json.Unmarshal(dbmsData, &dbmsList); err != nil {
		return err
	}
	dbmsName := c.O.DBName // install.ts: имя = БД из connectionString
	exists := false
	for _, d := range dbmsList.GetDbmss {
		if d.Name == dbmsName {
			exists = true
		}
	}
	if !exists {
		if _, err := gql(c, w, "mutation($values: DbmsInput!) { createDbms(values: $values) { id } }",
			map[string]any{"values": map[string]any{
				"name": dbmsName, "type": "postgres", "host": "localhost",
				"user": c.O.DBUser, "password": c.O.DBPass,
			}}); err != nil {
			return fmt.Errorf("createDbms: %w", err)
		}
		fmt.Fprintf(w, "DBMS %s создан (postgres@localhost)\n", dbmsName)
	}

	apps, err := gql(c, w, "{ getAllApp { id name } }", nil)
	if err != nil {
		return err
	}
	var appList struct {
		GetAllApp []struct {
			Name string `json:"name"`
		} `json:"getAllApp"`
	}
	if err := json.Unmarshal(apps, &appList); err != nil {
		return err
	}
	for _, a := range appList.GetAllApp {
		if a.Name == dbmsName {
			return nil // системное приложение уже есть
		}
	}
	if _, err := gql(c, w, "mutation($input: AppInput!) { installApp(input: $input) }",
		map[string]any{"input": map[string]any{
			"name": dbmsName, "description": dbmsName,
			"infrastructureType": "DatabaseSystem", "system": true,
		}}); err != nil {
		return fmt.Errorf("installApp(dbms): %w", err)
	}
	fmt.Fprintf(w, "приложение %s предустановлено (database_system)\n", dbmsName)
	return nil
}

// deployGUIApp — GUI как приложение платформы (app-режим):
// repo → installApp → image → build → configuration → version → instance → swarm update.
func deployGUIApp(c *Ctx, w io.Writer, nodeID string) error {
	const appName = "megapolos-gui"

	apps, err := gql(c, w, "{ getAllApp { id name } }", nil)
	if err != nil {
		return err
	}
	var appList struct {
		GetAllApp []struct {
			ID, Name string
		} `json:"getAllApp"`
	}
	if err := json.Unmarshal(apps, &appList); err != nil {
		return err
	}
	for _, a := range appList.GetAllApp {
		if a.Name == appName {
			fmt.Fprintln(w, "приложение megapolos-gui уже есть — докатываю swarm")
			_, err := gql(c, w, "mutation($id: String!) { updateNode(id: $id, init: false, withRebuild: false, containerIds: []) }",
				map[string]any{"id": nodeID})
			return err
		}
	}

	// repo get-or-create
	repos, err := gql(c, w, "{ getAllRepository { id name } }", nil)
	if err != nil {
		return err
	}
	var repoList struct {
		GetAllRepository []struct {
			ID, Name string
		} `json:"getAllRepository"`
	}
	if err := json.Unmarshal(repos, &repoList); err != nil {
		return err
	}
	repoID := ""
	for _, r := range repoList.GetAllRepository {
		if r.Name == appName {
			repoID = r.ID
		}
	}
	if repoID == "" {
		res, err := gql(c, w, "mutation($values: RepositoryInput!) { createRepository(values: $values) { id } }",
			map[string]any{"values": map[string]any{
				"name": appName, "url": srcURL(c, "megapolos-gui"), "repositoryType": "remote",
			}})
		if err != nil {
			return fmt.Errorf("createRepository: %w", err)
		}
		var created struct {
			CreateRepository struct {
				ID string `json:"id"`
			} `json:"createRepository"`
		}
		if err := json.Unmarshal(res, &created); err != nil {
			return err
		}
		repoID = created.CreateRepository.ID
	}

	if _, err := gql(c, w, "mutation($input: AppInput!) { installApp(input: $input) }",
		map[string]any{"input": map[string]any{
			"name": appName, "description": appName,
			"infrastructureType": "Application", "system": true,
		}}); err != nil {
		return fmt.Errorf("installApp(gui): %w", err)
	}
	apps2, err := gql(c, w, "{ getAllApp { id name } }", nil)
	if err != nil {
		return err
	}
	appList.GetAllApp = nil
	if err := json.Unmarshal(apps2, &appList); err != nil {
		return err
	}
	appID := ""
	for _, a := range appList.GetAllApp {
		if a.Name == appName {
			appID = a.ID
		}
	}
	if appID == "" {
		return fmt.Errorf("приложение %s не найдено после installApp", appName)
	}

	// image
	res, err := gql(c, w, "mutation($values: ImageInput!) { createImage(values: $values) { id } }",
		map[string]any{"values": map[string]any{
			"app": appID, "name": appName, "image": appName,
			"innerPort": 80, "buildNumber": 1, "repository": repoID, "branch": c.O.GUIRef,
		}})
	if err != nil {
		return fmt.Errorf("createImage: %w", err)
	}
	var img struct {
		CreateImage struct {
			ID string `json:"id"`
		} `json:"createImage"`
	}
	if err := json.Unmarshal(res, &img); err != nil {
		return err
	}
	imageID := img.CreateImage.ID

	fmt.Fprintln(w, "сборка образа GUI (docker build + push в registry)…")
	if _, err := gql(c, w, "mutation($imageId: String!) { buildImage(imageId: $imageId) }",
		map[string]any{"imageId": imageID}); err != nil {
		return fmt.Errorf("buildImage: %w", err)
	}
	for i := 0; i < 120; i++ {
		data, err := apiQuery(c, c.O.Token, "query($id: String!) { getImage(id: $id) { status } }",
			map[string]any{"id": imageID})
		if err == nil {
			var st struct {
				GetImage struct {
					Status string `json:"status"`
				} `json:"getImage"`
			}
			if json.Unmarshal(data, &st) == nil && st.GetImage.Status == "Built" {
				goto built
			}
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("образ GUI не собрался за 10 минут")
built:

	conf, err := gql(c, w, "mutation($appId: String!, $configurationData: ConfigurationDataInput!) { createConfiguration(appId: $appId, configurationData: $configurationData) { id } }",
		map[string]any{"appId": appID, "configurationData": map[string]any{
			"name": "default", "services": []any{},
		}})
	if err != nil {
		return fmt.Errorf("createConfiguration: %w", err)
	}
	var confParsed struct {
		CreateConfiguration struct {
			ID string `json:"id"`
		} `json:"createConfiguration"`
	}
	if err := json.Unmarshal(conf, &confParsed); err != nil {
		return err
	}

	ver, err := gql(c, w, "mutation($d: AppVersionInput!, $images: [AppVersionImageInput!]!) { createAppVersion(appVersionData: $d, images: $images) { id } }",
		map[string]any{
			"d":      map[string]any{"app": appID, "configuration": confParsed.CreateConfiguration.ID, "buildNumber": 1, "version": "1.0.0"},
			"images": []any{map[string]any{"imageId": imageID}},
		})
	if err != nil {
		return fmt.Errorf("createAppVersion: %w", err)
	}
	var verParsed struct {
		CreateAppVersion struct {
			ID string `json:"id"`
		} `json:"createAppVersion"`
	}
	if err := json.Unmarshal(ver, &verParsed); err != nil {
		return err
	}

	// API для браузера: явно заданный --api-url имеет приоритет; иначе в devMode —
	// api.megapolos.localhost (виртхост ноды на :443), в проде — <BaseDomain>:5104
	// (nginx.core.prod.template слушает 5104 ssl, на 443 api-виртхоста нет).
	apiURL := c.O.APIURL
	if apiURL == "" {
		if c.O.DevMode {
			apiURL = "https://api.megapolos.localhost"
		} else {
			apiURL = "https://" + c.O.BaseDomain + ":5104"
		}
	}
	container := map[string]any{
		"name": appName, "role": "app", "node": nodeID, "image": imageID,
		"outerPort": 3000, "volumes": []any{}, "dbs": []any{},
		"envs": []any{map[string]any{"name": "MEGAPOLOS_SERVER", "value": apiURL}},
	}
	if c.O.GUIDomain != "" {
		container["domain"] = map[string]any{"domainData": map[string]any{"name": c.O.GUIDomain}}
	}
	if _, err := gql(c, w, "mutation($d: InstanceDataInput!, $v: ID!) { createConfiguratedInstance(instanceData: $d, appVersionId: $v) { id } }",
		map[string]any{
			"d": map[string]any{"name": appName, "containers": []any{container}},
			"v": verParsed.CreateAppVersion.ID,
		}); err != nil {
		return fmt.Errorf("createConfiguratedInstance: %w", err)
	}
	if _, err := gql(c, w, "mutation($id: String!) { updateNode(id: $id, init: false, withRebuild: false, containerIds: []) }",
		map[string]any{"id": nodeID}); err != nil {
		return err
	}
	fmt.Fprintf(w, "приложение megapolos-gui задеплоено на https://%s (API %s)\n", c.O.GUIDomain, apiURL)
	return nil
}

// bootstrapStep: платформенная оркестрация через GraphQL (порт install.ts).
// Идёт ПОСЛЕ token (нужен запущенный API). Маркер — чтобы не гонять ansible
// при повторных запусках без смены режима.
func bootstrapStep() Step {
	mode := func(o *Opts) string {
		switch {
		case !o.GUI:
			return "none"
		case o.GUIApp:
			return "app"
		default:
			return "static"
		}
	}
	marker := func(o *Opts) string { return filepath.Join(o.InstallDir, ".megapolos-bootstrap") }
	return StepFunc{
		N: "bootstrap",
		D: []string{"token", "swarm", "ansible", "docker-images"},
		DetectF: func(c *Ctx) (bool, string) {
			mark := mode(c.O) + "|graphql|localhost"
			b, err := os.ReadFile(marker(c.O))
			if err == nil && string(b) == mark {
				return true, "bootstrap уже выполнен (режим " + mode(c.O) + ")"
			}
			return false, ""
		},
		RunF: func(c *Ctx, w io.Writer) error {
			// платформа ходит на ноду по ssh2 (порт 22 зашит): нужен root-вход по паролю
			if err := sh(c, w, fmt.Sprintf("echo 'root:%s' | chpasswd", c.O.NodeRootPassword)); err != nil {
				return err
			}
			if err := writeFile(c, w, "/etc/ssh/sshd_config.d/60-megapolos-root.conf", RenderSshdDropin(), 0o644, ""); err != nil {
				return err
			}
			if err := sh(c, w, "rm -f /etc/ssh/sshd_config.d/50-cloud-init.conf /etc/ssh/sshd_config.d/60-cloudimg-settings.conf"); err != nil {
				return err
			}
			if err := sh(c, w, "sshd -t && (systemctl reload ssh || systemctl reload sshd)"); err != nil {
				return err
			}

			nodeID, err := ensureNode(c, w)
			if err != nil {
				return err
			}
			if err := ensureRegistry(c, w); err != nil {
				return err
			}

			// цепочка ноды (ansible, долго) ∥ DBMS/preset (БД-записи, быстро)
			var wg sync.WaitGroup
			errs := make([]error, 2)
			wg.Add(2)
			go func() { defer wg.Done(); errs[0] = nodeChain(c, w, nodeID) }()
			go func() { defer wg.Done(); errs[1] = dbmsChain(c, w) }()
			wg.Wait()
			for _, e := range errs {
				if e != nil {
					return e
				}
			}

			// платформа помечает ноду running даже при провале ansible-задач
			// (jammy: pip-таска падала молча) — проверяем артефакты стадий явно
			if !outOK(c, "docker ps --format '{{.Names}}' | grep -qx nginx") {
				return fmt.Errorf("INIT: контейнер nginx не поднялся (см. journalctl -u megapolos-core)")
			}
			if !sys.FileExists(c, c.Ex, "/data/nginx/conf/core.conf") {
				return fmt.Errorf("PREPARE FOR CORE: нет /data/nginx/conf/core.conf")
			}
			if !sys.FileExists(c, c.Ex, "/data/registry/docker-compose.yml") {
				return fmt.Errorf("INSTALL REGISTRY: нет /data/registry/docker-compose.yml")
			}
			fmt.Fprintln(w, "артефакты стадий на месте (nginx-контейнер, core.conf, registry compose)")

			if c.O.GUIApp {
				if err := deployGUIApp(c, w, nodeID); err != nil {
					return err
				}
			}

			if err := os.WriteFile(marker(c.O), []byte(mode(c.O)+"|graphql|localhost"), 0o644); err != nil {
				return err
			}
			fmt.Fprintf(w, "bootstrap завершён (режим %s)\n", mode(c.O))
			return nil
		},
	}
}

// ansibleStep: плейбуки платформы требуют ansible-core >= 2.14
// (community.general.ansible_galaxy_install и др.); jammy тащит ansible 2.10
// из репо — слишком старый. Ставим ansible-core 2.16 в venv /opt/ansible-venv
// (оффлайн — wheels из bundle/pip), коллекции community.* — из
// bundle/ansible-collections или galaxy. Симлинки в /usr/local/bin перекрывают
// дистрибутивный /usr/bin/ansible* (PATH).
func ansibleStep() Step {
	venv := "/opt/ansible-venv"
	ansibleTooOld := func(c *Ctx) bool {
		// noble: "ansible [core 2.16.x]"; jammy: "ansible 2.10.8" (legacy)
		v, err := out(c, "PATH="+venv+"/bin:$PATH ansible --version 2>/dev/null | head -1")
		if err != nil || v == "" {
			return true
		}
		var major, minor int
		if strings.Contains(v, "core") {
			fmt.Sscanf(v[strings.Index(v, "core")+5:], "%d.%d", &major, &minor)
		} else {
			fmt.Sscanf(strings.TrimPrefix(v, "ansible "), "%d.%d", &major, &minor)
		}
		return major*100+minor < 214
	}
	return StepFunc{
		N: "ansible", D: []string{"packages"},
		DetectF: func(c *Ctx) (bool, string) {
			if ansibleTooOld(c) {
				return false, ""
			}
			if outOK(c, "ansible-doc community.general.ansible_galaxy_install >/dev/null 2>&1") {
				return true, "ansible-core >= 2.14, коллекции на месте"
			}
			return false, ""
		},
		RunF: func(c *Ctx, w io.Writer) error {
			if !ansibleTooOld(c) {
				fmt.Fprintln(w, "ansible достаточно свежий, только коллекции")
			} else {
				fmt.Fprintln(w, "дистрибутивный ansible слишком старый — ставлю ansible-core 2.16 в venv")
				shTolerant(c, w, "apt-get update") // lists могли протухнуть/быть пустыми
				if err := sh(c, w, aptCmd+" install python3-venv python3-pip"); err != nil {
					return err
				}
				pipArgs := "ansible-core==2.16.14"
				if c.O.BundleDir != "" && sys.FileExists(c, c.Ex, filepath.Join(c.O.BundleDir, "pip")) {
					pipArgs = "--no-index --find-links " + filepath.Join(c.O.BundleDir, "pip") + " ansible-core==2.16.14"
				}
				if err := sh(c, w, fmt.Sprintf(
					"python3 -m venv %s && %s/bin/pip install %s", venv, venv, pipArgs)); err != nil {
					return err
				}
				if err := sh(c, w, fmt.Sprintf(
					"for b in ansible ansible-playbook ansible-galaxy ansible-doc; do ln -sf %s/bin/$b /usr/local/bin/$b; done", venv)); err != nil {
					return err
				}
				// плейбуки платформы делают pip install с --break-system-packages —
				// jammy pip 22.0.2 флаг не знает. Обновляем системный pip.
				pipUp := "pip3 install --upgrade pip setuptools wheel"
				if c.O.BundleDir != "" && sys.FileExists(c, c.Ex, filepath.Join(c.O.BundleDir, "pip")) {
					pipUp = "pip3 install --no-index --find-links " + filepath.Join(c.O.BundleDir, "pip") + " --upgrade pip setuptools wheel"
				}
				if err := sh(c, w, pipUp); err != nil {
					return err
				}
				if err := sh(c, w, "pip3 --version && python3 -m pip install --help | grep -q break-system-packages"); err != nil {
					return fmt.Errorf("системный pip не обновился (--break-system-packages недоступен): %w", err)
				}
			}
			// коллекции: из бандла (оффлайн) или galaxy (онлайн)
			bundleCol := filepath.Join(c.O.BundleDir, "ansible-collections")
			if c.O.BundleDir != "" && sys.FileExists(c, c.Ex, bundleCol) {
				if err := sh(c, w, "/usr/local/bin/ansible-galaxy collection install "+bundleCol+"/*.tar.gz"); err != nil {
					return err
				}
			} else {
				if err := sh(c, w, "for i in 1 2 3; do ansible-galaxy collection install community.general:9.5.8 community.docker:4.3.1 community.crypto:2.22.3 && exit 0; sleep 5; done; exit 1"); err != nil {
					return err
				}
			}
			// venv ansible не видит dist-packages коллекции — но они нам и не нужны,
			// galaxy ставит в ~/.ansible/collections (root)
			return sh(c, w, "ansible-doc community.general.ansible_galaxy_install >/dev/null")
		},
	}
}
