package steps

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"megapolos/installer/internal/sys"
)

// ---- helpers ---------------------------------------------------------------

const aptCmd = "apt-get -y -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold"

var aptEnv = []string{"DEBIAN_FRONTEND=noninteractive"}

func sh(c *Ctx, w io.Writer, cmd string) error {
	fmt.Fprintf(w, "$ %s\n", cmd)
	return c.Ex.Run(c, sys.RunOpts{Cmd: cmd, Env: aptEnv}, w)
}

// shTolerant — выполнить, при ошибке только предупредить (как `|| warn` в bash).
func shTolerant(c *Ctx, w io.Writer, cmd string) {
	if err := sh(c, w, cmd); err != nil {
		fmt.Fprintf(w, "WARN: %s: %v\n", cmd, err)
	}
}

func out(c *Ctx, cmd string) (string, error) {
	return c.Ex.Output(c, sys.RunOpts{Cmd: cmd})
}

func outOK(c *Ctx, cmd string) bool {
	_, err := out(c, cmd)
	return err == nil
}

// asSvc — команда от сервисного юзера (sudo -u megapolos -H bash -c).
func asSvc(c *Ctx, w io.Writer, cmd string) error {
	fmt.Fprintf(w, "$ [as %s] %s\n", c.O.SvcUser, cmd)
	return c.Ex.Run(c, sys.RunOpts{Cmd: cmd, User: c.O.SvcUser}, w)
}

func writeFile(c *Ctx, w io.Writer, path, content string, perm os.FileMode, owner string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		return err
	}
	fmt.Fprintf(w, "записан %s\n", path)
	if owner != "" {
		return sh(c, w, fmt.Sprintf("chown %s %s", owner, path))
	}
	return nil
}

// srcURL — откуда клонировать репозиторий.
func srcURL(c *Ctx, repo string) string {
	if c.O.BundleDir != "" {
		p := filepath.Join(c.O.BundleDir, "repos", repo+".git")
		if sys.FileExists(c, c.Ex, p) {
			return p
		}
	}
	if strings.HasPrefix(c.O.GitBase, "http://"+c.O.HostIP) {
		return c.O.GitBase + "/" + repo + "/.git" // dumb-http зеркало
	}
	return c.O.GitBase + "/" + repo + ".git"
}

// npmOffline — флаг --offline для npm, если бандл с кэшем.
func npmOffline(c *Ctx) string {
	if c.O.BundleDir == "" {
		return ""
	}
	if sys.FileExists(c, c.Ex, filepath.Join(c.O.BundleDir, "npm-cache.tar.gz")) ||
		sys.FileExists(c, c.Ex, filepath.Join(c.O.BundleDir, "npm-cache-root.tar.gz")) {
		return "--offline"
	}
	return ""
}

// needSwap — RAM < 8G → swap нужен (vite build ест до 3G heap).
func needSwap(c *Ctx) bool {
	out, err := c.Ex.Output(c, sys.RunOpts{Cmd: "awk '/MemTotal/ {print $2}' /proc/meminfo"})
	if err != nil {
		return true
	}
	kb, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return true
	}
	return kb < 8*1024*1024
}

// shaMarker — маркер «npm install/build сделан для этого коммита».
func shaMarkerPath(dir string) string { return filepath.Join(dir, ".megapolos-sha") }

func headSHA(c *Ctx, dir string) string {
	sha, err := c.Ex.Output(c, sys.RunOpts{Cmd: "git rev-parse HEAD", Dir: dir})
	if err != nil {
		return ""
	}
	return sha
}

func shaMarkerDetect(c *Ctx, dir, extra string) (bool, string) {
	if !sys.FileExists(c, c.Ex, filepath.Join(dir, "node_modules")) {
		return false, ""
	}
	marker, err := os.ReadFile(shaMarkerPath(dir))
	if err != nil {
		return false, ""
	}
	cur := headSHA(c, dir)
	if cur != "" && strings.TrimSpace(string(marker)) == cur {
		if extra == "" || sys.FileExists(c, c.Ex, extra) {
			return true, "node_modules актуальны для " + cur[:8]
		}
	}
	return false, ""
}

func writeSHAMarker(c *Ctx, dir string) {
	if sha := headSHA(c, dir); sha != "" {
		_ = os.WriteFile(shaMarkerPath(dir), []byte(sha+"\n"), 0o644)
	}
}

// tailBuf — кольцевой буфер последних ~8 КБ вывода (для детекта фаталок npm).
type tailBuf struct{ b []byte }

func (t *tailBuf) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > 8192 {
		t.b = append([]byte(nil), t.b[len(t.b)-8192:]...)
	}
	return len(p), nil
}

