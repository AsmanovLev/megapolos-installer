// Package steps — шаги установки megapolos и параллельный runner (DAG).
package steps

import (
	"context"
	"fmt"
	"io"
	"sync"

	"megapolos/installer/internal/sys"
)

// State — состояние шага.
type State int

const (
	Pending State = iota
	Running
	Done
	Skipped // Detect() сказал «уже сделано»
	Failed
)

func (s State) String() string {
	switch s {
	case Pending:
		return "ожидает"
	case Running:
		return "выполняется"
	case Done:
		return "готово"
	case Skipped:
		return "пропущен"
	case Failed:
		return "ОШИБКА"
	}
	return "?"
}

// Event — событие смены состояния шага (для UI).
type Event struct {
	Step  string
	State State
	Note  string // например причина пропуска
}

// Opts — все параметры установки (заполняются из флагов/env/TUI).
type Opts struct {
	GitBase          string // резолвнутый базовый URL/путь репозиториев
	CoreRef, GUIRef  string // ветка/тег/sha
	APIURL           string
	DevMode, Debug   bool
	DBName, DBUser   string
	DBPass           string // preflight: из существующего config.json или свежий
	Secret           string // preflight: из существующего config.json или свежий
	InstallDir       string
	BundleDir        string // пусто = нет бандла
	AptProxy         string // пусто = нет кэша
	NpmRegistry      string // пусто = дефолтный npmjs
	AddSelfNode      bool
	NodeRootPassword string
	BaseDomain       string // базовый домен инстансов (megapolos.local); пусто = не создавать
	GUI              bool   // ставить и обслуживать GUI на этой машине (nginx :80)
	GUITLS           bool   // HTTPS для GUI (серт Megapolos Root CA, :443)
	LANIP            string // внешний IPv4 машины (для дефолтов и сводки)
	VMGUIPort        string // hostfwd-порт GUI (QEMU, из /etc/megapolos-vm.env)
	VMGUITLSPort     string // hostfwd-порт GUI HTTPS
	VMAPIPort        string // hostfwd-порт API
	Swap             string // auto|force|skip: auto = создавать только при RAM < 8G
	SvcUser          string
	HostIP           string
	Hostname         string
	Token            string // заполняется шагом token
	NodeMajor        int
	PgMajor          int
}

// Ctx — контекст, пробрасываемый в шаги.
type Ctx struct {
	context.Context
	Ex sys.Executor
	O  *Opts
}

// Step — один шаг установки.
type Step interface {
	Name() string
	Deps() []string
	// Detect возвращает (true, причина), если шаг уже выполнен и его можно пропустить.
	Detect(*Ctx) (bool, string)
	Run(*Ctx, io.Writer) error
}

// StepFunc — шаг из замыканий (так описаны все шаги установщика).
type StepFunc struct {
	N       string
	D       []string
	DetectF func(*Ctx) (bool, string)
	RunF    func(*Ctx, io.Writer) error
}

func (s StepFunc) Name() string                  { return s.N }
func (s StepFunc) Deps() []string                { return s.D }
func (s StepFunc) Detect(c *Ctx) (bool, string)  { return s.DetectF(c) }
func (s StepFunc) Run(c *Ctx, w io.Writer) error { return s.RunF(c, w) }

// UI — абстракция вывода (headless stdout или tview-панели).
type UI interface {
	// WriterFor возвращает writer для лога шага (вызывается один раз при старте шага).
	WriterFor(step string) io.Writer
	// Emit — событие смены состояния.
	Emit(Event)
}

type result struct {
	name string
	err  error
}

// Runner — параллельный исполнитель DAG шагов.
type Runner struct {
	Ctx   *Ctx
	Steps []Step
	Jobs  int // максимум параллельных шагов
	UI    UI
}

// Run выполняет шаги с учётом зависимостей; возвращает первую ошибку.
func (r *Runner) Run() error {
	jobs := r.Jobs
	if jobs < 1 {
		jobs = 1
	}
	ctx, cancel := context.WithCancel(r.Ctx)
	c := &Ctx{Context: ctx, Ex: r.Ctx.Ex, O: r.Ctx.O}
	defer cancel()

	total := len(r.Steps)
	done := map[string]bool{} // завершённые (успех или пропуск)
	started := map[string]bool{}
	running := 0
	completions := make(chan result, total)
	var firstErr error

	depsReady := func(s Step) bool {
		for _, d := range s.Deps() {
			if !done[d] {
				return false
			}
		}
		return true
	}

	for len(done) < total {
		// запускаем все готовые шаги, пока есть свободные слоты
		if firstErr == nil {
			for running < jobs {
				var next Step
				for _, s := range r.Steps {
					if !started[s.Name()] && depsReady(s) {
						next = s
						break
					}
				}
				if next == nil {
					break
				}
				started[next.Name()] = true
				running++
				go func(s Step) {
					skipped, note := s.Detect(c)
					if skipped {
						r.UI.Emit(Event{Step: s.Name(), State: Skipped, Note: note})
						completions <- result{name: s.Name()}
						return
					}
					r.UI.Emit(Event{Step: s.Name(), State: Running})
					w := r.UI.WriterFor(s.Name())
					err := s.Run(c, w)
					if err != nil {
						r.UI.Emit(Event{Step: s.Name(), State: Failed, Note: err.Error()})
					} else {
						r.UI.Emit(Event{Step: s.Name(), State: Done})
					}
					completions <- result{name: s.Name(), err: err}
				}(next)
			}
		}
		if running == 0 {
			if firstErr != nil {
				break
			}
			return fmt.Errorf("внутренняя ошибка: цикл в зависимостях шагов")
		}
		res := <-completions
		running--
		done[res.name] = true
		if res.err != nil && firstErr == nil {
			firstErr = fmt.Errorf("шаг %q: %w", res.name, res.err)
			cancel() // гасим бегущие шаги; новые не стартуют
		}
	}
	return firstErr
}

// prefixWriter — построчный writer с префиксом [step] (headless-режим).
type prefixWriter struct {
	mu   sync.Mutex
	step string
	w    io.Writer
	buf  []byte
}

// PrefixWriter создаёт writer, добавляющий "[step] " к каждой строке.
func PrefixWriter(step string, w io.Writer) io.Writer {
	return &prefixWriter{step: step, w: w}
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := len(b)
	p.buf = append(p.buf, b...)
	for {
		i := -1
		for j, c := range p.buf {
			if c == '\n' {
				i = j
				break
			}
		}
		if i < 0 {
			break
		}
		line := p.buf[:i+1]
		p.buf = p.buf[i+1:]
		fmt.Fprintf(p.w, "[%-12s] %s", p.step, line)
	}
	return n, nil
}

// Flush дописывает хвост без перевода строки.
func (p *prefixWriter) Flush() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.buf) > 0 {
		fmt.Fprintf(p.w, "[%-12s] %s\n", p.step, p.buf)
		p.buf = nil
	}
}
