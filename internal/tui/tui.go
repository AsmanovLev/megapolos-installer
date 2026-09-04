// Package tui — интерактивный интерфейс установщика на tview:
// визард параметров, затем экран прогресса со списком шагов и лог-панелями
// (по панели на параллельный слот — видно, чем занят каждый поток runner'а).
//
// Устойчивость терминала:
//   - экран создаём сами и гарантированно Fini() при любом выходе
//     (panic/SIGINT/SIGHUP) — иначе терминал остаётся в mouse-режиме
//     и колёсико печатает мусор вида «35;103;11M»;
//   - вывод процессов санитизируется (ANSI-escape, \r, control chars) —
//     сырой npm/apt вывод ломает разметку TextView;
//   - отрисовка троттлится (10 fps), логи буферизуются.
package tui

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"megapolos/installer/internal/steps"
	"megapolos/installer/internal/sys"
)

// newApp — фабрика приложения (в тестах подменяется на SimulationScreen).
var newApp = defaultApp

// screenFini — восстановление терминала (устанавливается фабрикой).
var screenFini func()

func defaultApp() *tview.Application {
	screen, err := tcell.NewScreen()
	if err != nil {
		panic(err)
	}
	screen.SetCursorStyle(tcell.CursorStyleBlinkingBlock) // заметный курсор в полях ввода
	screenFini = func() { screen.Fini() }
	return tview.NewApplication().SetScreen(screen)
}

// setTheme — классическая curses-палитра (whiptail / debian-installer):
// синий фон, серый диалог, чёрный текст, красная строка выбора.
func setTheme() {
	tview.Styles.PrimitiveBackgroundColor = tcell.ColorLightGray
	tview.Styles.PrimaryTextColor = tcell.ColorBlack
	tview.Styles.SecondaryTextColor = tcell.ColorNavy
	tview.Styles.TertiaryTextColor = tcell.ColorDarkSlateGray
	tview.Styles.BorderColor = tcell.ColorBlack
	tview.Styles.TitleColor = tcell.ColorNavy
	tview.Styles.GraphicsColor = tcell.ColorBlack
	tview.Styles.ContrastBackgroundColor = tcell.ColorWhite      // поля ввода
	tview.Styles.MoreContrastBackgroundColor = tcell.ColorMaroon // фокус/выбор
	tview.Styles.InverseTextColor = tcell.ColorWhite
	tview.Styles.ContrastSecondaryTextColor = tcell.ColorYellow
}

