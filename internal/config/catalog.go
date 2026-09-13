package config

import (
	"encoding/json"
	"fmt"
	"sort"
)

// Catalog is the part of the configuration that describes the lab rather than
// this process: pools, which pool each node serves, groups, users and the
// quota rules. It lives in the store - the PostgreSQL tables pools / nodes /
// groups / users / group_members / quota_rules, or catalog.json - and
// nowhere else: edited through the API, the dashboard or SQL, applied live.
// An empty catalog is valid; nothing can be scheduled until pools exist.
type Catalog struct {
	Pools  []Pool      `json:"pools"`
	Nodes  []Node      `json:"nodes,omitempty"`
	Groups []Group     `json:"groups,omitempty"`
	Users  []User      `json:"users,omitempty"`
	Quotas []QuotaRule `json:"quotas,omitempty"`
}

// Clone deep-copies a catalog (the types are plain data, so JSON is exact)
// in canonical form, so a file-authored catalog and one read back from the
// tables compare equal.
func (c *Catalog) Clone() *Catalog {
	b, _ := json.Marshal(c)
	var out Catalog
	json.Unmarshal(b, &out)
	if out.Pools == nil {
		out.Pools = []Pool{}
	}
	// Sets are stored and compared sorted: users, groups and nodes by name,
	// a user's groups, the rules by key. Pool order and a virtual pool's
	// member order are meaningful (display order; the first member is
	// inherited from) and stay as authored.
	sort.Slice(out.Users, func(i, j int) bool { return out.Users[i].Name < out.Users[j].Name })
	for i := range out.Users {
		sort.Strings(out.Users[i].Groups)
	}
	sort.Slice(out.Groups, func(i, j int) bool { return out.Groups[i].Name < out.Groups[j].Name })
	sort.Slice(out.Nodes, func(i, j int) bool { return out.Nodes[i].Name < out.Nodes[j].Name })
	sortRules(out.Quotas)
	return &out
}

// Equal compares two catalogs as authored (in canonical form).
func (c *Catalog) Equal(o *Catalog) bool {
	a, _ := json.Marshal(c.Clone())
	b, _ := json.Marshal(o.Clone())
	return string(a) == string(b)
}

// Normalize validates the catalog and returns a copy with defaults filled in
// and virtual pools inheriting from their first member. The receiver is left
// as authored, so what is stored never accumulates derived values.
func (c *Catalog) Normalize() (*Catalog, error) {
	n := c.Clone()
	names := map[string]bool{}
	for i := range n.Pools {
		p := &n.Pools[i]
		if p.Name == "" {
			return nil, fmt.Errorf("pool %d has no name", i)
		}
		if names[p.Name] {
			return nil, fmt.Errorf("duplicate pool %q", p.Name)
		}
		names[p.Name] = true
	}
	// Virtual pools inherit before defaults are filled in, so "global" can be
	// just a name and a list of members.
	for i := range n.Pools {
		if p := &n.Pools[i]; p.IsVirtual() {
			if err := n.checkSpans(p); err != nil {
				return nil, err
			}
			m, _ := n.Pool(p.Spans[0])
			p.inherit(m)
		}
	}
	for i := range n.Pools {
		p := &n.Pools[i]
		if !p.Runtime.Valid() {
			return nil, fmt.Errorf("pool %q: runtime must be process or container", p.Name)
		}
		if p.CacheDir == "" {
			p.CacheDir = "/var/lib/dtp/cache"
		}
	}
	seenNode := map[string]bool{}
	for i := range n.Nodes {
		nd := &n.Nodes[i]
		if nd.Name == "" {
			return nil, fmt.Errorf("node %d has no name", i)
		}
		if seenNode[nd.Name] {
			return nil, fmt.Errorf("duplicate node %q", nd.Name)
		}
		seenNode[nd.Name] = true
		if nd.Pool != "" {
			p, ok := n.Pool(nd.Pool)
			if !ok {
				return nil, fmt.Errorf("node %q is assigned to unknown pool %q", nd.Name, nd.Pool)
			}
			if p.IsVirtual() {
				return nil, fmt.Errorf("node %q: %q is a virtual pool; assign the node to one of the pools it spans", nd.Name, nd.Pool)
			}
		}
	}
	for i := range n.Pools {
		if p := &n.Pools[i]; p.IsVirtual() {
			if err := n.checkMembersAgree(p); err != nil {
				return nil, err
			}
		}
	}
	if err := n.validateQuotas(); err != nil {
		return nil, err
	}
	return n, nil
}

