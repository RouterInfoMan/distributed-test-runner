// Package store keeps regressions and runs in memory - the scheduler's
// authority - and mirrors every mutation to a Backend: one JSON file per
// regression, or PostgreSQL tables that can be queried and kept for years.
// The in-memory copy is loaded from the backend at startup.
package store

import (
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/andrei/distributed-test-platform/internal/config"
	"github.com/andrei/distributed-test-platform/internal/model"
)

// record is one regression and all of its run attempts, the unit the file
// backend persists.
type record struct {
	Regression *model.Regression `json:"regression"`
	Runs       []*model.Run      `json:"runs"`
	Tokens     map[string]string `json:"tokens,omitempty"` // token -> run id
}

// Backend is where the store mirrors its state.
type Backend interface {
	// Load returns every stored regression with its runs (tokens restored).
	Load() ([]*record, error)
	// SaveRecord persists a new regression together with its first runs.
	SaveRecord(rec *record) error
	// SaveRegression persists the regression header after a mutation.
	SaveRegression(rec *record) error
	// SaveRun persists one run after a mutation (or a newly added retry).
	SaveRun(rec *record, run *model.Run) error
	// LoadCatalog returns the stored pools and quotas as authored, or nil
	// when nothing has been stored yet (first start: the config file seeds it).
	LoadCatalog() (*config.Catalog, error)
	// SaveCatalog replaces the stored catalog.
	SaveCatalog(cat *config.Catalog) error
	Close() error
}

// Store is safe for concurrent use.
type Store struct {
	mu       sync.RWMutex
	be       Backend
	log      *slog.Logger
	byReg    map[string]*record
	byRun    map[string]*model.Run
	byToken  map[string]*model.Run
	revision uint64 // bumped on every mutation; the dashboard long-polls on it
	waiters  []chan struct{}
}

// Open uses the JSON-file backend rooted at dir.
func Open(dir string) (*Store, error) {
	be, err := NewFileBackend(dir)
	if err != nil {
		return nil, err
	}
	return OpenWith(be, nil)
}

// OpenWith loads everything the backend holds. Runs left mid-flight by a
// master restart are re-queued; the backend allocation, if any, is
// reconciled away by the scheduler.
func OpenWith(be Backend, log *slog.Logger) (*Store, error) {
	if log == nil {
		log = slog.Default()
	}
	s := &Store{
		be:      be,
		log:     log,
		byReg:   map[string]*record{},
		byRun:   map[string]*model.Run{},
		byToken: map[string]*model.Run{},
	}
	recs, err := be.Load()
	if err != nil {
		return nil, err
	}
	for _, rec := range recs {
		if rec.Regression == nil {
			continue
		}
		s.byReg[rec.Regression.ID] = rec
		specs := map[string]model.SuiteSpec{}
		for _, sp := range rec.Regression.Suites {
			specs[sp.Name] = sp
		}
		for _, r := range rec.Runs {
			r.Spec = specs[r.Suite]
			s.byRun[r.ID] = r
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

// Close releases the backend.
func (s *Store) Close() error { return s.be.Close() }

// LoadCatalog reads the stored pools and quotas (nil when never seeded).
func (s *Store) LoadCatalog() (*config.Catalog, error) { return s.be.LoadCatalog() }

// SaveCatalog replaces the stored pools and quotas.
func (s *Store) SaveCatalog(cat *config.Catalog) error { return s.be.SaveCatalog(cat) }

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

// Notify bumps the revision for a change made outside the store's own
// mutations (a catalog reload), so the dashboard's long-poll wakes up.
func (s *Store) Notify() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.touch()
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

// tokensLocked refreshes the record's token index before it is persisted.
func (rec *record) tokens() map[string]string {
	rec.Tokens = map[string]string{}
	for _, r := range rec.Runs {
		if r.Token != "" {
			rec.Tokens[r.Token] = r.ID
		}
	}
	return rec.Tokens
}

// persist failures are logged, not returned: the in-memory state stays
// authoritative and the next mutation of the same object writes it again.
func (s *Store) persistErr(what string, err error) {
	if err != nil {
		s.log.Warn("store: persist failed", "what", what, "err", err)
	}
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
	rec.tokens()
	if err := s.be.SaveRecord(rec); err != nil {
		// A submission that cannot be written is refused outright, so a caller
		// never gets an ID the store might forget.
		delete(s.byReg, reg.ID)
		for _, r := range runs {
			delete(s.byRun, r.ID)
			delete(s.byToken, r.Token)
		}
		return fmt.Errorf("store: %w", err)
	}
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
	rec.tokens()
	s.persistErr("run "+r.ID, s.be.SaveRun(rec, r))
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
	if rec, ok := s.byReg[r.RegressionID]; ok {
		s.persistErr("run "+r.ID, s.be.SaveRun(rec, r))
	}
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
	s.persistErr("regression "+regID, s.be.SaveRegression(rec))
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
