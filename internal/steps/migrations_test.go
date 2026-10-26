package steps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMigrationsUniqueNames — имена в реестре уникальны и упорядочены
// (имя начинается с номера: 026, 031, 033a…). Дубли имён сломали бы
// таблицу учёта (PRIMARY KEY name) и порядок применения.
func TestMigrationsUniqueNames(t *testing.T) {
	seen := map[string]bool{}
	for i, m := range migrations {
		if m.Name == "" {
			t.Errorf("migrations[%d]: пустое имя", i)
		}
		if seen[m.Name] {
			t.Errorf("миграция %q встречается в реестре дважды", m.Name)
		}
		seen[m.Name] = true
		if !strings.HasSuffix(m.Name, ".sql") {
			t.Errorf("миграция %q: ожидается файл .sql", m.Name)
		}
	}
	if len(migrations) == 0 {
		t.Fatal("реестр миграций пуст")
	}
}

// TestMigrationsOrder — реестр идёт в том же порядке, что и файлы в
// megapolos-core/install (по префиксу номера).
func TestMigrationsOrder(t *testing.T) {
	prev := ""
	for _, m := range migrations {
		if m.Name < prev {
			t.Errorf("нарушен порядок: %q идёт после %q", m.Name, prev)
		}
		prev = m.Name
	}
}

// TestMigrationsSentinels — у каждой миграции с ожидаемыми объектами задан
// сентинел (иначе неидемпотентный файл выполнялся бы повторно) либо миграция
// явно помечена как идемпотентная (Sentinel пуст допустим только для
// 028/034-dedupe — они написаны идемпотентно).
func TestMigrationsSentinels(t *testing.T) {
	// Миграции, которым не нужен Sentinel: написаны идемпотентно.
	allowNoSentinel := map[string]bool{
		"034-dedupe-internal-network-connection.sql": true, // DELETE + CREATE UNIQUE INDEX IF NOT EXISTS
	}
	// Миграции данных (INSERT/UPDATE), которым не нужен Expect*: результат —
	// строки, а не объект схемы; проверяется сентинелом.
	allowNoExpect := map[string]bool{
		"028-add-preset-tech-stacks.sql": true, // INSERT ... WHERE NOT EXISTS
	}
	for _, m := range migrations {
		if m.Sentinel == "" && !allowNoSentinel[m.Name] {
			t.Errorf("миграция %q: не задан Sentinel и нет разрешения", m.Name)
		}
		hasExpect := m.ExpectTable != "" || m.ExpectColumn != ""
		if !hasExpect && !allowNoExpect[m.Name] {
			t.Errorf("миграция %q: нет Expect* для финальной проверки схемы", m.Name)
		}
	}
}

// TestMigrationFilesExist — файлы реестра есть в ../..//megapolos-core/install.
// Тест пропускается, если репозиторий ядра недоступен (сборка в изолированном
// окружении/бандле).
func TestMigrationFilesExist(t *testing.T) {
	root := filepath.Join("..", "..", "..", "megapolos-core", "install")
	if _, err := os.Stat(root); err != nil {
		t.Skipf("каталог миграций ядра недоступен (%s): %v", root, err)
	}
	for _, m := range migrations {
		p := filepath.Join(root, m.Name)
		if _, err := os.Stat(p); err != nil {
			t.Errorf("файл миграции отсутствует: %s", p)
		}
	}
}

// TestShellQuoteLiteral — строковый литерал SQL экранирует одинарные кавычки.
func TestShellQuoteLiteral(t *testing.T) {
	cases := map[string]string{
		"simple":     "'simple'",
		"with'quote": "'with''quote'",
		"a'b'c":      "'a''b''c'",
		"":           "''",
	}
	for in, want := range cases {
		if got := shellQuoteLiteral(in); got != want {
			t.Errorf("shellQuoteLiteral(%q) = %q, ожидалось %q", in, got, want)
		}
	}
}

// TestShellQuote — экранирование для bash -c (одинарные кавычки).
func TestShellQuote(t *testing.T) {
	got := shellQuote("SELECT 1 FROM t WHERE n='x'")
	want := `'SELECT 1 FROM t WHERE n='\''x'\'''`
	if got != want {
		t.Errorf("shellQuote = %q, ожидалось %q", got, want)
	}
}
