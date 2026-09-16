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
	)
	flag.Parse()

	if *showVersion {
		fmt.Println("megapolos-installer dev")
		return
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
		// домены — из base-domain, прод-режим (без devMode).
		opts.GUI = true
		opts.GUIApp = true
		opts.AddSelfNode = true
		opts.DevMode = false
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
