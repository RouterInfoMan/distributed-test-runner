// Package sched is the master's control loop: it admits queued suites against
// the pool slot ledger, dispatches them to the backend, reconciles backend
// state, retries failures, and keeps each regression's rollup current.
package sched

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path"
	"sort"
	"sync"
	"time"

	"github.com/andrei/distributed-test-platform/internal/backend"
	"github.com/andrei/distributed-test-platform/internal/config"
	"github.com/andrei/distributed-test-platform/internal/model"
	"github.com/andrei/distributed-test-platform/internal/s3"
	"github.com/andrei/distributed-test-platform/internal/store"
)

// TickInterval is how often the control loop runs. Suites are long-lived, so
// a one-second cadence is plenty and keeps the Nomad API load trivial.
const TickInterval = time.Second

// dispatchGrace is how long a run may sit in "dispatched" before the master
// stops waiting for the backend to place it and reports the delay.
const dispatchGrace = 10 * time.Minute

// PoolStatus is the dashboard's view of one pool's capacity.
type PoolStatus struct {
	Name        string        `json:"name"`
	Description string        `json:"description,omitempty"`
	Runtime     model.Runtime `json:"runtime"`
	Driver      string        `json:"driver"`
	Slot        config.Slot   `json:"slot"`
	Nodes       []NodeStatus  `json:"nodes"`
	Slots       int           `json:"slots"`
	Used        int           `json:"used"`
	Queued      int           `json:"queued"`
	Error       string        `json:"error,omitempty"`
}

// NodeStatus is one worker plus what it is currently running.
type NodeStatus struct {
	backend.NodeInfo
	Used    int      `json:"used"`
	Running []string `json:"running,omitempty"` // suite names
}

type Scheduler struct {
	cfg *config.Config
	st  *store.Store
	be  backend.Backend
	s3  *s3.Client
	log *slog.Logger

	mu        sync.RWMutex
	inventory []backend.NodeInfo
	invErr    string
}

func New(cfg *config.Config, st *store.Store, be backend.Backend, s3c *s3.Client, log *slog.Logger) *Scheduler {
	return &Scheduler{cfg: cfg, st: st, be: be, s3: s3c, log: log}
}

// Submit validates a submission, materializes the first attempt of every suite
// and queues them.
func (s *Scheduler) Submit(sub *model.Submission) (*model.Regression, error) {
	if err := sub.Normalize(); err != nil {
		return nil, err
	}
	for _, su := range sub.Suites {
		pool, ok := s.cfg.Pool(su.Pool)
		if !ok {
			return nil, fmt.Errorf("suite %q: unknown pool %q", su.Name, su.Pool)
		}
		if su.Runtime != "" && su.Runtime != pool.Runtime {
			return nil, fmt.Errorf("suite %q: pool %q provides runtime %q, suite asked for %q",
				su.Name, pool.Name, pool.Runtime, su.Runtime)
		}
	}

	id := SanitizeID(sub.RegressionID)
	if id == "" {
		id = NewRegressionID(sub.Name)
	}
	now := time.Now().UTC()

	reg := &model.Regression{
		ID:          id,
		Name:        sub.Name,
		Priority:    sub.Priority,
		Labels:      sub.Labels,
		State:       model.RegPending,
		SubmittedAt: now,
		Suites:      sub.Suites,
		ArtifactURI: fmt.Sprintf("s3://%s/%s", s.cfg.S3.Bucket, resultPrefix(id)),
	}
	reg.Totals.Suites = len(sub.Suites)
	reg.Totals.SuitesQueued = len(sub.Suites)

	runs := make([]*model.Run, 0, len(sub.Suites))
	for _, su := range sub.Suites {
		runs = append(runs, s.newRun(reg, su, 1))
	}
	if err := s.st.Create(reg, runs); err != nil {
		return nil, err
	}
	s.log.Info("regression submitted", "regression", reg.ID, "suites", len(runs))
	return reg, nil
}

func (s *Scheduler) newRun(reg *model.Regression, su model.SuiteSpec, attempt int) *model.Run {
	pool, _ := s.cfg.Pool(su.Pool)
	rt := su.Runtime
	if rt == "" && pool != nil {
		rt = pool.Runtime
	}
	maxAttempts := su.MaxAttempts()
	if su.Retries == nil && s.cfg.DefaultRetries > 0 {
		maxAttempts = s.cfg.DefaultRetries + 1
	}
	return &model.Run{
		ID:           NewRunID(),
		RegressionID: reg.ID,
		Suite:        su.Name,
		Attempt:      attempt,
		MaxAttempts:  maxAttempts,
		Pool:         su.Pool,
		Runtime:      rt,
		State:        model.RunQueued,
		Priority:     reg.Priority,
		QueuedAt:     time.Now().UTC(),
		Token:        NewToken(),
		Spec:         su,
	}
}