// npmRetry — 3 попытки npm (сеть флакает); прерванный install оставляет
// недокачанные пакеты в node_modules → чистим перед повтором.
// На попытку — 20 мин: node-pre-gyp/node-gyp качают с nodejs.org мимо npm-таймаутов
// и могут виснуть на чёрных дырах в сети (наблюдали ESTABLISHED без трафика).
// ENOTCACHED — детерминированная ошибка кэша: повторять бессмысленно, выходим сразу.
func npmRetry(c *Ctx, w io.Writer, dir string, args string) error {
	var err error
	for i := 1; i <= 3; i++ {
		attemptCtx, cancel := context.WithTimeout(c, 20*time.Minute)
		attempt := &Ctx{Context: attemptCtx, Ex: c.Ex, O: c.O}
		var tail tailBuf
		err = asSvc(attempt, io.MultiWriter(w, &tail), fmt.Sprintf("cd %s && npm %s", dir, args))
		cancel()
		if err == nil {
			return nil
		}
		if bytes.Contains(tail.b, []byte("ENOTCACHED")) {
			return fmt.Errorf("npm %s: ENOTCACHED — оффлайн-кэш неполон или registry не совпадает с ключами кэша: %w", args, err)
		}
		fmt.Fprintf(w, "WARN: npm %s — попытка %d/3 неудачна (%v), повтор через 15с\n", args, i, err)
		_ = asSvc(c, w, "rm -rf "+dir+"/node_modules")
		select {
		case <-c.Done():
			return c.Err()
		case <-time.After(15 * time.Second):
		}
	}
	return fmt.Errorf("npm %s: 3 попытки исчерпаны: %w", args, err)
}

// cloneOrPull — клонировать/обновить репозиторий и переключиться на ref
// (ветка, тег или sha коммита).
func cloneOrPull(repo, ref string) func(*Ctx, io.Writer) error {
	return func(c *Ctx, w io.Writer) error {
		dir := filepath.Join(c.O.InstallDir, repo)
		if err := sh(c, w, fmt.Sprintf("mkdir -p %s && chown %s:%s %s", c.O.InstallDir, c.O.SvcUser, c.O.SvcUser, c.O.InstallDir)); err != nil {
			return err
		}
		url := srcURL(c, repo)
		if c.O.BundleDir != "" && !strings.Contains(url, "://") {
			// sqfs из контейнерной сборки имеет владельца root:root, а клонируем
			// под megapolos → git safe.directory (git>=2.35 «dubious ownership»)
			for _, p := range []string{url, url + "/.git"} {
				shTolerant(c, w, fmt.Sprintf("sudo -u %s git config --global --add safe.directory %s", c.O.SvcUser, p))
			}
		}
		if sys.FileExists(c, c.Ex, filepath.Join(dir, ".git")) {
			shTolerant(c, w, "") // noop для читаемости лога
			if err := asSvc(c, w, fmt.Sprintf("cd %s && git fetch --all --tags --prune", dir)); err != nil {
				fmt.Fprintf(w, "WARN: git fetch: %v (оффлайн? продолжаю на локальной копии)\n", err)
			}
		} else {
			if err := sh(c, w, "rm -rf "+dir); err != nil {
				return err
			}
			fmt.Fprintf(w, "клонирую %s → %s\n", url, dir)
			if err := asSvc(c, w, fmt.Sprintf("git clone --progress %s %s", url, dir)); err != nil {
				return err
			}
		}
		// checkout ref; pull только для веток (для sha/тега ff-only упадёт — это норма)
		if err := asSvc(c, w, fmt.Sprintf("cd %s && git checkout %s && (git pull --ff-only origin %s || true)", dir, ref, ref)); err != nil {
			return fmt.Errorf("ref %q не найден в %s: %w", ref, repo, err)
		}
		return asSvc(c, w, fmt.Sprintf("cd %s && echo 'установлен ревизия:' $(git rev-parse HEAD) && git log -1 --oneline", dir))
	}
}

// ---- шаги ------------------------------------------------------------------

