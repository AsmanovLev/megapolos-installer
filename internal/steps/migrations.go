package steps

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"megapolos/installer/internal/sys"
)

// =============================================================================
// Схема БД: реестр миграций install/0NN-*.sql
//
// Проблема, которую закрывает файл:
//   - newpostgresql.sql (снапшот) заливается ТОЛЬКО в пустую БД, поэтому
//     исправления дампа не попадают на существующую установку;
//   - src/migrations каталога нет, таблицы трекинга нет, install/0NN-*.sql
//     не выполняет ни ядро, ни установщик (их применяли руками);
//   - единственный «догон» схемы — mikro-orm schema:update, который не создаёт
//     M2M-pivot, а его ошибка глушилась как WARN.
// Итог на стенде: новое ядро + старая схема → createAppVersion падал с
// «отношение app_module_version не существует».
//
// Решение: миграции применяются установщиком по порядку, с таблицей учёта
// megapolos_schema_migration и сентинелами. Сентинел — SQL-проверка «объект
// уже есть»; если да, файл не выполняется (в 026/027/029/030/031 нет
// IF NOT EXISTS, прямой прогон упал бы на already exists).
// =============================================================================

const migrationsTable = "megapolos_schema_migration"

// migration — одна инкрементальная миграция снапшота.
type migration struct {
	// Name — имя файла в megapolos-core/install.
	Name string
	// Sentinel — SQL, возвращающий строку, если объект миграции уже существует.
	// Пусто — миграция всегда выполняется (данные/идемпотентный скрипт).
	Sentinel string
	// Expect — что должно существовать после применения (имя таблицы).
	// Используется финальной проверкой схемы (assertSchemaSentinels).
	ExpectTable string
	// ExpectColumn — опционально: table.column, которое должно существовать.
	ExpectColumn string
}

// migrations — реестр в порядке применения. Список начинается с 026:
// объекты 001..025 содержатся в newpostgresql.sql и в любом живом стенде.
var migrations = []migration{
	{
		Name:         "026-add-infrastructure-type-to-app.sql",
		Sentinel:     colSentinel("app", "infrastructure_type"),
		ExpectColumn: "app.infrastructure_type",
	},
	{
		Name:         "027-add-system-flag-to-app.sql",
		Sentinel:     colSentinel("app", "system"),
		ExpectColumn: "app.system",
	},
	{
		Name:     "028-add-preset-tech-stacks.sql",
		Sentinel: "SELECT 1 FROM tech_stack WHERE name = 'HTML/CSS/JS' LIMIT 1",
	},
	{
		Name:         "029-add-is_private-to-repository.sql",
		Sentinel:     colSentinel("repository", "is_private"),
		ExpectColumn: "repository.is_private",
	},
	{
		Name:         "030-add-is_zero-to-app_version.sql",
		Sentinel:     colSentinel("app_version", "is_zero"),
		ExpectColumn: "app_version.is_zero",
	},
	{
		Name: "031-add-app-module-version.sql",
		// Миграция не идемпотентна (CREATE TABLE без IF NOT EXISTS) —
		// проверяем таблицу, колонку и оба pivot-а.
		Sentinel:     "SELECT 1 FROM information_schema.tables WHERE table_schema='public' AND table_name='app_module_version' LIMIT 1",
		ExpectTable:  "app_module_version",
		ExpectColumn: "app_version.hash",
	},
	{
		Name:         "032-add-is-default-to-configuration.sql",
		Sentinel:     colSentinel("configuration", "is_default"),
		ExpectColumn: "configuration.is_default",
	},
	{
		Name: "033-add-internal-network-connection.sql",
		// Таблица создаётся IF NOT EXISTS, но ADD CONSTRAINT — без проверки.
		Sentinel:    "SELECT 1 FROM information_schema.tables WHERE table_schema='public' AND table_name='internal_network_connection' LIMIT 1",
		ExpectTable: "internal_network_connection",
	},
	{
		Name:         "033-add-root-domain-to-configuration.sql",
		Sentinel:     colSentinel("configuration", "root_domain_id"),
		ExpectColumn: "configuration.root_domain_id",
	},
	{
		Name:        "034-add-node-metric.sql",
		Sentinel:    "SELECT 1 FROM information_schema.tables WHERE table_schema='public' AND table_name='node_metric' LIMIT 1",
		ExpectTable: "node_metric",
	},
	{
		Name: "034-dedupe-internal-network-connection.sql",
		// Идемпотентна (DELETE + CREATE UNIQUE INDEX IF NOT EXISTS), но
		// осмысленна только вместе с 033 — сентинела нет.
		ExpectTable: "internal_network_connection",
	},
	{
		Name:         "035-node-topology.sql",
		Sentinel:     colSentinel("node", "public_address"),
		ExpectColumn: "node.public_address",
	},
	{
		Name:        "036-certificate.sql",
		Sentinel:    "SELECT 1 FROM information_schema.tables WHERE table_schema='public' AND table_name='certificate' LIMIT 1",
		ExpectTable: "certificate",
	},
	{
		// Каталоги приложений: несколько источников вместо одного config.catalogUrl.
		Name:        "037-add-catalog.sql",
		Sentinel:    "SELECT 1 FROM information_schema.tables WHERE table_schema='public' AND table_name='catalog' LIMIT 1",
		ExpectTable: "catalog",
	},
	{
		// Тип источника каталога: remote | local. Без этой колонки ядро не
		// поднимает Local App Store (seedCatalogs ищет kind='local'), поэтому
		// шаг db-migrate обязан её создать и проверить в финальной сверке.
		Name:         "038-add-catalog-kind.sql",
		Sentinel:     colSentinel("catalog", "kind"),
		ExpectColumn: "catalog.kind",
	},
}

