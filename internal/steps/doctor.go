package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"megapolos/installer/internal/sys"
)

// =============================================================================
// --doctor / --info: диагностика существующей установки.
//
// --doctor: что есть, чего нет, что сломано, какие стадии завершены.
// Рекомендация: --resume (продолжить), --retry-stage=<X> (перезапустить одну),
// --wipe (с нуля).
//
// --info: показать последние ansible-логи платформы (LogRepo через API),
//         чтобы понять ПОЧЕМУ стадия упала (без этого молчаливый сбой не виден).
//
// --resume автоматически вызывает --doctor перед стартом: если найдена
// сломанная стадия (маркер есть, артефакта нет) --resume сам подставит
// --retry-stage=<сломанная> и продолжит.
// =============================================================================

// detectMarker — есть ли маркер стадии.
func detectMarker(key string) bool {
	_, err := os.Stat(stageMarkerPath(key))
	return err == nil
}

// stageArtifactOK — проверка артефакта стадии.
func stageArtifactOK(c *Ctx, key string) bool {
	for _, s := range nodeStages() {
		if s.Key == key {
			return s.HasArtifact(c)
		}
	}
	return false
}

// serviceRunning — активен ли systemd-юнит.
func serviceRunning(unit string) bool {
	cmd := exec.Command("systemctl", "is-active", "--quiet", unit)
	return cmd.Run() == nil
}

// coreCwdOK — существует ли рабочий каталог живого процесса megapolos-core.
// Если каталог установки удалили под работающим core, cwd процесса «мертв»
// (readlink → "... (deleted)") и дочерние процессы (ansible) падают; core
// нужно перезапустить.
func coreCwdOK() bool {
	b, err := exec.Command("systemctl", "show", "megapolos-core", "-p", "MainPID", "--value").Output()
	if err != nil {
		return true // не смогли определить — не считаем проблемой
	}
	pid := strings.TrimSpace(string(b))
	if pid == "" || pid == "0" {
		return true
	}
	// Сравниваем inode фактического cwd процесса и текущего пути: если каталог
	// удалили и создали заново, путь существует, но inode другой — cwd мёртв.
	procFi, err := os.Stat("/proc/" + pid + "/cwd")
	if err != nil {
		return true // /proc недоступен — не наш случай
	}
	target, err := os.Readlink("/proc/" + pid + "/cwd")
	if err != nil {
		return true
	}
	target = strings.TrimSuffix(target, " (deleted)")
	pathFi, err := os.Stat(target)
	if err != nil {
		return false
	}
	return os.SameFile(procFi, pathFi)
}

// newDoctorCtx — Ctx с пустыми Opts для диагностических вызовов.
func newDoctorCtx() *Ctx {
	// DBName нужен секции миграций схемы: doctor не получает Opts из main,
	// поэтому берём имя БД из installer.cfg (или переменной окружения/дефолт).
	dbName := os.Getenv("MEGAPOLOS_DB_NAME")
	if dbName == "" {
		dbName = "megapolos"
	}
	if b, err := os.ReadFile("/var/lib/megapolos/installer.cfg"); err == nil {
		var cfg struct{ DBName string }
		if json.Unmarshal(b, &cfg) == nil && cfg.DBName != "" {
			dbName = cfg.DBName
		}
	}
	return &Ctx{
		Context: context.Background(),
		Ex:      sys.Real{},
		O:       &Opts{DBName: dbName},
	}
}

// DoctorReport — структура диагностики (для --resume).
type DoctorReport struct {
	AllStagesDone  bool
	FirstBroken    string // ключ первой сломанной стадии (маркер есть, артефакта нет)
	APIHealthy     bool   // API отвечает и токен валиден (иначе --resume обязан перевыпустить токен)
	Recommendation string
}

