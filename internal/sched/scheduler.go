// Package sched is the master's control loop: it admits queued suites against
// the per-node slot ledger and the quota rules, picks the node, dispatches to
// the backend, reconciles backend state, retries failures, and keeps each
// regression's rollup current.
//
//	scheduler.go   the loop and its wiring
//	submit.go      submission and cancellation
//	ledger.go      the per-tick slot ledger: capacity, node choice, quota rules
//	admit.go       admission and dispatch (the RunSpec)
//	reconcile.go   backend status, runner events, timeouts, finishing attempts
//	rollup.go      per-regression totals, state and manifest
//	nodes.go       inventory and node agents
//	catalog.go     applying and reloading the catalog
//	views.go       what the API and dashboard show
//	ids.go         regression and run ids
package sched

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/andrei/distributed-test-platform/internal/backend"
	"github.com/andrei/distributed-test-platform/internal/buildrepo"
	"github.com/andrei/distributed-test-platform/internal/config"
	"github.com/andrei/distributed-test-platform/internal/s3"
	"github.com/andrei/distributed-test-platform/internal/store"
)

// TickInterval is how often the control loop runs. Suites are long-lived, so
// a one-second cadence is plenty and keeps the Nomad API load trivial.
const TickInterval = time.Second

type Scheduler struct {
	cfg  *config.Config
	st   *store.Store
	be   backend.Backend
	s3   *s3.Client
	repo *buildrepo.Repo // nil without an object store
	log  *slog.Logger

	mu        sync.RWMutex
	inventory []backend.NodeInfo
	invErr    string

	catMu sync.Mutex // serializes catalog writes

	agMu   sync.Mutex
	agents map[string]*AgentInfo // node name -> latest heartbeat
}

func New(cfg *config.Config, st *store.Store, be backend.Backend, s3c *s3.Client, log *slog.Logger) *Scheduler {
	s := &Scheduler{cfg: cfg, st: st, be: be, s3: s3c, log: log, agents: map[string]*AgentInfo{}}
	if s3c != nil && cfg.S3.BuildsBucket != "" {
		s.repo = buildrepo.New(s3c, cfg.S3.BuildsBucket)
	}
	return s
}

// Builds exposes the build repository (nil without an object store).
func (s *Scheduler) Builds() *buildrepo.Repo { return s.repo }

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

// Backend exposes the active backend for the API layer.
func (s *Scheduler) Backend() backend.Backend { return s.be }

// Config exposes the loaded config for the API layer.
func (s *Scheduler) Config() *config.Config { return s.cfg }
