package sched

import (
	"fmt"
	"sort"

	"github.com/andrei/distributed-test-platform/internal/backend"
	"github.com/andrei/distributed-test-platform/internal/config"
	"github.com/andrei/distributed-test-platform/internal/model"
)

// The slot ledger: one tick's view of every node and of who holds slots
// where, against which admission, node choice and the quota rules are
// decided.

// ledger is one tick's view of every node - which pool it serves, how many
// of its slots are taken - and of who is holding slots where, which is what
// the quota rules are checked against. Pools are sums over their nodes; a
// virtual pool over its members' nodes.
type ledger struct {
	cat      *config.Catalog
	nodes    map[string]*nodeLedger   // node id -> node
	byPool   map[string][]*nodeLedger // concrete pool -> its nodes
	byUser   map[string]int           // user -> in-flight runs (fair share)
	inFlight []flight
}

type nodeLedger struct {
	info backend.NodeInfo
	pool string // from the catalog; "" = unassigned
	used int
}

func (n *nodeLedger) free() int {
	if !n.info.Ready || n.pool == "" {
		return 0
	}
	return max(0, n.info.Slots-n.used)
}

// flight is an in-flight run as the rules see it: whose, on which node, and
// the pools it counts in.
type flight struct {
	user  string
	node  string
	pools map[string]bool
}

func (s *Scheduler) newLedger(runs []*model.Run) *ledger {
	l := &ledger{
		cat:    s.cfg.Catalog(),
		nodes:  map[string]*nodeLedger{},
		byPool: map[string][]*nodeLedger{},
		byUser: map[string]int{},
	}
	s.mu.RLock()
	for _, n := range s.inventory {
		nl := &nodeLedger{info: n, pool: l.cat.NodePool(n.Name)}
		l.nodes[n.ID] = nl
		if nl.pool != "" {
			l.byPool[nl.pool] = append(l.byPool[nl.pool], nl)
		}
	}
	s.mu.RUnlock()
	for _, r := range runs {
		if r.State == model.RunDispatched || r.State == model.RunRunning {
			l.charge(r)
		}
	}
	return l
}

func (l *ledger) charge(r *model.Run) {
	if n, ok := l.nodes[r.NodeID]; ok {
		n.used++
	}
	l.byUser[r.User]++
	l.inFlight = append(l.inFlight, flight{user: r.User, node: r.NodeName, pools: l.poolsOf(r.Pool, r.NodeName)})
}

// poolsOf is the set of pools a run counts in: the pool it was submitted to,
// the concrete pool of the node it sits on, and every virtual pool spanning
// that one - so a rule on "global" sees work submitted straight to a member
// too, and a rule on a member sees work the virtual pool put there.
func (l *ledger) poolsOf(pool, node string) map[string]bool {
	out := map[string]bool{pool: true}
	concrete := l.cat.NodePool(node)
	if concrete == "" {
		if p, ok := l.cat.Pool(pool); ok && !p.IsVirtual() {
			concrete = pool
		}
	}
	if concrete != "" {
		out[concrete] = true
		for i := range l.cat.Pools {
			for _, m := range l.cat.Pools[i].Spans {
				if m == concrete {
					out[l.cat.Pools[i].Name] = true
				}
			}
		}
	}
	return out
}

// count is how many in-flight runs a rule sees: those in its scope by the
// subject it governs - the one user for a per-user rule, everyone it covers
// for a "together" rule.
func (l *ledger) count(r *config.QuotaRule, user string) int {
	n := 0
	for _, f := range l.inFlight {
		switch r.Scope {
		case config.ScopePool:
			if !f.pools[r.Target] {
				continue
			}
		case config.ScopeNode:
			if f.node != r.Target {
				continue
			}
		}
		switch {
		case !r.Together:
			if f.user != user {
				continue
			}
		case r.Subject == config.SubjectGroup:
			if !contains(l.cat.UserGroups(f.user), r.Name) {
				continue
			}
		}
		n++
	}
	return n
}

// block explains why the run may not take a slot right now under the rules,
// or returns "". With a node it also checks the scopes that depend on where
// the run would land (the node itself, its concrete pool and the virtual
// pools spanning it); without, only the scopes known from the submission.
func (l *ledger) block(r *model.Run, n *nodeLedger) string {
	scopes := []config.Scope{config.Everywhere, config.PoolScope(r.Pool)}
	if n != nil {
		for p := range l.poolsOf(r.Pool, n.info.Name) {
			if p != r.Pool {
				scopes = append(scopes, config.PoolScope(p))
			}
		}
		scopes = append(scopes, config.NodeScope(n.info.Name))
	}
	for _, sc := range scopes {
		if cap := l.cat.UserCap(r.User, sc); cap != nil {
			if used := l.count(cap, r.User); used >= cap.MaxSlots {
				return fmt.Sprintf("waiting: %s: %d/%d slots (rule %s)", cap.Describe(), used, cap.MaxSlots, cap.Key())
			}
		}
		for _, tr := range l.cat.TogetherRules(r.User, sc) {
			if used := l.count(tr, r.User); used >= tr.MaxSlots {
				return fmt.Sprintf("waiting: %s: %d/%d slots (rule %s)", tr.Describe(), used, tr.MaxSlots, tr.Key())
			}
		}
	}
	return ""
}

// poolNodes lists the nodes a pool can place on (a virtual pool: its
// members' nodes), by name.
func (l *ledger) poolNodes(p *config.Pool) []*nodeLedger {
	var out []*nodeLedger
	for _, np := range p.NodePools() {
		out = append(out, l.byPool[np]...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].info.Name < out[j].info.Name })
	return out
}

// capacity is the ready slot count a pool can draw on; used what is taken.
func (l *ledger) capacity(p *config.Pool) (slots, used int) {
	for _, n := range l.poolNodes(p) {
		if n.info.Ready {
			slots += n.info.Slots
			used += min(n.used, n.info.Slots)
		}
	}
	return slots, used
}

// pick chooses the node for a run: in the pool, ready, a slot free, its
// labels satisfying the pool's constraints plus the suite's requires, and no
// rule scoped to it (or to its pools) exhausted. The least loaded node wins,
// so work spreads. With no node it also returns why the last candidate a
// rule refused was refused, "" when the pool is simply full.
func (l *ledger) pick(p *config.Pool, r *model.Run) (*nodeLedger, string) {
	var best *nodeLedger
	refused := ""
	for _, n := range l.poolNodes(p) {
		if n.free() == 0 || !satisfies(n.info.Meta, p.Constraints, r.Spec.Requires) {
			continue
		}
		if why := l.block(r, n); why != "" {
			refused = why
			continue
		}
		if best == nil || n.free()*best.info.Slots > best.free()*n.info.Slots {
			best = n
		}
	}
	return best, refused
}

func satisfies(meta map[string]string, sets ...map[string]string) bool {
	for _, set := range sets {
		for k, want := range set {
			if meta[k] != want {
				return false
			}
		}
	}
	return true
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func displayUser(u string) string {
	if u == "" {
		return "(anonymous)"
	}
	return u
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