// All строит полный список шагов установки.
func All(o *Opts) []Step {
	coreDir := filepath.Join(o.InstallDir, "megapolos-core")
	guiDir := filepath.Join(o.InstallDir, "megapolos-gui")

	steps := []Step{
		StepFunc{
			N: "bundle-debs", D: nil,
			DetectF: func(c *Ctx) (bool, string) {
				if c.O.BundleDir == "" || !sys.FileExists(c, c.Ex, filepath.Join(c.O.BundleDir, "debs")) {
					return true, "бандл не найден — пакеты из репозиториев/кэша"
				}
				return false, ""
			},
			RunF: func(c *Ctx, w io.Writer) error {
				fmt.Fprintf(w, "бандл-режим: dpkg -i %s/debs/*.deb (без сети)\n", c.O.BundleDir)
				// dpkg ставит ровно те версии, что в бандле (apt лез бы в сеть за новыми)
				shTolerant(c, w, fmt.Sprintf("dpkg -i %s/debs/*.deb >/dev/null 2>&1", c.O.BundleDir))
				shTolerant(c, w, aptCmd+" install -f")
				return nil
			},
		},
		StepFunc{
			N: "base", D: []string{"bundle-debs"},
			DetectF: func(c *Ctx) (bool, string) {
				pkgs := []string{"curl", "ca-certificates", "gnupg", "lsb-release", "git", "build-essential", "python3", "openssl"}
				if c.O.GUI {
					pkgs = append(pkgs, "nginx") // nginx нужен только для отдачи GUI
				}
				for _, p := range pkgs {
					if !sys.DpkgInstalled(c, c.Ex, p) {
						return false, ""
					}
				}
				return true, "базовые пакеты уже установлены"
			},
			RunF: func(c *Ctx, w io.Writer) error {
				if c.O.AptProxy != "" {
					if err := writeFile(c, w, "/etc/apt/apt.conf.d/99megapolos-proxy", RenderAptProxy(c.O.AptProxy), 0o644, ""); err != nil {
						return err
					}
					fmt.Fprintf(w, "apt через кэш %s\n", c.O.AptProxy)
				}
				shTolerant(c, w, "apt-get update")
				pkgs := "curl ca-certificates gnupg lsb-release git build-essential python3 openssl"
				if c.O.GUI {
					pkgs += " nginx" // nginx нужен только для отдачи GUI
				}
				return sh(c, w, aptCmd+" install "+pkgs)
			},
		},
		StepFunc{
			N: "swap", D: nil,
			DetectF: func(c *Ctx) (bool, string) {
				if c.O.Swap == "skip" {
					return true, "отключено (--swap skip)"
				}
				if outOK(c, "swapon --show | grep -q .") {
					return true, "swap уже есть"
				}
				if c.O.Swap != "force" && !needSwap(c) {
					return true, "RAM ≥ 8G — swap не нужен"
				}
				return false, ""
			},
			RunF: func(c *Ctx, w io.Writer) error {
				if outOK(c, "swapon --show | grep -q .") {
					fmt.Fprintln(w, "swap уже есть — пропускаю")
					return nil
				}
				if c.O.Swap == "skip" {
					fmt.Fprintln(w, "swap отключён (--swap skip)")
					return nil
				}
				if c.O.Swap != "force" && !needSwap(c) {
					fmt.Fprintln(w, "RAM ≥ 8G — swap не создаю")
					return nil
				}
				if err := sh(c, w, "fallocate -l 2G /swapfile && chmod 600 /swapfile && mkswap /swapfile && swapon /swapfile"); err != nil {
					return err
				}
				return sh(c, w, "grep -q '^/swapfile' /etc/fstab || echo '/swapfile none swap sw 0 0' >> /etc/fstab")
			},
		},
		packagesStep(),
		userStep(),
		swarmStep(),
		dockerImagesStep(),
		cloneStep("megapolos-core", coreDir),
		dbStep(coreDir),
		npmStep("npm:core", coreDir, "megapolos-core", "package-lock.core.json", "install"),
		coreConfigStep(coreDir),
		tokenStep(),
	}
	if o.AddSelfNode {
		// bootstrap = платформенная оркестрация из install.ts:
		// нода → INIT → PREPARE FOR CORE → INSTALL REGISTRY → (опц.) деплой GUI-приложения
		steps = append(steps, bootstrapStep())
	}
	if o.GUI && !o.GUIApp {
		// static GUI: клон/сборка фронта и nginx на этой машине
		steps = append(steps,
			cloneStep("megapolos-gui", guiDir),
			guiConfigStep(guiDir),
			guiNpmStep(guiDir),
			guiBuildStep(guiDir),
		)
		if o.GUITLS {
			steps = append(steps, guiTLSStep(coreDir, guiDir))
		}
	}
	steps = append(steps, systemdStep(coreDir, guiDir, o.GUI && !o.GUIApp))
	if o.BaseDomain != "" {
		steps = append(steps, baseDomainStep())
	}
	return steps
}

