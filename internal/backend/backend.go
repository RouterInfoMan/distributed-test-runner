// Package backend abstracts "put this suite on a node and tell me how it went".
// The Nomad backend generates a batch job per suite attempt; the local backend
// forks dtp-runner on the master host and exists so the platform can be
// exercised end to end without a cluster.
package backend

import (
	"context"

	"github.com/andrei/distributed-test-platform/internal/model"
)

// NodeInfo is one worker as the scheduler sees it.
type NodeInfo struct {
	ID     string            `json:"id"`
	Name   string            `json:"name"`
	Pool   string            `json:"pool"`
	Slots  int               `json:"slots"`
	Ready  bool              `json:"ready"`
	Status string            `json:"status"`
	Meta   map[string]string `json:"meta,omitempty"`
}

// Placement is what the backend knows the moment a run is handed off.
type Placement struct {
	BackendID string // Nomad job id, or a local handle
	NodeID    string // known immediately only for the local backend
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

// Backend is implemented by the Nomad and local drivers.
type Backend interface {
	Name() string
	// Inventory lists the worker nodes and their slot counts.
	Inventory(ctx context.Context) ([]NodeInfo, error)
	// Dispatch places one suite attempt.
	Dispatch(ctx context.Context, run *model.Run, spec model.RunSpec) (Placement, error)
	// Poll reports the backend's view of a dispatched run.
	Poll(ctx context.Context, run *model.Run) (Status, error)
	// Stop cancels a run and releases its slot.
	Stop(ctx context.Context, run *model.Run) error
	// Healthy reports backend reachability for the dashboard.
	Healthy(ctx context.Context) error
}