// colSentinel — SQL-проверка существования колонки.
func colSentinel(table, column string) string {
	return fmt.Sprintf(
		"SELECT 1 FROM information_schema.columns WHERE table_name='%s' AND column_name='%s' LIMIT 1",
		table, column)
}

// psqlQ — запуск SQL-запроса от postgres; возвращает stdout.
func psqlQ(c *Ctx, sql string) (string, error) {
	return out(c, fmt.Sprintf(
		"sudo -u postgres psql -d %s -X -A -t -c %s",
		c.O.DBName, shellQuote(sql)))
}

// psqlTrue — true, если запрос вернул непустой результат.
func psqlTrue(c *Ctx, sql string) bool {
	s, err := psqlQ(c, sql)
	return err == nil && strings.TrimSpace(s) != ""
}

// shellQuote — безопасное экранирование для одинарных кавычек bash.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ensureMigrationsTable — таблица учёта применённых миграций.
// source: 'dump' (снапшот содержал объекты), 'sql' (файл выполнен),
// 'sentinel' (объекты уже были, файл пропущен).
func ensureMigrationsTable(c *Ctx, w io.Writer) error {
	return sh(c, w, fmt.Sprintf(
		`sudo -u postgres psql -v ON_ERROR_STOP=1 -q -d %s -c `+
			`"CREATE TABLE IF NOT EXISTS %s (`+
			`name text PRIMARY KEY, `+
			`applied_at timestamp(6) NOT NULL DEFAULT now(), `+
			`source text NOT NULL)"`,
		c.O.DBName, migrationsTable))
}

// migrationApplied — запись о миграции уже есть в таблице учёта.
func migrationApplied(c *Ctx, name string) bool {
	return psqlTrue(c, fmt.Sprintf(
		"SELECT 1 FROM %s WHERE name = %s LIMIT 1",
		migrationsTable, shellQuoteLiteral(name)))
}

// markMigration — фиксирует миграцию как применённую (идемпотентно).
func markMigration(c *Ctx, w io.Writer, name, source string) error {
	return sh(c, w, fmt.Sprintf(
		`sudo -u postgres psql -v ON_ERROR_STOP=1 -q -d %s -c `+
			`"INSERT INTO %s (name, source) VALUES (%s, %s) `+
			`ON CONFLICT (name) DO NOTHING"`,
		c.O.DBName, migrationsTable,
		shellQuoteLiteral(name), shellQuoteLiteral(source)))
}

