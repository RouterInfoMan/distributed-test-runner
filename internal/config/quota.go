package config

import (
	"fmt"
	"sort"
	"strings"
)

// QuotaRule caps concurrent slots. It is one row of the rule table:
//
//	subject × scope → max slots
//
// The subject is a user, a group, or everyone ("global"); for a group or
// everyone the limit applies to each user separately unless Together is
// set, in which case it applies to all of them added up. The scope is a
// node, a pool, or everywhere ("global"). A limit of 0 forbids; no rule
// means no limit.
//
// Rules of different scopes all apply. Within one scope the per-user cap
// that governs a user is the most specific one: the user's own rule, else
// the most permissive of their groups' rules, else the everyone rule; the
// "together" rules of the user's groups and of everyone apply on top.
type QuotaRule struct {
	Subject  string `json:"subject"`            // user | group | global
	Name     string `json:"name,omitempty"`     // the user or the group; empty for global
	Together bool   `json:"together,omitempty"` // group/global: the limit is on all of them together, not on each
	Scope    string `json:"scope"`              // node | pool | global
	Target   string `json:"target,omitempty"`   // the node or the pool; empty for global
	MaxSlots int    `json:"max_slots"`
	Note     string `json:"note,omitempty"`
}

// Subject and scope kinds.
const (
	SubjectUser   = "user"
	SubjectGroup  = "group"
	SubjectGlobal = "global" // everyone

	ScopeNode   = "node"
	ScopePool   = "pool"
	ScopeGlobal = "global" // everywhere
)

// Scope is where a rule counts: a node, a pool, or everywhere.
type Scope struct {
	Kind   string
	Target string
}

// Everywhere is the global scope.
var Everywhere = Scope{Kind: ScopeGlobal}

// PoolScope and NodeScope build scopes.
func PoolScope(name string) Scope { return Scope{Kind: ScopePool, Target: name} }
func NodeScope(name string) Scope { return Scope{Kind: ScopeNode, Target: name} }

func (s Scope) String() string {
	switch s.Kind {
	case ScopePool:
		return "in pool " + s.Target
	case ScopeNode:
		return "on node " + s.Target
	}
	return "anywhere"
}

// ScopeOf is the rule's scope.
func (r *QuotaRule) ScopeOf() Scope { return Scope{Kind: r.Scope, Target: r.Target} }

// Key identifies a rule: subject[:name][/together]@scope[:target], for
// example user:carol@global, group:release@pool:high-perf-pool,
// global/together@node:rcp-hp-1. It is what the API paths and the CLI use.
func (r *QuotaRule) Key() string {
	subj := r.Subject
	if r.Name != "" {
		subj += ":" + r.Name
	}
	if r.Together {
		subj += "/together"
	}
	scope := r.Scope
	if r.Target != "" {
		scope += ":" + r.Target
	}
	return subj + "@" + scope
}

// ParseRuleKey turns a key back into the rule's subject and scope (its
// limit and note are not part of the key).
func ParseRuleKey(key string) (QuotaRule, error) {
	var r QuotaRule
	subj, scope, ok := strings.Cut(key, "@")
	if !ok {
		return r, fmt.Errorf("quota rule key %q: want subject@scope", key)
	}
	if strings.HasSuffix(subj, "/together") {
		r.Together = true
		subj = strings.TrimSuffix(subj, "/together")
	}
	r.Subject, r.Name, _ = strings.Cut(subj, ":")
	r.Scope, r.Target, _ = strings.Cut(scope, ":")
	return r, r.validateShape()
}

