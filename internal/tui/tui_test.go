package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"megapolos/installer/internal/steps"
)

// withSimScreen подменяет фабрику приложения на SimulationScreen.
func withSimScreen(t *testing.T) *tcell.SimulationScreen {
	t.Helper()
	screen := tcell.NewSimulationScreen("UTF-8")
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	orig := newApp
	newApp = func() *tview.Application {
		app := tview.NewApplication().SetScreen(screen) // SetScreen вызывает screen.Init()
		screen.SetSize(120, 40)                         // → размер задаём ПОСЛЕ него
		return app
	}
	t.Cleanup(func() { newApp = orig })
	return &screen
}

func screenText(s *tcell.SimulationScreen) string {
	cells, w, h := (*s).GetContents()
	var sb strings.Builder
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := cells[y*w+x]
			if len(c.Runes) > 0 {
				sb.WriteRune(c.Runes[0])
			} else {
				sb.WriteByte(' ')
			}
		}
		sb.WriteByte('\n')
	}
	return sb.String()
}

func TestWizardRendersAndCancels(t *testing.T) {
	screen := withSimScreen(t)
	o := &steps.Opts{
		HostIP: "10.0.2.2", CoreRef: "main", GUIRef: "main",
		APIURL: "http://localhost:5100", DevMode: true,
		DBName: "megapolos", DBUser: "megapolos",
		AddSelfNode: true, NodeRootPassword: "megapolos",
	}

	done := make(chan error, 1)
	go func() { done <- Run(o, 2) }()

	// ждём отрисовку визарда
	// ВАЖНО: не вызываем screen.Show() из теста — draw держит lock экрана,
	// а app.SetRoot/QueueUpdateDraw в другой горутине держит lock приложения
	// и ждёт lock экрана → классический дедлок. Читаем только GetContents.
	deadline := time.Now().Add(5 * time.Second)
	for {
		txt := screenText(screen)
		if strings.Contains(txt, "Megapolos — установка") && strings.Contains(txt, "core ref") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("визард не отрисовался:\n%s", txt)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Tab — регрессия: перестилизация фокуса через QueueUpdateDraw из InputCapture
	// дедлокила main-loop (QueueUpdate блокируется до выполнения в main-горутине).
	// Если вернётся — Esc ниже не дойдёт и тест упрётся в таймаут.
	(*screen).InjectKey(tcell.KeyTab, 0, tcell.ModNone)
	time.Sleep(100 * time.Millisecond)

	// Esc → отмена
	(*screen).InjectKey(tcell.KeyEscape, 0, tcell.ModNone)
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "отменено") {
			t.Fatalf("ждали «отменено пользователем», получили: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() не завершился после Esc")
	}
}

func TestSanitize(t *testing.T) {
	// npm-подобный вывод: ANSI-цвета, \r-прогресс, control chars
	in := []byte("\x1b[32mok\x1b[0m line\rprogress 50%\x1b[K\nnext\tline\x07bell\n")
	got := string(sanitize(in))
	want := "ok line\nprogress 50%\nnext\tlinebell\n"
	if got != want {
		t.Fatalf("sanitize:\n got %q\nwant %q", got, want)
	}
	// неполная строка без \n тоже чистится при записи… проверяем побайтово, что нет ESC
	for _, b := range got {
		if b == 0x1b {
			t.Fatal("остался ESC")
		}
	}
}

// TestInputFieldShowsCursor: сфокусированное поле ввода должно показывать
// терминальный курсор на позиции текста (tview InputField → textArea.Draw →
// screen.ShowCursor). Регрессия: «не вижу курсор в полях».
func TestInputFieldShowsCursor(t *testing.T) {
	screen := tcell.NewSimulationScreen("UTF-8")
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	app := tview.NewApplication().SetScreen(screen) // SetScreen вызывает screen.Init()
	screen.SetSize(100, 30)                         // размер — после Init(), иначе сбросится к 80×25
	form := tview.NewForm()
	form.AddInputField("Поле", "abc", 40, nil, nil)
	done := make(chan struct{})
	go func() {
		_ = app.SetRoot(form, true).Run()
		close(done)
	}()
	defer func() { app.Stop(); <-done }()

	// ждём отрисовки с курсором (до 2с)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, _, vis := screen.GetCursor()
		if vis {
			return // курсор виден — ок
		}
		time.Sleep(50 * time.Millisecond)
	}
	_, _, vis := screen.GetCursor()
	t.Fatalf("курсор не виден при фокусе на InputField (vis=%v)", vis)
}