// packagesStep: node18 + postgresql16 + docker + ansible (apt сериализован внутри).
func packagesStep() Step {
	nodeOK := func(c *Ctx) bool {
		v, err := out(c, "node -v 2>/dev/null | sed 's/v\\([0-9]*\\).*/\\1/'")
		if err != nil || v == "" {
			return false
		}
		var major int
		fmt.Sscanf(v, "%d", &major)
		return major >= c.O.NodeMajor
	}
	return StepFunc{
		N: "packages", D: []string{"base"},
		DetectF: func(c *Ctx) (bool, string) {
			if nodeOK(c) &&
				sys.DpkgInstalled(c, c.Ex, fmt.Sprintf("postgresql-%d", c.O.PgMajor)) &&
				sys.CommandExists(c, c.Ex, "docker") && sys.CommandExists(c, c.Ex, "ansible") &&
				sys.DpkgInstalled(c, c.Ex, "docker-compose-v2") &&
				sys.DpkgInstalled(c, c.Ex, "python3-docker") && sys.DpkgInstalled(c, c.Ex, "python3-passlib") {
				return true, "node/postgres/docker/ansible уже на месте"
			}
			return false, ""
		},
		RunF: func(c *Ctx, w io.Writer) error {
			// --- node ---
			if !nodeOK(c) {
				shTolerant(c, w, aptCmd+" install nodejs npm")
				if !nodeOK(c) {
					fmt.Fprintf(w, "в дистрибутиве старый node, ставлю NodeSource %d\n", c.O.NodeMajor)
					if err := sh(c, w, fmt.Sprintf("curl -fsSL https://deb.nodesource.com/setup_%d.x | bash -", c.O.NodeMajor)); err != nil {
						return err
					}
					if err := sh(c, w, aptCmd+" install nodejs"); err != nil {
						return err
					}
				}
			}
			if !nodeOK(c) {
				return fmt.Errorf("node %d+ не установлен", c.O.NodeMajor)
			}
			shTolerant(c, w, "node -v && npm -v")
			// устойчивость npm к флакающей сети (root)
			for _, kv := range []string{"fetch-retries=5", "fetch-retry-mintimeout=10000", "fetch-retry-maxtimeout=120000", "fetch-timeout=600000"} {
				kv2 := strings.SplitN(kv, "=", 2)
				if err := sh(c, w, fmt.Sprintf("npm config set %s %s", kv2[0], kv2[1])); err != nil {
					return err
				}
			}
			// verdaccio — только в онлайне. С бандл-кэшем registry ОБЯЗАН быть
			// npmjs.org: ключи cacache привязаны к URL реестра, verdaccio-URL
			// в кэше нет → ENOTCACHED. А --offline (only-if-cached) вообще
			// запрещает сетевые запросы, verdaccio не поможет.
			if c.O.NpmRegistry != "" && npmOffline(c) == "" {
				if err := sh(c, w, "npm config set registry "+c.O.NpmRegistry); err != nil {
					return err
				}
				fmt.Fprintf(w, "npm через зеркало %s\n", c.O.NpmRegistry)
			} else {
				shTolerant(c, w, "npm config delete registry")
			}
			// глобальный nodemon/ts-node НЕ нужны: prod/bootstrap берут ts-node
			// из локальных node_modules core (npm run резолвит .bin)
			// --- postgresql ---
			pgPkg := fmt.Sprintf("postgresql-%d", c.O.PgMajor)
			if !sys.DpkgInstalled(c, c.Ex, pgPkg) {
				if err := sh(c, w, "install -d /usr/share/keyrings"); err != nil {
					return err
				}
				if !sys.FileExists(c, c.Ex, "/usr/share/keyrings/pgdg.gpg") {
					if err := sh(c, w, "curl -fsSL https://www.postgresql.org/media/keys/ACCC4CF8.asc | gpg --dearmor -o /usr/share/keyrings/pgdg.gpg"); err != nil {
						return err
					}
				}
				if err := sh(c, w, `echo "deb [signed-by=/usr/share/keyrings/pgdg.gpg] http://apt.postgresql.org/pub/repos/apt $(lsb_release -cs)-pgdg main" > /etc/apt/sources.list.d/pgdg.list`); err != nil {
					return err
				}
				shTolerant(c, w, "apt-get update")
				if err := sh(c, w, aptCmd+" install "+pgPkg); err != nil {
					return err
				}
			}
			if err := sh(c, w, "systemctl enable --now postgresql"); err != nil {
				return err
			}
			// --- docker + ansible ---
			// docker-compose-v2: registry платформы поднимается через docker compose
			if !sys.CommandExists(c, c.Ex, "docker") || !sys.CommandExists(c, c.Ex, "ansible") ||
				!sys.DpkgInstalled(c, c.Ex, "docker-compose-v2") {
				if err := sh(c, w, aptCmd+" install docker.io docker-compose-v2 ansible"); err != nil {
					return err
				}
			}
			// python-зависимости для ansible-модулей платформы (docker, htpasswd, crypto)
			if err := sh(c, w, aptCmd+" install python3-docker python3-passlib python3-cryptography python3-jsondiff"); err != nil {
				return err
			}
			return sh(c, w, "systemctl enable --now docker")
		},
	}
}