// validateShape checks the rule's kinds and that names are present exactly
// where they belong; existence of the named things is checked by the catalog.
func (r *QuotaRule) validateShape() error {
	switch r.Subject {
	case SubjectUser, SubjectGroup:
		if r.Name == "" {
			return fmt.Errorf("quota rule: subject %s needs a name", r.Subject)
		}
	case SubjectGlobal:
		if r.Name != "" {
			return fmt.Errorf("quota rule: a global subject has no name (got %q)", r.Name)
		}
	default:
		return fmt.Errorf("quota rule: subject must be user, group or global (got %q)", r.Subject)
	}
	if r.Subject == SubjectUser && r.Together {
		return fmt.Errorf("quota rule for user %s: \"together\" only makes sense for a group or everyone", r.Name)
	}
	switch r.Scope {
	case ScopeNode, ScopePool:
		if r.Target == "" {
			return fmt.Errorf("quota rule %s: scope %s needs a target", r.Key(), r.Scope)
		}
	case ScopeGlobal:
		if r.Target != "" {
			return fmt.Errorf("quota rule %s: a global scope has no target (got %q)", r.Key(), r.Target)
		}
	default:
		return fmt.Errorf("quota rule %s: scope must be node, pool or global (got %q)", r.Key(), r.Scope)
	}
	if r.MaxSlots < 0 {
		return fmt.Errorf("quota rule %s: max_slots must be >= 0", r.Key())
	}
	return nil
}

// Describe renders the rule for people: "each member of core-devs in pool
// high-perf-pool", "carol anywhere", "everyone together on node rcp-hp-1".
func (r *QuotaRule) Describe() string {
	var who string
	switch r.Subject {
	case SubjectUser:
		who = r.Name
	case SubjectGroup:
		if r.Together {
			who = r.Name + " together"
		} else {
			who = "each member of " + r.Name
		}
	default:
		if r.Together {
			who = "everyone together"
		} else {
			who = "each user"
		}
	}
	return who + " " + r.ScopeOf().String()
}