// Doctor — диагностика; возвращает отчёт + exit code (0 = ОК, 1 = проблемы).
func Doctor(w io.Writer) (DoctorReport, int) {
	c := newDoctorCtx()
	report := DoctorReport{}

	fmt.Fprintln(w, "=== doctor: megapolos installer diagnostic ===")
	fmt.Fprintf(w, "host: %s\n", hostnameOut())
	fmt.Fprintf(w, "root: %v (euid=%d)\n", os.Geteuid() == 0, os.Geteuid())
	fmt.Fprintln(w)

	// 1. Базовые сервисы
	fmt.Fprintln(w, "[1] Сервисы")
	check(w, "systemd: megapolos-core", serviceRunning("megapolos-core"))
	check(w, "docker daemon", serviceRunning("docker"))
	check(w, "postgresql", serviceRunning("postgresql"))
	check(w, "docker: nginx", dockerContainerRunning("nginx"))
	check(w, "docker: registry", dockerContainerRunning("docker-registry"))
	check(w, "gui (port 3000)", portOpen("127.0.0.1:3000"))
	fmt.Fprintln(w)

	// 2. Артефакты стадий
	fmt.Fprintln(w, "[2] Стадии платформы (маркер / артефакт)")
	stages := nodeStages()
	for i, s := range stages {
		marker := detectMarker(s.Key)
		artifact := stageArtifactOK(c, s.Key)
		state := "✓ готово"
		if !artifact {
			if marker {
				state = "✗ СЛОМАНО: маркер есть, артефакта нет"
				if report.FirstBroken == "" {
					report.FirstBroken = s.Key
				}
			} else {
				state = "— не выполнено"
				if report.FirstBroken == "" {
					// первая невыполненная стадия (без маркера) тоже = кандидат на retry
					report.FirstBroken = s.Key
				}
				_ = i
			}
		}
		fmt.Fprintf(w, "  %-22s  %s\n", s.Label, state)
	}
	report.AllStagesDone = report.FirstBroken == ""
	fmt.Fprintln(w)

	// 3. Файлы конфигурации
	fmt.Fprintln(w, "[3] Конфигурация платформы")
	checkPath(w, "/opt/megapolos (каталог установки)", "/opt/megapolos")
	checkPath(w, "/opt/megapolos/megapolos-core/.env", "/opt/megapolos/megapolos-core/.env")
	// checkCatalog проверяет только «адрес записан в config.json», а вкладки
	// App Store строятся из таблицы catalog (сид ядра seedCatalogs). Без второй
	// проверки doctor показывал ✓ на стенде, где вкладок нет вовсе.
	checkCatalog(w, "/opt/megapolos/megapolos-core/config/config.json")
	catHealth := inspectCatalogs(c)
	renderCatalogHealth(w, catHealth)
	checkPath(w, "/data/nginx/conf/init.conf", "/data/nginx/conf/init.conf")
	checkPath(w, "/data/nginx/conf/core.conf", "/data/nginx/conf/core.conf")
	checkPath(w, "/data/registry/docker-compose.yml", "/data/registry/docker-compose.yml")
	checkPath(w, "/root/megapolos-token.txt", "/root/megapolos-token.txt")
	fmt.Fprintln(w)

	// 4. Порты
	fmt.Fprintln(w, "[4] Порты")
	checkPort(w, "API core (5100)", "127.0.0.1:5100")
	checkPort(w, "nginx prod (5104)", "127.0.0.1:5104")
	checkPort(w, "nginx TLS (4443)", "127.0.0.1:4443")
	fmt.Fprintln(w)

	// 5. Схема БД: применённые миграции
	fmt.Fprintln(w, "[5] Миграции схемы БД")
	pending := pendingMigrations(c)
	if len(pending) == 0 {
		fmt.Fprintf(w, "  ✓ все миграции реестра применены (БД %s)\n", c.O.DBName)
	} else {
		fmt.Fprintf(w, "  ✗ неприменённых миграций: %d\n", len(pending))
		for _, name := range pending {
			fmt.Fprintf(w, "      - %s\n", name)
		}
		fmt.Fprintln(w, "  → запустите установщик: миграции догонит шаг db-migrate")
	}
	if err := assertSchemaSentinels(c); err != nil {
		fmt.Fprintf(w, "  ✗ %v\n", err)
	} else {
		fmt.Fprintln(w, "  ✓ все ожидаемые объекты схемы на месте")
	}
	fmt.Fprintln(w)

	// 6. API / нода
	fmt.Fprintln(w, "[6] Megapolos Core API")
	apiState := doctorAPIState(c)
	report.APIHealthy = !strings.HasPrefix(apiState, "✗") && !strings.HasPrefix(apiState, "—")
	fmt.Fprintf(w, "  %s\n", apiState)
	fmt.Fprintln(w)

	// 7. Рекомендация

	rec := doctorRecommend(c, report.APIHealthy, catHealth)
	report.Recommendation = rec
	fmt.Fprintln(w, rec)
	fmt.Fprintln(w)

	problems := countProblems(c, report.APIHealthy, catHealth)
	if problems > 0 {
		fmt.Fprintf(w, "Найдено проблем: %d\n", problems)
		return report, 1
	}
	if !report.AllStagesDone {
		return report, 1
	}
	fmt.Fprintln(w, "Всё в порядке (exit 0)")
	return report, 0
}

