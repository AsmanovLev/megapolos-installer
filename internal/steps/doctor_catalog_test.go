package steps

import (
	"strings"
	"testing"
)

// =============================================================================
// Тесты проверки источников App Store в --doctor.
//
// Покрывают то, на чём мы обожглись: doctor показывал «✓ Каталог приложений»
// (проверка только catalogUrl в config.json), хотя вкладок App Store не было —
// отсутствовала колонка catalog.kind, и сид ядра молча падал в catch.
//
// Проверяется парсер вывода psql и текст рендера; SQL-запросы не выполняются,
// БД не требуется.
// =============================================================================

// TestParseCatalogRows_WithKind — обычный случай после миграции 038.
func TestParseCatalogRows_WithKind(t *testing.T) {
	out := strings.Join([]string{
		"Megapolos|https://catalog.megapolos.net|remote|t|t|10",
		"Local App Store|local://app-export|local|f|t|20",
		"Каталог на порте 5000|http://localhost:5000|remote|f|f|100",
	}, "\n")

	rows := parseCatalogRows(out, true)
	if len(rows) != 3 {
		t.Fatalf("ждали 3 записи, получили %d: %+v", len(rows), rows)
	}
	if rows[0].Name != "Megapolos" || rows[0].Kind != "remote" || !rows[0].IsDefault || !rows[0].Enabled {
		t.Errorf("Megapolos разобран неверно: %+v", rows[0])
	}
	if rows[0].Priority != 10 {
		t.Errorf("priority: %d, ждали 10", rows[0].Priority)
	}
	if rows[1].Kind != "local" || rows[1].IsDefault {
		t.Errorf("Local App Store разобран неверно: %+v", rows[1])
	}
	if rows[1].URL != "local://app-export" {
		t.Errorf("псевдо-url локального каталога: %q", rows[1].URL)
	}
	if rows[2].Enabled {
		t.Errorf("третий каталог должен быть отключён: %+v", rows[2])
	}
}

// TestParseCatalogRows_WithoutKind — колонки kind нет (миграция 038 не
// применена). Все записи должны считаться remote, как после
// ADD COLUMN ... DEFAULT 'remote'.
func TestParseCatalogRows_WithoutKind(t *testing.T) {
	out := "Megapolos|https://catalog.megapolos.net|t|t|10"
	rows := parseCatalogRows(out, false)
	if len(rows) != 1 {
		t.Fatalf("ждали 1 запись, получили %d", len(rows))
	}
	if rows[0].Kind != "remote" {
		t.Errorf("без колонки kind записи должны быть remote, получили %q", rows[0].Kind)
	}
	if !rows[0].IsDefault || !rows[0].Enabled || rows[0].Priority != 10 {
		t.Errorf("флаги разобраны неверно: %+v", rows[0])
	}
}

// TestParseCatalogRows_Empty — пустая таблица (сид не выполнился).
func TestParseCatalogRows_Empty(t *testing.T) {
	for _, in := range []string{"", "\n", "   \n  \n"} {
		if rows := parseCatalogRows(in, true); len(rows) != 0 {
			t.Errorf("для входа %q ждали 0 записей, получили %+v", in, rows)
		}
	}
}

// TestParseCatalogRows_Malformed — битые строки пропускаются, а не паникуют.
func TestParseCatalogRows_Malformed(t *testing.T) {
	out := strings.Join([]string{
		"Megapolos|https://catalog.megapolos.net|remote|t|t|10",
		"мусор|без-полей",
		"",
		"|local://app-export|local|f|t|20",
	}, "\n")
	rows := parseCatalogRows(out, true)
	if len(rows) != 2 {
		t.Fatalf("ждали 2 валидные записи, получили %d: %+v", len(rows), rows)
	}
	// Пустое имя допустимо парсером (валидация — не его задача), важно, что
	// строка с недостаточным числом полей не попала в результат.
	if rows[1].Kind != "local" {
		t.Errorf("вторая запись должна быть local: %+v", rows[1])
	}
}

