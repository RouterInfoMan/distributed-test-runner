package sched

import (
	"sort"

	"github.com/andrei/distributed-test-platform/internal/backend"
	"github.com/andrei/distributed-test-platform/internal/config"
	"github.com/andrei/distributed-test-platform/internal/model"
)

// Read-only views for the API and the dashboard: pools with their nodes,
// unassigned nodes, and the quota rules with what they currently see.

// PoolStatus is the dashboard's view of one pool's capacity.
type PoolStatus struct {
	Name        string        `json:"name"`
	Description string        `json:"description,omitempty"`
	Runtime     model.Runtime `json:"runtime"`
	Driver      string        `json:"driver"`
	Spans       []string      `json:"spans,omitempty"` // virtual pool: the pools it draws nodes from
	Nodes       []NodeStatus  `json:"nodes"`
	Slots       int           `json:"slots"`
	Used        int           `json:"used"`
	Queued      int           `json:"queued"`
	Error       string        `json:"error,omitempty"`
}

// NodeStatus is one worker plus what it is currently running.
type NodeStatus struct {
	backend.NodeInfo
	Pool    string     `json:"pool,omitempty"` // the catalog's assignment; "" = unassigned
	Used    int        `json:"used"`
	Running []string   `json:"running,omitempty"` // suite names
	Agent   *AgentInfo `json:"agent,omitempty"`   // the node's dtp-node, when one reports in
}

// Pools renders the slot ledger for the API and dashboard.
func (s *Scheduler) Pools() []PoolStatus {
	s.mu.RLock()
	invErr := s.invErr
	s.mu.RUnlock()

	runs := s.st.AllRuns()
	l := s.newLedger(runs)
	perNode := map[string][]string{}
	perPoolQueued := map[string]int{}
	for _, r := range runs {
		switch r.State {
		case model.RunDispatched, model.RunRunning:
			if r.NodeID != "" {
				perNode[r.NodeID] = append(perNode[r.NodeID], r.Suite)
			}
		case model.RunQueued:
			perPoolQueued[r.Pool]++
		}
	}
	status := func(n *nodeLedger) NodeStatus {
		return NodeStatus{NodeInfo: n.info, Pool: n.pool, Used: n.used, Running: perNode[n.info.ID], Agent: s.agentFor(n.info)}
	}

	out := make([]PoolStatus, 0, len(l.cat.Pools))
	for i := range l.cat.Pools {
		p := &l.cat.Pools[i]
		slots, used := l.capacity(p)
		ps := PoolStatus{
			Name:        p.Name,
			Description: p.Description,
			Runtime:     p.Runtime,
			Driver:      p.Driver(),
			Spans:       p.Spans,
			Slots:       slots,
			Used:        used,
			Queued:      perPoolQueued[p.Name],
			Error:       invErr,
		}
		// Nodes are listed under the pool they are assigned to; a virtual
		// pool shows its aggregate and names its members.
		if !p.IsVirtual() {
			for _, n := range l.byPool[p.Name] {
				ps.Nodes = append(ps.Nodes, status(n))
			}
			sort.Slice(ps.Nodes, func(a, b int) bool { return ps.Nodes[a].Name < ps.Nodes[b].Name })
		}
		out = append(out, ps)
	}
	return out
}

// Unassigned lists nodes the backend sees that no pool claims; they get no
// new work until assigned (dashboard, dtp nodes assign, or the config API),
// though what was placed there before keeps running.
func (s *Scheduler) Unassigned() []NodeStatus {
	runs := s.st.AllRuns()
	l := s.newLedger(runs)
	perNode := map[string][]string{}
	for _, r := range runs {
		if (r.State == model.RunDispatched || r.State == model.RunRunning) && r.NodeID != "" {
			perNode[r.NodeID] = append(perNode[r.NodeID], r.Suite)
		}
	}
	out := []NodeStatus{}
	for _, n := range l.nodes {
		if n.pool == "" {
			out = append(out, NodeStatus{NodeInfo: n.info, Used: n.used, Running: perNode[n.info.ID], Agent: s.agentFor(n.info)})
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out
}

// RuleStatus is one quota rule with what it currently sees.
type RuleStatus struct {
	config.QuotaRule
	Key      string       `json:"key"`
	Describe string       `json:"describe"`
	Used     int          `json:"used"`            // together: slots the subject holds in scope; per user: what the users below hold
	Queued   int          `json:"queued"`          // queued runs of those users that could count here
	Users    []UserStatus `json:"users,omitempty"` // per-user rules: the users the rule governs
}

// UserStatus is one user's slot usage within a rule's scope and the cap that
// governs them there.
type UserStatus struct {
	User   string `json:"user"`
	Used   int    `json:"used"`
	Queued int    `json:"queued"`
	Cap    int    `json:"cap"`
	Via    string `json:"via,omitempty"` // the governing rule, when it is a more specific one than the row's
}

// Quotas renders every rule with its current usage: for a "together" rule
// the subject's slots in scope; for a per-user rule each user it governs -
// the user, the group's members, or (for the everyone rule) every user the
// catalog lists or the store has seen - with their slots in scope and the
// cap that actually applies to them, which may come from a more specific
// rule.
func (s *Scheduler) Quotas() []RuleStatus {
	runs := s.st.AllRuns()
	l := s.newLedger(runs)
	seen := map[string]bool{}
	for _, u := range l.cat.Users {
		seen[u.Name] = true
	}
	var queued []*model.Run
	for _, r := range runs {
		seen[r.User] = true // anyone who ever submitted, listed or not
		if r.State == model.RunQueued {
			queued = append(queued, r)
		}
	}
	everyone := sortedKeys(seen)
	// queuedIn counts a user's queued runs that could count in the scope
	// once placed: by the submitted pool (or the pools it spans, or those
	// spanning it), since the node is not known yet; never for a node scope.
	queuedIn := func(user string, rule *config.QuotaRule) int {
		n := 0
		for _, r := range queued {
			if r.User != user {
				continue
			}
			switch rule.Scope {
			case config.ScopeNode:
				continue
			case config.ScopePool:
				if !l.poolsOf(r.Pool, "")[rule.Target] && !l.poolsOf(rule.Target, "")[r.Pool] {
					continue
				}
			}
			n++
		}
		return n
	}

	rules := l.cat.Clone().Quotas
	out := make([]RuleStatus, 0, len(rules))
	for i := range rules {
		r := &rules[i]
		rs := RuleStatus{QuotaRule: *r, Key: r.Key(), Describe: r.Describe()}
		if r.Together {
			rs.Used = l.count(r, "")
			if r.Subject == config.SubjectGroup {
				for _, u := range l.cat.Members(r.Name) {
					rs.Queued += queuedIn(u, r)
				}
			} else {
				for _, u := range everyone {
					rs.Queued += queuedIn(u, r)
				}
			}
			out = append(out, rs)
			continue
		}
		var users []string
		switch r.Subject {
		case config.SubjectUser:
			users = []string{r.Name}
		case config.SubjectGroup:
			users = l.cat.Members(r.Name)
		default:
			users = everyone
		}
		rs.Users = []UserStatus{}
		for _, u := range users {
			us := UserStatus{User: u, Used: l.count(r, u), Queued: queuedIn(u, r), Cap: r.MaxSlots}
			if g := l.cat.UserCap(u, r.ScopeOf()); g != nil && g.Key() != r.Key() {
				us.Cap, us.Via = g.MaxSlots, g.Key()
			}
			rs.Used += us.Used
			rs.Queued += us.Queued
			rs.Users = append(rs.Users, us)
		}
		out = append(out, rs)
	}
	return out
}