// userStep: сервисный юзер megapolos.
func userStep() Step {
	return StepFunc{
		N: "user", D: []string{"packages"},
		DetectF: func(c *Ctx) (bool, string) {
			if outOK(c, "id "+c.O.SvcUser) && sys.FileExists(c, c.Ex, "/etc/sudoers.d/90-"+c.O.SvcUser) {
				return true, "пользователь " + c.O.SvcUser + " существует"
			}
			return false, ""
		},
		RunF: func(c *Ctx, w io.Writer) error {
			if !outOK(c, "id "+c.O.SvcUser) {
				if err := sh(c, w, "useradd -m -s /bin/bash "+c.O.SvcUser); err != nil {
					return err
				}
			}
			home := "/home/" + c.O.SvcUser
			if err := sh(c, w, fmt.Sprintf("mkdir -p %s && chown %s:%s %s", home, c.O.SvcUser, c.O.SvcUser, home)); err != nil {
				return err
			}
			if err := sh(c, w, "usermod -aG docker "+c.O.SvcUser); err != nil {
				return err
			}
			// ansible-плейбуки ядра используют become: yes → sudo без пароля
			if err := writeFile(c, w, "/etc/sudoers.d/90-"+c.O.SvcUser, RenderSudoers(c.O.SvcUser), 0o440, ""); err != nil {
				return err
			}
			for _, kv := range []string{"fetch-retries=5", "fetch-retry-mintimeout=10000", "fetch-retry-maxtimeout=120000", "fetch-timeout=600000"} {
				kv2 := strings.SplitN(kv, "=", 2)
				if err := asSvc(c, w, fmt.Sprintf("npm config set %s %s", kv2[0], kv2[1])); err != nil {
					return err
				}
			}
			// verdaccio только онлайн (см. комментарий в packages-шаге)
			if c.O.NpmRegistry != "" && npmOffline(c) == "" {
				if err := asSvc(c, w, "npm config set registry "+c.O.NpmRegistry); err != nil {
					return err
				}
			} else {
				_ = asSvc(c, w, "npm config delete registry || true")
			}
			if c.O.BundleDir != "" {
				cache := filepath.Join(c.O.BundleDir, "npm-cache.tar.gz")
				if sys.FileExists(c, c.Ex, cache) {
					if err := asSvc(c, w, "mkdir -p "+home); err != nil {
						return err
					}
					// внутри тарбола .npm/ — распаковываем в HOME
					if err := sh(c, w, fmt.Sprintf("tar xzf %s -C %s/", cache, home)); err != nil {
						return err
					}
					if err := sh(c, w, fmt.Sprintf("chown -R %s:%s %s/.npm", c.O.SvcUser, c.O.SvcUser, home)); err != nil {
						return err
					}
				}
				// node-gyp headers: sqlite3 компилируется из исходников,
				// без кэша node-gyp качает headers с nodejs.org (виснет при мёртвой сети)
				ngCache := filepath.Join(c.O.BundleDir, "node-gyp-cache.tar.gz")
				if sys.FileExists(c, c.Ex, ngCache) {
					fmt.Fprintln(w, "бандл-режим: разворачиваю node-gyp headers")
					if err := sh(c, w, fmt.Sprintf("tar xzf %s -C %s/", ngCache, home)); err != nil {
						return err
					}
					if err := sh(c, w, fmt.Sprintf("chown -R %s:%s %s/.cache", c.O.SvcUser, c.O.SvcUser, home)); err != nil {
						return err
					}
				}
			}
			return nil
		},
	}
}

// cloneStep: клонирование core/gui (параллельная пара).
func cloneStep(repo, dir string) Step {
	return StepFunc{
		N: "clone:" + repo, D: []string{"base", "user"},
		DetectF: func(c *Ctx) (bool, string) {
			// clone_or_pull инкрементален и быстр на зеркале — всегда выполняем,
			// но если .git уже есть, это fetch+checkout (~2с)
			return false, ""
		},
		RunF: func(c *Ctx, w io.Writer) error {
			ref := c.O.CoreRef
			if repo == "megapolos-gui" {
				ref = c.O.GUIRef
			}
			return cloneOrPull(repo, ref)(c, w)
		},
	}
}