// Run — визард + прогон установки (один цикл tview-приложения).
// Возвращает ошибку установки или «отменено пользователем».
func Run(o *steps.Opts, jobs int) (runErr error) {
	setTheme()
	app := newApp()
	// терминал восстанавливаем ВСЕГДА, даже при panic
	defer func() {
		if screenFini != nil {
			screenFini()
		}
	}()
	defer func() {
		if r := recover(); r != nil {
			runErr = fmt.Errorf("внутренняя ошибка TUI: %v", r)
		}
	}()
	// SIGHUP (обрыв ssh) / SIGTERM — аккуратный выход
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGHUP, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		<-sigCh
		app.Stop()
	}()

	pages := tview.NewPages()

	// ---------------- визард ----------------
	localLabel := "локальное зеркало http://" + o.HostIP + ":8000"
	gitlabLabel := "gitlab.com https://gitlab.com/megapolos"
	srcIdx := 1
	if strings.HasPrefix(o.GitBase, "http://"+o.HostIP) {
		srcIdx = 0
	}

	form := tview.NewForm()
	form.SetBorder(true).SetTitle(" Megapolos — установка ")
	form.AddDropDown("Источник репозиториев", []string{localLabel, gitlabLabel}, srcIdx, nil)
	form.AddInputField("core ref (ветка/тег/sha)", o.CoreRef, 40, nil, func(s string) { o.CoreRef = strings.TrimSpace(s) })
	form.AddInputField("gui ref (ветка/тег/sha)", o.GUIRef, 40, nil, func(s string) { o.GUIRef = strings.TrimSpace(s) })
	form.AddInputField("API URL для GUI", o.APIURL, 40, nil, func(s string) { o.APIURL = strings.TrimSpace(s) })
	form.AddCheckbox("GUI на этой машине (nginx :80)", o.GUI, func(b bool) { o.GUI = b })
	form.AddCheckbox("HTTPS для GUI (серт Megapolos CA, :443)", o.GUITLS, func(b bool) { o.GUITLS = b })
	form.AddCheckbox("devMode (localhost, self-signed CA)", o.DevMode, func(b bool) { o.DevMode = b })
	form.AddCheckbox("debug-логи ядра", o.Debug, func(b bool) { o.Debug = b })
	swapInitial := o.Swap == "force" || ((o.Swap == "" || o.Swap == "auto") && localNeedSwap())
	form.AddCheckbox("swap 2G (при <8G RAM)", swapInitial, func(b bool) {
		if b {
			o.Swap = "force"
		} else {
			o.Swap = "skip"
		}
	})
	form.AddInputField("Базовый домен", o.BaseDomain, 40, nil, func(s string) { o.BaseDomain = strings.TrimSpace(s) })
	form.AddInputField("Имя БД", o.DBName, 40, nil, func(s string) { o.DBName = strings.TrimSpace(s) })
	form.AddInputField("Пользователь БД", o.DBUser, 40, nil, func(s string) { o.DBUser = strings.TrimSpace(s) })
	form.AddCheckbox("Себя-нода (root@127.0.0.1:22)", o.AddSelfNode, func(b bool) { o.AddSelfNode = b })
	form.AddPasswordField("Пароль root для ноды", o.NodeRootPassword, 40, '*', func(s string) { o.NodeRootPassword = s })
	form.AddButton("Начать установку", func() {
		idx, _ := form.GetFormItemByLabel("Источник репозиториев").(*tview.DropDown).GetCurrentOption()
		if idx == 0 {
			o.GitBase = "http://" + o.HostIP + ":8000"
		} else {
			o.GitBase = "https://gitlab.com/megapolos"
		}
		if o.CoreRef == "" || o.GUIRef == "" {
			runErr = fmt.Errorf("core/gui ref не могут быть пустыми")
			app.Stop()
			return
		}
		startRun(app, pages, o, jobs, &runErr)
	})
	cancel := func() {
		runErr = fmt.Errorf("отменено пользователем")
		app.Stop()
	}
	form.AddButton("Выход", cancel)
	form.SetCancelFunc(cancel)
	// кнопки в классике: чёрные на сером, фокус — белые на красном
	for i := 0; i < form.GetButtonCount(); i++ {
		form.GetButton(i).
			SetStyle(tcell.StyleDefault.Foreground(tcell.ColorBlack).Background(tcell.ColorSilver)).
			SetBackgroundColorActivated(tcell.ColorMaroon).
			SetLabelColorActivated(tcell.ColorWhite)
	}
	// dropdown: фокус и выбранный пункт списка — белые на красном
	if dd, ok := form.GetFormItemByLabel("Источник репозиториев").(*tview.DropDown); ok {
		maroon := tcell.StyleDefault.Foreground(tcell.ColorWhite).Background(tcell.ColorMaroon)
		plain := tcell.StyleDefault.Foreground(tcell.ColorBlack).Background(tcell.ColorLightGray)
		dd.SetListStyles(plain, maroon)
		dd.SetFocusedStyle(maroon)
	}
	// подсветка поля под фокусом (InputField/Checkbox не имеют focused-стилей
	// в tview 0.42 → перестилизуем по событию фокуса)
	unfocusedField := tcell.StyleDefault.Foreground(tcell.ColorBlack).Background(tcell.ColorWhite)
	focusedField := tcell.StyleDefault.Foreground(tcell.ColorWhite).Background(tcell.ColorMaroon)
	labelPlain := tcell.StyleDefault.Foreground(tcell.ColorBlack)
	labelFocus := tcell.StyleDefault.Foreground(tcell.ColorMaroon).Bold(true)
	restyle := func() {
		fi, _ := form.GetFocusedItemIndex()
		for i := 0; i < form.GetFormItemCount(); i++ {
			switch item := form.GetFormItem(i).(type) {
			case *tview.InputField:
				if i == fi {
					item.SetFieldStyle(focusedField)
					item.SetLabelStyle(labelFocus)
				} else {
					item.SetFieldStyle(unfocusedField)
					item.SetLabelStyle(labelPlain)
				}
			case *tview.Checkbox:
				if i == fi {
					item.SetFieldBackgroundColor(tcell.ColorMaroon)
					item.SetFieldTextColor(tcell.ColorWhite)
					item.SetLabelStyle(labelFocus)
				} else {
					item.SetFieldBackgroundColor(tcell.ColorLightGray)
					item.SetFieldTextColor(tcell.ColorBlack)
					item.SetLabelStyle(labelPlain)
				}
			}
		}
	}
	restyle()
	// ширина полей ввода — чтобы влезали в диалог на узких терминалах
	for _, label := range []string{"core ref (ветка/тег/sha)", "gui ref (ветка/тег/sha)", "API URL для GUI", "Базовый домен", "Имя БД", "Пользователь БД", "Пароль root для ноды"} {
		if f, ok := form.GetFormItemByLabel(label).(*tview.InputField); ok {
			f.SetFieldWidth(34)
		}
	}
	// QueueUpdateDraw БЛОКИРУЕТ до выполнения в main-горутине; InputCapture сам
	// выполняется в main-горутине → прямой вызов = дедлок (фриз после 1-й клавиши).
	// Поэтому перестилизацию фокуса запускаем из свежей горутины: она выполнится
	// уже ПОСЛЕ обработки клавиши формой (фокус смещён).
	form.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		go app.QueueUpdateDraw(restyle)
		return ev
	})
	form.SetMouseCapture(func(action tview.MouseAction, ev *tcell.EventMouse) (tview.MouseAction, *tcell.EventMouse) {
		go app.QueueUpdateDraw(restyle)
		return action, ev
	})

	// синий backdrop + центрированный «диалог» — классический вид curses-установщика
	// (ширина/высота пропорциональные — адаптируется к размеру терминала)
	backdrop := tview.NewBox().SetBackgroundColor(tcell.ColorNavy)
	dialog := tview.NewFlex().
		AddItem(nil, 0, 1, false).
		AddItem(tview.NewFlex().SetDirection(tview.FlexRow).
			AddItem(nil, 0, 1, false).
			AddItem(form, 0, 5, true).
			AddItem(nil, 0, 1, false), 0, 4, true).
		AddItem(nil, 0, 1, false)

	pages.AddPage("backdrop", backdrop, true, true)
	pages.AddPage("wizard", dialog, true, true)

	// курсор: tcell при Init() сбрасывает стиль курсора → форсим его после
	// КАЖДОЙ отрисовки (InputField рисует курсор через встроенную TextArea,
	// но со стилем по умолчанию он тонкий и теряется на красном фоне).
	app.SetAfterDrawFunc(func(screen tcell.Screen) {
		screen.SetCursorStyle(tcell.CursorStyleBlinkingBlock)
	})

	if err := app.SetRoot(pages, true).EnableMouse(true).Run(); err != nil && runErr == nil {
		return err
	}
	return runErr
}