// shellQuoteLiteral — строковый литерал SQL в одинарных кавычках.
func shellQuoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// markAllMigrationsFromDump — после заливки снапшота: все миграции реестра
// считаются применёнными (дамп содержит их объекты). source='dump'.
func markAllMigrationsFromDump(c *Ctx, w io.Writer) error {
	if err := ensureMigrationsTable(c, w); err != nil {
		return err
	}
	for _, m := range migrations {
		if err := markMigration(c, w, m.Name, "dump"); err != nil {
			return err
		}
	}
	fmt.Fprintf(w, "снапшот: %d миграций помечены применёнными (source=dump)\n", len(migrations))
	return nil
}

// pendingMigrations — имена миграций реестра, которые ещё не отмечены в
// таблице учёта. Легковесная проверка для DetectF (без выполнения SQL).
// Если таблицы учёта нет, но БД непустая — все миграции считаются
// неприменёнными: db-migrate должен один раз пройтись и расставить отметки.
func pendingMigrations(c *Ctx) []string {
	have := map[string]bool{}
	if psqlTrue(c, fmt.Sprintf(
		"SELECT 1 FROM information_schema.tables WHERE table_schema='public' AND table_name='%s' LIMIT 1",
		migrationsTable)) {
		s, err := psqlQ(c, fmt.Sprintf("SELECT name FROM %s", migrationsTable))
		if err == nil {
			for _, line := range strings.Split(s, "\n") {
				if name := strings.TrimSpace(line); name != "" {
					have[name] = true
				}
			}
		}
	}
	var pending []string
	for _, m := range migrations {
		if !have[m.Name] {
			pending = append(pending, m.Name)
		}
	}
	return pending
}

// migrationPath — путь к файлу миграции: сначала бандл, затем клон ядра.
func migrationPath(c *Ctx, coreDir, name string) string {
	if c.O.BundleDir != "" {
		p := filepath.Join(c.O.BundleDir, "pg", "migrations", name)
		if sys.FileExists(c, c.Ex, p) {
			return p
		}
	}
	return filepath.Join(coreDir, "install", name)
}

// migrationObjectsPresent — все ожидаемые объекты миграции физически есть.
// Для миграций без Expect* (данные) считается, что проверять нечего.
func migrationObjectsPresent(c *Ctx, m migration) bool {
	if m.ExpectTable != "" {
		q := fmt.Sprintf(
			"SELECT 1 FROM information_schema.tables WHERE table_schema='public' AND table_name='%s' LIMIT 1",
			m.ExpectTable)
		if !psqlTrue(c, q) {
			return false
		}
	}
	if m.ExpectColumn != "" {
		parts := strings.SplitN(m.ExpectColumn, ".", 2)
		q := fmt.Sprintf(
			"SELECT 1 FROM information_schema.columns WHERE table_name='%s' AND column_name='%s' LIMIT 1",
			parts[0], parts[1])
		if !psqlTrue(c, q) {
			return false
		}
	}
	return true
}

