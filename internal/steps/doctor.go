package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
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
	return &Ctx{
		Context: context.Background(),
		Ex:      sys.Real{},
		O:       &Opts{},
	}
}

// DoctorReport — структура диагностики (для --resume).
type DoctorReport struct {
	AllStagesDone bool
	FirstBroken   string // ключ первой сломанной стадии (маркер есть, артефакта нет)
	APIHealthy    bool   // API отвечает и токен валиден (иначе --resume обязан перевыпустить токен)
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

	// 5. API / нода
	fmt.Fprintln(w, "[5] Megapolos Core API")
	apiState := doctorAPIState(c)
	report.APIHealthy = !strings.HasPrefix(apiState, "✗") && !strings.HasPrefix(apiState, "—")
	fmt.Fprintf(w, "  %s\n", apiState)
	fmt.Fprintln(w)

	// 6. Рекомендация
	fmt.Fprintln(w, "[6] Рекомендация")
	rec := doctorRecommend(c, report.APIHealthy)
	report.Recommendation = rec
	fmt.Fprintln(w, rec)
	fmt.Fprintln(w)

	problems := countProblems(c, report.APIHealthy)
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
func doctorRecommend(c *Ctx, apiHealthy bool) string {
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
func countProblems(c *Ctx, apiHealthy bool) int {
	n := 0
	for _, s := range nodeStages() {
		if detectMarker(s.Key) && !stageArtifactOK(c, s.Key) {
			n++
		}
	}
	if !apiHealthy {
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
			ID           string `json:"id"`
			Name         string `json:"name"`
			Type         string `json:"type"`
			NodeName     string `json:"nodeName"`
			CreateDate   string `json:"createDate"`
			IsClosed     bool   `json:"isClosed"`
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