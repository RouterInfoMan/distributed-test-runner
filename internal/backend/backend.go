// Package backend abstracts "run this suite on that node and tell me how it
// went". The master chooses the node; a backend only executes there. The
// Nomad backend generates a batch job per suite attempt pinned to the node;
// the local backend forks dtp-runner on the master host and exists so the
// platform can be exercised end to end without a cluster.
package backend

import (
	"context"

	"github.com/andrei/distributed-test-platform/internal/model"
)

// NodeInfo is one worker as the scheduler sees it.
// NodeInfo is one worker as the backend sees it. Pool membership is not the
// backend's business: the master's catalog assigns nodes to pools. Slots and
// Slot are what the node declared (meta.dtp.slots, meta.dtp.slot.*).
type NodeInfo struct {
	ID     string            `json:"id"`
	Name   string            `json:"name"`
	Slots  int               `json:"slots"`
	Slot   model.Slot        `json:"slot"`
	Ready  bool              `json:"ready"`
	Status string            `json:"status"`
	Meta   map[string]string `json:"meta,omitempty"`
}

// Placement is what the backend knows the moment a run is handed off.
type Placement struct {
	BackendID string // Nomad job id, or a local handle
	NodeID    string
	NodeName  string
}

// Phase is the backend's view of an attempt, independent of what the runner
// reported over HTTP. It is how the master notices lost allocations.
type Phase string

const (
	PhaseUnknown  Phase = "unknown"
	PhasePending  Phase = "pending"
	PhaseRunning  Phase = "running"
	PhaseComplete Phase = "complete"
	PhaseFailed   Phase = "failed"
	PhaseLost     Phase = "lost"
)

// Status is one poll of a dispatched run.
type Status struct {
	Phase    Phase
	AllocID  string
	NodeID   string
	NodeName string
	ExitCode *int
	Message  string
}

// BulkPoller is implemented by backends that can report every dispatched run
// in one round trip. The scheduler prefers it over Poll, so reconciliation
// costs one API call per tick rather than one per in-flight suite.
type BulkPoller interface {
	// PollAll returns a status for every run it could see, keyed by run ID.
	// Runs missing from the map are polled individually.
	PollAll(ctx context.Context, runs []*model.Run) (map[string]Status, error)
}

// Backend is implemented by the Nomad and local drivers.
type Backend interface {
	Name() string
	// Inventory lists the worker nodes and their slot counts.
	Inventory(ctx context.Context) ([]NodeInfo, error)
	// Dispatch launches one suite attempt on the node the scheduler chose
	// (run.NodeID / run.NodeName), reserving spec.Slot there.
	Dispatch(ctx context.Context, run *model.Run, spec model.RunSpec) (Placement, error)
	// Poll reports the backend's view of a dispatched run.
	Poll(ctx context.Context, run *model.Run) (Status, error)
	// Stop cancels a run and releases its slot.
	Stop(ctx context.Context, run *model.Run) error
	// Healthy reports backend reachability for the dashboard.
	Healthy(ctx context.Context) error
}
