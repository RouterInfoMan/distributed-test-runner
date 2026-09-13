package sched

import (
	"context"
	"sort"
	"time"

	"github.com/andrei/distributed-test-platform/internal/backend"
	"github.com/andrei/distributed-test-platform/internal/config"
	"github.com/andrei/distributed-test-platform/internal/model"
)

// Nodes as the scheduler sees them: the backend's inventory, and the dtp-node
// agents that report in. Which pool a node serves is the catalog's business.

// AgentInfo is the latest heartbeat from a node's dtp-node agent.
type AgentInfo struct {
	Name         string            `json:"name"`
	Pool         string            `json:"pool,omitempty"` // the pool node.yaml suggests for first registration
	NodeID       string            `json:"node_id,omitempty"`
	Labels       map[string]string `json:"labels"`
	Slots        int               `json:"slots"`
	Slot         model.Slot        `json:"slot"`
	AgentVersion string            `json:"agent_version"`
	CacheEntries int               `json:"cache_entries"`
	CacheBytes   int64             `json:"cache_bytes"`
	At           time.Time         `json:"at"`      // as reported
	SeenAt       time.Time         `json:"seen_at"` // when the master received it
	Stale        bool              `json:"stale"`   // no heartbeat for over three minutes
}

// AgentStale is how long without a heartbeat before an agent is reported stale.
const AgentStale = 3 * time.Minute

// Heartbeat records an agent report. A node the catalog does not know yet is
// added - assigned to the pool its node.yaml suggests when that pool exists,
// unassigned otherwise; after that the catalog's assignment wins.
func (s *Scheduler) Heartbeat(a AgentInfo) error {
	a.SeenAt = time.Now().UTC()
	s.agMu.Lock()
	first := s.agents[a.Name] == nil
	s.agents[a.Name] = &a
	s.agMu.Unlock()
	if first {
		s.log.Info("agent registered", "node", a.Name, "suggested_pool", a.Pool, "slots", a.Slots, "version", a.AgentVersion)
	}
	cat := s.cfg.RawCatalog()
	if _, known := cat.NodePool(a.Name), knownNode(cat, a.Name); !known {
		next := cat.Clone()
		pool := ""
		if _, ok := cat.Pool(a.Pool); ok && a.Pool != "" {
			pool = a.Pool
		}
		next.SetNode(config.Node{Name: a.Name, Pool: pool})
		if err := s.ApplyCatalog(next); err != nil {
			s.log.Warn("could not register node", "node", a.Name, "err", err)
		} else if pool == "" {
			s.log.Info("node registered unassigned; assign it to a pool in the dashboard or with dtp nodes assign", "node", a.Name)
		}
	}
	s.st.Notify()
	return nil
}

func knownNode(cat *config.Catalog, name string) bool {
	for _, n := range cat.Nodes {
		if n.Name == name {
			return true
		}
	}
	return false
}

// Agents lists every agent that has reported, by name.
func (s *Scheduler) Agents() []AgentInfo {
	s.agMu.Lock()
	defer s.agMu.Unlock()
	out := make([]AgentInfo, 0, len(s.agents))
	for _, a := range s.agents {
		cp := *a
		cp.Stale = time.Since(cp.SeenAt) > AgentStale
		out = append(out, cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *Scheduler) agentFor(node backend.NodeInfo) *AgentInfo {
	s.agMu.Lock()
	defer s.agMu.Unlock()
	a, ok := s.agents[node.Name]
	if !ok {
		return nil
	}
	cp := *a
	cp.Stale = time.Since(cp.SeenAt) > AgentStale
	return &cp
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