// doctorAPIState — состояние API core (через GraphQL getVersion).
func doctorAPIState(c *Ctx) string {
	token, _ := readTokenFromFile("/root/megapolos-token.txt")
	if token == "" {
		return "— токен не найден в /root/megapolos-token.txt"
	}
	c.O.Token = token
	data, err := apiQuery(c, token, "{ getAllAppVersion { id version buildNumber } }", nil)
	if err != nil {
		return fmt.Sprintf("✗ API не отвечает: %v", err)
	}
	return fmt.Sprintf("✓ API отвечает: %s", strings.TrimSpace(string(data)))
}

// doctorRecommend — текстовая рекомендация.
// catHealth учитывает источники App Store: без записи kind='local' установка
// формально «завершена», но вкладки Local App Store нет — --wipe здесь лишний,
// достаточно --resume (шаг db-migrate догонит 038 и ядро пересоздаст сид).
func doctorRecommend(c *Ctx, apiHealthy bool, catHealth CatalogHealth) string {
	init := detectMarker(stageInit) && stageArtifactOK(c, stageInit)
	prepare := detectMarker(stagePrepareForCore) && stageArtifactOK(c, stagePrepareForCore)
	registry := detectMarker(stageInstallRegistry) && stageArtifactOK(c, stageInstallRegistry)

	if !serviceRunning("megapolos-core") {
		return "megapolos-core не запущен:\n  sudo systemctl start megapolos-core\n" +
			"Если упал: journalctl -u megapolos-core -b -n 50"
	}
	if !portOpen("127.0.0.1:5100") {
		return "API core не слушает порт 5100 (сервис запущен, но порт закрыт):\n" +
			"  journalctl -u megapolos-core -b -n 50"
	}
	if !apiHealthy {
		return "Токен недействителен или API не отвечает. Перевыпустить токен:\n" +
			"  sudo installer --resume\n" +
			"(шаг token перечитает JWT из журнала megapolos-core и перезапишет /root/megapolos-token.txt)"
	}
	if _, err := os.Stat("/opt/megapolos"); err != nil {
		return "Каталог установки /opt/megapolos отсутствует. Восстановить:\n" +
			"  sudo installer --resume\n" +
			"(повторно склонирует core, восстановит config.json с прежними секретами из /var/lib/megapolos/installer.cfg)"
	}
	if !coreCwdOK() {
		return "Рабочий каталог процесса megapolos-core удалён (cwd = deleted). Перезапустить core:\n" +
			"  sudo installer --resume\n" +
			"(шаг systemd перезапустит сервис из восстановленного каталога)"
	}

	// Источники App Store: формально все стадии готовы, но вкладки Local App
	// Store нет. Это лечится --resume (db-migrate применит 038), а не --wipe.
	if catHealth.Checked && !catHealth.HasTable {
		return "Таблицы catalog нет, App Store не работает. Применить схему:\n" +
			"  sudo installer --resume   (шаг db-migrate применит 037-add-catalog.sql)"
	}
	if catHealth.Checked && catHealth.HasTable && len(catHealth.Rows) == 0 {
		return "Таблица catalog пуста — сид ядра не выполнился. Проверить и повторить сид:\n" +
			"  journalctl -u megapolos-core -b | grep -i seedCatalogs\n" +
			"  sudo installer --resume"
	}
	if catHealth.Checked && !catHealth.HasKind {
		return "Колонка catalog.kind отсутствует — вкладки Local App Store не будет.\n" +
			"  sudo installer --resume   (шаг db-migrate применит 038-add-catalog-kind.sql)"
	}
	if catHealth.Checked && catHealth.HasKind && !catHealth.HasLocal() {
		return "Нет записи kind='local' — вкладки Local App Store не будет.\n" +
			"  sudo systemctl restart megapolos-core   (сид ядра создаст Local App Store)\n" +
			"  если не помогло: journalctl -u megapolos-core -b | grep -i seedCatalogs"
	}

	switch {
	case init && prepare && registry:
		return "Установка завершена. Переустановить с нуля: --wipe"
	case init && prepare && !registry:
		return "Запустить только последнюю стадию:\n  sudo installer --retry-stage=install-registry"
	case init && !prepare:
		return "Перезапустить PREPARE FOR CORE (ansible не создал core.conf):\n  sudo installer --retry-stage=prepare-for-core\n" +
			"Перед этим посмотреть ansible-лог:\n  sudo installer --info"
	case !init:
		return "Продолжить с первой стадии:\n  sudo installer --resume\n" +
			"(если возврат к чистому состоянию: sudo installer --wipe)"
	default:
		return "Состояние неоднозначно — запустите --info для просмотра ansible-логов"
	}
}