// TestCatalogHealth_Broken — что считается поломкой App Store.
func TestCatalogHealth_Broken(t *testing.T) {
	cases := []struct {
		name string
		h    CatalogHealth
		want bool
	}{
		{
			name: "нормально: remote + local",
			h: CatalogHealth{
				Checked: true, HasTable: true, HasKind: true,
				Rows: []CatalogRow{
					{Name: "Megapolos", Kind: "remote", IsDefault: true, Enabled: true},
					{Name: "Local App Store", Kind: "local", Enabled: true},
				},
			},
			want: false,
		},
		{
			name: "нет проверки (БД недоступна) — не выдумываем проблему",
			h:    CatalogHealth{Checked: false},
			want: false,
		},
		{
			name: "нет таблицы catalog",
			h:    CatalogHealth{Checked: true, HasTable: false},
			want: true,
		},
		{
			name: "нет колонки kind",
			h: CatalogHealth{
				Checked: true, HasTable: true, HasKind: false,
				Rows: []CatalogRow{{Name: "Megapolos", URL: "u", Kind: "remote", IsDefault: true, Enabled: true}},
			},
			want: true,
		},
		{
			name: "таблица пуста",
			h:    CatalogHealth{Checked: true, HasTable: true, HasKind: true},
			want: true,
		},
		{
			name: "нет локального каталога",
			h: CatalogHealth{
				Checked: true, HasTable: true, HasKind: true,
				Rows: []CatalogRow{{Name: "Megapolos", Kind: "remote", IsDefault: true, Enabled: true}},
			},
			want: true,
		},
		{
			name: "remote отключён, local есть",
			h: CatalogHealth{
				Checked: true, HasTable: true, HasKind: true,
				Rows: []CatalogRow{
					{Name: "Megapolos", Kind: "remote", IsDefault: true, Enabled: false},
					{Name: "Local App Store", Kind: "local", Enabled: true},
				},
			},
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.h.Broken(); got != tc.want {
				t.Errorf("Broken() = %v, ждали %v", got, tc.want)
			}
		})
	}
}

// TestCatalogHealth_HasLocal — поиск локального каталога.
func TestCatalogHealth_HasLocal(t *testing.T) {
	h := CatalogHealth{Rows: []CatalogRow{
		{Name: "Megapolos", Kind: "remote", IsDefault: true},
	}}
	if h.HasLocal() {
		t.Error("HasLocal() = true, локального каталога нет")
	}
	h.Rows = append(h.Rows, CatalogRow{Name: "Local App Store", Kind: "local"})
	if !h.HasLocal() {
		t.Error("HasLocal() = false, локальный каталог есть")
	}
}

// TestRenderCatalogHealth_Healthy — обе вкладки на месте.
func TestRenderCatalogHealth_Healthy(t *testing.T) {
	h := CatalogHealth{
		Checked: true, HasTable: true, HasKind: true,
		AppExportOK: true, AppExportCount: 3,
		Rows: []CatalogRow{
			{Name: "Megapolos", URL: "https://catalog.megapolos.net", Kind: "remote", IsDefault: true, Enabled: true, Priority: 10},
			{Name: "Local App Store", URL: "local://app-export", Kind: "local", Enabled: true, Priority: 20},
		},
	}
	var sb strings.Builder
	renderCatalogHealth(&sb, h)
	out := sb.String()

	if !strings.Contains(out, "✓ Каталог приложений") {
		t.Errorf("нет отметки основного каталога:\n%s", out)
	}
	if !strings.Contains(out, "https://catalog.megapolos.net") {
		t.Errorf("нет адреса каталога:\n%s", out)
	}
	if !strings.Contains(out, "✓ Локальное хранилище") {
		t.Errorf("нет отметки локального каталога:\n%s", out)
	}
	if !strings.Contains(out, "шаблонов: 3") {
		t.Errorf("не показано количество шаблонов:\n%s", out)
	}
	if strings.Contains(out, "✗") {
		t.Errorf("в здоровом состоянии не должно быть ✗:\n%s", out)
	}
}

// TestRenderCatalogHealth_NoKind — наш инцидент: колонки catalog.kind нет.
// Рендер обязан объяснить причину и подсказать миграцию 038.
func TestRenderCatalogHealth_NoKind(t *testing.T) {
	h := CatalogHealth{
		Checked: true, HasTable: true, HasKind: false,
		Rows: []CatalogRow{
			{Name: "Megapolos", URL: "https://catalog.megapolos.net", Kind: "remote", IsDefault: true, Enabled: true},
		},
	}
	var sb strings.Builder
	renderCatalogHealth(&sb, h)
	out := sb.String()

	if !strings.Contains(out, "✗ Локальное хранилище") {
		t.Errorf("нет ✗ для локального хранилища:\n%s", out)
	}
	if !strings.Contains(out, "catalog.kind") {
		t.Errorf("не названа причина (catalog.kind):\n%s", out)
	}
	if !strings.Contains(out, "038-add-catalog-kind.sql") {
		t.Errorf("нет подсказки про миграцию 038:\n%s", out)
	}
	if !strings.Contains(out, "seedCatalogs failed") {
		t.Errorf("не упомянут упавший сид:\n%s", out)
	}
}

