// megapolos-installer — установщик Megapolos (megapolos-core + megapolos-gui).
//
// Запускается ВНУТРИ целевой машины (Ubuntu 24.04 / Debian 12) от root.
//
//	curl -fsSL http://<host>:8000/installer -o /tmp/installer && sudo /tmp/installer
//	sudo /mnt/megapolos-bundle/installer            # из смонтированного sqfs-бандла
//	sudo ./installer --no-tui --yes                 // полностью без вопросов
//
// Режимы: TUI (tview) при наличии терминала; headless по --no-tui или без tty.
// Шаги идемпотентны (Detect перед Run), исполняются параллельно по DAG (-jobs).
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"megapolos/installer/internal/steps"
	"megapolos/installer/internal/sys"
	"megapolos/installer/internal/tui"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

// probe — жив ли HTTP-сервис (любой ответ, даже 404).
func probe(url string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

// randomHex — криптостойкая hex-строка n байт.
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// readVMEnv — KEY=VALUE из cloud-init write_files (проброшенные порты QEMU).
func readVMEnv(path string) map[string]string {
	out := map[string]string{}
	b, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok && k != "" {
			out[k] = v
		}
	}
	return out
}

// detectLANIP — внешний IPv4 машины: адрес исходящего интерфейса
// (udp-Dial не шлёт пакеты, только спрашивает маршрут), fallback — первый
// не-loopback IPv4 интерфейса. "" если ничего не нашли.
func detectLANIP() string {
	if conn, err := net.Dial("udp", "10.0.2.2:8000"); err == nil {
		defer conn.Close()
		if a, ok := conn.LocalAddr().(*net.UDPAddr); ok && !a.IP.IsLoopback() {
			return a.IP.String()
		}
	}
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && ipn.IP.To4() != nil {
			return ipn.IP.String()
		}
	}
	return ""
}

// existingConfig — secret и пароль БД из существующего config.json (идемпотентность).
func existingConfig(coreDir string) (secret, dbPass string) {
	b, err := os.ReadFile(filepath.Join(coreDir, "config", "config.json"))
	if err != nil {
		return "", ""
	}
	var cfg struct {
		Secret           string `json:"secret"`
		ConnectionString string `json:"connectionString"`
	}
	if json.Unmarshal(b, &cfg) != nil {
		return "", ""
	}
	secret = cfg.Secret
	// postgres://user:pass@host:port/db
	s := cfg.ConnectionString
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
		if at := strings.LastIndex(s, "@"); at >= 0 {
			up := s[:at]
			if c := strings.Index(up, ":"); c >= 0 {
				dbPass = up[c+1:]
			}
		}
	}
	return secret, dbPass
}

// installerCfgPath — путь к сохранённому конфигу установщика (для --resume).
const installerCfgPath = "/var/lib/megapolos/installer.cfg"

