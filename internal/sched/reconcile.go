package sched

import (
	"context"
	"fmt"
	"time"

	"github.com/andrei/distributed-test-platform/internal/backend"
	"github.com/andrei/distributed-test-platform/internal/model"
)

// Reconciliation: what the backend says about in-flight runs, what the runner
// reports, timeouts, and how an attempt ends (retry or verdict).

// reconcile aligns run state with the backend: it notices allocations that
// started, finished without a callback, were lost, or overran their timeout.
func (s *Scheduler) reconcile(ctx context.Context) {
	var inflight []*model.Run
	for _, r := range s.st.AllRuns() {
		if r.State != model.RunDispatched && r.State != model.RunRunning {
			continue
		}
		if s.enforceTimeout(ctx, r) {
			continue
		}
		inflight = append(inflight, r)
	}
	if len(inflight) == 0 {
		return
	}
	// One listing covers every run the backend can see; the rest are polled
	// one by one, which is how "no allocation yet" is told from "job gone".
	bulk := map[string]backend.Status{}
	if bp, ok := s.be.(backend.BulkPoller); ok {
		if m, err := bp.PollAll(ctx, inflight); err != nil {
			s.log.Debug("bulk poll failed", "err", err)
		} else {
			bulk = m
		}
	}
	for _, r := range inflight {
		st, ok := bulk[r.ID]
		if !ok {
			var err error
			if st, err = s.be.Poll(ctx, r); err != nil {
				s.log.Debug("poll failed", "run", r.ID, "err", err)
				continue
			}
		}
		s.applyStatus(ctx, r, st)
	}
}

func (s *Scheduler) applyStatus(ctx context.Context, r *model.Run, st backend.Status) {
	// Placement details are useful even before the runner checks in.
	if st.NodeID != "" && r.NodeID != st.NodeID {
		s.st.UpdateRun(r.ID, func(run *model.Run) {
			run.NodeID, run.NodeName, run.AllocID = st.NodeID, st.NodeName, st.AllocID
		})
	}

	switch st.Phase {
	case backend.PhaseRunning:
		// The runner normally reports "started" itself; this covers a runner
		// that could not reach the master.
		if r.State == model.RunDispatched {
			now := time.Now().UTC()
			s.st.UpdateRun(r.ID, func(run *model.Run) {
				if run.State == model.RunDispatched {
					run.State = model.RunRunning
					run.StartedAt = &now
				}
			})
		}
	case backend.PhaseComplete, backend.PhaseFailed:
		// A run that already reported its own result is authoritative; the
		// backend phase only matters when the callback never arrived.
		cur, ok := s.st.Run(r.ID)
		if !ok || cur.State.Terminal() {
			return
		}
		state := model.RunPassed
		msg := ""
		if st.Phase == backend.PhaseFailed || (st.ExitCode != nil && *st.ExitCode != 0) {
			state = model.RunErrored
			msg = st.Message
			if msg == "" {
				msg = "task exited non-zero without reporting results"
			}
		} else if cur.Summary.Tests == 0 {
			state = model.RunErrored
			msg = "task completed but reported no test results"
		}
		s.finish(ctx, r.ID, state, msg, st.ExitCode)
	case backend.PhaseLost:
		s.finish(ctx, r.ID, model.RunErrored, "allocation lost: "+st.Message, nil)
	case backend.PhasePending:
		if r.DispatchedAt != nil && time.Since(*r.DispatchedAt) > dispatchGrace {
			s.st.UpdateRun(r.ID, func(run *model.Run) {
				run.Message = "waiting for placement for " + time.Since(*r.DispatchedAt).Round(time.Second).String()
			})
		}
	}
}

// enforceTimeout kills a run that has overrun its suite timeout. The deadline
// starts at dispatch so a task wedged before startup is caught too.
func (s *Scheduler) enforceTimeout(ctx context.Context, r *model.Run) bool {
	timeout := r.Spec.Timeout
	if timeout == 0 {
		timeout = s.cfg.DefaultTimeout
	}
	start := r.DispatchedAt
	if r.StartedAt != nil {
		start = r.StartedAt
	}
	if start == nil {
		return false
	}
	// Grace covers build fetch and artifact upload outside the test itself.
	deadline := start.Add(timeout.Duration() + 5*time.Minute)
	if time.Now().Before(deadline) {
		return false
	}
	s.log.Warn("run timed out", "run", r.ID, "suite", r.Suite, "timeout", timeout.String())
	if err := s.be.Stop(ctx, r); err != nil {
		s.log.Warn("stop after timeout failed", "run", r.ID, "err", err)
	}
	s.finish(ctx, r.ID, model.RunTimeout,
		fmt.Sprintf("exceeded timeout %s", timeout.String()), nil)
	return true
}