// localNeedSwap — эвристика дефолта чекбокса swap в визарде (RAM < 8G).
var memTotalRe = regexp.MustCompile(`MemTotal:\s+(\d+)`)

func localNeedSwap() bool {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return true
	}
	m := memTotalRe.FindSubmatch(b)
	if m == nil {
		return true
	}
	kb, _ := strconv.ParseInt(string(m[1]), 10, 64)
	return kb < 8*1024*1024
}

// osc52Copy — копирование в буфер обмена через OSC 52 (работает и по ssh,
// если терминал поддерживает: kitty/alacritty/wezterm/xterm+allowWindowOps;
// GNOME Terminal/VTE игнорирует — тогда логи забирать из файла).
func osc52Copy(data string) error {
	f, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "\x1b]52;c;%s\x07", base64.StdEncoding.EncodeToString([]byte(data)))
	return err
}

// ansiRe — ANSI escape-последовательности (CSI, OSC, простые ESC).
var ansiRe = regexp.MustCompile("\x1b\\[[0-9;?]*[a-zA-Z]|\x1b\\][^\x07\x1b]*(\x07|\x1b\\\\)|\x1b[()][0-9A-B]|\x1b[=>#]?[0-9]?")

// sanitize убирает из вывода процессов всё, что ломает TextView:
// ANSI-escapes, \r (прогресс-бары), control chars кроме \n и \t.
func sanitize(p []byte) []byte {
	p = ansiRe.ReplaceAll(p, nil)
	p = bytes.ReplaceAll(p, []byte("\r\n"), []byte("\n"))
	p = bytes.ReplaceAll(p, []byte("\r"), []byte("\n"))
	out := p[:0]
	for _, b := range p {
		if b == '\n' || b == '\t' || b >= 0x20 {
			out = append(out, b)
		}
	}
	return out
}