// Run drives the control loop until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) {
	t := time.NewTicker(TickInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.tick(ctx)
		}
	}
}

func (s *Scheduler) tick(ctx context.Context) {
	s.refreshInventory(ctx)
	s.reconcile(ctx)
	s.admit(ctx)
	s.rollupAll(ctx)
}

func (s *Scheduler) refreshInventory(ctx context.Context) {
	inv, err := s.be.Inventory(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.invErr = err.Error()
		return
	}
	s.invErr = ""
	s.inventory = inv
}

// ---------------------------------------------------------------------------
// admission: the slot ledger
// ---------------------------------------------------------------------------

// capacity returns total ready slots per pool.
func (s *Scheduler) capacity() map[string]int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]int{}
	for _, n := range s.inventory {
		if n.Ready {
			out[n.Pool] += n.Slots
		}
	}
	return out
}

// inFlight counts runs holding a slot, per pool.
func inFlight(runs []*model.Run) map[string]int {
	out := map[string]int{}
	for _, r := range runs {
		if r.State == model.RunDispatched || r.State == model.RunRunning {
			out[r.Pool]++
		}
	}
	return out
}

// admit dispatches queued runs while their pool has free slots. Ordering is
// priority-desc then FIFO, so a nightly regression cannot jump ahead of an
// interactive one submitted with higher priority.
func (s *Scheduler) admit(ctx context.Context) {
	all := s.st.AllRuns()
	total := s.capacity()
	used := inFlight(all)

	var queued []*model.Run
	for _, r := range all {
		if r.State == model.RunQueued {
			queued = append(queued, r)
		}
	}
	sort.SliceStable(queued, func(i, j int) bool {
		if queued[i].Priority != queued[j].Priority {
			return queued[i].Priority > queued[j].Priority
		}
		return queued[i].QueuedAt.Before(queued[j].QueuedAt)
	})

	for _, r := range queued {
		free := total[r.Pool] - used[r.Pool]
		if free <= 0 {
			continue
		}
		if err := s.dispatch(ctx, r); err != nil {
			s.log.Warn("dispatch failed", "run", r.ID, "suite", r.Suite, "err", err)
			continue
		}
		used[r.Pool]++
	}
}

func (s *Scheduler) dispatch(ctx context.Context, r *model.Run) error {
	spec, err := s.buildRunSpec(r)
	if err != nil {
		s.fail(r.ID, model.RunErrored, "build run spec: "+err.Error())
		return err
	}
	pl, derr := s.be.Dispatch(ctx, r, spec)
	if derr != nil {
		return derr
	}
	now := time.Now().UTC()
	s.st.UpdateRun(r.ID, func(run *model.Run) {
		run.State = model.RunDispatched
		run.DispatchedAt = &now
		run.BackendID = pl.BackendID
		if pl.NodeID != "" {
			run.NodeID, run.NodeName = pl.NodeID, pl.NodeName
		}
		run.ArtifactURI = fmt.Sprintf("s3://%s/%s", s.cfg.S3.Bucket, spec.S3.Prefix)
	})
	s.log.Info("dispatched", "run", r.ID, "suite", r.Suite, "pool", r.Pool,
		"attempt", r.Attempt, "backend_id", pl.BackendID)
	return nil
}

// buildRunSpec produces the self-contained instruction set the node-side runner
// executes: what to fetch, what to run, where to put the results.
func (s *Scheduler) buildRunSpec(r *model.Run) (model.RunSpec, error) {
	pool, ok := s.cfg.Pool(r.Pool)
	if !ok {
		return model.RunSpec{}, fmt.Errorf("unknown pool %q", r.Pool)
	}
	cmd := r.Spec.Command
	if len(cmd) == 0 {
		cmd = pool.Default.Command
	}
	if len(cmd) == 0 {
		return model.RunSpec{}, fmt.Errorf("pool %q has no default command and suite sets none", pool.Name)
	}
	timeout := r.Spec.Timeout
	if timeout == 0 {
		timeout = s.cfg.DefaultTimeout
	}

	env := map[string]string{}
	for k, v := range r.Spec.Env {
		env[k] = v
	}
	env["DTP_SUITE"] = r.Suite
	env["DTP_REGRESSION_ID"] = r.RegressionID
	env["DTP_ATTEMPT"] = fmt.Sprint(r.Attempt)

	return model.RunSpec{
		RunID:        r.ID,
		RegressionID: r.RegressionID,
		Suite:        r.Suite,
		Attempt:      r.Attempt,
		Build:        r.Spec.Build,
		Command:      cmd,
		Env:          env,
		Timeout:      timeout,
		CacheDir:     pool.CacheDir,
		S3: model.S3Target{
			Endpoint:  s.cfg.S3.Endpoint,
			Region:    s.cfg.S3.Region,
			Bucket:    s.cfg.S3.Bucket,
			Prefix:    runPrefix(r),
			AccessKey: s.cfg.S3.AccessKey,
			SecretKey: s.cfg.S3.SecretKey,
		},
		MasterURL: s.cfg.PublicURL,
		Token:     r.Token,
	}, nil
}

