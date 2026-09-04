// Package sys — выполнение shell-команд с потоковым выводом.
// Реальный Executor ходит в bash; в тестах подставляется fake.
package sys

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
)

// RunOpts — параметры одной команды.
type RunOpts struct {
	Cmd  string   // выполняется через bash -c
	Dir  string   // рабочий каталог (пусто = текущий)
	User string   // непусто = sudo -u <user> -H bash -c
	Env  []string // дополнительные переменные окружения (KEY=VAL)
}

// Executor — то, что умеет выполнять команды.
type Executor interface {
	// Run выполняет команду, стримя stdout+stderr в w (не nil).
	Run(ctx context.Context, o RunOpts, w io.Writer) error
	// Output выполняет команду и возвращает объединённый вывод (обрезанный).
	Output(ctx context.Context, o RunOpts) (string, error)
}

// Real — настоящий исполнитель через bash.
type Real struct{}

// muWriter — writer с мьютексом: stdout и stderr процесса пишут конкурентно.
type muWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (m *muWriter) Write(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.w.Write(p)
}

func buildCmd(ctx context.Context, o RunOpts) *exec.Cmd {
	var cmd *exec.Cmd
	if o.User != "" {
		cmd = exec.CommandContext(ctx, "sudo", "-u", o.User, "-H", "bash", "-c", o.Cmd)
	} else {
		cmd = exec.CommandContext(ctx, "bash", "-c", o.Cmd)
	}
	if o.Dir != "" {
		cmd.Dir = o.Dir
	}
	if len(o.Env) > 0 {
		cmd.Env = append(os.Environ(), o.Env...)
	}
	// своя группа процессов — чтобы при отмене убить всё дерево
	// (sudo → bash → npm → node-gyp), а не только sudo
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

// Run implements Executor.
func (Real) Run(ctx context.Context, o RunOpts, w io.Writer) error {
	cmd := buildCmd(ctx, o)
	mw := &muWriter{w: w}
	cmd.Stdout = mw
	cmd.Stderr = mw
	if err := cmd.Start(); err != nil {
		return err
	}
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	select {
	case err := <-wait:
		return err
	case <-ctx.Done():
		// SIGKILL всей группе: иначе node-gyp и прочие внуки остаются жить
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-wait
		return ctx.Err()
	}
}

// Output implements Executor.
func (r Real) Output(ctx context.Context, o RunOpts) (string, error) {
	var buf bytes.Buffer
	err := r.Run(ctx, o, &muWriter{w: &buf})
	return strings.TrimSpace(buf.String()), err
}

// CommandExists — есть ли команда в PATH.
func CommandExists(ctx context.Context, e Executor, name string) bool {
	return e.Run(ctx, RunOpts{Cmd: "command -v " + name + " >/dev/null 2>&1"}, io.Discard) == nil
}

// DpkgInstalled — установлен ли deb-пакет.
func DpkgInstalled(ctx context.Context, e Executor, pkg string) bool {
	return e.Run(ctx, RunOpts{Cmd: "dpkg -s " + pkg + " >/dev/null 2>&1"}, io.Discard) == nil
}

// FileExists — существует ли файл (проверка через test, чтобы работало и в fake).
func FileExists(ctx context.Context, e Executor, path string) bool {
	return e.Run(ctx, RunOpts{Cmd: "test -e " + path}, io.Discard) == nil
}