// TestWizardNarrowTerm: при 80 колонках форма не должна рисовать текст
// за границами экрана (регрессия «поля выпирают»: раньше fieldWidth был
// фиксированные 40 и рвался на узких терминалах).
func TestWizardNarrowTerm(t *testing.T) {
	screen := tcell.NewSimulationScreen("UTF-8")
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	orig := newApp
	newApp = func() *tview.Application {
		app := tview.NewApplication().SetScreen(screen) // SetScreen вызывает screen.Init()
		screen.SetSize(80, 24)                          // размер — после Init()
		return app
	}
	t.Cleanup(func() { newApp = orig })

	o := &steps.Opts{
		HostIP: "10.0.2.2", CoreRef: "main", GUIRef: "main",
		APIURL: "http://localhost:5100", DevMode: true,
		DBName: "megapolos", DBUser: "megapolos", BaseDomain: "megapolos.local",
		AddSelfNode: true, NodeRootPassword: "megapolos",
	}
	done := make(chan error, 1)
	go func() { done <- Run(o, 2) }()
	defer func() {
		screen.InjectKey(tcell.KeyEscape, 0, tcell.ModNone)
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		txt := screenText(&screen)
		if strings.Contains(txt, "core ref") { // отрисовался
			// регрессия «выпирания»: текст полей рисовался ПОВЕРХ рамки и за ней.
			// ОК: на колонках 0 и w-1 только рамочные символы или пробелы.
			w, h := screen.Size()
			frame := func(r rune) bool {
				return r == ' ' || strings.ContainsRune("┌─┐│└┘├┤┬┴┼", r)
			}
			for y := 0; y < h; y++ {
				l, _, _, _ := screen.GetContent(0, y)
				r, _, _, _ := screen.GetContent(w-1, y)
				if !frame(l) || !frame(r) {
					line := ""
					for x := 0; x < w; x++ {
						rr, _, _, _ := screen.GetContent(x, y)
						line += string(rr)
					}
					t.Fatalf("текст вне рамки на строке %d (выпирание):\n%q", y, line)
				}
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("визард не отрисовался за 5с")
}

// TestCursorMarkerNavigation: полный E2E маркера курсора — ввод текста,
// движение стрелками, позиция и символ под маркером, prefilled-поле.
func TestCursorMarkerNavigation(t *testing.T) {
	screen := tcell.NewSimulationScreen("UTF-8")
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	orig := newApp
	newApp = func() *tview.Application {
		app := tview.NewApplication().SetScreen(screen) // SetScreen вызывает screen.Init()
		screen.SetSize(100, 30)                         // размер — после Init()
		return app
	}
	t.Cleanup(func() { newApp = orig })

	o := &steps.Opts{
		HostIP: "10.0.2.2", CoreRef: "main", GUIRef: "main",
		APIURL: "http://localhost:5100", DevMode: true,
		DBName: "megapolos", DBUser: "megapolos",
		AddSelfNode: true, NodeRootPassword: "megapolos",
	}
	done := make(chan error, 1)
	go func() { done <- Run(o, 2) }()
	defer func() {
		screen.InjectKey(tcell.KeyEscape, 0, tcell.ModNone)
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
	}()

	// ждём визард
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(screenText(&screen), "core ref") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	redCell := func() (x, y int, ch rune, ok bool) {
		w, h := screen.Size()
		for yy := 0; yy < h; yy++ {
			for xx := 0; xx < w; xx++ {
				r, _, style, _ := screen.GetContent(xx, yy)
				fg, bg, _ := style.Decompose()
				if bg == tcell.ColorRed && fg == tcell.ColorWhite {
					return xx, yy, r, true
				}
			}
		}
		return 0, 0, 0, false
	}
	type runeAt = func(x, y int) rune
	var getRune runeAt = func(x, y int) rune {
		r, _, _, _ := screen.GetContent(x, y)
		return r
	}
	// columnOf — колонка (cell-индекс) первого вхождения sub. strings.Index
	// возвращает байтовое смещение, а row содержит многобайтные руны (кириллица
	// в лейблах, «», рамки) — с ним mx (колонка) сравнивать нельзя.
	columnOf := func(row, sub string) int {
		bi := strings.Index(row, sub)
		if bi < 0 {
			return -1
		}
		return len([]rune(row[:bi]))
	}

	// Tab: dropdown → первое поле («URL/путь», пустое)
	screen.InjectKey(tcell.KeyTab, 0, tcell.ModNone)

	// вводим "abc": маркер должен встать сразу за 'c' (пустая ячейка)
	for _, r := range "abc" {
		screen.InjectKey(tcell.KeyRune, r, tcell.ModNone)
	}
	time.Sleep(150 * time.Millisecond)

	mx, my, mch, ok := redCell()
	if !ok {
		t.Fatal("маркер не найден после ввода")
	}
	// в строке маркера ищем "abc": конец текста = x('c')+1
	w, _ := screen.Size()
	row := ""
	for x := 0; x < w; x++ {
		row += string(getRune(x, my))
	}
	ci := columnOf(row, "abc")
	if ci < 0 {
		t.Fatalf("введённый текст %q не найден в строке маркера %d: %q", "abc", my, row)
	}
	if mx != ci+3 {
		t.Fatalf("маркер на x=%d, а конец %q — x=%d (строка: %q)", mx, "abc", ci+3, row)
	}

	// ← : маркер над 'c'
	screen.InjectKey(tcell.KeyLeft, 0, tcell.ModNone)
	time.Sleep(120 * time.Millisecond)
	mx, _, mch, _ = redCell()
	if mch != 'c' {
		t.Fatalf("после ← маркер должен быть над 'c', получили %q (x=%d)", string(mch), mx)
	}
	// ← : над 'b'
	screen.InjectKey(tcell.KeyLeft, 0, tcell.ModNone)
	time.Sleep(120 * time.Millisecond)
	mx, _, mch, _ = redCell()
	if mch != 'b' {
		t.Fatalf("после ←← маркер должен быть над 'b', получили %q (x=%d)", string(mch), mx)
	}
	// → : обратно над 'c'
	screen.InjectKey(tcell.KeyRight, 0, tcell.ModNone)
	time.Sleep(120 * time.Millisecond)
	_, _, mch, _ = redCell()
	if mch != 'c' {
		t.Fatalf("после → маркер должен вернуться над 'c', получили %q", string(mch))
	}

	// prefilled-поле: Tab → core ref ("main", курсор в конце, маркер за 'n')
	screen.InjectKey(tcell.KeyTab, 0, tcell.ModNone)
	time.Sleep(120 * time.Millisecond)
	mx, my, mch, ok = redCell()
	if !ok {
		t.Fatal("маркер не найден на prefilled-поле")
	}
	row = ""
	for x := 0; x < w; x++ {
		row += string(getRune(x, my))
	}
	mi := columnOf(row, "main")
	if mi < 0 {
		t.Fatalf("%q не найдено в строке prefilled-поля: %q", "main", row)
	}
	if mx != mi+4 {
		t.Fatalf("маркер на x=%d, а конец %q — x=%d (строка: %q)", mx, "main", mi+4, row)
	}
	// ←×4 → маркер над 'm' (первая буква)
	for i := 0; i < 4; i++ {
		screen.InjectKey(tcell.KeyLeft, 0, tcell.ModNone)
	}
	time.Sleep(150 * time.Millisecond)
	_, _, mch, _ = redCell()
	if mch != 'm' {
		t.Fatalf("после ←×4 маркер должен быть над 'm', получили %q", string(mch))
	}
}
