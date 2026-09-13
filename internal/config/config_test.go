package config

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/andrei/distributed-test-platform/internal/model"
)

func catalogOf(t *testing.T, body string) (*Catalog, error) {
	t.Helper()
	var cat Catalog
	if err := json.Unmarshal([]byte(body), &cat); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.Normalize(); err != nil {
		return nil, err
	}
	c := Default()
	if err := c.SetCatalog(&cat); err != nil {
		return nil, err
	}
	return c.Catalog(), nil
}

const members = `
  {"name":"hp","runtime":"container","task_driver":"docker",
   "cache_dir":"/c","docker_network":"net","default":{"image":"img","command":["/run"]}},
  {"name":"mid","runtime":"container","task_driver":"docker",
   "cache_dir":"/c","docker_network":"net","default":{"image":"img","command":["/run"]}}`

// A virtual pool needs only a name and members; everything a job needs is
// inherited from the first member - in the live catalog only, never in the
// stored (raw) one.
func TestVirtualPoolInherits(t *testing.T) {
	live, err := catalogOf(t, `{"pools":[`+members+`,{"name":"global","spans":["hp","mid"]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	g, _ := live.Pool("global")
	if !g.IsVirtual() || g.Runtime != model.RuntimeContainer || g.Driver() != "docker" ||
		g.Default.Image != "img" || g.DockerNetwork != "net" || g.CacheDir != "/c" || len(g.Default.Command) != 1 {
		t.Fatalf("virtual pool did not inherit from its first member: %+v", g)
	}
	if got := strings.Join(g.NodePools(), ","); got != "hp,mid" {
		t.Fatalf("NodePools = %s", got)
	}
	raw := &Catalog{Pools: []Pool{{Name: "global", Spans: []string{"hp"}}, {Name: "hp", Runtime: model.RuntimeContainer, Default: struct {
		Image   string   `json:"image,omitempty"`
		Command []string `json:"command,omitempty"`
	}{Image: "img"}}}}
	if _, err := raw.Normalize(); err != nil {
		t.Fatal(err)
	}
	if raw.Pools[0].Runtime != "" || raw.Pools[0].Default.Image != "" {
		t.Fatalf("Normalize must not touch the authored catalog: %+v", raw.Pools[0])
	}
}

// What a virtual pool cannot paper over is rejected up front.
func TestVirtualPoolRejectsMismatch(t *testing.T) {
	cases := map[string]string{
		"unknown member": `{"name":"global","spans":["hp","nope"]}`,
		"virtual member": `{"name":"g1","spans":["hp"]},{"name":"g2","spans":["g1"]}`,
		"mixed runtime":  `{"name":"dev","runtime":"process"},{"name":"global","spans":["hp","dev"]}`,
		"duplicate name": `{"name":"hp","runtime":"container"}`,
	}
	for name, extra := range cases {
		if _, err := catalogOf(t, `{"pools":[`+members+`,`+extra+`]}`); err == nil {
			t.Errorf("%s: expected a validation error", name)
		}
	}
}

// Nodes are assigned to concrete pools in the catalog; a virtual pool sees
// the nodes of everything it spans. Dropping a pool unassigns its nodes and
// removes it from spans and allowlists instead of leaving dangling names.
func TestNodeAssignment(t *testing.T) {
	live, err := catalogOf(t, `{"pools":[`+members+`,{"name":"global","spans":["hp","mid"]}],
	  "nodes":[{"name":"n1","pool":"hp"},{"name":"n2","pool":"mid"},{"name":"n3"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if live.NodePool("n1") != "hp" || live.NodePool("n3") != "" || live.NodePool("nope") != "" {
		t.Fatalf("NodePool: %+v", live.Nodes)
	}
	if got := strings.Join(live.PoolNodes("global"), ","); got != "n1,n2" {
		t.Fatalf("a virtual pool spans its members' nodes, got %q", got)
	}
	if got := strings.Join(live.PoolNodes("hp"), ","); got != "n1" {
		t.Fatalf("PoolNodes(hp) = %q", got)
	}
	for name, bad := range map[string]string{
		"unknown pool":   `"nodes":[{"name":"n1","pool":"nope"}]`,
		"virtual pool":   `"nodes":[{"name":"n1","pool":"global"}]`,
		"duplicate node": `"nodes":[{"name":"n1","pool":"hp"},{"name":"n1"}]`,
	} {
		if _, err := catalogOf(t, `{"pools":[`+members+`,{"name":"global","spans":["hp","mid"]}],`+bad+`}`); err == nil {
			t.Errorf("%s: expected a validation error", name)
		}
	}
	raw := live.Clone()
	raw.SetNode(Node{Name: "n3", Pool: "mid"})
	raw.SetNode(Node{Name: "n3", Pool: "mid"}) // idempotent: no duplicate row
	if got := strings.Join(raw.PoolNodes("mid"), ","); got != "n2,n3" || len(raw.Nodes) != 3 {
		t.Fatalf("SetNode: %+v", raw.Nodes)
	}
	raw.SetRule(QuotaRule{Subject: "global", Scope: "pool", Target: "mid", MaxSlots: 3})
	raw.SetRule(QuotaRule{Subject: "global", Scope: "pool", Target: "hp", MaxSlots: 3})
	if !raw.RemovePool("mid") || raw.RemovePool("mid") {
		t.Fatal("RemovePool must report whether it removed something")
	}
	gl, _ := raw.Pool("global")
	if raw.NodePool("n2") != "" || raw.NodePool("n3") != "" || strings.Join(gl.Spans, ",") != "hp" || len(raw.Quotas) != 1 || raw.Quotas[0].Target != "hp" {
		t.Fatalf("removing a pool must unassign its nodes and drop it from spans and rules: nodes=%+v rules=%+v global=%+v", raw.Nodes, raw.Quotas, gl.Spans)
	}
	if _, err := raw.Normalize(); err != nil {
		t.Fatalf("the catalog must stay valid after a pool removal: %v", err)
	}
	if !raw.RemoveNode("n1") || raw.RemoveNode("n1") || len(raw.Nodes) != 2 {
		t.Fatalf("RemoveNode: %+v", raw.Nodes)
	}
}

// An empty catalog is valid: the master runs with no pools until the store
// provides some, and a bad replacement leaves the previous catalog live.
func TestEmptyAndRejectedCatalog(t *testing.T) {
	c := Default()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(c.Catalog().Pools) != 0 {
		t.Fatalf("a fresh config must start with no pools, got %d", len(c.Catalog().Pools))
	}
	var good Catalog
	json.Unmarshal([]byte(`{"pools":[`+members+`]}`), &good)
	if err := c.SetCatalog(&good); err != nil {
		t.Fatal(err)
	}
	bad := good.Clone()
	bad.SetPool(Pool{Name: "global", Spans: []string{"nope"}})
	if err := c.SetCatalog(bad); err == nil {
		t.Fatal("expected the bad catalog to be rejected")
	}
	if len(c.Catalog().Pools) != 2 {
		t.Fatalf("the previous catalog must stay live after a rejection, got %d pools", len(c.Catalog().Pools))
	}
}

func TestQuotaRuleKeys(t *testing.T) {
	for _, r := range []QuotaRule{
		{Subject: "global", Scope: "global", MaxSlots: 6},
		{Subject: "global", Together: true, Scope: "node", Target: "rcp-hp-1", MaxSlots: 4},
		{Subject: "user", Name: "carol", Scope: "global", MaxSlots: 2},
		{Subject: "group", Name: "release", Scope: "pool", Target: "hp", MaxSlots: 4},
		{Subject: "group", Name: "core-devs", Together: true, Scope: "global", MaxSlots: 8},
	} {
		back, err := ParseRuleKey(r.Key())
		if err != nil {
			t.Fatalf("%s: %v", r.Key(), err)
		}
		if back.Key() != r.Key() {
			t.Fatalf("key round trip: %s -> %s", r.Key(), back.Key())
		}
	}
	if k := (&QuotaRule{Subject: "group", Name: "core-devs", Together: true, Scope: "global"}).Key(); k != "group:core-devs/together@global" {
		t.Fatalf("key format: %s", k)
	}
	for _, bad := range []string{"carol", "user@global", "user:carol/together@global", "team:x@global", "user:carol@pool", "global@rack:1"} {
		if _, err := ParseRuleKey(bad); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
	if d := (&QuotaRule{Subject: "group", Name: "release", Scope: "pool", Target: "hp"}).Describe(); d != "each member of release in pool hp" {
		t.Fatalf("describe: %s", d)
	}
}

const lab = `{"pools":[` + members + `],
  "nodes":[{"name":"n1","pool":"hp"}],
  "groups":[{"name":"core"},{"name":"wide"}],
  "users":[{"name":"alice","groups":["core"]},{"name":"dana","groups":["core","wide"]},{"name":"carol","groups":["core"]},{"name":"zed"}],
  "quotas":[
    {"subject":"global","scope":"global","max_slots":2},
    {"subject":"global","together":true,"scope":"node","target":"n1","max_slots":5},
    {"subject":"group","name":"core","scope":"global","max_slots":4},
    {"subject":"group","name":"core","together":true,"scope":"global","max_slots":6},
    {"subject":"group","name":"core","scope":"pool","target":"mid","max_slots":0},
    {"subject":"group","name":"wide","scope":"global","max_slots":9},
    {"subject":"user","name":"carol","scope":"global","max_slots":1}
  ]}`

// The per-user cap that governs a user in a scope is the most specific rule:
// their own, else the most permissive of their groups', else the everyone
// rule; "together" rules apply on top; scopes are independent.
func TestQuotaResolution(t *testing.T) {
	live, err := catalogOf(t, lab)
	if err != nil {
		t.Fatal(err)
	}
	cap := func(user string, scope Scope) int {
		if r := live.UserCap(user, scope); r != nil {
			return r.MaxSlots
		}
		return -1
	}
	if cap("alice", Everywhere) != 4 || cap("dana", Everywhere) != 9 || cap("carol", Everywhere) != 1 || cap("zed", Everywhere) != 2 || cap("nobody", Everywhere) != 2 {
		t.Fatalf("anywhere caps: alice %d dana %d carol %d zed %d nobody %d", cap("alice", Everywhere), cap("dana", Everywhere), cap("carol", Everywhere), cap("zed", Everywhere), cap("nobody", Everywhere))
	}
	if cap("alice", PoolScope("mid")) != 0 || cap("zed", PoolScope("mid")) != -1 || cap("alice", PoolScope("hp")) != -1 {
		t.Fatalf("pool caps: alice/mid %d zed/mid %d alice/hp %d", cap("alice", PoolScope("mid")), cap("zed", PoolScope("mid")), cap("alice", PoolScope("hp")))
	}
	if f := live.Forbidden("alice", PoolScope("mid")); f == nil || f.Key() != "group:core@pool:mid" {
		t.Fatalf("a 0 rule forbids: %+v", f)
	}
	if live.Forbidden("zed", PoolScope("mid")) != nil || live.Forbidden("carol", Everywhere) != nil {
		t.Fatal("a positive cap or no rule does not forbid")
	}
	keys := func(rs []*QuotaRule) string {
		var out []string
		for _, r := range rs {
			out = append(out, r.Key())
		}
		return strings.Join(out, " ")
	}
	if got := keys(live.TogetherRules("alice", Everywhere)); got != "group:core/together@global" {
		t.Fatalf("alice's together rules anywhere: %q", got)
	}
	if got := keys(live.TogetherRules("zed", NodeScope("n1"))); got != "global/together@node:n1" {
		t.Fatalf("everyone-together applies to anyone on n1: %q", got)
	}
	if got := keys(live.TogetherRules("zed", Everywhere)); got != "" {
		t.Fatalf("zed is in no group: %q", got)
	}
	if got := strings.Join(live.Members("core"), ","); got != "alice,carol,dana" {
		t.Fatalf("members of core: %s", got)
	}
	if got := strings.Join(live.UserGroups("dana"), ","); got != "core,wide" || live.UserGroups("nobody") != nil {
		t.Fatalf("UserGroups: %s", got)
	}
}

func TestQuotaValidation(t *testing.T) {
	for name, bad := range map[string]string{
		"unknown group":  `"users":[{"name":"x","groups":["nope"]}]`,
		"duplicate user": `"users":[{"name":"x"},{"name":"x"}]`,
		"dup group":      `"groups":[{"name":"a"},{"name":"a"}]`,
		"unknown user":   `"quotas":[{"subject":"user","name":"nope","scope":"global","max_slots":1}]`,
		"unknown rgroup": `"quotas":[{"subject":"group","name":"nope","scope":"global","max_slots":1}]`,
		"unknown pool":   `"quotas":[{"subject":"global","scope":"pool","target":"nope","max_slots":1}]`,
		"unknown node":   `"quotas":[{"subject":"global","scope":"node","target":"nope","max_slots":1}]`,
		"user together":  `"users":[{"name":"x"}],"quotas":[{"subject":"user","name":"x","together":true,"scope":"global","max_slots":1}]`,
		"negative":       `"quotas":[{"subject":"global","scope":"global","max_slots":-1}]`,
		"bad subject":    `"quotas":[{"subject":"team","scope":"global","max_slots":1}]`,
		"named global":   `"quotas":[{"subject":"global","name":"x","scope":"global","max_slots":1}]`,
		"duplicate rule": `"quotas":[{"subject":"global","scope":"global","max_slots":1},{"subject":"global","scope":"global","max_slots":2}]`,
	} {
		if _, err := catalogOf(t, `{"pools":[`+members+`],`+bad+`}`); err == nil {
			t.Errorf("%s: expected a validation error", name)
		}
	}
	live, _ := catalogOf(t, `{"pools":[`+members+`]}`)
	if live.UserCap("anyone", Everywhere) != nil || live.TogetherRules("anyone", Everywhere) != nil {
		t.Fatal("no rules means no limits")
	}
}

// Removing a group, user, pool or node takes its rules (and memberships)
// with it, so the catalog stays valid.
func TestRemovalsDropRules(t *testing.T) {
	live, err := catalogOf(t, lab)
	if err != nil {
		t.Fatal(err)
	}
	c := live.Clone()
	if !c.RemoveGroup("core") || c.RemoveGroup("core") {
		t.Fatal("RemoveGroup must report whether it removed something")
	}
	if u, _ := c.User("dana"); strings.Join(u.Groups, ",") != "wide" {
		t.Fatalf("membership must go with the group: %v", u.Groups)
	}
	if _, ok := c.Rule("group:core/together@global"); ok {
		t.Fatal("the group's rules must go with it")
	}
	c.RemoveUser("carol")
	c.RemoveNode("n1")
	c.RemovePool("mid")
	if len(c.Quotas) != 2 {
		t.Fatalf("want the everyone rule and wide's rule left, got %+v", c.Quotas)
	}
	if _, err := c.Normalize(); err != nil {
		t.Fatalf("catalog must stay valid after removals: %v", err)
	}
	c.SetRule(QuotaRule{Subject: "global", Scope: "global", MaxSlots: 3})
	c.SetRule(QuotaRule{Subject: "global", Scope: "global", MaxSlots: 3})
	if r, _ := c.Rule("global@global"); r.MaxSlots != 3 || len(c.Quotas) != 2 {
		t.Fatalf("SetRule replaces by key: %+v", c.Quotas)
	}
	if !c.RemoveRule("global@global") || c.RemoveRule("global@global") {
		t.Fatal("RemoveRule must report whether it removed something")
	}
}