func (s *Scheduler) fail(runID string, state model.RunState, msg string) {
	s.finish(context.Background(), runID, state, msg, nil)
}

// finish marks a run terminal and queues a retry when one is allowed.
func (s *Scheduler) finish(ctx context.Context, runID string, state model.RunState, msg string, exit *int) {
	now := time.Now().UTC()
	var finished *model.Run
	s.st.UpdateRun(runID, func(run *model.Run) {
		if run.State.Terminal() {
			return
		}
		run.State = state
		run.FinishedAt = &now
		run.Message = msg // the verdict replaces any interim progress note
		if exit != nil {
			run.ExitCode = exit
		}
		cp := *run
		finished = &cp
	})
	if finished == nil {
		return
	}
	s.log.Info("run finished", "run", runID, "suite", finished.Suite,
		"state", string(state), "tests", finished.Summary.Tests,
		"failed", finished.Summary.Failed+finished.Summary.Errors)

	// Release the backend's record of the job; the artifacts already live in
	// the object store.
	if s.be.Name() == "nomad" {
		go func() {
			c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			s.be.Stop(c, finished)
		}()
	}

	if state.Retryable() && finished.Attempt < finished.MaxAttempts {
		reg, _, ok := s.st.Regression(finished.RegressionID)
		if ok {
			retry := s.newRun(reg, finished.Spec, finished.Attempt+1)
			retry.MaxAttempts = finished.MaxAttempts
			s.st.AddRun(retry)
			s.log.Info("retry queued", "suite", finished.Suite,
				"attempt", retry.Attempt, "of", finished.MaxAttempts)
		}
	}
}

// Ingest applies a runner event. The runner is the authority on test results;
// the backend poll is only a safety net.
func (s *Scheduler) Ingest(ctx context.Context, ev *model.RunEvent) error {
	run, ok := s.st.Run(ev.RunID)
	if !ok {
		return fmt.Errorf("unknown run %q", ev.RunID)
	}
	now := time.Now().UTC()

	switch ev.Phase {
	case "started":
		s.st.UpdateRun(run.ID, func(r *model.Run) {
			if r.State.Terminal() {
				return
			}
			r.State = model.RunRunning
			if r.StartedAt == nil {
				r.StartedAt = &now
			}
			if ev.NodeID != "" {
				r.NodeID, r.NodeName = ev.NodeID, ev.NodeName
			}
			if ev.AllocID != "" {
				r.AllocID = ev.AllocID
			}
			r.Message = ev.Message
		})
		return nil

	case "progress":
		s.st.UpdateRun(run.ID, func(r *model.Run) {
			if r.State != model.RunRunning && r.State != model.RunDispatched {
				return
			}
			r.Summary = ev.Summary
			r.Cases = ev.Cases
			if ev.Message != "" {
				r.Message = ev.Message
			}
		})
		return nil

	case "finished":
		state := ev.State
		if state == "" {
			state = model.RunPassed
			if ev.Summary.Failed+ev.Summary.Errors > 0 {
				state = model.RunFailed
			}
		}
		s.st.UpdateRun(run.ID, func(r *model.Run) {
			r.Summary = ev.Summary
			r.Cases = ev.Cases
			r.Artifacts = ev.Artifacts
			if ev.NodeID != "" {
				r.NodeID, r.NodeName = ev.NodeID, ev.NodeName
			}
			if ev.AllocID != "" {
				r.AllocID = ev.AllocID
			}
			if r.StartedAt == nil {
				r.StartedAt = &now
			}
		})
		s.finish(ctx, run.ID, state, ev.Message, ev.ExitCode)
		return nil
	}
	return fmt.Errorf("unknown phase %q", ev.Phase)
}