// countProblems — сколько критичных проблем найдено.
// Сломанный App Store (нет таблицы/колонки kind/локального каталога) —
// критичная проблема: ядро при этом работает, но установить приложение
// из локального каталога нельзя.
func countProblems(c *Ctx, apiHealthy bool, catHealth CatalogHealth) int {
	n := 0
	for _, s := range nodeStages() {
		if detectMarker(s.Key) && !stageArtifactOK(c, s.Key) {
			n++
		}
	}
	if !apiHealthy {
		n++
	}
	if catHealth.Broken() {
		n++
	}
	// Каталог установки отсутствует (удалён вручную) — API может ещё
	// отвечать из памяти процесса, но после рестарта core не поднимется.
	if _, err := os.Stat("/opt/megapolos"); err != nil {
		n++
	}
	if !coreCwdOK() {
		n++
	}
	if !serviceRunning("megapolos-core") {
		n++
	}
	if !dockerContainerRunning("nginx") {
		n++
	}
	if !dockerContainerRunning("docker-registry") {
		n++
	}
	if !portOpen("127.0.0.1:5100") {
		n++
	}
	if !portOpen("127.0.0.1:5104") {
		n++
	}
	if !portOpen("127.0.0.1:3000") {
		n++
	}
	return n
}

// =============================================================================
// --info: логи ansible платформенных стадий
// =============================================================================