// dbStep: роль, БД, дамп.
func dbStep(coreDir string) StepFunc {
	return StepFunc{
		N: "db", D: []string{"packages", "clone:megapolos-core"},
		DetectF: func(c *Ctx) (bool, string) {
			if !outOK(c, fmt.Sprintf("sudo -u postgres psql -tAc \"SELECT 1 FROM pg_roles WHERE rolname='%s'\" | grep -q 1", c.O.DBUser)) {
				return false, ""
			}
			if !outOK(c, fmt.Sprintf("sudo -u postgres psql -tAc \"SELECT 1 FROM pg_database WHERE datname='%s'\" | grep -q 1", c.O.DBName)) {
				return false, ""
			}
			tables, _ := out(c, fmt.Sprintf("sudo -u postgres psql -d %s -tAc \"SELECT count(*) FROM information_schema.tables WHERE table_schema='public'\"", c.O.DBName))
			if tables != "" && tables != "0" {
				return true, fmt.Sprintf("БД %s уже с %s таблицами", c.O.DBName, tables)
			}
			return false, ""
		},
		RunF: func(c *Ctx, w io.Writer) error {
			if !outOK(c, fmt.Sprintf("sudo -u postgres psql -tAc \"SELECT 1 FROM pg_roles WHERE rolname='%s'\" | grep -q 1", c.O.DBUser)) {
				if err := sh(c, w, fmt.Sprintf("sudo -u postgres psql -c \"CREATE ROLE %s SUPERUSER LOGIN PASSWORD '%s'\"", c.O.DBUser, c.O.DBPass)); err != nil {
					return err
				}
			}
			if err := sh(c, w, fmt.Sprintf("sudo -u postgres psql -c \"ALTER ROLE %s PASSWORD '%s'\"", c.O.DBUser, c.O.DBPass)); err != nil {
				return err
			}
			if !outOK(c, fmt.Sprintf("sudo -u postgres psql -tAc \"SELECT 1 FROM pg_database WHERE datname='%s'\" | grep -q 1", c.O.DBName)) {
				if err := sh(c, w, fmt.Sprintf("sudo -u postgres createdb -O %s %s", c.O.DBUser, c.O.DBName)); err != nil {
					return err
				}
			}
			tables, _ := out(c, fmt.Sprintf("sudo -u postgres psql -d %s -tAc \"SELECT count(*) FROM information_schema.tables WHERE table_schema='public'\"", c.O.DBName))
			if tables == "0" || tables == "" {
				dump := filepath.Join(c.O.BundleDir, "pg", "newpostgresql.sql")
				if c.O.BundleDir == "" || !sys.FileExists(c, c.Ex, dump) {
					dump = filepath.Join(coreDir, "install", "newpostgresql.sql")
				}
				fmt.Fprintf(w, "заливаю дамп %s\n", dump)
				// дамп снят pg_dump 18: выкидываем \restrict/\unrestrict и SET transaction_timeout (PG17+)
				sed := `sed -e '/^\\restrict/d' -e '/^\\unrestrict/d' -e '/^SET transaction_timeout/d' `
				return sh(c, w, sed+dump+" | sudo -u postgres psql -q -d "+c.O.DBName+" >/dev/null")
			}
			fmt.Fprintf(w, "в БД уже %s таблиц, дамп не заливаю\n", tables)
			return nil
		},
	}
}

// npmStep: npm install для core (и общая фабрика).
func npmStep(name, dir, repo, lockFile, installArgs string) Step {
	return StepFunc{
		N: name, D: []string{"clone:" + repo},
		DetectF: func(c *Ctx) (bool, string) { return shaMarkerDetect(c, dir, "") },
		RunF: func(c *Ctx, w io.Writer) error {
			// npm --offline требует package-lock.json, а в git его нет — берём из бандла
			if c.O.BundleDir != "" {
				lock := filepath.Join(dir, "package-lock.json")
				src := filepath.Join(c.O.BundleDir, lockFile)
				if !sys.FileExists(c, c.Ex, lock) && sys.FileExists(c, c.Ex, src) {
					if err := sh(c, w, fmt.Sprintf("cp %s %s && chown %s:%s %s", src, lock, c.O.SvcUser, c.O.SvcUser, lock)); err != nil {
						return err
					}
				}
			}
			if err := npmRetry(c, w, dir, "install "+npmOffline(c)); err != nil {
				return err
			}
			writeSHAMarker(c, dir)
			return nil
		},
	}
}

// guiNpmStep: конфиг GUI + npm install --force.
// guiConfigStep: public/config/config.json (server URL для браузера).
// Отдельный лёгкий шаг: смена --api-url перерендерит конфиг без npm install.
// Пишет и в build/ (то, что реально отдаёт nginx), если сборка уже есть.
func guiConfigStep(guiDir string) Step {
	pub := filepath.Join(guiDir, "public", "config", "config.json")
	bin := filepath.Join(guiDir, "build", "config", "config.json")
	return StepFunc{
		N: "config:gui", D: []string{"clone:megapolos-gui"},
		DetectF: func(c *Ctx) (bool, string) {
			want := RenderGUIConfig(c.O.APIURL)
			b, err := os.ReadFile(pub)
			if err != nil || string(b) != want {
				return false, ""
			}
			if bb, err := os.ReadFile(bin); err == nil && string(bb) != want {
				return false, ""
			}
			return true, "config.json актуален"
		},
		RunF: func(c *Ctx, w io.Writer) error {
			if err := writeFile(c, w, pub, RenderGUIConfig(c.O.APIURL), 0o644, c.O.SvcUser+":"+c.O.SvcUser); err != nil {
				return err
			}
			if fi, err := os.Stat(filepath.Dir(bin)); err == nil && fi.IsDir() {
				if err := writeFile(c, w, bin, RenderGUIConfig(c.O.APIURL), 0o644, c.O.SvcUser+":"+c.O.SvcUser); err != nil {
					return err
				}
			}
			fmt.Fprintf(w, "GUI config: server=%s\n", c.O.APIURL)
			return nil
		},
	}
}

