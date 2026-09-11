package sched

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/andrei/distributed-test-platform/internal/backend"
	"github.com/andrei/distributed-test-platform/internal/config"
	"github.com/andrei/distributed-test-platform/internal/model"
	"github.com/andrei/distributed-test-platform/internal/store"
)

// fakeBackend accepts dispatches and lets a test drive each run's phase.
type fakeBackend struct {
	mu         sync.Mutex
	slots      int
	dispatched []string
	phase      map[string]backend.Phase
}

func newFake(slots int) *fakeBackend {
	return &fakeBackend{slots: slots, phase: map[string]backend.Phase{}}
}

func (f *fakeBackend) Name() string                           { return "fake" }
func (f *fakeBackend) Healthy(context.Context) error          { return nil }
func (f *fakeBackend) Stop(context.Context, *model.Run) error { return nil }

func (f *fakeBackend) Inventory(context.Context) ([]backend.NodeInfo, error) {
	return []backend.NodeInfo{{
		ID: "n1", Name: "n1", Pool: "p", Slots: f.slots, Ready: true, Status: "ready",
	}}, nil
}

func (f *fakeBackend) Dispatch(_ context.Context, run *model.Run, _ model.RunSpec) (backend.Placement, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dispatched = append(f.dispatched, run.ID)
	f.phase[run.ID] = backend.PhaseRunning
	return backend.Placement{BackendID: "job-" + run.ID, NodeID: "n1", NodeName: "n1"}, nil
}

func (f *fakeBackend) Poll(_ context.Context, run *model.Run) (backend.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return backend.Status{Phase: f.phase[run.ID], NodeID: "n1", NodeName: "n1"}, nil
}

func (f *fakeBackend) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.dispatched)
}

func testScheduler(t *testing.T, slots int) (*Scheduler, *store.Store, *fakeBackend) {
	t.Helper()
	cfg := &config.Config{
		StateDir:       t.TempDir(),
		Backend:        "local",
		DefaultTimeout: model.Duration(time.Minute),
		Pools: []config.Pool{{
			Name:     "p",
			Runtime:  model.RuntimeProcess,
			Slot:     config.Slot{CPU: 100, Memory: 128, Disk: 128},
			CacheDir: t.TempDir(),
		}},
	}
	cfg.Pools[0].Default.Command = []string{"/bin/true"}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	be := newFake(slots)
	sc := New(cfg, st, be, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return sc, st, be
}

func submitSuites(t *testing.T, sc *Scheduler, n int, retries int) *model.Regression {
	t.Helper()
	sub := &model.Submission{Name: "t", Defaults: model.SuiteDefaults{Pool: "p", Retries: &retries}}
	for i := 0; i < n; i++ {
		sub.Suites = append(sub.Suites, model.SuiteSpec{Name: string(rune('a' + i))})
	}
	reg, err := sc.Submit(sub)
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

// Admission must never exceed the pool's advertised slot count.
func TestAdmissionRespectsSlots(t *testing.T) {
	sc, _, be := testScheduler(t, 2)
	submitSuites(t, sc, 5, 0)
	ctx := context.Background()

	sc.tick(ctx)
	if got := be.count(); got != 2 {
		t.Fatalf("with 2 slots, want 2 dispatched, got %d", got)
	}
	sc.tick(ctx)
	if got := be.count(); got != 2 {
		t.Fatalf("slots still busy, want 2 dispatched, got %d", got)
	}

	// Free one slot; exactly one more suite may start.
	be.mu.Lock()
	be.phase[be.dispatched[0]] = backend.PhaseComplete
	first := be.dispatched[0]
	be.mu.Unlock()
	sc.Ingest(ctx, &model.RunEvent{
		RunID: first, Phase: "finished", State: model.RunPassed,
		Summary: model.Summary{Tests: 1, Passed: 1},
	})
	sc.tick(ctx)
	if got := be.count(); got != 3 {
		t.Fatalf("after one slot freed, want 3 dispatched, got %d", got)
	}
}

// A failed suite is retried up to its limit, and passing on a retry is flaky.
func TestRetryAndFlaky(t *testing.T) {
	sc, st, be := testScheduler(t, 4)
	reg := submitSuites(t, sc, 1, 2)
	ctx := context.Background()

	sc.tick(ctx)
	be.mu.Lock()
	run1 := be.dispatched[0]
	be.mu.Unlock()
	sc.Ingest(ctx, &model.RunEvent{
		RunID: run1, Phase: "finished", State: model.RunFailed,
		Summary: model.Summary{Tests: 4, Passed: 3, Failed: 1},
	})

	sc.tick(ctx)
	if got := be.count(); got != 2 {
		t.Fatalf("failure should queue a retry, dispatched=%d", got)
	}

	be.mu.Lock()
	run2 := be.dispatched[1]
	be.mu.Unlock()
	sc.Ingest(ctx, &model.RunEvent{
		RunID: run2, Phase: "finished", State: model.RunPassed,
		Summary: model.Summary{Tests: 4, Passed: 4},
	})
	sc.Rollup(ctx, reg.ID)

	got, runs, _ := st.Regression(reg.ID)
	if got.State != model.RegPassed {
		t.Fatalf("want regression passed, got %s", got.State)
	}
	if got.Totals.Flaky != 1 {
		t.Fatalf("want 1 flaky suite, got %d", got.Totals.Flaky)
	}
	if len(runs) != 2 {
		t.Fatalf("want 2 attempts recorded, got %d", len(runs))
	}
}

// Exhausting retries leaves the regression failed, not stuck.
func TestRetriesExhausted(t *testing.T) {
	sc, st, be := testScheduler(t, 4)
	reg := submitSuites(t, sc, 1, 1)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		sc.tick(ctx)
		be.mu.Lock()
		run := be.dispatched[i]
		be.mu.Unlock()
		sc.Ingest(ctx, &model.RunEvent{
			RunID: run, Phase: "finished", State: model.RunFailed,
			Summary: model.Summary{Tests: 2, Passed: 1, Failed: 1},
		})
	}
	sc.tick(ctx)
	if got := be.count(); got != 2 {
		t.Fatalf("want exactly 2 attempts (1 + 1 retry), got %d", got)
	}
	got, _, _ := st.Regression(reg.ID)
	if got.State != model.RegFailed {
		t.Fatalf("want regression failed, got %s", got.State)
	}
	if got.Totals.Flaky != 0 {
		t.Fatalf("a suite that never passed is not flaky, got %d", got.Totals.Flaky)
	}
}

// A pool named by no node has no capacity; work waits instead of erroring.
func TestUnknownPoolRejectedAtSubmit(t *testing.T) {
	sc, _, _ := testScheduler(t, 1)
	_, err := sc.Submit(&model.Submission{
		Suites: []model.SuiteSpec{{Name: "x", Pool: "nope"}},
	})
	if err == nil {
		t.Fatal("expected submission to a nonexistent pool to be rejected")
	}
}