// ShowInfo — показать последние ansible-логи. Возвращает exit code.
func ShowInfo(w io.Writer) int {
	c := newDoctorCtx()
	token, _ := readTokenFromFile("/root/megapolos-token.txt")
	if token == "" {
		fmt.Fprintln(w, "токен не найден в /root/megapolos-token.txt (megapolos-core не запущен?)")
		return 1
	}
	c.O.Token = token
	fmt.Fprintln(w, "=== --info: последние ansible-логи платформы ===")

	// Берём 10 свежих логов (getAllLog: без лимита — берём все).
	// Затем для каждого качаем getLog(id) { text } (хвост ansible-вывода).
	query := `{
		getAllLog {
			id
			name
			type
			nodeId
			nodeName
			createDate
			isClosed
		}
	}`
	data, err := apiQuery(c, token, query, nil)
	if err != nil {
		fmt.Fprintf(w, "✗ getAllLog: %v\n", err)
		return 1
	}

	var parsed struct {
		GetAllLog []struct {
			ID         string `json:"id"`
			Name       string `json:"name"`
			Type       string `json:"type"`
			NodeName   string `json:"nodeName"`
			CreateDate string `json:"createDate"`
			IsClosed   bool   `json:"isClosed"`
		} `json:"getAllLog"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		fmt.Fprintf(w, "✗ parse: %v\n", err)
		return 1
	}

	if len(parsed.GetAllLog) == 0 {
		fmt.Fprintln(w, "нет логов (стадии ansible ещё не запускались)")
		return 0
	}

	// Показываем только 10 самых свежих
	logs := parsed.GetAllLog
	if len(logs) > 10 {
		logs = logs[len(logs)-10:]
	}

	fmt.Fprintf(w, "Найдено логов: %d (свежие первые; вывод обрезан до 80 последних строк)\n\n",
		len(parsed.GetAllLog))
	for _, l := range logs {
		fmt.Fprintf(w, "--- %s | %s | node=%s | %s | closed=%v ---\n",
			shortID(l.ID), l.Type, l.NodeName, l.CreateDate, l.IsClosed)
		logData, err := apiQuery(c, token,
			fmt.Sprintf(`{ getLog(id: "%s") { text } }`, l.ID), nil)
		if err != nil {
			fmt.Fprintf(w, "(getLog: %v)\n", err)
			continue
		}
		var logWrapped struct {
			GetLog struct {
				Text string `json:"text"`
			} `json:"getLog"`
		}
		if err := json.Unmarshal(logData, &logWrapped); err != nil {
			fmt.Fprintf(w, "(parse log: %v)\n", err)
			continue
		}
		log := logWrapped.GetLog.Text
		lines := strings.Split(log, "\n")
		if len(lines) > 80 {
			fmt.Fprintf(w, "... (показаны последние 60 из %d строк)\n", len(lines))
			lines = lines[len(lines)-60:]
		}
		fmt.Fprintln(w, strings.Join(lines, "\n"))
		fmt.Fprintln(w)
	}
	return 0
}

// =============================================================================
// helpers
// =============================================================================

func hostnameOut() string {
	out, err := exec.Command("hostname").Output()
	if err != nil {
		return "?"
	}
	return strings.TrimSpace(string(out))
}

func shortID(id string) string {
	if len(id) >= 8 {
		return id[:8]
	}
	return id
}

func dockerContainerRunning(name string) bool {
	cmd := exec.Command("docker", "ps", "--format", "{{.Names}}")
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == name {
			return true
		}
	}
	return false
}

func portOpen(addr string) bool {
	parts := strings.SplitN(addr, ":", 2)
	if len(parts) != 2 {
		return false
	}
	cmd := exec.Command("bash", "-c",
		fmt.Sprintf("timeout 2 bash -c 'echo > /dev/tcp/%s/%s' 2>/dev/null", parts[0], parts[1]))
	return cmd.Run() == nil
}

func check(w io.Writer, name string, ok bool) {
	if ok {
		fmt.Fprintf(w, "  ✓ %s\n", name)
	} else {
		fmt.Fprintf(w, "  ✗ %s\n", name)
	}
}

// checkCatalog — состояние catalogUrl в config.json ядра.
// Пустое значение означает неработающий App Store: ядро бросает
// "catalogUrl is not configured" при любом обращении к каталогу.
func checkCatalog(w io.Writer, path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(w, "  ✗ Каталог приложений  (%s: %v)\n", path, err)
		return
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		fmt.Fprintf(w, "  ✗ Каталог приложений  (%s не разобран: %v)\n", path, err)
		return
	}
	url, _ := m["catalogUrl"].(string)
	if strings.TrimSpace(url) == "" {
		fmt.Fprintf(w, "  ✗ Каталог приложений  (catalogUrl пуст — App Store не работает)\n")
		fmt.Fprintln(w, "      → installer --resume (шаг config:catalog проставит адрес)")
		return
	}
	state := url
	if !catalogReachable(url) {
		// недоступность каталога не делает установку сломанной (офлайн-нода),
		// поэтому это предупреждение, а не ✗
		state = url + "  (недоступен — офлайн?)"
	}
	fmt.Fprintf(w, "  ✓ Каталог приложений  %s\n", state)
}

// catalogReachable — быстрый GET {url}/apps с таймаутом.
func catalogReachable(url string) bool {
	cmd := exec.Command("curl", "-fsS", "-m", "5", "-o", "/dev/null", "-A", "megapolos-installer",
		strings.TrimSuffix(url, "/")+"/apps")
	return cmd.Run() == nil
}

// =============================================================================
// Состояние источников App Store (таблица catalog)
//
// checkCatalog() проверяет только «адрес записан в config.json». Вкладки App
// Store строятся не из конфига, а из таблицы catalog, которую наполняет сид
// ядра seedCatalogs() при старте:
//
//   Megapolos       kind=remote, isDefault=true  — адрес из config.catalogUrl;
//   Local App Store kind=local                   — создаётся ВСЕГДА, если нет.
//
// Пока doctor этого не видел, он показывал «✓ Каталог приложений» на стендах,
// где вкладок не было вовсе — например, если отсутствовала колонка
// catalog.kind и сид молча падал в catch (только запись в лог ядра).
// =============================================================================

// CatalogRow — одна запись таблицы catalog.
type CatalogRow struct {
	Name      string
	URL       string
	Kind      string
	IsDefault bool
	Enabled   bool
	Priority  int
}

// CatalogHealth — результат инспекции таблицы catalog.
type CatalogHealth struct {
	// Checked=false — проверить не удалось (нет БД/psql); SkipReason объясняет.
	Checked    bool
	SkipReason string

	HasTable bool
	HasKind  bool
	Rows     []CatalogRow

	// Шаблоны в локальном хранилище (таблица app_export).
	AppExportCount int
	AppExportOK    bool
}

// HasLocal — есть ли запись kind='local' (вкладка Local App Store).
func (h CatalogHealth) HasLocal() bool {
	for _, r := range h.Rows {
		if r.Kind == "local" {
			return true
		}
	}
	return false
}

// HasEnabledRemote — есть ли хотя бы один включённый remote-каталог.
func (h CatalogHealth) HasEnabledRemote() bool {
	for _, r := range h.Rows {
		if r.Kind != "local" && r.Enabled {
			return true
		}
	}
	return false
}

// DefaultRow — предустановленный каталог (isDefault), если он записан в БД.
func (h CatalogHealth) DefaultRow() (CatalogRow, bool) {
	for _, r := range h.Rows {
		if r.IsDefault {
			return r, true
		}
	}
	return CatalogRow{}, false
}

// Broken — состояние, при котором App Store заведомо не работает полностью
// или частично: нет таблицы, нет колонки kind, нет локального каталога, либо
// нет ни одного включённого remote-каталога. Недоступность remote по сети
// сюда НЕ входит — офлайн-нода штатно живёт на Local App Store.
func (h CatalogHealth) Broken() bool {
	if !h.Checked {
		return false // нет данных — не выдумываем проблему
	}
	if !h.HasTable || !h.HasKind {
		return true
	}
	return !h.HasLocal() || !h.HasEnabledRemote()
}

// inspectCatalogs — четыре SQL-запроса к БД ядра: таблица, колонка kind,
// записи каталогов, количество шаблонов в app_export.
//
// postgresql недоступен → Checked=false (это не поломка App Store, а упавшая
// БД: её покажет секция [1] Сервисы).
func inspectCatalogs(c *Ctx) CatalogHealth {
	var h CatalogHealth
	if _, err := psqlQ(c, "SELECT 1"); err != nil {
		h.SkipReason = "postgresql не отвечает"
		return h
	}
	h.Checked = true

	h.HasTable = psqlTrue(c,
		"SELECT 1 FROM information_schema.tables "+
			"WHERE table_schema='public' AND table_name='catalog' LIMIT 1")
	if !h.HasTable {
		return h
	}

	h.HasKind = psqlTrue(c,
		"SELECT 1 FROM information_schema.columns "+
			"WHERE table_name='catalog' AND column_name='kind' LIMIT 1")

	// Порядок как во вкладках GUI: priority, затем имя.
	// Без kind запрос упадёт — читаем только при наличии колонки, подставляя
	// литерал 'remote' (все существующие записи после миграции 038 такие).
	cols := "name, url, is_default, enabled, priority"
	if h.HasKind {
		cols = "name, url, kind, is_default, enabled, priority"
	}
	if out, err := psqlQ(c, "SELECT "+cols+" FROM catalog ORDER BY priority, name"); err == nil {
		h.Rows = parseCatalogRows(out, h.HasKind)
	}

	if out, err := psqlQ(c, "SELECT count(*) FROM app_export"); err == nil {
		if n, convErr := strconv.Atoi(strings.TrimSpace(out)); convErr == nil {
			h.AppExportCount = n
			h.AppExportOK = true
		}
	}
	return h
}

// parseCatalogRows разбирает вывод `psql -A -t` (строки, поля через '|').
// withKind=false — в выборке не было колонки kind, считаем все записи remote
// (так они и выглядят после ADD COLUMN ... DEFAULT 'remote').
func parseCatalogRows(out string, withKind bool) []CatalogRow {
	var rows []CatalogRow
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, "|")
		want := 5
		if withKind {
			want = 6
		}
		if len(parts) < want {
			continue
		}
		row := CatalogRow{
			Name:      strings.TrimSpace(parts[0]),
			URL:       strings.TrimSpace(parts[1]),
			Kind:      "remote",
			IsDefault: strings.TrimSpace(parts[want-3]) == "t",
			Enabled:   strings.TrimSpace(parts[want-2]) == "t",
		}
		if withKind {
			if k := strings.TrimSpace(parts[2]); k != "" {
				row.Kind = k
			}
		}
		if p, err := strconv.Atoi(strings.TrimSpace(parts[want-1])); err == nil {
			row.Priority = p
		}
		rows = append(rows, row)
	}
	return rows
}

// renderCatalogHealth печатает состояние источников App Store.
// Локальный каталог обязателен: без него вторая вкладка не появится.
func renderCatalogHealth(w io.Writer, h CatalogHealth) {
	if !h.Checked {
		reason := h.SkipReason
		if reason == "" {
			reason = "нет доступа к БД"
		}
		fmt.Fprintf(w, "  — Каталоги в БД       (проверка пропущена: %s)\n", reason)
		return
	}

	if !h.HasTable {
		fmt.Fprintln(w, "  ✗ Каталоги в БД       таблицы catalog нет — App Store не покажет вкладок")
		fmt.Fprintln(w, "      → sudo installer --resume   (шаг db-migrate применит 037-add-catalog.sql)")
		return
	}

	if len(h.Rows) == 0 {
		fmt.Fprintln(w, "  ✗ Каталоги в БД       таблица catalog пуста — вкладок не будет")
		fmt.Fprintln(w, "      → сид ядра не выполнился: journalctl -u megapolos-core -b | grep -i seedCatalogs")
		fmt.Fprintln(w, "      → либо: sudo installer --resume")
		return
	}

	// Основной (remote) каталог: запись с isDefault.
	if def, ok := h.DefaultRow(); ok {
		state := "remote, по умолчанию"
		if !def.Enabled {
			state = "remote, ОТКЛЮЧЁН"
		}
		mark := "✓"
		if !def.Enabled {
			mark = "✗"
		}
		fmt.Fprintf(w, "  %s Каталог приложений  %s  (%s, приоритет %d)\n", mark, def.URL, state, def.Priority)
		if !def.Enabled {
			fmt.Fprintln(w, "      → включите галку «Каталог участвует в App Store» в GUI")
		}
	} else {
		fmt.Fprintln(w, "  ✗ Каталог приложений  нет записи is_default — вкладки по умолчанию нет")
		fmt.Fprintln(w, "      → sudo installer --resume (сид ядра создаст «Megapolos» из config.catalogUrl)")
	}

	// Локальное хранилище: единственный каталог, который обязан быть всегда.
	if h.HasLocal() {
		switch {
		case !h.AppExportOK:
			fmt.Fprintln(w, "  ✓ Локальное хранилище Local App Store  (local, шаблонов: ?)")
		case h.AppExportCount == 0:
			fmt.Fprintln(w, "  ✓ Локальное хранилище Local App Store  (local, шаблонов: 0 — загрузите JSON через «Upload App Config»)")
		default:
			fmt.Fprintf(w, "  ✓ Локальное хранилище Local App Store  (local, шаблонов: %d)\n", h.AppExportCount)
		}
	} else if !h.HasKind {
		fmt.Fprintln(w, "  ✗ Локальное хранилище НЕТ — вкладки «Local App Store» не будет")
		fmt.Fprintln(w, "      → колонка catalog.kind отсутствует: миграция 038 не применена,")
		fmt.Fprintln(w, "        сид ядра упал в catch (в логе: «[catalog] seedCatalogs failed»)")
		fmt.Fprintln(w, "      → sudo installer --resume   (шаг db-migrate применит 038-add-catalog-kind.sql)")
	} else {
		fmt.Fprintln(w, "  ✗ Локальное хранилище НЕТ — вкладки «Local App Store» не будет")
		fmt.Fprintln(w, "      → колонка kind есть, запись не создана: ядро не стартовало после миграции?")
		fmt.Fprintln(w, "      → sudo systemctl restart megapolos-core, затем sudo installer --doctor")
	}

	// Прочие каталоги (добавленные пользователем) — только перечисление.
	var others []CatalogRow
	for _, r := range h.Rows {
		if r.Kind == "local" || r.IsDefault {
			continue
		}
		others = append(others, r)
	}
	if len(others) > 0 {
		fmt.Fprintf(w, "  · Прочие каталоги:    %d\n", len(others))
		for _, r := range others {
			state := "включён"
			if !r.Enabled {
				state = "отключён"
			}
			fmt.Fprintf(w, "      - %s  %s  (%s, %s)\n", r.Name, r.URL, r.Kind, state)
		}
	}
}

func checkPath(w io.Writer, name, path string) {
	if _, err := os.Stat(path); err == nil {
		fmt.Fprintf(w, "  ✓ %s  (%s)\n", name, path)
	} else {
		fmt.Fprintf(w, "  ✗ %s  (%s: %v)\n", name, path, err)
	}
}

func checkPort(w io.Writer, name, addr string) {
	parts := strings.SplitN(addr, ":", 2)
	if len(parts) != 2 {
		fmt.Fprintf(w, "  ✗ %s  (bad addr: %s)\n", name, addr)
		return
	}
	cmd := exec.Command("bash", "-c",
		fmt.Sprintf("timeout 2 bash -c 'echo > /dev/tcp/%s/%s' 2>/dev/null && echo open || echo closed", parts[0], parts[1]))
	out, err := cmd.Output()
	if err != nil {
		fmt.Fprintf(w, "  ✗ %s  (%s)\n", name, addr)
		return
	}
	if strings.Contains(string(out), "open") {
		fmt.Fprintf(w, "  ✓ %s  (%s)\n", name, addr)
	} else {
		fmt.Fprintf(w, "  ✗ %s  (%s: closed)\n", name, addr)
	}
}

func readTokenFromFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}