// saveInstallerCfg — сохраняет ключевые opts в JSON-файл (для --resume).
// Используется после успешной установки.
func saveInstallerCfg(opts *steps.Opts) {
	if err := os.MkdirAll("/var/lib/megapolos", 0o755); err != nil {
		return
	}
	cfg := struct {
		Source         string
		CoreRef        string
		GUIRef         string
		APIURL         string
		GUI            bool
		GUIApp         bool
		GUIDomain      string
		GUITLS         bool
		Standalone     bool
		BaseDomain     string
		RepoPackages   bool
		ForceCompat    bool
		DevMode        bool
		DBName         string
		DBUser         string
		InstallDir     string
		NodeRootPass   string
		Swap           string
		HostIP         string
		// Секреты: нужны, чтобы восстановиться после потери config.json
		// (например удалили /opt/megapolos) при живой БД — иначе новый
		// пароль роли ≠ пароль в БД → 28P01.
		Secret string
		DBPass string
	}{
		Source: opts.Source, CoreRef: opts.CoreRef, GUIRef: opts.GUIRef,
		APIURL: opts.APIURL, GUI: opts.GUI, GUIApp: opts.GUIApp,
		GUIDomain: opts.GUIDomain, GUITLS: opts.GUITLS, Standalone: opts.Standalone,
		BaseDomain: opts.BaseDomain, RepoPackages: opts.RepoPackages,
		ForceCompat: opts.ForceCompat, DevMode: opts.DevMode,
		DBName: opts.DBName, DBUser: opts.DBUser,
		InstallDir: opts.InstallDir, NodeRootPass: opts.NodeRootPassword,
		Swap: opts.Swap, HostIP: opts.HostIP,
		Secret: opts.Secret, DBPass: opts.DBPass,
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	_ = os.WriteFile(installerCfgPath, b, 0o600)
}

// loadInstallerSecrets — secret и пароль БД из сохранённого installer.cfg.
// Нужны как fallback, когда config.json потерян, а БД/роль ещё живы.
func loadInstallerSecrets() (secret, dbPass string) {
	b, err := os.ReadFile(installerCfgPath)
	if err != nil {
		return "", ""
	}
	var cfg struct {
		Secret string
		DBPass string
	}
	if json.Unmarshal(b, &cfg) != nil {
		return "", ""
	}
	return cfg.Secret, cfg.DBPass
}

// loadInstallerCfg — загружает ранее сохранённый конфиг (для --resume).
// CLI-флаги имеют приоритет (отмечены через flag.Visit).
// Возвращает map с КЛЮЧАМИ FLAG-имён (lower-case), не JSON-ключами.
func loadInstallerCfg(explicit map[string]bool) map[string]string {
	data, err := os.ReadFile(installerCfgPath)
	if err != nil {
		return nil
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil
	}
	// JSON-ключ → flag-имя
	flagName := map[string]string{
		"Source": "source", "CoreRef": "core-ref", "GUIRef": "gui-ref",
		"APIURL": "api-url", "GUI": "gui", "GUIApp": "gui-app",
		"GUIDomain": "gui-domain", "GUITLS": "gui-tls", "Standalone": "standalone",
		"BaseDomain": "base-domain", "RepoPackages": "repo-packages",
		"ForceCompat": "force-compatibility", "DevMode": "dev-mode",
		"DBName": "db-name", "DBUser": "db-user", "InstallDir": "dir",
		"NodeRootPass": "node-root-password", "Swap": "swap", "HostIP": "host-ip",
	}
	out := make(map[string]string)
	for jsonKey, v := range cfg {
		fName, ok := flagName[jsonKey]
		if !ok {
			continue
		}
		if explicit[fName] {
			continue // CLI перебил
		}
		switch x := v.(type) {
		case string:
			if x != "" {
				out[fName] = x
			}
		case bool:
			if x {
				out[fName] = "true"
			}
		}
	}
	return out
}

// applyCfgToFlag — если значение в карте есть — установить его в flag-var.
func applyCfgToFlag(name, val string, set func(string)) {
	if val == "" {
		return
	}
	set(val)
}

// rawGraphQL — простой POST к GraphQL endpoint без retry. Используется
// в detectFromRunningCore (разовая проверка из main до старта установки).
func rawGraphQL(endpoint, token, query string, variables map[string]any) (json.RawMessage, error) {
	body, _ := json.Marshal(map[string]any{"query": query, "variables": variables})
	req, err := http.NewRequest("POST", endpoint, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("graphql %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return b, nil
}

// detectFromRunningCore — пытается вытащить base-domain / gui-domain / api-url
// из живого ядра через GraphQL. Возвращает map с ключами "baseDomain",
// "guiDomain", "apiURL" (пусто если ничего нет).
func detectFromRunningCore() map[string]string {
	out := map[string]string{}
	token, err := os.ReadFile("/root/megapolos-token.txt")
	if err != nil {
		return out
	}
	tok := strings.TrimSpace(string(token))
	if tok == "" {
		return out
	}
	// base-domain: getAllDomain с флагом isBaseDomain
	data, err := rawGraphQL("http://127.0.0.1:5100", tok,
		`{ getAllDomain { id name isBaseDomain } }`, nil)
	if err == nil {
		var parsed struct {
			GetAllDomain []struct {
				Name         string `json:"name"`
				IsBaseDomain bool   `json:"isBaseDomain"`
			} `json:"getAllDomain"`
		}
		if json.Unmarshal(data, &parsed) == nil {
			for _, d := range parsed.GetAllDomain {
				if d.IsBaseDomain {
					out["baseDomain"] = d.Name
					break
				}
			}
			// GUI-домен: ищем "gui.<baseDomain>"
			if base, ok := out["baseDomain"]; ok {
				guiGuess := "gui." + base
				for _, d := range parsed.GetAllDomain {
					if d.Name == guiGuess {
						out["guiDomain"] = guiGuess
					}
				}
			}
		}
	}
	// API URL: читаем /opt/megapolos/megapolos-core/.env (MEGAPOLOS_SERVER)
	if envData, err := os.ReadFile("/opt/megapolos/megapolos-core/.env"); err == nil {
		for _, line := range strings.Split(string(envData), "\n") {
			if strings.HasPrefix(line, "MEGAPOLOS_SERVER=") {
				out["apiURL"] = strings.Trim(strings.TrimPrefix(line, "MEGAPOLOS_SERVER="), "\"' ")
			}
		}
	}
	return out
}

// existingInstallPrompt — интерактивный выбор при обнаружении существующей установки.
// Возвращает действие: "resume", "edit", "wipe", или пустую строку (выход).
func existingInstallPrompt(baseDomain, guiDomain, apiURL string) string {
	fmt.Println()
	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║     ОБНАРУЖЕНА СУЩЕСТВУЮЩАЯ УСТАНОВКА MEGAPOLOS              ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")
	fmt.Println()
	if baseDomain != "" {
		fmt.Printf("  Базовый домен:      %s\n", baseDomain)
	}
	if guiDomain != "" {
		fmt.Printf("  Домен GUI:          %s\n", guiDomain)
	}
	if apiURL != "" {
		fmt.Printf("  API URL:            %s\n", apiURL)
	}
	fmt.Println()
	fmt.Println("  Выберите действие:")
	fmt.Println()
	fmt.Println("  1) Восстановить с этими настройками  (--resume)")
	fmt.Println("     Продолжить установку с прежними параметрами.")
	fmt.Println()
	fmt.Println("  2) Поменять настройки и восстановить")
	fmt.Println("     Запустить мастер настройки с текущими значениями.")
	fmt.Println()
	fmt.Println("  3) Переустановить с нуля  (--wipe --reset-db)")
	fmt.Println("     Удалить всё и установить заново.")
	fmt.Println()
	fmt.Print("> Введите номер [1]: ")

	// Читаем с терминала: при запуске через `curl | bash` (или с
	// перенаправленным stdin) fmt.Scanln(os.Stdin) мгновенно получает EOF и
	// молча берёт дефолт (resume) — выглядит как «не спрашивает, сразу чинит».
	in := os.Stdin
	if f, err := os.OpenFile("/dev/tty", os.O_RDONLY, 0); err == nil {
		defer f.Close()
		in = f
	}
	var choice string
	fmt.Fscanln(in, &choice)
	if choice == "" {
		choice = "1"
	}
	switch choice {
	case "1":
		return "resume"
	case "2":
		return "edit"
	case "3":
		return "wipe"
	default:
		return "resume"
	}
}

func main() {
	var (
		coreRef      = flag.String("core-ref", envOr("MEGAPOLOS_CORE_REF", "main"), "ветка/тег/sha megapolos-core")
		guiRef       = flag.String("gui-ref", envOr("MEGAPOLOS_GUI_REF", "main"), "ветка/тег/sha megapolos-gui")
		source       = flag.String("source", envOr("MEGAPOLOS_SOURCE", "auto"), "bundle | gitlab | local | auto | <URL или путь к репо>")
		sourceCustom = flag.String("source-custom", envOr("MEGAPOLOS_SOURCE_CUSTOM", ""), "URL/путь источника при --source custom (или впиши прямо в --source)")
		apiURL       = flag.String("api-url", envOr("MEGAPOLOS_API_URL", ""), "URL API для GUI (пусто = http://<lan-ip>:5100)")
		guiOn        = flag.Bool("gui", envOr("MEGAPOLOS_GUI", "true") != "false", "ставить GUI на эту машину (nginx :8080)")
		guiApp       = flag.Bool("gui-app", envOr("MEGAPOLOS_GUI_APP", "false") == "true", "GUI как приложение платформы (домен+серты), а не статический nginx")
		guiDomain    = flag.String("gui-domain", envOr("MEGAPOLOS_GUI_DOMAIN", ""), "домен GUI-приложения (пусто = gui.<base-domain>)")
		guiTLS       = flag.Bool("gui-tls", envOr("MEGAPOLOS_GUI_TLS", "true") != "false", "HTTPS для GUI (серт Megapolos Root CA, :4443)")
		standalone   = flag.Bool("standalone", envOr("MEGAPOLOS_STANDALONE", "false") == "true", "независимый деплой: 1 нода, GUI-app, домены из base-domain, прод-режим")
		wipe         = flag.Bool("wipe", envOr("MEGAPOLOS_WIPE", "false") == "true", "очистить предыдущую установку перед стартом")
		resetDB      = flag.Bool("reset-db", envOr("MEGAPOLOS_RESET_DB", "false") == "true", "сбросить БД при wipe (dropdb + dropuser + пересоздать)")
		repoPackages = flag.Bool("repo-packages", envOr("MEGAPOLOS_REPO_PACKAGES", "false") == "true", "пакеты из репозиториев (без bundle-debs)")
		forceCompat = flag.Bool("force-compatibility", envOr("MEGAPOLOS_FORCE_COMPAT", "false") == "true", "пропустить проверку совместимости бандла")
		devMode      = flag.Bool("dev-mode", envBool("MEGAPOLOS_DEV_MODE", true), "devMode (все контейнеры на localhost)")
		debug        = flag.Bool("debug", envBool("MEGAPOLOS_DEBUG", false), "debug-логи ядра")
		dbName       = flag.String("db-name", envOr("MEGAPOLOS_DB_NAME", "megapolos"), "имя БД")
		dbUser       = flag.String("db-user", envOr("MEGAPOLOS_DB_USER", "megapolos"), "пользователь БД")
		dir          = flag.String("dir", envOr("MEGAPOLOS_DIR", "/opt/megapolos"), "каталог установки")
		bundle       = flag.String("bundle", envOr("MEGAPOLOS_BUNDLE_DIR", ""), "каталог оффлайн-бандла (пусто = автопоиск)")
		selfNode     = flag.String("self-node", envOr("MEGAPOLOS_ADD_SELF_NODE", "auto"), "true/false/auto: добавить этот хост как ноду")
		nodePass     = flag.String("node-root-password", envOr("MEGAPOLOS_NODE_ROOT_PASSWORD", "megapolos"), "пароль root для SSH себя-ноды")
		baseDomain   = flag.String("base-domain", envOr("MEGAPOLOS_BASE_DOMAIN", "megapolos.local"), "базовый домен инстансов (пусто = не создавать)")
		swapMode     = flag.String("swap", envOr("MEGAPOLOS_SWAP", "auto"), "swap: auto (только при RAM<8G) | force | skip")
		jobs         = flag.Int("jobs", 2, "максимум параллельных шагов (1 = строго последовательно)")
		yes          = flag.Bool("yes", false, "принять все значения по умолчанию")
		noTUI        = flag.Bool("no-tui", false, "без TUI (текстовый вывод)")
		hostIP       = flag.String("host-ip", envOr("MEGAPOLOS_HOST_IP", ""), "IP хоста с кэшами/зеркалом (пусто = vm.env HOST_IP, иначе 10.0.2.2)")
		showVersion  = flag.Bool("version", false, "версия и выход")
		printCmd     = flag.Bool("print-command", false, "вывести exact command для воспроизведения и выйти")
resume       = flag.Bool("resume", false, "продолжить установку: пропустить уже завершённые стадии (по маркерам и артефактам)")
	retryStage   = flag.String("retry-stage", "", "повторить только стадию: init|prepare-for-core|install-registry (без --resume)")
	showInfo     = flag.Bool("info", false, "показать последние логи ansible (стадии платформы) и выйти")
	doctor       = flag.Bool("doctor", false, "диагностика существующей установки (что есть/чего нет) и рекомендация: --resume | --retry-stage | --wipe")
	)
	flag.Parse()

	// Go flag останавливается на первом не-флаге: если юзер случайно передал
	// первым аргументом сам бинарник (напр. `install.sh | bash -s -- megapolos-installer --wipe`),
	// все последующие --флаги молча игнорируются и установка идёт с дефолтами.
	// Лучше упасть с понятной ошибкой, чем тихо поставить не то.
	if rest := flag.Args(); len(rest) > 0 {
		fmt.Fprintf(os.Stderr, "FAIL: лишние аргументы без флага: %v\n", rest)
		fmt.Fprintln(os.Stderr, "Похоже, первым аргументом передан сам бинарник — тогда все --флаги после него игнорируются.")
		fmt.Fprintln(os.Stderr, "Правильно:  curl -fsSL <install.sh> | bash -s -- --standalone --base-domain=example.com ...")
		os.Exit(2)
	}
	if strings.HasPrefix(*selfNode, "-") {
		fmt.Fprintf(os.Stderr, "FAIL: --self-node получил значение флага %q — пропущено значение.\n", *selfNode)
		fmt.Fprintln(os.Stderr, "Строковый флаг без '=' съедает следующий аргумент: используй --self-node=true|false|auto.")
		os.Exit(2)
	}

	// Если есть сохранённый конфиг (от прошлого запуска) и юзер не передал
	// соответствующий флаг явно — подставляем значения из файла. Это критично
	// для --resume / curl|bash без флагов: иначе base-domain сбросится на
	// дефолт и установщик создаст «левый» домен рядом с существующим.
	explicit := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	if cfg := loadInstallerCfg(explicit); cfg != nil {
		applyCfgToFlag("source", cfg["source"], func(v string) { *source = v })
		applyCfgToFlag("core-ref", cfg["core-ref"], func(v string) { *coreRef = v })
		applyCfgToFlag("gui-ref", cfg["gui-ref"], func(v string) { *guiRef = v })
		applyCfgToFlag("api-url", cfg["api-url"], func(v string) { *apiURL = v })
		applyCfgToFlag("gui-domain", cfg["gui-domain"], func(v string) { *guiDomain = v })
		applyCfgToFlag("base-domain", cfg["base-domain"], func(v string) { *baseDomain = v })
		applyCfgToFlag("db-name", cfg["db-name"], func(v string) { *dbName = v })
		applyCfgToFlag("db-user", cfg["db-user"], func(v string) { *dbUser = v })
		applyCfgToFlag("dir", cfg["dir"], func(v string) { *dir = v })
		applyCfgToFlag("node-root-password", cfg["node-root-password"], func(v string) { *nodePass = v })
		applyCfgToFlag("swap", cfg["swap"], func(v string) { *swapMode = v })
		applyCfgToFlag("host-ip", cfg["host-ip"], func(v string) { *hostIP = v })
		// bool-флаги (ключи — flag-имена, lower-case)
		if v, ok := cfg["gui"]; ok { *guiOn = v != "false" }
		if v, ok := cfg["gui-app"]; ok { *guiApp = v != "false" }
		if v, ok := cfg["gui-tls"]; ok { *guiTLS = v != "false" }
		if v, ok := cfg["standalone"]; ok { *standalone = v != "false" }
		if v, ok := cfg["repo-packages"]; ok { *repoPackages = v != "false" }
		if v, ok := cfg["force-compatibility"]; ok { *forceCompat = v != "false" }
		if v, ok := cfg["dev-mode"]; ok { *devMode = v != "false" }
		fmt.Fprintf(os.Stderr, "[INFO] применён сохранённый конфиг: %s\n", installerCfgPath)
	}

	// Согласование GUI-флагов: gui-app не может быть включён при выключенном gui.
	// Иначе `--resume --gui=false` на слабой машине подхватывает сохранённый
	// gui-app=true и пытается деплоить GUI-приложение без образа (swarm deploy fail).
	if !*guiOn {
		*guiApp = false
	}

	// Если cfg-файла нет (напр. установка упала до первого сохранения),
	// пробуем вытащить существующие значения из живого ядра через GraphQL.
	// Это позволяет --resume подхватить прошлый --base-domain без явного флага.
	if _, err := os.Stat(installerCfgPath); err != nil {
		if detected := detectFromRunningCore(); len(detected) > 0 {
			fmt.Fprintf(os.Stderr, "[INFO] обнаружены параметры существующей установки: %v\n", detected)
			if v, ok := detected["baseDomain"]; ok && !explicit["base-domain"] {
				*baseDomain = v
			}
			if v, ok := detected["guiDomain"]; ok && !explicit["gui-domain"] {
				*guiDomain = v
			}
			if v, ok := detected["apiURL"]; ok && !explicit["api-url"] {
				*apiURL = v
			}
		}
	}

	// Интерактивный выбор при обнаружении существующей установки.
	// Показываем только если:
	// - есть следы установки (cfg-файл ИЛИ маркеры стадий)
	// - не в специальных режимах (--doctor, --info, --print-command)
	// - явно не попросили --wipe
	hasExisting := false
	if _, err := os.Stat(installerCfgPath); err == nil {
		hasExisting = true
	}
	if !hasExisting {
		for _, key := range []string{"init", "prepare-for-core", "install-registry"} {
			if _, err := os.Stat("/var/lib/megapolos/stage-" + key + ".done"); err == nil {
				hasExisting = true
				break
			}
		}
	}
	autoResume := os.Getenv("MEGAPOLOS_AUTO_RESUME") == "1"
	// Показываем интерактивный выбор если:
	// - есть следы установки И
	// - не в специальных режимах (--doctor, --info...) И
	// - не вызвано с --wipe И
	// - (авто-добавлен --resume.install.sh) ИЛИ (не подавлен интерактив --yes/--noTUI)
	skipPrompt := !hasExisting || *doctor || *showInfo || *printCmd || *wipe || (*resume && !autoResume) || *yes || *noTUI
	if !skipPrompt {
		action := existingInstallPrompt(*baseDomain, *guiDomain, *apiURL)
		switch action {
		case "resume":
			*resume = true
			*yes = true
			*noTUI = true
		case "edit":
			*resume = true
			*yes = false
			*noTUI = false
		case "wipe":
			*wipe = true
			*resetDB = true
			*yes = true
			*noTUI = true
		default:
			fmt.Println("Отменено.")
			os.Exit(0)
		}
	}

	// Вайп (флагом или выбранный в меню) = установка с нуля: не тащим
	// домены/API-URL из сохранённого конфига, если они не заданы явно.
	// Иначе после --wipe всплывает прежний base-domain (напр. megapolos.local).
	if *wipe {
		if !explicit["base-domain"] {
			*baseDomain = envOr("MEGAPOLOS_BASE_DOMAIN", "megapolos.local")
		}
		if !explicit["gui-domain"] {
			*guiDomain = envOr("MEGAPOLOS_GUI_DOMAIN", "")
		}
		if !explicit["api-url"] {
			*apiURL = envOr("MEGAPOLOS_API_URL", "")
		}
	}

	if *showVersion {
		fmt.Println("megapolos-installer dev")
		return
	}

	if *printCmd {
		fmt.Println(buildCommandLine(*coreRef, *guiRef, *source, *sourceCustom, *apiURL,
			*guiDomain, *dbName, *dbUser, *dir, *bundle, *selfNode, "***", *baseDomain, *swapMode, *hostIP,
			*guiOn, *guiApp, *guiTLS, *standalone, *wipe, *resetDB, *repoPackages, *forceCompat,
			*devMode, *debug, *yes, *noTUI))
		return
	}
	if *doctor {
		_, code := steps.Doctor(os.Stderr)
		os.Exit(code)
	}
	if *showInfo {
		os.Exit(steps.ShowInfo(os.Stderr))
	}
	// --resume: сначала диагностика, потом — если нашли сломанную стадию —
	// подставляем --retry-stage, чтобы не переустанавливать всё с нуля.
	// При --wipe не идём в resume-ветку: иначе doctor увидит ещё живую старую
	// установку и выйдет «нечего продолжать» до того, как отработает wipe
	// (в т.ч. когда install.sh авто-добавил --resume).
	if *resume && *retryStage == "" && !*wipe {
		report, code := steps.Doctor(os.Stderr)
		if code == 0 && report.AllStagesDone && report.APIHealthy {
			fmt.Fprintln(os.Stderr, "установка завершена — нечего продолжать")
			os.Exit(0)
		}
		if !report.APIHealthy {
			fmt.Fprintf(os.Stderr, "\n>>> токен/API нездоров — продолжаю установку (шаг token перевыпустит токен)\n\n")
		}
		if report.FirstBroken != "" {
			fmt.Fprintf(os.Stderr, "\n>>> авто-выбор: --retry-stage=%s\n\n", report.FirstBroken)
			*retryStage = report.FirstBroken
		}
		// идём дальше в обычный main() — nodeChain увидит RetryStage
	}
	// Recovery-режимы (--resume / --retry-stage): если явно передано
	// (не авто-добавлено install.sh) — берём дефолты без вопросов.
	// Если MEGAPOLOS_AUTO_RESUME=1 (авто-добавлено) — показываем интерактивный
	// выбор, чтобы юзер мог поменять настройки или выбрать переустановку.
	if (*resume || *retryStage != "") && !autoResume {
		*yes = true
		*noTUI = true
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "FAIL: запусти от root: sudo installer")
		os.Exit(1)
	}
	if _, err := os.Stat("/usr/bin/apt-get"); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL: нужен apt (Debian/Ubuntu)")
		os.Exit(1)
	}

	// --- автодетект бандла (рядом с бинарём или в PWD лежит debs/) ---
	bundleDir := *bundle
	if bundleDir == "" {
		exeDir := ""
		if exe, err := os.Executable(); err == nil {
			exeDir = filepath.Dir(exe)
		}
		for _, d := range []string{".", exeDir} {
			if fi, err := os.Stat(filepath.Join(d, "debs")); err == nil && fi.IsDir() {
				abs, _ := filepath.Abs(d)
				bundleDir = abs
				break
			}
		}
	}

	// --- автодетект кэшей хоста ---
	// bridge-VM: хост доступен по LAN-IP (записан в vm.env при create-vm.sh);
	// user-net (slirp): 10.0.2.2 из vm.env. БЕЗ vm.env/флага зеркала нет —
	// на целевой машине вне стенда это норма (bundle/gitlab).
	vmEnv := readVMEnv("/etc/megapolos-vm.env")
	hostIPVal := *hostIP
	if hostIPVal == "" {
		hostIPVal = vmEnv["HOST_IP"] // пусто = зеркала нет (КИИ-сценарий)
	}
	aptProxy, npmRegistry := "", ""
	if hostIPVal != "" {
		if probe("http://" + hostIPVal + ":3142") {
			aptProxy = "http://" + hostIPVal + ":3142"
		}
		if probe("http://" + hostIPVal + ":4873") {
			npmRegistry = "http://" + hostIPVal + ":4873"
		}
	}

	// --- источник репозиториев: bundle | gitlab | local | auto | <url|path> ---
	src, srcErr := steps.ResolveSource(*source, *sourceCustom, bundleDir, hostIPVal, probe)
	if srcErr != nil {
		fmt.Fprintln(os.Stderr, "FAIL: "+srcErr.Error())
		os.Exit(1)
	}
	gitBase := src.Base

	hostname, _ := os.Hostname()
	lanIP := detectLANIP()
	// API URL: явный флаг/env > проброс QEMU (localhost:API_PORT) > LAN IP > localhost.
	// В QEMU user-net гостевой IP (10.0.2.15) с хоста недоступен — только проброс.
	apiURLVal := *apiURL
	if apiURLVal == "" && !*standalone && !*guiApp {
		// автодетект ТОЛЬКО для devMode/локальной отладки;
		// в standalone/gui-app bootstrap сам вычислит HTTPS URL
		switch {
		case vmEnv["NET"] != "bridge" && vmEnv["API_PORT"] != "":
			apiURLVal = "http://localhost:" + vmEnv["API_PORT"]
		case lanIP != "":
			apiURLVal = "http://" + lanIP + ":5100"
		default:
			apiURLVal = "http://localhost:5100"
		}
	}
	secret, dbPass := existingConfig(filepath.Join(*dir, "megapolos-core"))
	// Recovery без wipe: config.json мог быть потерян (удалили каталог), но
	// БД/роль живы. Берём секреты из installer.cfg, иначе новый пароль роли
	// не совпадёт с БД → 28P01. При --wipe БД пересоздаётся — можно свежие.
	if (secret == "" || dbPass == "") && !*wipe {
		if s2, p2 := loadInstallerSecrets(); s2 != "" && p2 != "" {
			if secret == "" {
				secret = s2
			}
			if dbPass == "" {
				dbPass = p2
			}
		}
	}
	if secret == "" {
		secret = randomHex(32)
	}
	if dbPass == "" {
		dbPass = randomHex(16)
	}

	guiDomainVal := *guiDomain
	if guiDomainVal == "" && *guiApp {
		guiDomainVal = "gui." + *baseDomain
	}

	opts := &steps.Opts{
		GitBase:          gitBase,
		Source:           *source,
		CoreRef:          *coreRef,
		GUIRef:           *guiRef,
		APIURL:           apiURLVal,
		DevMode:          *devMode,
		Debug:            *debug,
		DBName:           *dbName,
		DBUser:           *dbUser,
		DBPass:           dbPass,
		Secret:           secret,
		InstallDir:       *dir,
		BundleDir:        bundleDir,
		AptProxy:         aptProxy,
		NpmRegistry:      npmRegistry,
		NodeRootPassword: *nodePass,
		BaseDomain:       *baseDomain,
		GUI:              *guiOn || *guiApp,
		GUIApp:           *guiApp,
		GUIDomain:        guiDomainVal,
		GUITLS:           *guiTLS,
		Standalone:       *standalone,
		Wipe:             *wipe,
		ResetDB:          *resetDB,
		RepoPackages:     *repoPackages,
		ForceCompat:      *forceCompat,
		LANIP:            lanIP,
		Swap:             *swapMode,
		SvcUser:          "megapolos",
		HostIP:           hostIPVal,
		SrcKind:          src.Kind,
		SrcHuman:         src.Human,
		Hostname:         hostname,
		NodeMajor:        18,
		PgMajor:          16,
		Resume:           *resume,
		RetryStage:       *retryStage,
	}
	// hostfwd-порты имеют смысл только в user-net (в bridge VM доступна по LAN-IP)
	if vmEnv["NET"] != "bridge" {
		opts.VMGUIPort = vmEnv["GUI_PORT"]
		opts.VMGUITLSPort = vmEnv["GUI_TLS_PORT"]
		opts.VMAPIPort = vmEnv["API_PORT"]
	}
	switch strings.ToLower(*selfNode) {
	case "true", "yes", "1":
		opts.AddSelfNode = true
	case "false", "no", "0":
		opts.AddSelfNode = false
	default:
		opts.AddSelfNode = true // auto → да (и в TUI вопрос стоит по умолчанию «да»)
	}
	if opts.Standalone {
		// Независимый деплой: 1 нода, GUI как приложение платформы,
		// домены — из base-domain, self-signed CA (devMode=true, certbot не нужен).
		// Но явный --gui=false / --gui-app=false (например, слабая машина) важнее.
		if !explicit["gui"] && !explicit["gui-app"] {
			opts.GUI = true
			opts.GUIApp = true
		}
		opts.AddSelfNode = true
		opts.DevMode = true
		if opts.BaseDomain != "" {
			opts.GUIDomain = "gui." + opts.BaseDomain
			if opts.APIURL == "" {
				opts.APIURL = "https://" + opts.BaseDomain + ":5104"
			}
		}
	}

	// TUI только при живом терминале
	tty := !*noTUI && !*yes
	if tty {
		if fi, err := os.Stat("/dev/tty"); err != nil || fi == nil {
			tty = false
		} else if f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0); err != nil {
			tty = false
		} else {
			f.Close()
		}
	}

	if tty {
		if err := tui.Run(opts, *jobs); err != nil {
			fmt.Fprintln(os.Stderr, "FAIL:", err)
			os.Exit(1)
		}
		return
	}

	// ---- headless ----
	if bundleDir != "" {
		fmt.Println("==> бандл-режим:", bundleDir)
	}
	if aptProxy != "" {
		fmt.Println("==> apt-кэш:", aptProxy)
	}
	if npmRegistry != "" {
		fmt.Println("==> npm-зеркало:", npmRegistry)
	}
	fmt.Printf("==> источник: %s [%s] (core@%s, gui@%s), jobs=%d\n", opts.SrcHuman, gitBase, opts.CoreRef, opts.GUIRef, *jobs)

	ui := steps.NewHeadlessUI()
	r := &steps.Runner{
		Ctx:   &steps.Ctx{Context: context.Background(), Ex: sys.Real{}, O: opts},
		Steps: steps.All(opts),
		Jobs:  *jobs,
		UI:    ui,
	}
	if err := r.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		os.Exit(1)
	}
	// Сохраняем конфиг для --resume / повторных curl|bash.
	saveInstallerCfg(opts)
	fmt.Println("\n==> ГОТОВО")
	ip := opts.LANIP
	if ip == "" {
		ip = "127.0.0.1"
	}
	if opts.GUI {
		if opts.GUIApp {
			fmt.Printf("  GUI (приложение):        https://%s/\n", opts.GUIDomain)
		} else {
			if opts.VMGUIPort != "" {
				fmt.Printf("  GUI (через проброс VM):  http://localhost:%s/\n", opts.VMGUIPort)
				if opts.GUITLS && opts.VMGUITLSPort != "" {
					fmt.Printf("  GUI HTTPS:               https://localhost:%s/\n", opts.VMGUITLSPort)
				}
			}
			// static GUI: гость 8080/4443 (80/443 занимает nginx-контейнер ноды)
			fmt.Printf("  GUI (LAN/внутри):        http://%s:8080/", ip)
			if opts.GUITLS {
				fmt.Printf("  и  https://%s:4443/", ip)
			}
		}
		fmt.Println()
	}
	fmt.Printf("  API:    %s\n", opts.APIURL)
	fmt.Printf("  CA:     %s/api/ca/download (скачать и добавить в доверенные)\n", opts.APIURL)
	if opts.Token != "" {
		fmt.Printf("  Токен:  %s\n", opts.Token)
	}
	fmt.Println("          (также /root/megapolos-token.txt)")
	if opts.BaseDomain != "" {
		fmt.Printf("  Домен:  %s — DNS: *.%s → %s (или /etc/hosts)\n", opts.BaseDomain, opts.BaseDomain, ip)
	}
	if opts.AddSelfNode {
		fmt.Printf("  Нода:   %s → 127.0.0.1 (root)\n", hostname)
	}
	fmt.Println("  Логи:   journalctl -u megapolos-core -f")
}

