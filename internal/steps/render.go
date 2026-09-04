package steps

import (
	"encoding/json"
	"fmt"
)

// RenderCoreConfig — config/config.json ядра (строгий JSON, без комментариев).
// Порядок полей как у исторического конфига.
func RenderCoreConfig(secret, dbUser, dbPass, dbName string, debug, devMode bool) string {
	// struct с точным порядком полей
	cfg := struct {
		Secret            string `json:"secret"`
		ConnectionString  string `json:"connectionString"`
		RegistryHost      string `json:"registryHost"`
		RegistryUser      string `json:"registryUser"`
		RegistryPassword  string `json:"registryPassword"`
		Debug             bool   `json:"debug"`
		DevMode           bool   `json:"devMode"`
		PublicSchema      bool   `json:"publicSchema"`
		AllowUnauthorized bool   `json:"allowUnauthorized"`
		NoRoot            bool   `json:"noRoot"`
		CatalogURL        string `json:"catalogUrl"`
	}{
		Secret:           secret,
		ConnectionString: fmt.Sprintf("postgres://%s:%s@localhost:5432/%s", dbUser, dbPass, dbName),
		// registry: локальный приватный registry платформы (install.ts INSTALL REGISTRY)
		RegistryHost:     "localhost",
		RegistryUser:     "megapolos",
		RegistryPassword: "megapolos",
		Debug:            debug,
		DevMode:          devMode,
		PublicSchema:     false,
		NoRoot:           true, // noRoot=true безопасно и при root-запуске
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	return string(b) + "\n"
}

// RenderGUIConfig — public/config/config.json фронта (читается в рантайме).
func RenderGUIConfig(apiURL string) string {
	b, _ := json.Marshal(map[string]string{"server": apiURL})
	return string(b) + "\n"
}

// RenderCoreUnit — systemd-юнит ядра. Сервис обязан работать от root:
// платформа делает spawn(..., uid: 0) для shell/ansible (Process.ts),
// непривилегированному юзеру ядро возвращает EPERM.
func RenderCoreUnit(coreDir string) string {
	return fmt.Sprintf(`[Unit]
Description=Megapolos Core (GraphQL API :5100)
After=network-online.target postgresql.service docker.service
Wants=network-online.target

[Service]
Type=simple
User=root
Group=root
WorkingDirectory=%s
ExecStart=/usr/bin/npm run prod
Restart=on-failure
RestartSec=5
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
`, coreDir)
}

// RenderNginxSite — nginx-конфиг GUI.
// GUI static nginx слушает 8080/4443: порты 80/443 в госте занимает
// nginx-контейнер ноды (платформа, INIT) — там живут api. и app-виртхосты.
func RenderNginxSite(guiBuildDir string) string {
	return fmt.Sprintf(`server {
    listen 8080 default_server;
    root %s;
    index index.html;
    client_max_body_size 10g;
    location / { try_files $uri $uri/ /index.html; }
}
`, guiBuildDir)
}

// RenderNginxSiteTLS — GUI по HTTPS (443) сертификатом Megapolos Root CA.
// :80 остаётся открытым (curl/health), без редиректа — внешний порт
// проброса известен только хосту, редирект сломал бы его.
func RenderNginxSiteTLS(guiBuildDir, crt, key string) string {
	return fmt.Sprintf(`server {
    listen 8080 default_server;
    listen 4443 ssl default_server;
    server_name _;

    ssl_certificate     %s;
    ssl_certificate_key %s;

    root %s;
    index index.html;
    client_max_body_size 10g;
    location / { try_files $uri $uri/ /index.html; }
}
`, crt, key, guiBuildDir)
}

// RenderAptProxy — apt-конфиг с прокси-кэшем и короткими таймаутами (сеть флакает).
func RenderAptProxy(proxy string) string {
	return fmt.Sprintf(`Acquire::http::Proxy "%s";
Acquire::http::Timeout "15";
Acquire::https::Timeout "15";
Acquire::Retries "1";
`, proxy)
}

// RenderSshdDropin — разрешение root-входа по паролю (для себя-ноды).
func RenderSshdDropin() string {
	return "PermitRootLogin yes\nPasswordAuthentication yes\n"
}

// RenderSudoers — sudo без пароля сервисному юзеру (ansible become: yes).
func RenderSudoers(user string) string {
	return fmt.Sprintf("%s ALL=(ALL) NOPASSWD:ALL\n", user)
}

// RenderMotd — приветствие после установки.
func RenderMotd(token string) string {
	return fmt.Sprintf(`
  Megapolos установлен.
  GUI:        http://<ip-vm>/
  API:        http://<ip-vm>:5100
  Root-токен: %s
  (токен также в /root/megapolos-token.txt)

`, token)
}
