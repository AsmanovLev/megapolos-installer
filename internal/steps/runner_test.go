package steps

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"megapolos/installer/internal/sys"
)

type nopUI struct{ t *testing.T }

func (n nopUI) WriterFor(step string) io.Writer { return io.Discard }
func (n nopUI) Emit(e Event)                    { n.t.Logf("event: %s → %s %s", e.Step, e.State, e.Note) }

func fakeStep(name string, deps []string, skip bool, runErr error, delay time.Duration, counter *int32) StepFunc {
	return StepFunc{
		N: name, D: deps,
		DetectF: func(*Ctx) (bool, string) {
			if skip {
				return true, "уже сделано"
			}
			return false, ""
		},
		RunF: func(c *Ctx, w io.Writer) error {
			if counter != nil {
				atomic.AddInt32(counter, 1)
			}
			if delay > 0 {
				select {
				case <-c.Done():
					return c.Err()
				case <-time.After(delay):
				}
			}
			return runErr
		},
	}
}

func testCtx() *Ctx {
	return &Ctx{Context: context.Background(), Ex: sys.Real{}, O: &Opts{}}
}

func TestRunnerOrderAndParallelism(t *testing.T) {
	var ran int32
	// a → b, c → d; a и c параллельны (jobs=2, каждый по 200мс → общее < 350мс)
	st := []Step{
		fakeStep("a", nil, false, nil, 200*time.Millisecond, &ran),
		fakeStep("c", nil, false, nil, 200*time.Millisecond, &ran),
		fakeStep("b", []string{"a"}, false, nil, 0, &ran),
		fakeStep("d", []string{"c"}, false, nil, 0, &ran),
	}
	r := &Runner{Ctx: testCtx(), Steps: st, Jobs: 2, UI: nopUI{t}}
	start := time.Now()
	if err := r.Run(); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 350*time.Millisecond {
		t.Errorf("похоже, нет параллелизма: %v", d)
	}
	if ran != 4 {
		t.Errorf("выполнено %d шагов, ждали 4", ran)
	}
}

func TestRunnerSkip(t *testing.T) {
	var ran int32
	st := []Step{
		fakeStep("a", nil, true, nil, 0, &ran),
		fakeStep("b", []string{"a"}, false, nil, 0, &ran),
	}
	r := &Runner{Ctx: testCtx(), Steps: st, Jobs: 1, UI: nopUI{t}}
	if err := r.Run(); err != nil {
		t.Fatal(err)
	}
	if ran != 1 {
		t.Errorf("skip-нутый шаг не должен выполняться; ran=%d", ran)
	}
}

func TestRunnerFailureBlocksDependents(t *testing.T) {
	var ran int32
	st := []Step{
		fakeStep("a", nil, false, errors.New("boom"), 0, &ran),
		fakeStep("b", []string{"a"}, false, nil, 0, &ran),
	}
	r := &Runner{Ctx: testCtx(), Steps: st, Jobs: 2, UI: nopUI{t}}
	err := r.Run()
	if err == nil {
		t.Fatal("ждали ошибку")
	}
	if ran != 1 {
		t.Errorf("зависимый шаг b не должен был запуститься; ran=%d", ran)
	}
}

func TestRunnerCycleDetection(t *testing.T) {
	st := []Step{
		fakeStep("a", []string{"b"}, false, nil, 0, nil),
		fakeStep("b", []string{"a"}, false, nil, 0, nil),
	}
	r := &Runner{Ctx: testCtx(), Steps: st, Jobs: 2, UI: nopUI{t}}
	if err := r.Run(); err == nil {
		t.Fatal("ждали ошибку цикла зависимостей")
	}
}