// styleDark — тёмная панель для экрана прогресса (независимо от светлой темы визарда).
func styleDark(tv *tview.TextView, title string) {
	tv.SetBackgroundColor(tcell.ColorBlack)
	tv.SetTextColor(tcell.ColorLightGray)
	tv.SetBorderColor(tcell.ColorGray)
	tv.SetTitleColor(tcell.ColorWhite)
	tv.SetTitle(title)
}

// pane — лог-панель: накапливает санитизированный вывод, тикер сбрасывает в TextView.
// Параллельно дублирует строки в лог-файл (с префиксом имени шага).
type pane struct {
	tv      *tview.TextView
	name    string // текущий шаг (для префикса в лог-файле)
	logFile *os.File

	mu      sync.Mutex
	pending []byte
	line    []byte // недописанная строка (ждём \n)
	size    int    // оценка размера содержимого tv
}

const paneCap = 512 * 1024

func (p *pane) write(b []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.line = append(p.line, b...)
	for {
		i := bytes.IndexByte(p.line, '\n')
		if i < 0 {
			break
		}
		raw := p.line[:i+1]
		p.line = p.line[i+1:]
		clean := sanitize(raw)
		p.pending = append(p.pending, clean...)
		if p.logFile != nil && len(clean) > 1 {
			line := strings.TrimRight(string(clean), "\n")
			fmt.Fprintf(p.logFile, "[%s] %s\n", p.name, line)
		}
	}
}

// flush вызывается ТОЛЬКО из главной горутины (через QueueUpdateDraw тикера).
func (p *pane) flush() {
	p.mu.Lock()
	if len(p.pending) == 0 {
		p.mu.Unlock()
		return
	}
	data := p.pending
	p.pending = nil
	p.size += len(data)
	needTrim := p.size > paneCap
	p.mu.Unlock()

	if needTrim {
		full := p.tv.GetText(false)
		if len(full) > paneCap/2 {
			full = full[len(full)-paneCap/2:]
		}
		p.tv.SetText(full)
		p.mu.Lock()
		p.size = len(full)
		p.mu.Unlock()
	}
	fmt.Fprint(p.tv, string(data))
	p.tv.ScrollToEnd()
}

func (p *pane) reset(step string) {
	p.mu.Lock()
	p.name = step
	p.pending = nil
	p.line = nil
	p.size = 0
	p.mu.Unlock()
	p.tv.Clear()
	p.tv.SetTitle(" [yellow]●[-] " + step + " ")
}

// paneWriter — writer шага в панель.
type paneWriter struct{ p *pane }

func (w *paneWriter) Write(b []byte) (int, error) {
	w.p.write(b)
	return len(b), nil
}

// stepLine — снапшот состояния шага для отрисовки (иммутабельный).
type stepLine struct {
	name  string
	state steps.State
	note  string
}

// runUI — UI-адаптер runner'а для tview.
type runUI struct {
	app    *tview.Application
	panes  [2]*pane
	render func([]stepLine) // вызывается через QueueUpdateDraw

	mu      sync.Mutex
	order   []string
	state   map[string]steps.State
	note    map[string]string
	slot    map[string]int // step → pane index
	slotUse [2]string      // pane index → step
}

func (ui *runUI) snapshot() []stepLine {
	out := make([]stepLine, len(ui.order))
	for i, n := range ui.order {
		out[i] = stepLine{n, ui.state[n], ui.note[n]}
	}
	return out
}

func (ui *runUI) Emit(e steps.Event) {
	ui.mu.Lock()
	ui.state[e.Step] = e.State
	ui.note[e.Step] = e.Note
	if e.State == steps.Running {
		for i, used := range ui.slotUse {
			if used == "" {
				ui.slotUse[i] = e.Step
				ui.slot[e.Step] = i
				pane, step := ui.panes[i], e.Step
				ui.app.QueueUpdateDraw(func() {
					pane.reset(step)
				})
				break
			}
		}
	}
	if e.State == steps.Done || e.State == steps.Failed || e.State == steps.Skipped {
		if i, ok := ui.slot[e.Step]; ok {
			pane, step, st := ui.panes[i], e.Step, e.State
			mark := "[green]✓[-]"
			if st == steps.Failed {
				mark = "[red]✗[-]"
			}
			ui.app.QueueUpdateDraw(func() {
				pane.tv.SetTitle(" " + mark + " " + step + " ")
			})
			delete(ui.slot, e.Step)
			ui.slotUse[i] = ""
		}
	}
	snap := ui.snapshot() // под мьютексом — никаких гонок при рендере
	ui.mu.Unlock()
	ui.app.QueueUpdateDraw(func() { ui.render(snap) })
}

