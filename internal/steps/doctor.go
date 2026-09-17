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
	check(w, "docker: registry", dockerContainerRunning("registry"))
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
	fmt.Fprintf(w, "  %s\n", apiState)
	fmt.Fprintln(w)

	// 6. Рекомендация
	fmt.Fprintln(w, "[6] Рекомендация")
	rec := doctorRecommend(c)
	report.Recommendation = rec
	fmt.Fprintln(w, rec)
	fmt.Fprintln(w)

	problems := countProblems(c)
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
	data, err := apiQuery(c, token, "{ getAppVersion }", nil)
	if err != nil {
		return fmt.Sprintf("✗ API не отвечает: %v", err)
	}
	return fmt.Sprintf("✓ API отвечает: %s", strings.TrimSpace(string(data)))
}

// doctorRecommend — текстовая рекомендация.
func doctorRecommend(c *Ctx) string {
	init := detectMarker(stageInit) && stageArtifactOK(c, stageInit)
	prepare := detectMarker(stagePrepareForCore) && stageArtifactOK(c, stagePrepareForCore)
	registry := detectMarker(stageInstallRegistry) && stageArtifactOK(c, stageInstallRegistry)

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
func countProblems(c *Ctx) int {
	n := 0
	for _, s := range nodeStages() {
		if detectMarker(s.Key) && !stageArtifactOK(c, s.Key) {
			n++
		}
	}
	if !serviceRunning("megapolos-core") {
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

	// Берём 10 свежих логов (getAllLogCommit: в ядре он доступен с лимитом).
	// Затем для каждого качаем getLogCommit(id) { log } (хвост ansible-вывода).
	query := `{
		getAllLogCommit(limit: 10) {
			id
			name
			type
			nodeId
			nodeName
			creationDate
			isClosed
		}
	}`
	data, err := apiQuery(c, token, query, nil)
	if err != nil {
		fmt.Fprintf(w, "✗ getAllLogCommit: %v\n", err)
		return 1
	}

	var parsed struct {
		GetAllLogCommit []struct {
			ID           string `json:"id"`
			Name         string `json:"name"`
			Type         string `json:"type"`
			NodeName     string `json:"nodeName"`
			CreationDate string `json:"creationDate"`
			IsClosed     bool   `json:"isClosed"`
		} `json:"getAllLogCommit"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		fmt.Fprintf(w, "✗ parse: %v\n", err)
		return 1
	}

	if len(parsed.GetAllLogCommit) == 0 {
		fmt.Fprintln(w, "нет логов (стадии ansible ещё не запускались)")
		return 0
	}

	fmt.Fprintf(w, "Найдено логов: %d (свежие первые; вывод обрезан до 80 последних строк)\n\n",
		len(parsed.GetAllLogCommit))
	for _, l := range parsed.GetAllLogCommit {
		fmt.Fprintf(w, "--- %s | %s | node=%s | %s | closed=%v ---\n",
			shortID(l.ID), l.Type, l.NodeName, l.CreationDate, l.IsClosed)
		logData, err := apiQuery(c, token,
			fmt.Sprintf(`{ getLogCommit(id: "%s") { log } }`, l.ID), nil)
		if err != nil {
			fmt.Fprintf(w, "(getLogCommit: %v)\n", err)
			continue
		}
		var logWrapped struct {
			GetLogCommit struct {
				Log string `json:"log"`
			} `json:"getLogCommit"`
		}
		if err := json.Unmarshal(logData, &logWrapped); err != nil {
			fmt.Fprintf(w, "(parse log: %v)\n", err)
			continue
		}
		log := logWrapped.GetLogCommit.Log
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
	cmd := exec.Command("bash", "-c",
		fmt.Sprintf("timeout 2 bash -c 'echo > /dev/tcp/%s' 2>/dev/null && echo open || echo closed", addr))
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