// checkSpans validates the member list of a virtual pool.
func (c *Catalog) checkSpans(p *Pool) error {
	seen := map[string]bool{}
	for _, name := range p.Spans {
		m, ok := c.Pool(name)
		if !ok {
			return fmt.Errorf("pool %q spans unknown pool %q", p.Name, name)
		}
		if m.IsVirtual() {
			return fmt.Errorf("pool %q spans %q, which is itself virtual", p.Name, name)
		}
		if seen[name] {
			return fmt.Errorf("pool %q spans %q twice", p.Name, name)
		}
		seen[name] = true
	}
	return nil
}

// checkMembersAgree enforces what a virtual pool cannot paper over: a job
// runs under one driver, so every member must use the same runtime and
// driver. Slot sizes may differ (see largestSlot).
func (c *Catalog) checkMembersAgree(p *Pool) error {
	for _, name := range p.Spans {
		m, _ := c.Pool(name)
		if m.Runtime != p.Runtime {
			return fmt.Errorf("pool %q (%s) spans %q (%s): members must share one runtime",
				p.Name, p.Runtime, m.Name, m.Runtime)
		}
		if m.Driver() != p.Driver() {
			return fmt.Errorf("pool %q (%s) spans %q (%s): members must share one task driver",
				p.Name, p.Driver(), m.Name, m.Driver())
		}
	}
	return nil
}

// inherit copies the settings a virtual pool left empty from a member.
func (p *Pool) inherit(m *Pool) {
	if p.Runtime == "" {
		p.Runtime = m.Runtime
	}
	if p.TaskDriver == "" {
		p.TaskDriver = m.TaskDriver
	}
	if p.Default.Image == "" {
		p.Default.Image = m.Default.Image
	}
	if len(p.Default.Command) == 0 {
		p.Default.Command = m.Default.Command
	}
	if len(p.RunnerCommand) == 0 {
		p.RunnerCommand = m.RunnerCommand
	}
	if p.DockerNetwork == "" {
		p.DockerNetwork = m.DockerNetwork
	}
	if p.RunnerURL == "" {
		p.RunnerURL = m.RunnerURL
	}
	if p.CacheDir == "" {
		p.CacheDir = m.CacheDir
	}
	if p.HostVolume == "" {
		p.HostVolume = m.HostVolume
	}
}

// User looks a user up by name.
func (c *Catalog) User(name string) (*User, bool) {
	for i := range c.Users {
		if c.Users[i].Name == name {
			return &c.Users[i], true
		}
	}
	return nil, false
}

// Group looks a group up by name.
func (c *Catalog) Group(name string) (*Group, bool) {
	for i := range c.Groups {
		if c.Groups[i].Name == name {
			return &c.Groups[i], true
		}
	}
	return nil, false
}

// Members lists the users of a group.
func (c *Catalog) Members(group string) []string {
	var out []string
	for _, u := range c.Users {
		for _, g := range u.Groups {
			if g == group {
				out = append(out, u.Name)
				break
			}
		}
	}
	return out
}

// NodePool is the pool a node is assigned to ("" when unassigned).
func (c *Catalog) NodePool(name string) string {
	for _, n := range c.Nodes {
		if n.Name == name {
			return n.Pool
		}
	}
	return ""
}