func (ui *runUI) WriterFor(step string) io.Writer {
	ui.mu.Lock()
	i, ok := ui.slot[step]
	ui.mu.Unlock()
	if !ok {
		return io.Discard
	}
	return &paneWriter{p: ui.panes[i]}
}

// startRun — переключает на экран прогресса и запускает runner в горутине.
func startRun(app *tview.Application, pages *tview.Pages, o *steps.Opts, jobs int, runErr *error) {
	all := steps.All(o)
	order := make([]string, 0, len(all))
	state := map[string]steps.State{}
	for _, s := range all {
		order = append(order, s.Name())
		state[s.Name()] = steps.Pending
	}

	// лог установки в файл (дублируется из панелей)
	logPath := "/var/log/megapolos-install.log"
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err == nil {
		fmt.Fprintf(logFile, "\n===== %s: старт установки =====\n", time.Now().Format("2006-01-02 15:04:05"))
	}

	stepsTV := tview.NewTextView().SetDynamicColors(true).SetWrap(false)
	stepsTV.SetBorder(true)
	styleDark(stepsTV, " Шаги ")
	pane0 := &pane{tv: tview.NewTextView().SetWrap(true).SetScrollable(true), logFile: logFile}
	pane0.tv.SetBorder(true)
	styleDark(pane0.tv, " — ")
	pane1 := &pane{tv: tview.NewTextView().SetWrap(true).SetScrollable(true), logFile: logFile}
	pane1.tv.SetBorder(true)
	styleDark(pane1.tv, " — ")
	panes := [2]*pane{pane0, pane1}
	status := tview.NewTextView().SetDynamicColors(true)
	status.SetBackgroundColor(tcell.ColorBlack)
	status.SetTextColor(tcell.ColorLightGray)
	status.SetText("[gray]установка…  (c — скопировать логи, q — выход после завершения)[-]")

	logs := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(pane0.tv, 0, 1, false).
		AddItem(pane1.tv, 0, 1, false)
	runRoot := tview.NewFlex().
		AddItem(stepsTV, 40, 0, false).
		AddItem(tview.NewFlex().SetDirection(tview.FlexRow).
			AddItem(logs, 0, 1, false).
			AddItem(status, 1, 0, false), 0, 1, true)

	ui := &runUI{
		app: app, panes: panes,
		order: order, state: state, note: map[string]string{}, slot: map[string]int{},
	}
	ui.render = func(snap []stepLine) {
		var sb strings.Builder
		for _, sl := range snap {
			icon := "[gray]○[-]"
			switch sl.state {
			case steps.Running:
				icon = "[yellow]●[-]"
			case steps.Done:
				icon = "[green]✓[-]"
			case steps.Skipped:
				icon = "[gray]-[-]"
			case steps.Failed:
				icon = "[red]✗[-]"
			}
			sb.WriteString(icon + " " + sl.name + "\n")
			if sl.note != "" && (sl.state == steps.Skipped || sl.state == steps.Failed) {
				note := sl.note
				if len(note) > 60 {
					note = note[:60] + "…"
				}
				sb.WriteString("    [gray]" + tview.Escape(note) + "[-]\n")
			}
		}
		stepsTV.SetText(sb.String())
	}
	ui.render(ui.snapshot())

	// тикер отрисовки логов (10 fps — npm выдаёт тысячи строк, не рисуем каждую)
	stopTick := make(chan struct{})
	go func() {
		t := time.NewTicker(100 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stopTick:
				return
			case <-t.C:
				app.QueueUpdateDraw(func() {
					pane0.flush()
					pane1.flush()
				})
			}
		}
	}()

	r := &steps.Runner{
		Ctx:   &steps.Ctx{Context: context.Background(), Ex: sys.Real{}, O: o},
		Steps: all,
		Jobs:  jobs,
		UI:    ui,
	}

	finished := make(chan struct{})
	go func() {
		defer close(finished)
		defer close(stopTick)
		err := r.Run()
		*runErr = err
		app.QueueUpdateDraw(func() {
			pane0.flush()
			pane1.flush()
			if logFile != nil {
				logFile.Close()
			}
			if err != nil {
				status.SetText("[red]ОШИБКА: " + tview.Escape(err.Error()) + "[-]   (c — логи в буфер, q — выход)")
			} else {
				showSummary(app, pages, o, logPath)
			}
		})
	}()

	app.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		if ev.Key() == tcell.KeyCtrlC {
			select {
			case <-finished:
			default:
				*runErr = fmt.Errorf("прервано пользователем")
			}
			app.Stop()
			return nil
		}
		if ev.Rune() == 'c' {
			// логи в буфер обмена (OSC 52) — работает и по ssh, если терминал умеет
			var sb strings.Builder
			sb.WriteString(stepsTV.GetText(false) + "\n")
			for _, p := range panes {
				sb.WriteString("=== " + p.name + " ===\n" + p.tv.GetText(false) + "\n")
			}
			logs := sb.String()
			const osc52Limit = 48 * 1024 // у терминалов лимиты на OSC 52
			if len(logs) > osc52Limit {
				logs = logs[len(logs)-osc52Limit:]
			}
			if err := osc52Copy(logs); err != nil {
				status.SetText("[yellow]буфер недоступен — логи в файле " + logPath + "[-]")
			} else {
				status.SetText("[green]логи в буфере обмена[-] (если не вставилось — терминал без OSC 52, файл: " + logPath + ")")
			}
			return nil
		}
		if ev.Rune() == 'q' || ev.Key() == tcell.KeyEscape {
			select {
			case <-finished:
				app.Stop()
				return nil
			default:
				status.SetText("[yellow]установка ещё идёт… (Ctrl-C — прервать)[-]")
				return nil
			}
		}
		return ev
	})

	pages.AddAndSwitchToPage("run", runRoot, true)
}