func guiNpmStep(guiDir string) Step {
	return StepFunc{
		N: "npm:gui", D: []string{"clone:megapolos-gui"},
		DetectF: func(c *Ctx) (bool, string) { return shaMarkerDetect(c, guiDir, "") },
		RunF: func(c *Ctx, w io.Writer) error {
			if c.O.BundleDir != "" {
				lock := filepath.Join(guiDir, "package-lock.json")
				src := filepath.Join(c.O.BundleDir, "package-lock.gui.json")
				if !sys.FileExists(c, c.Ex, lock) && sys.FileExists(c, c.Ex, src) {
					if err := sh(c, w, fmt.Sprintf("cp %s %s && chown %s:%s %s", src, lock, c.O.SvcUser, c.O.SvcUser, lock)); err != nil {
						return err
					}
				}
			}
			if err := npmRetry(c, w, guiDir, "install --force "+npmOffline(c)); err != nil {
				return err
			}
			writeSHAMarker(c, guiDir)
			return nil
		},
	}
}

// guiBuildStep: vite build. Зависит от обоих npm (RAM: build ест до 3G).
func guiBuildStep(guiDir string) Step {
	return StepFunc{
		N: "build:gui", D: []string{"npm:gui", "npm:core"},
		DetectF: func(c *Ctx) (bool, string) {
			return shaMarkerDetect(c, guiDir, filepath.Join(guiDir, "build", "index.html"))
		},
		RunF: func(c *Ctx, w io.Writer) error {
			if err := asSvc(c, w, "cd "+guiDir+" && DISABLE_ESLINT_PLUGIN=true NODE_OPTIONS=--max-old-space-size=3072 npm run build"); err != nil {
				return err
			}
			writeSHAMarker(c, guiDir)
			return nil
		},
	}
}

// coreConfigStep: config/config.json ядра (только если отсутствует).
func coreConfigStep(coreDir string) Step {
	return StepFunc{
		N: "config:core", D: []string{"clone:megapolos-core"},
		DetectF: func(c *Ctx) (bool, string) {
			if sys.FileExists(c, c.Ex, filepath.Join(coreDir, "config", "config.json")) {
				return true, "config.json уже есть"
			}
			return false, ""
		},
		RunF: func(c *Ctx, w io.Writer) error {
			return writeFile(c, w, filepath.Join(coreDir, "config", "config.json"),
				RenderCoreConfig(c.O.Secret, c.O.DBUser, c.O.DBPass, c.O.DBName, c.O.Debug, c.O.DevMode),
				0o600, c.O.SvcUser+":"+c.O.SvcUser)
		},
	}
}

// systemdStep: юниты + nginx + (ре)старт.
func systemdStep(coreDir, guiDir string, gui bool) Step {
	deps := []string{"npm:core", "config:core"}
	if gui {
		deps = append(deps, "build:gui")
	}
	return StepFunc{
		N: "systemd", D: deps,
		DetectF: func(c *Ctx) (bool, string) {
			unit, err1 := os.ReadFile("/etc/systemd/system/megapolos-core.service")
			if err1 != nil || string(unit) != RenderCoreUnit(coreDir) ||
				!outOK(c, "systemctl is-active -q megapolos-core") {
				return false, ""
			}
			if gui {
				site, err2 := os.ReadFile("/etc/nginx/sites-available/megapolos-gui")
				if err2 != nil || string(site) != RenderNginxSite(filepath.Join(guiDir, "build")) ||
					!outOK(c, "systemctl is-active -q nginx") {
					return false, ""
				}
			}
			return true, "юниты без изменений, сервисы активны"
		},
		RunF: func(c *Ctx, w io.Writer) error {
			if err := writeFile(c, w, "/etc/systemd/system/megapolos-core.service", RenderCoreUnit(coreDir), 0o644, ""); err != nil {
				return err
			}
			cmd := "systemctl daemon-reload && systemctl enable --now megapolos-core && systemctl restart megapolos-core"
			if gui {
				if err := writeFile(c, w, "/etc/nginx/sites-available/megapolos-gui", RenderNginxSite(filepath.Join(guiDir, "build")), 0o644, ""); err != nil {
					return err
				}
				if err := sh(c, w, "ln -sf /etc/nginx/sites-available/megapolos-gui /etc/nginx/sites-enabled/megapolos-gui && rm -f /etc/nginx/sites-enabled/default"); err != nil {
					return err
				}
				cmd += " && systemctl enable --now nginx && systemctl restart nginx"
			}
			return sh(c, w, cmd)
		},
	}
}

var tokenRe = regexp.MustCompile(`token: '(eyJ[A-Za-z0-9_.\-]+)`)