// PoolNodes lists the nodes assigned to a concrete pool, or to any member of
// a virtual one.
func (c *Catalog) PoolNodes(pool string) []string {
	p, ok := c.Pool(pool)
	if !ok {
		return nil
	}
	want := map[string]bool{}
	for _, np := range p.NodePools() {
		want[np] = true
	}
	var out []string
	for _, n := range c.Nodes {
		if want[n.Pool] {
			out = append(out, n.Name)
		}
	}
	return out
}

// SetNode assigns a node to a pool (adding the node when new).
func (c *Catalog) SetNode(n Node) {
	for i := range c.Nodes {
		if c.Nodes[i].Name == n.Name {
			c.Nodes[i] = n
			return
		}
	}
	c.Nodes = append(c.Nodes, n)
}

// RemoveNode forgets a node and the rules scoped to it; false when there
// was none by that name.
func (c *Catalog) RemoveNode(name string) bool {
	for i := range c.Nodes {
		if c.Nodes[i].Name == name {
			c.Nodes = append(c.Nodes[:i], c.Nodes[i+1:]...)
			c.dropRules(func(r *QuotaRule) bool { return r.Scope == ScopeNode && r.Target == name })
			return true
		}
	}
	return false
}

// SetUser adds or replaces a user.
func (c *Catalog) SetUser(u User) {
	for i := range c.Users {
		if c.Users[i].Name == u.Name {
			c.Users[i] = u
			return
		}
	}
	c.Users = append(c.Users, u)
}

// RemoveUser drops a user and their own rules; false when there was none by
// that name.
func (c *Catalog) RemoveUser(name string) bool {
	for i := range c.Users {
		if c.Users[i].Name == name {
			c.Users = append(c.Users[:i], c.Users[i+1:]...)
			c.dropRules(func(r *QuotaRule) bool { return r.Subject == SubjectUser && r.Name == name })
			return true
		}
	}
	return false
}

func (c *Catalog) Pool(name string) (*Pool, bool) {
	for i := range c.Pools {
		if c.Pools[i].Name == name {
			return &c.Pools[i], true
		}
	}
	return nil, false
}

// SetPool adds or replaces one pool, keeping the authored order.
func (c *Catalog) SetPool(p Pool) {
	for i := range c.Pools {
		if c.Pools[i].Name == p.Name {
			c.Pools[i] = p
			return
		}
	}
	c.Pools = append(c.Pools, p)
}

// RemovePool drops a pool, unassigning its nodes and dropping it from
// virtual pools' spans and from the rules scoped to it; false when there was
// none by that name.
func (c *Catalog) RemovePool(name string) bool {
	for i := range c.Pools {
		if c.Pools[i].Name == name {
			c.Pools = append(c.Pools[:i], c.Pools[i+1:]...)
			for j := range c.Nodes {
				if c.Nodes[j].Pool == name {
					c.Nodes[j].Pool = ""
				}
			}
			for j := range c.Pools {
				c.Pools[j].Spans = without(c.Pools[j].Spans, name)
			}
			c.dropRules(func(r *QuotaRule) bool { return r.Scope == ScopePool && r.Target == name })
			return true
		}
	}
	return false
}

func without(list []string, item string) []string {
	out := list[:0:0]
	for _, x := range list {
		if x != item {
			out = append(out, x)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// SetGroup adds or replaces a group.
func (c *Catalog) SetGroup(g Group) {
	for i := range c.Groups {
		if c.Groups[i].Name == g.Name {
			c.Groups[i] = g
			return
		}
	}
	c.Groups = append(c.Groups, g)
}

// RemoveGroup drops a group, the memberships that pointed at it and its
// rules; false when there was none by that name.
func (c *Catalog) RemoveGroup(name string) bool {
	for i := range c.Groups {
		if c.Groups[i].Name == name {
			c.Groups = append(c.Groups[:i], c.Groups[i+1:]...)
			for j := range c.Users {
				c.Users[j].Groups = without(c.Users[j].Groups, name)
			}
			c.dropRules(func(r *QuotaRule) bool { return r.Subject == SubjectGroup && r.Name == name })
			return true
		}
	}
	return false
}