// applyPendingMigrations — прогоняет миграции реестра по порядку.
//
// Правила для каждой миграции:
//  1. Отметки нет, но объекты на месте (старый стенд / ручное применение /
//     снапшот) → пометить source='sentinel', файл НЕ выполнять
//     (026/027/029/030/031 не идемпотентны и упали бы на already exists).
//  2. Отметка есть, а объекта физически нет (БД пересоздали/объект удалили) →
//     снять отметку и применить файл заново — самовосстановление.
//  3. Иначе — выполнить файл (source='sql').
func applyPendingMigrations(c *Ctx, w io.Writer, coreDir string) error {
	if err := ensureMigrationsTable(c, w); err != nil {
		return err
	}

	appliedSQL, skippedSentinel, upToDate, repaired := 0, 0, 0, 0

	for _, m := range migrations {
		applied := migrationApplied(c, m.Name)
		present := migrationObjectsPresent(c, m)

		if applied && present {
			upToDate++
			continue
		}
		if applied && !present {
			fmt.Fprintf(w, "  %s: отмечена применённой, но объектов нет — применяю заново\n", m.Name)
			if err := sh(c, w, fmt.Sprintf(
				`sudo -u postgres psql -v ON_ERROR_STOP=1 -q -d %s -c "DELETE FROM %s WHERE name = %s"`,
				c.O.DBName, migrationsTable, shellQuoteLiteral(m.Name))); err != nil {
				return err
			}
			repaired++
		}

		// Объекты миграции уже есть — файл может быть не идемпотентен.
		if !applied && m.Sentinel != "" && psqlTrue(c, m.Sentinel) {
			if err := markMigration(c, w, m.Name, "sentinel"); err != nil {
				return err
			}
			fmt.Fprintf(w, "  %s: объекты уже есть — помечено без выполнения\n", m.Name)
			skippedSentinel++
			continue
		}

		path := migrationPath(c, coreDir, m.Name)
		if !sys.FileExists(c, c.Ex, path) {
			return fmt.Errorf("миграция %s не найдена (ожидался %s)", m.Name, path)
		}
		fmt.Fprintf(w, "  применяю %s\n", m.Name)
		if err := sh(c, w, fmt.Sprintf(
			"sudo -u postgres psql -v ON_ERROR_STOP=1 -q -d %s -f %s",
			c.O.DBName, path)); err != nil {
			return fmt.Errorf("миграция %s: %w", m.Name, err)
		}
		if err := markMigration(c, w, m.Name, "sql"); err != nil {
			return err
		}
		appliedSQL++
	}

	fmt.Fprintf(w, "миграции: выполнено %d, помечено по сентинелу %d, переприменено %d, уже актуально %d\n",
		appliedSQL, skippedSentinel, repaired, upToDate)
	return nil
}

// assertSchemaSentinels — финальная проверка: все ожидаемые объекты на месте.
// Неполная схема → ошибка шага (а не WARN), иначе bootstrap падает позже
// на createAppVersion с «отношение … не существует».
func assertSchemaSentinels(c *Ctx) error {
	var missing []string
	for _, m := range migrations {
		if m.ExpectTable != "" {
			q := fmt.Sprintf(
				"SELECT 1 FROM information_schema.tables WHERE table_schema='public' AND table_name='%s' LIMIT 1",
				m.ExpectTable)
			if !psqlTrue(c, q) {
				missing = append(missing, "таблица "+m.ExpectTable)
			}
		}
		if m.ExpectColumn != "" {
			parts := strings.SplitN(m.ExpectColumn, ".", 2)
			q := fmt.Sprintf(
				"SELECT 1 FROM information_schema.columns WHERE table_name='%s' AND column_name='%s' LIMIT 1",
				parts[0], parts[1])
			if !psqlTrue(c, q) {
				missing = append(missing, "колонка "+m.ExpectColumn)
			}
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("схема БД неполная, отсутствует: %s\n"+
			"createAppVersion/деплой упадут на этих объектах.\n"+
			"Проверьте: sudo -u postgres psql -d %s -c '\\dt'",
			strings.Join(missing, ", "), c.O.DBName)
	}
	return nil
}

// migrationStates — отчёт для --info/диагностики: какие миграции применены.
func migrationStates(c *Ctx) string {
	if !psqlTrue(c, fmt.Sprintf(
		"SELECT 1 FROM information_schema.tables WHERE table_schema='public' AND table_name='%s' LIMIT 1",
		migrationsTable)) {
		return "таблица " + migrationsTable + " отсутствует (миграции не применялись)"
	}
	s, err := psqlQ(c, fmt.Sprintf(
		"SELECT name || ' | ' || source FROM %s ORDER BY name", migrationsTable))
	if err != nil {
		return "ошибка чтения " + migrationsTable + ": " + err.Error()
	}
	return strings.TrimSpace(s)
}