// TestRenderCatalogHealth_NoTable — таблицы catalog нет вовсе.
func TestRenderCatalogHealth_NoTable(t *testing.T) {
	var sb strings.Builder
	renderCatalogHealth(&sb, CatalogHealth{Checked: true, HasTable: false})
	out := sb.String()

	if !strings.Contains(out, "✗ Каталоги в БД") {
		t.Errorf("нет ✗ про отсутствие таблицы:\n%s", out)
	}
	if !strings.Contains(out, "037-add-catalog.sql") {
		t.Errorf("нет подсказки про миграцию 037:\n%s", out)
	}
	// Без таблицы бессмысленно рассуждать про локальный каталог.
	if strings.Contains(out, "Local App Store") {
		t.Errorf("не должно быть деталей про Local App Store:\n%s", out)
	}
}

// TestRenderCatalogHealth_EmptyTable — таблица есть, записей нет.
func TestRenderCatalogHealth_EmptyTable(t *testing.T) {
	var sb strings.Builder
	renderCatalogHealth(&sb, CatalogHealth{Checked: true, HasTable: true, HasKind: true})
	out := sb.String()

	if !strings.Contains(out, "пуста") {
		t.Errorf("не сказано, что таблица пуста:\n%s", out)
	}
	if !strings.Contains(out, "seedCatalogs") {
		t.Errorf("нет подсказки про сид ядра:\n%s", out)
	}
}

// TestRenderCatalogHealth_Skipped — БД недоступна: это не ✗, а «—».
// Иначе doctor пугает пользователя проблемой App Store, когда упал postgresql
// (это отдельная и более важная проблема, её показывает секция [1]).
func TestRenderCatalogHealth_Skipped(t *testing.T) {
	var sb strings.Builder
	renderCatalogHealth(&sb, CatalogHealth{Checked: false, SkipReason: "postgresql не отвечает"})
	out := sb.String()

	if strings.Contains(out, "✗") {
		t.Errorf("при недоступной БД не должно быть ✗:\n%s", out)
	}
	if !strings.Contains(out, "проверка пропущена") {
		t.Errorf("не сказано, что проверка пропущена:\n%s", out)
	}
	if !strings.Contains(out, "postgresql не отвечает") {
		t.Errorf("не названа причина пропуска:\n%s", out)
	}
}

// TestRenderCatalogHealth_RemoteDisabled — основной каталог отключён в GUI.
func TestRenderCatalogHealth_RemoteDisabled(t *testing.T) {
	h := CatalogHealth{
		Checked: true, HasTable: true, HasKind: true, AppExportOK: true,
		Rows: []CatalogRow{
			{Name: "Megapolos", URL: "https://catalog.megapolos.net", Kind: "remote", IsDefault: true, Enabled: false},
			{Name: "Local App Store", URL: "local://app-export", Kind: "local", Enabled: true},
		},
	}
	var sb strings.Builder
	renderCatalogHealth(&sb, h)
	out := sb.String()

	if !strings.Contains(out, "ОТКЛЮЧЁН") {
		t.Errorf("не показано, что основной каталог отключён:\n%s", out)
	}
	if !strings.Contains(out, "«Каталог участвует в App Store»") {
		t.Errorf("нет подсказки, как включить каталог:\n%s", out)
	}
}

// TestRenderCatalogHealth_OtherCatalogs — пользовательские каталоги (напр.
// локальный megapolos-catalog на :5000) только перечисляются.
func TestRenderCatalogHealth_OtherCatalogs(t *testing.T) {
	h := CatalogHealth{
		Checked: true, HasTable: true, HasKind: true, AppExportOK: true,
		Rows: []CatalogRow{
			{Name: "Megapolos", URL: "https://catalog.megapolos.net", Kind: "remote", IsDefault: true, Enabled: true, Priority: 10},
			{Name: "Local App Store", URL: "local://app-export", Kind: "local", Enabled: true, Priority: 20},
			{Name: "Каталог на порте 5000", URL: "http://localhost:5000", Kind: "remote", Enabled: true, Priority: 100},
		},
	}
	var sb strings.Builder
	renderCatalogHealth(&sb, h)
	out := sb.String()

	if !strings.Contains(out, "Прочие каталоги") {
		t.Errorf("нет секции прочих каталогов:\n%s", out)
	}
	if !strings.Contains(out, "Каталог на порте 5000") {
		t.Errorf("не перечислен пользовательский каталог:\n%s", out)
	}
	if strings.Contains(out, "✗") {
		t.Errorf("наличие лишнего каталога не должно давать ✗:\n%s", out)
	}
}