// guiTLSStep: HTTPS для GUI. Сертификат подписан Megapolos Root CA (тот же CA,
// что платформа выдаёт на инстансы в devMode) — импортировал CA в браузер,
// доверяешь и GUI, и приложениям. :80 остаётся открытым.
func guiTLSStep(coreDir, guiDir string) Step {
	const crtPath, keyPath = "/etc/megapolos/gui.crt", "/etc/megapolos/gui.key"
	caDir := filepath.Join(coreDir, "data", "ca")
	return StepFunc{
		N: "gui:tls", D: []string{"token"},
		DetectF: func(c *Ctx) (bool, string) {
			if _, err := os.Stat(crtPath); err != nil {
				return false, ""
			}
			site, err := os.ReadFile("/etc/nginx/sites-available/megapolos-gui")
			if err != nil || !strings.Contains(string(site), "4443 ssl") { // 4443 — наша схема; 80/443 занимает nginx ноды
				return false, ""
			}
			if !outOK(c, "systemctl is-active -q nginx") {
				return false, ""
			}
			return true, "HTTPS для GUI настроен"
		},
		RunF: func(c *Ctx, w io.Writer) error {
			// CA лениво генерится ядром — дёргаем endpoint, чтобы гарантировать
			if _, err := os.Stat(filepath.Join(caDir, "ca.key")); err != nil {
				shTolerant(c, w, "curl -s -o /dev/null http://127.0.0.1:5100/api/ca/download")
			}
			if _, err := os.Stat(filepath.Join(caDir, "ca.key")); err != nil {
				return fmt.Errorf("Megapolos Root CA не найден в %s (ядро уже поднялось?)", caDir)
			}
			san := "DNS:localhost,DNS:gui." + c.O.BaseDomain + ",IP:127.0.0.1"
			if c.O.LANIP != "" {
				san += ",IP:" + c.O.LANIP
			}
			if err := sh(c, w, "mkdir -p /etc/megapolos && "+
				"openssl genrsa -out "+keyPath+" 2048 && chmod 600 "+keyPath+" && "+
				"openssl req -new -key "+keyPath+" -out /tmp/gui.csr -subj '/CN=megapolos-gui' -addext 'subjectAltName="+san+"' && "+
				"openssl x509 -req -in /tmp/gui.csr -CA "+filepath.Join(caDir, "ca.crt")+" -CAkey "+filepath.Join(caDir, "ca.key")+" "+
				"-CAcreateserial -out "+crtPath+" -days 825 -copy_extensions copyall && rm -f /tmp/gui.csr"); err != nil {
				return err
			}
			fmt.Fprintf(w, "сертификат GUI подписан Megapolos CA (SAN: %s)\n", san)
			if err := writeFile(c, w, "/etc/nginx/sites-available/megapolos-gui",
				RenderNginxSiteTLS(filepath.Join(guiDir, "build"), crtPath, keyPath), 0o644, ""); err != nil {
				return err
			}
			return sh(c, w, "nginx -t && systemctl reload nginx")
		},
	}
}

// tokenStep: ждём API и забираем JWT из boot-лога.
func tokenStep() Step {
	return StepFunc{
		N: "token", D: []string{"systemd"},
		DetectF: func(c *Ctx) (bool, string) {
			b, err := os.ReadFile("/root/megapolos-token.txt")
			if err != nil {
				return false, ""
			}
			tok := strings.TrimSpace(string(b))
			if tok == "" {
				return false, ""
			}
			if _, err := apiQuery(c, tok, "{ __typename }", nil); err == nil {
				c.O.Token = tok
				return true, "токен из /root/megapolos-token.txt валиден"
			}
			return false, ""
		},
		RunF: func(c *Ctx, w io.Writer) error {
			fmt.Fprintln(w, "жду API на :5100 и забираю root-токен из журнала")
			var token string
			for i := 0; i < 60; i++ {
				select {
				case <-c.Done():
					return c.Err()
				default:
				}
				logs, _ := out(c, "journalctl -u megapolos-core -b --no-pager 2>/dev/null")
				if m := tokenRe.FindAllStringSubmatch(logs, -1); len(m) > 0 {
					token = m[len(m)-1][1] // последний токен текущего boot
					break
				}
				time.Sleep(3 * time.Second)
			}
			if token == "" {
				return fmt.Errorf("токен не найден за 3 минуты (journalctl -u megapolos-core)")
			}
			c.O.Token = token
			if err := writeFile(c, w, "/root/megapolos-token.txt", token+"\n", 0o600, ""); err != nil {
				return err
			}
			svcHome := "/home/" + c.O.SvcUser
			if err := writeFile(c, w, svcHome+"/megapolos-token.txt", token+"\n", 0o600, c.O.SvcUser+":"+c.O.SvcUser); err != nil {
				return err
			}
			return writeFile(c, w, "/etc/motd", RenderMotd(token), 0o644, "")
		},
	}
}
