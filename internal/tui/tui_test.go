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
	screen.SetSize(120, 40)
	orig := newApp
	newApp = func() *tview.Application {
		return tview.NewApplication().SetScreen(screen)
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