// covers reports whether a per-user or together rule concerns this user:
// their own rule, one of their groups', or the global one.
func (r *QuotaRule) covers(user string, groups []string) bool {
	switch r.Subject {
	case SubjectUser:
		return r.Name == user
	case SubjectGroup:
		for _, g := range groups {
			if g == r.Name {
				return true
			}
		}
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// resolution against a catalog
// ---------------------------------------------------------------------------

// UserGroups lists the groups a user belongs to; an unknown user is in none.
func (c *Catalog) UserGroups(user string) []string {
	if u, ok := c.User(user); ok {
		return u.Groups
	}
	return nil
}

// UserCap is the per-user rule that governs user in scope: their own rule,
// else the most permissive of their groups' rules, else the everyone rule;
// nil when nothing limits them there.
func (c *Catalog) UserCap(user string, scope Scope) *QuotaRule {
	groups := c.UserGroups(user)
	var own, best, global *QuotaRule
	for i := range c.Quotas {
		r := &c.Quotas[i]
		if r.Together || r.ScopeOf() != scope || !r.covers(user, groups) {
			continue
		}
		switch r.Subject {
		case SubjectUser:
			own = r
		case SubjectGroup:
			if best == nil || r.MaxSlots > best.MaxSlots {
				best = r
			}
		default:
			global = r
		}
	}
	switch {
	case own != nil:
		return own
	case best != nil:
		return best
	}
	return global
}

// TogetherRules lists the aggregate rules that apply to user in scope: those
// of the user's groups and the everyone-together rule, all of which must
// hold.
func (c *Catalog) TogetherRules(user string, scope Scope) []*QuotaRule {
	groups := c.UserGroups(user)
	var out []*QuotaRule
	for i := range c.Quotas {
		r := &c.Quotas[i]
		if r.Together && r.ScopeOf() == scope && r.covers(user, groups) {
			out = append(out, r)
		}
	}
	return out
}

// Forbidden reports the rule that gives user zero slots in scope, if any;
// used at submission to refuse work that could never start.
func (c *Catalog) Forbidden(user string, scope Scope) *QuotaRule {
	if r := c.UserCap(user, scope); r != nil && r.MaxSlots == 0 {
		return r
	}
	return nil
}

// Rule looks a rule up by key.
func (c *Catalog) Rule(key string) (*QuotaRule, bool) {
	for i := range c.Quotas {
		if c.Quotas[i].Key() == key {
			return &c.Quotas[i], true
		}
	}
	return nil, false
}

// SetRule adds or replaces the rule with r's key.
func (c *Catalog) SetRule(r QuotaRule) {
	key := r.Key()
	for i := range c.Quotas {
		if c.Quotas[i].Key() == key {
			c.Quotas[i] = r
			return
		}
	}
	c.Quotas = append(c.Quotas, r)
}

// RemoveRule drops the rule with the key; false when there was none.
func (c *Catalog) RemoveRule(key string) bool {
	for i := range c.Quotas {
		if c.Quotas[i].Key() == key {
			c.Quotas = append(c.Quotas[:i], c.Quotas[i+1:]...)
			return true
		}
	}
	return false
}

// dropRules removes every rule the predicate matches.
func (c *Catalog) dropRules(match func(*QuotaRule) bool) {
	kept := c.Quotas[:0]
	for i := range c.Quotas {
		if !match(&c.Quotas[i]) {
			kept = append(kept, c.Quotas[i])
		}
	}
	c.Quotas = kept
	if len(c.Quotas) == 0 {
		c.Quotas = nil
	}
}

// validateQuotas checks groups, users and rules against each other and the
// pools and nodes.
func (c *Catalog) validateQuotas() error {
	groups := map[string]bool{}
	for i := range c.Groups {
		g := &c.Groups[i]
		if g.Name == "" {
			return fmt.Errorf("group %d has no name", i)
		}
		if groups[g.Name] {
			return fmt.Errorf("duplicate group %q", g.Name)
		}
		groups[g.Name] = true
	}
	users := map[string]bool{}
	for i := range c.Users {
		u := &c.Users[i]
		if u.Name == "" {
			return fmt.Errorf("user %d has no name", i)
		}
		if users[u.Name] {
			return fmt.Errorf("duplicate user %q", u.Name)
		}
		users[u.Name] = true
		seen := map[string]bool{}
		for _, g := range u.Groups {
			if !groups[g] {
				return fmt.Errorf("user %q is in unknown group %q", u.Name, g)
			}
			if seen[g] {
				return fmt.Errorf("user %q lists group %q twice", u.Name, g)
			}
			seen[g] = true
		}
	}
	nodes := map[string]bool{}
	for _, n := range c.Nodes {
		nodes[n.Name] = true
	}
	keys := map[string]bool{}
	for i := range c.Quotas {
		r := &c.Quotas[i]
		if err := r.validateShape(); err != nil {
			return err
		}
		switch r.Subject {
		case SubjectUser:
			if !users[r.Name] {
				return fmt.Errorf("quota rule %s names unknown user %q", r.Key(), r.Name)
			}
		case SubjectGroup:
			if !groups[r.Name] {
				return fmt.Errorf("quota rule %s names unknown group %q", r.Key(), r.Name)
			}
		}
		switch r.Scope {
		case ScopePool:
			if _, ok := c.Pool(r.Target); !ok {
				return fmt.Errorf("quota rule %s names unknown pool %q", r.Key(), r.Target)
			}
		case ScopeNode:
			if !nodes[r.Target] {
				return fmt.Errorf("quota rule %s names unknown node %q", r.Key(), r.Target)
			}
		}
		if keys[r.Key()] {
			return fmt.Errorf("duplicate quota rule %s", r.Key())
		}
		keys[r.Key()] = true
	}
	return nil
}

// sortRules orders rules canonically: by scope (everywhere, pools, nodes),
// then by subject (everyone, groups, users), each-before-together, by name.
func sortRules(rules []QuotaRule) {
	rank := func(kind string, kinds ...string) int {
		for i, k := range kinds {
			if k == kind {
				return i
			}
		}
		return len(kinds)
	}
	sort.SliceStable(rules, func(i, j int) bool {
		a, b := rules[i], rules[j]
		if ra, rb := rank(a.Scope, ScopeGlobal, ScopePool, ScopeNode), rank(b.Scope, ScopeGlobal, ScopePool, ScopeNode); ra != rb {
			return ra < rb
		}
		if a.Target != b.Target {
			return a.Target < b.Target
		}
		if ra, rb := rank(a.Subject, SubjectGlobal, SubjectGroup, SubjectUser), rank(b.Subject, SubjectGlobal, SubjectGroup, SubjectUser); ra != rb {
			return ra < rb
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return !a.Together && b.Together
	})
}