func buildCommandLine(coreRef, guiRef, source, sourceCustom, apiURL, guiDomain, dbName, dbUser, dir, bundle, selfNode, nodePass, baseDomain, swapMode, hostIP string, guiOn, guiApp, guiTLS, standalone, wipe, resetDB, repoPackages, forceCompat, devMode, debug, yes, noTUI bool) string {
	var args []string
	add := func(name, val string) {
		if val == "" || val == "false" {
			return
		}
		if val == "true" {
			args = append(args, "--"+name)
			return
		}
		args = append(args, "--"+name+"="+val)
	}
	addb := func(name string, val bool) {
		if val {
			args = append(args, "--"+name)
		}
	}
	if coreRef != "main" {
		add("core-ref", coreRef)
	}
	if guiRef != "main" {
		add("gui-ref", guiRef)
	}
	if source != "auto" {
		add("source", source)
	}
	if sourceCustom != "" {
		add("source-custom", sourceCustom)
	}
	if apiURL != "" {
		add("api-url", apiURL)
	}
	addb("gui", guiOn)
	addb("gui-app", guiApp)
	if guiDomain != "" {
		add("gui-domain", guiDomain)
	}
	addb("gui-tls", guiTLS)
	addb("standalone", standalone)
	addb("wipe", wipe)
	addb("reset-db", resetDB)
	addb("repo-packages", repoPackages)
	addb("force-compatibility", forceCompat)
	addb("dev-mode", devMode)
	addb("debug", debug)
	if dbName != "megapolos" {
		add("db-name", dbName)
	}
	if dbUser != "megapolos" {
		add("db-user", dbUser)
	}
	if dir != "/opt/megapolos" {
		add("dir", dir)
	}
	if bundle != "" {
		add("bundle", bundle)
	}
	if selfNode != "auto" {
		add("self-node", selfNode)
	}
	if nodePass != "megapolos" {
		add("node-root-password", "***")
	}
	if baseDomain != "megapolos.local" {
		add("base-domain", baseDomain)
	}
	if swapMode != "auto" {
		add("swap", swapMode)
	}
	if hostIP != "" {
		add("host-ip", hostIP)
	}
	addb("yes", yes)
	addb("no-tui", noTUI)
	return "megapolos-installer " + strings.Join(args, " ")
}
