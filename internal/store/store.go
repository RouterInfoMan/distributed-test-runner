// Package store keeps regressions and runs in memory and mirrors each
// regression to one JSON file under the state dir. That is enough durability
// for a PoC and keeps the master free of an external database; the interface is
// narrow enough to swap for Postgres later.
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/andrei/distributed-test-platform/internal/model"
)

// record is the on-disk shape: a regression and all of its run attempts.
type record struct {
	Regression *model.Regression `json:"regression"`
	Runs       []*model.Run      `json:"runs"`
	Tokens     map[string]string `json:"tokens,omitempty"` // token -> run id
}

// Store is safe for concurrent use.
type Store struct {
	mu       sync.RWMutex
	dir      string
	byReg    map[string]*record
	byRun    map[string]*model.Run
	byToken  map[string]*model.Run
	revision uint64 // bumped on every mutation; the dashboard long-polls on it
	waiters  []chan struct{}
}

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{
		dir:     dir,
		byReg:   map[string]*record{},
		byRun:   map[string]*model.Run{},
		byToken: map[string]*model.Run{},
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		var rec record
		if err := json.Unmarshal(b, &rec); err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		if rec.Regression == nil {
			continue
		}
		s.byReg[rec.Regression.ID] = &rec
		specs := map[string]model.SuiteSpec{}
		for _, sp := range rec.Regression.Suites {
			specs[sp.Name] = sp
		}
		for _, r := range rec.Runs {
			r.Spec = specs[r.Suite]
			s.byRun[r.ID] = r
			// A run left mid-flight by a master restart is re-queued: the
			// backend allocation, if any, is reconciled away by the scheduler.
			if !r.State.Terminal() && r.State != model.RunQueued {
				r.State = model.RunQueued
				r.DispatchedAt, r.StartedAt = nil, nil
			}
		}
		for tok, runID := range rec.Tokens {
			if r, ok := s.byRun[runID]; ok {
				r.Token = tok
				s.byToken[tok] = r
			}
		}
	}
	return s, nil
}

// ---------------------------------------------------------------------------
// mutation helpers
// ---------------------------------------------------------------------------

func (s *Store) touch() {
	s.revision++
	for _, ch := range s.waiters {
		close(ch)
	}
	s.waiters = nil
}

// Revision returns the current mutation counter.
func (s *Store) Revision() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.revision
}

// Wait blocks until the revision moves past since, or the timeout elapses.
// Used by the dashboard's long-poll so the UI updates immediately.
func (s *Store) Wait(since uint64, timeout time.Duration) uint64 {
	s.mu.Lock()
	if s.revision > since {
		rev := s.revision
		s.mu.Unlock()
		return rev
	}
	ch := make(chan struct{})
	s.waiters = append(s.waiters, ch)
	s.mu.Unlock()

	select {
	case <-ch:
	case <-time.After(timeout):
	}
	return s.Revision()
}

func (s *Store) persistLocked(regID string) {
	rec, ok := s.byReg[regID]
	if !ok {
		return
	}
	rec.Tokens = map[string]string{}
	for _, r := range rec.Runs {
		if r.Token != "" {
			rec.Tokens[r.Token] = r.ID
		}
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return
	}
	tmp := filepath.Join(s.dir, regID+".json.tmp")
	final := filepath.Join(s.dir, regID+".json")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return
	}
	os.Rename(tmp, final)
}

// ---------------------------------------------------------------------------
// API
// ---------------------------------------------------------------------------

// Create stores a regression together with its first attempt per suite.
func (s *Store) Create(reg *model.Regression, runs []*model.Run) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.byReg[reg.ID]; exists {
		return fmt.Errorf("regression %q already exists", reg.ID)
	}
	rec := &record{Regression: reg, Runs: runs}
	s.byReg[reg.ID] = rec
	for _, r := range runs {
		s.byRun[r.ID] = r
		if r.Token != "" {
			s.byToken[r.Token] = r
		}
	}
	s.persistLocked(reg.ID)
	s.touch()
	return nil
}

// AddRun appends a retry attempt.
func (s *Store) AddRun(r *model.Run) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.byReg[r.RegressionID]
	if !ok {
		return
	}
	rec.Runs = append(rec.Runs, r)
	s.byRun[r.ID] = r
	if r.Token != "" {
		s.byToken[r.Token] = r
	}
	s.persistLocked(r.RegressionID)
	s.touch()
}

// UpdateRun mutates a run under the store lock and re-persists its regression.
func (s *Store) UpdateRun(runID string, fn func(*model.Run)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.byRun[runID]
	if !ok {
		return false
	}
	fn(r)
	s.persistLocked(r.RegressionID)
	s.touch()
	return true
}

// UpdateRegression mutates a regression under the store lock.
func (s *Store) UpdateRegression(regID string, fn func(*model.Regression, []*model.Run)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.byReg[regID]
	if !ok {
		return false
	}
	fn(rec.Regression, rec.Runs)
	s.persistLocked(regID)
	s.touch()
	return true
}

func (s *Store) Regression(id string) (*model.Regression, []*model.Run, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.byReg[id]
	if !ok {
		return nil, nil, false
	}
	return cloneReg(rec.Regression), cloneRuns(rec.Runs), true
}

// Regressions returns every regression, newest first.
func (s *Store) Regressions() []*model.Regression {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*model.Regression, 0, len(s.byReg))
	for _, rec := range s.byReg {
		out = append(out, cloneReg(rec.Regression))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SubmittedAt.After(out[j].SubmittedAt) })
	return out
}

func (s *Store) Run(id string) (*model.Run, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.byRun[id]
	if !ok {
		return nil, false
	}
	return cloneRun(r), true
}

// RunByToken resolves the runner's bearer token.
func (s *Store) RunByToken(token string) (*model.Run, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.byToken[token]
	if !ok {
		return nil, false
	}
	return cloneRun(r), true
}

// AllRuns returns a snapshot of every run, oldest queue entry first.
func (s *Store) AllRuns() []*model.Run {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*model.Run
	for _, rec := range s.byReg {
		out = append(out, cloneRuns(rec.Runs)...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].QueuedAt.Before(out[j].QueuedAt) })
	return out
}

// ---------------------------------------------------------------------------
// cloning: callers get snapshots, never live pointers
// ---------------------------------------------------------------------------

func cloneReg(r *model.Regression) *model.Regression {
	c := *r
	return &c
}

func cloneRun(r *model.Run) *model.Run {
	c := *r
	c.Cases = append([]model.Case(nil), r.Cases...)
	c.Artifacts = append([]model.ArtRef(nil), r.Artifacts...)
	return &c
}

func cloneRuns(rs []*model.Run) []*model.Run {
	out := make([]*model.Run, len(rs))
	for i, r := range rs {
		out[i] = cloneRun(r)
	}
	return out
}
