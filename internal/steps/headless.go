package steps

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// HeadlessUI — вывод в stdout без TUI: события строками, логи шагов с префиксами.
type HeadlessUI struct {
	mu      sync.Mutex
	started time.Time
}

func NewHeadlessUI() *HeadlessUI { return &HeadlessUI{started: time.Now()} }

func (h *HeadlessUI) WriterFor(step string) io.Writer {
	return PrefixWriter(step, os.Stdout)
}

func (h *HeadlessUI) Emit(e Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ts := time.Now().Format("15:04:05")
	switch e.State {
	case Running:
		fmt.Printf("\n==> [%s] %s\n", ts, e.Step)
	case Done:
		fmt.Printf("==> [%s] %s: готово\n", ts, e.Step)
	case Skipped:
		fmt.Printf("==> [%s] %s: пропущен (%s)\n", ts, e.Step, e.Note)
	case Failed:
		fmt.Printf("==> [%s] %s: ОШИБКА: %s\n", ts, e.Step, e.Note)
	}
}