// showSummary — финальный «чек»: плоский текст без рамок, удобно копировать.
func showSummary(app *tview.Application, pages *tview.Pages, o *steps.Opts, logPath string) {
	token := o.Token
	if token == "" {
		token = "(не найден — см. /root/megapolos-token.txt)"
	}
	ip := o.LANIP
	if ip == "" {
		ip = "127.0.0.1"
	}
	var sb strings.Builder
	sb.WriteString("\n [green::b]Установка завершена[-:-:-]\n\n")
	if o.GUI {
		if o.VMGUIPort != "" {
			fmt.Fprintf(&sb, " GUI (через проброс VM):  [yellow]http://localhost:%s/[-]\n", o.VMGUIPort)
			if o.GUITLS && o.VMGUITLSPort != "" {
				fmt.Fprintf(&sb, " GUI HTTPS:               [yellow]https://localhost:%s/[-]\n", o.VMGUITLSPort)
			}
		}
		fmt.Fprintf(&sb, " GUI (LAN/внутри):        [yellow]http://%s/[-]", ip)
		if o.GUITLS {
			fmt.Fprintf(&sb, "  и  [yellow]https://%s/[-]", ip)
		}
		sb.WriteString("\n")
	}
	fmt.Fprintf(&sb, " API:                     [yellow]%s[-]\n", o.APIURL)
	if o.VMAPIPort != "" {
		fmt.Fprintf(&sb, "   (внутри VM:            http://%s:5100)\n", ip)
	}
	fmt.Fprintf(&sb, " CA-сертификат:           [yellow]%s/api/ca/download[-]\n", o.APIURL)
	if o.GUITLS {
		sb.WriteString(" (GUI по HTTPS подписан тем же CA — импортировал CA, доверяешь и GUI)\n")
	}
	sb.WriteString("\n Токен для входа в GUI:\n")
	fmt.Fprintf(&sb, " [yellow]%s[-]\n", token)
	sb.WriteString(" (также в /root/megapolos-token.txt)\n")
	if o.BaseDomain != "" {
		fmt.Fprintf(&sb, "\n Базовый домен:           [yellow]%s[-]\n DNS:                     *.%s → %s (или /etc/hosts)\n", o.BaseDomain, o.BaseDomain, ip)
	}
	fmt.Fprintf(&sb, "\n Лог установки:           %s   (c — копия в буфер)\n\n [gray]q / Esc — выход[-]", logPath)

	tv := tview.NewTextView().SetDynamicColors(true).SetWrap(false)
	tv.SetBackgroundColor(tcell.ColorDefault)
	tv.SetTextColor(tcell.ColorLightGray)
	tv.SetText(sb.String())
	pages.AddAndSwitchToPage("done", tv, true)
}
