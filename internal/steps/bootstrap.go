package steps

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"megapolos/installer/internal/sys"
)

// bootstrapStep: платформенная оркестрация через `npm run bootstrap` (install.ts):
// нода → INIT (nginx, единый CA) → PREPARE FOR CORE → INSTALL REGISTRY →
// (опционально, GUIApp) деплой GUI как приложения платформы с доменом.
// install.ts идемпотентен сам (get-or-create); маркер — чтобы не гонять ansible
// при повторных запусках без изменений режима.
func bootstrapStep(coreDir string) Step {
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
		D: []string{"npm:core", "config:core", "db", "swarm", "registry-image"},
		DetectF: func(c *Ctx) (bool, string) {
			mark := mode(c.O) + "|localhost"
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
			// cloud-init дроп-ины запрещают парольную аутентификацию
			if err := sh(c, w, "rm -f /etc/ssh/sshd_config.d/50-cloud-init.conf /etc/ssh/sshd_config.d/60-cloudimg-settings.conf"); err != nil {
				return err
			}
			if err := sh(c, w, "sshd -t && (systemctl reload ssh || systemctl reload sshd)"); err != nil {
				return err
			}

			env := []string{
				// host = localhost (не 127.0.0.1): node-nginx ждёт серт <host>.crt,
				// а registry.conf — localhost.crt; при IP имена разъезжаются и
				// контейнер падает (upstream deploy.sh тоже использует localhost)
				"MEGAPOLOS_NODE_HOST=localhost",
				"MEGAPOLOS_NODE_USER=root",
				"MEGAPOLOS_NODE_PASSWORD=" + c.O.NodeRootPassword,
			}
			if c.O.GUIApp {
				// GUI как приложение платформы: домен + серты от Megapolos CA,
				// API через nginx ноды (api.megapolos.localhost в devMode)
				apiURL := "https://api.megapolos.localhost"
				if !c.O.DevMode {
					apiURL = "https://api." + c.O.GUIDomain
				}
				env = append(env,
					"MEGAPOLOS_BOOTSTRAP_APP_REPO="+srcURL(c, "megapolos-gui"),
					"MEGAPOLOS_BOOTSTRAP_APP_NAME=megapolos-gui",
					"MEGAPOLOS_BOOTSTRAP_APP_BRANCH="+c.O.GUIRef,
					"MEGAPOLOS_BOOTSTRAP_APP_PORT=80",
					"MEGAPOLOS_BOOTSTRAP_APP_OUTER_PORT=3000",
					"MEGAPOLOS_BOOTSTRAP_APP_DOMAIN="+c.O.GUIDomain,
					"MEGAPOLOS_BOOTSTRAP_APP_SERVER="+apiURL,
				)
				fmt.Fprintf(w, "деплой GUI-приложения: https://%s (API %s)\n", c.O.GUIDomain, apiURL)
			}
			fmt.Fprintf(w, "npm run bootstrap (install.ts: нода, INIT, PREPARE, REGISTRY)…\n")
			if err := c.Ex.Run(c, sys.RunOpts{
				Cmd: "npm run bootstrap", Dir: coreDir, Env: env,
			}, w); err != nil {
				return fmt.Errorf("install.ts: %w", err)
			}
			if err := os.WriteFile(marker(c.O), []byte(mode(c.O)+"|localhost"), 0o644); err != nil {
				return err
			}
			fmt.Fprintf(w, "bootstrap завершён (режим %s)\n", mode(c.O))
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

// registryImageStep: образ registry:2 для приватного registry платформы.
// Офлайн — docker load из бандла (бандл собирается после онлайн-прогона).
func registryImageStep() Step {
	return StepFunc{
		N: "registry-image", D: []string{"packages"},
		DetectF: func(c *Ctx) (bool, string) {
			if outOK(c, "docker image inspect registry:2 >/dev/null 2>&1") {
				return true, "образ registry:2 уже есть"
			}
			return false, ""
		},
		RunF: func(c *Ctx, w io.Writer) error {
			bundleImg := filepath.Join(c.O.BundleDir, "docker", "registry-2.tar.gz")
			if c.O.BundleDir != "" && sys.FileExists(c, c.Ex, bundleImg) {
				return sh(c, w, "zcat "+bundleImg+" | docker load")
			}
			return sh(c, w, "docker pull registry:2")
		},
	}
}