// resultPrefix is the object-store layout: results/<regression_id>/...
func resultPrefix(regID string) string { return path.Join("results", regID) + "/" }

func runPrefix(r *model.Run) string {
	return path.Join("results", r.RegressionID, r.Suite, fmt.Sprintf("attempt-%d", r.Attempt)) + "/"
}

// ---------------------------------------------------------------------------
// reconciliation
// ---------------------------------------------------------------------------

// reconcile aligns run state with the backend: it notices allocations that
// started, finished without a callback, were lost, or overran their timeout.
func (s *Scheduler) reconcile(ctx context.Context) {
	for _, r := range s.st.AllRuns() {
		if r.State != model.RunDispatched && r.State != model.RunRunning {
			continue
		}
		if s.enforceTimeout(ctx, r) {
			continue
		}
		st, err := s.be.Poll(ctx, r)
		if err != nil {
			s.log.Debug("poll failed", "run", r.ID, "err", err)
			continue
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
		if msg != "" {
			run.Message = msg
		}
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

// ---------------------------------------------------------------------------
// result ingestion (called by the HTTP API when a runner reports in)
// ---------------------------------------------------------------------------

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

// ---------------------------------------------------------------------------
// rollup
// ---------------------------------------------------------------------------

func (s *Scheduler) rollupAll(ctx context.Context) {
	for _, reg := range s.st.Regressions() {
		if reg.State == model.RegPassed || reg.State == model.RegFailed ||
			reg.State == model.RegErrored || reg.State == model.RegCanceled {
			continue
		}
		s.Rollup(ctx, reg.ID)
	}
}

// LatestPerSuite returns the newest attempt of each suite.
func LatestPerSuite(runs []*model.Run) map[string]*model.Run {
	out := map[string]*model.Run{}
	for _, r := range runs {
		if cur, ok := out[r.Suite]; !ok || r.Attempt > cur.Attempt {
			out[r.Suite] = r
		}
	}
	return out
}

// Rollup recomputes a regression's totals and state, and writes the final
// manifest to the object store once everything is terminal.
func (s *Scheduler) Rollup(ctx context.Context, regID string) {
	var completed bool
	var snapshot *model.Regression
	var snapRuns []*model.Run

	s.st.UpdateRegression(regID, func(reg *model.Regression, runs []*model.Run) {
		latest := LatestPerSuite(runs)
		t := model.Totals{Suites: len(reg.Suites)}
		anyStarted, allTerminal := false, true

		for _, su := range reg.Suites {
			r, ok := latest[su.Name]
			if !ok {
				t.SuitesQueued++
				allTerminal = false
				continue
			}
			switch r.State {
			case model.RunQueued:
				t.SuitesQueued++
				allTerminal = false
			case model.RunDispatched, model.RunRunning:
				t.SuitesRunning++
				anyStarted, allTerminal = true, false
			case model.RunPassed:
				t.SuitesPassed++
				anyStarted = true
			case model.RunFailed:
				t.SuitesFailed++
				anyStarted = true
			default: // errored, timeout, canceled
				t.SuitesErrored++
				anyStarted = true
			}
			t.Tests += r.Summary.Tests
			t.Passed += r.Summary.Passed
			t.Failed += r.Summary.Failed + r.Summary.Errors
			t.Skipped += r.Summary.Skipped
			if r.State == model.RunPassed && r.Attempt > 1 {
				t.Flaky++ // suite only went green after a retry
			}
		}

		reg.Totals = t
		switch {
		case allTerminal && anyStarted:
			switch {
			case t.SuitesErrored > 0:
				reg.State = model.RegErrored
			case t.SuitesFailed > 0:
				reg.State = model.RegFailed
			default:
				reg.State = model.RegPassed
			}
			if reg.FinishedAt == nil {
				now := time.Now().UTC()
				reg.FinishedAt = &now
			}
			completed = true
		case anyStarted:
			reg.State = model.RegRunning
			if reg.StartedAt == nil {
				now := time.Now().UTC()
				reg.StartedAt = &now
			}
		default:
			reg.State = model.RegPending
		}

		cp := *reg
		snapshot = &cp
		snapRuns = runs
	})

	if completed && snapshot != nil {
		s.writeManifest(ctx, snapshot, snapRuns)
	}
}

// Manifest is the machine-readable summary written to
// results/<regression_id>/manifest.json when a regression finishes.
type Manifest struct {
	Regression  *model.Regression `json:"regression"`
	Runs        []*model.Run      `json:"runs"`
	GeneratedAt time.Time         `json:"generated_at"`
}

func (s *Scheduler) writeManifest(ctx context.Context, reg *model.Regression, runs []*model.Run) {
	if s.s3 == nil {
		return
	}
	m := Manifest{Regression: reg, Runs: runs, GeneratedAt: time.Now().UTC()}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return
	}
	key := resultPrefix(reg.ID) + "manifest.json"
	c, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := s.s3.PutBytes(c, s.cfg.S3.Bucket, key, b, "application/json"); err != nil {
		s.log.Warn("manifest upload failed", "regression", reg.ID, "err", err)
		return
	}
	s.log.Info("regression complete", "regression", reg.ID, "state", string(reg.State),
		"tests", reg.Totals.Tests, "failed", reg.Totals.Failed,
		"manifest", fmt.Sprintf("s3://%s/%s", s.cfg.S3.Bucket, key))
}

// ---------------------------------------------------------------------------
// views
// ---------------------------------------------------------------------------

// Pools renders the slot ledger for the API and dashboard.
func (s *Scheduler) Pools() []PoolStatus {
	s.mu.RLock()
	inv := append([]backend.NodeInfo(nil), s.inventory...)
	invErr := s.invErr
	s.mu.RUnlock()

	runs := s.st.AllRuns()
	perNode := map[string][]string{}
	perPoolUsed := map[string]int{}
	perPoolQueued := map[string]int{}
	for _, r := range runs {
		switch r.State {
		case model.RunDispatched, model.RunRunning:
			perPoolUsed[r.Pool]++
			if r.NodeID != "" {
				perNode[r.NodeID] = append(perNode[r.NodeID], r.Suite)
			}
		case model.RunQueued:
			perPoolQueued[r.Pool]++
		}
	}

	out := make([]PoolStatus, 0, len(s.cfg.Pools))
	for i := range s.cfg.Pools {
		p := &s.cfg.Pools[i]
		ps := PoolStatus{
			Name:        p.Name,
			Description: p.Description,
			Runtime:     p.Runtime,
			Driver:      p.Driver(),
			Slot:        p.Slot,
			Used:        perPoolUsed[p.Name],
			Queued:      perPoolQueued[p.Name],
			Error:       invErr,
		}
		for _, n := range inv {
			if n.Pool != p.Name {
				continue
			}
			ns := NodeStatus{NodeInfo: n, Running: perNode[n.ID]}
			ns.Used = len(ns.Running)
			ps.Nodes = append(ps.Nodes, ns)
			if n.Ready {
				ps.Slots += n.Slots
			}
		}
		sort.Slice(ps.Nodes, func(a, b int) bool { return ps.Nodes[a].Name < ps.Nodes[b].Name })
		out = append(out, ps)
	}
	return out
}

// Cancel stops every non-terminal run in a regression.
func (s *Scheduler) Cancel(ctx context.Context, regID string) error {
	_, runs, ok := s.st.Regression(regID)
	if !ok {
		return fmt.Errorf("unknown regression %q", regID)
	}
	for _, r := range runs {
		if r.State.Terminal() {
			continue
		}
		if r.State != model.RunQueued {
			if err := s.be.Stop(ctx, r); err != nil {
				s.log.Warn("stop failed", "run", r.ID, "err", err)
			}
		}
		now := time.Now().UTC()
		s.st.UpdateRun(r.ID, func(run *model.Run) {
			run.State = model.RunCanceled
			run.FinishedAt = &now
			run.Message = "canceled"
		})
	}
	s.st.UpdateRegression(regID, func(reg *model.Regression, _ []*model.Run) {
		reg.State = model.RegCanceled
		if reg.FinishedAt == nil {
			now := time.Now().UTC()
			reg.FinishedAt = &now
		}
	})
	return nil
}

// Backend exposes the active backend for the API layer.
func (s *Scheduler) Backend() backend.Backend { return s.be }

// Config exposes the loaded config for the API layer.
func (s *Scheduler) Config() *config.Config { return s.cfg }
