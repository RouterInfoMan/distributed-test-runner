package sched

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andrei/distributed-test-platform/internal/backend"
	"github.com/andrei/distributed-test-platform/internal/config"
	"github.com/andrei/distributed-test-platform/internal/model"
	"github.com/andrei/distributed-test-platform/internal/store"
)

// fakeBackend accepts dispatches and lets a test drive each run's phase. It
// places where the scheduler said (run.NodeID) like the Nomad backend does,
// but refuses when that node is full or not in a pool the run may use, so
// tests catch a ledger that over-admits or mis-places.
type fakeBackend struct {
	mu         sync.Mutex
	cfg        *config.Config
	nodes      []backend.NodeInfo
	nodeUse    map[string]int
	dispatched []string
	node       map[string]string // run id -> node id
	phase      map[string]backend.Phase
}

func newFake(cfg *config.Config, nodes ...backend.NodeInfo) *fakeBackend {
	return &fakeBackend{cfg: cfg, nodes: nodes, nodeUse: map[string]int{},
		node: map[string]string{}, phase: map[string]backend.Phase{}}
}

func (f *fakeBackend) Name() string                           { return "fake" }
func (f *fakeBackend) Healthy(context.Context) error          { return nil }
func (f *fakeBackend) Stop(context.Context, *model.Run) error { return nil }

func (f *fakeBackend) Inventory(context.Context) ([]backend.NodeInfo, error) {
	return f.nodes, nil
}

func (f *fakeBackend) Dispatch(_ context.Context, run *model.Run, spec model.RunSpec) (backend.Placement, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pool, _ := f.cfg.Pool(run.Pool)
	for _, n := range f.nodes {
		if n.ID != run.NodeID {
			continue
		}
		if !contains(pool.NodePools(), f.cfg.Catalog().NodePool(n.Name)) {
			return backend.Placement{}, fmt.Errorf("fake: %s placed on %s, which is not in %s", run.Suite, n.Name, run.Pool)
		}
		if f.nodeUse[n.ID] >= n.Slots {
			return backend.Placement{}, fmt.Errorf("fake: %s is full; %s over-admitted", n.Name, run.Suite)
		}
		if spec.Slot != n.Slot {
			return backend.Placement{}, fmt.Errorf("fake: %s asks %s, node %s offers %s", run.Suite, spec.Slot, n.Name, n.Slot)
		}
		f.nodeUse[n.ID]++
		f.dispatched = append(f.dispatched, run.ID)
		f.node[run.ID] = n.ID
		f.phase[run.ID] = backend.PhaseRunning
		return backend.Placement{BackendID: "job-" + run.ID, NodeID: n.ID, NodeName: n.Name}, nil
	}
	return backend.Placement{}, fmt.Errorf("fake: %s has no node chosen (%q)", run.Suite, run.NodeID)
}

func (f *fakeBackend) Poll(_ context.Context, run *model.Run) (backend.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := f.node[run.ID]
	return backend.Status{Phase: f.phase[run.ID], NodeID: n, NodeName: n}, nil
}

func (f *fakeBackend) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.dispatched)
}

// finish reports the i-th dispatched run as passed and frees its slot.
func (f *fakeBackend) finish(t *testing.T, sc *Scheduler, i int) {
	t.Helper()
	f.mu.Lock()
	id := f.dispatched[i]
	f.phase[id] = backend.PhaseComplete
	f.nodeUse[f.node[id]]--
	f.mu.Unlock()
	if err := sc.Ingest(context.Background(), &model.RunEvent{
		RunID: id, Phase: "finished", State: model.RunPassed,
		Summary: model.Summary{Tests: 1, Passed: 1},
	}); err != nil {
		t.Fatal(err)
	}
}

// testNode is a node as the backend reports it plus the pool the catalog
// assigns it to (the backend knows nothing about pools).
type testNode struct {
	backend.NodeInfo
	pool string
}

var testSlot = model.Slot{CPU: 100, Memory: 128, Disk: 128}

func node(id, pool string, slots int) testNode {
	return testNode{NodeInfo: backend.NodeInfo{ID: id, Name: id, Slots: slots, Slot: testSlot, Ready: true, Status: "ready"}, pool: pool}
}

// testConfig is a config plus the catalog the test wants live; the catalog is
// applied by newScheduler, after any quota edits.
type testConfig struct {
	*config.Config
	cat *config.Catalog
}

func baseConfig(t *testing.T, pools ...config.Pool) *testConfig {
	t.Helper()
	cfg := &config.Config{
		StateDir:       t.TempDir(),
		Backend:        "local",
		DefaultTimeout: model.Duration(time.Minute),
	}
	cat := &config.Catalog{Pools: pools}
	for i := range cat.Pools {
		if !cat.Pools[i].IsVirtual() {
			cat.Pools[i].Runtime = model.RuntimeProcess
			cat.Pools[i].CacheDir = t.TempDir()
			cat.Pools[i].Default.Command = []string{"/bin/true"}
		}
	}
	return &testConfig{Config: cfg, cat: cat}
}

func newScheduler(t *testing.T, tc *testConfig, nodes ...testNode) (*Scheduler, *store.Store, *fakeBackend) {
	t.Helper()
	cfg := tc.Config
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	infos := make([]backend.NodeInfo, 0, len(nodes))
	for _, n := range nodes {
		infos = append(infos, n.NodeInfo)
		if n.pool != "" {
			tc.cat.SetNode(config.Node{Name: n.Name, Pool: n.pool})
		}
	}
	if err := cfg.SetCatalog(tc.cat); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	be := newFake(cfg, infos...)
	sc := New(cfg, st, be, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return sc, st, be
}

func testScheduler(t *testing.T, slots int) (*Scheduler, *store.Store, *fakeBackend) {
	t.Helper()
	return newScheduler(t, baseConfig(t, config.Pool{Name: "p"}), node("n1", "p", slots))
}

func submitSuites(t *testing.T, sc *Scheduler, n int, retries int) *model.Regression {
	t.Helper()
	return submitAs(t, sc, "", "p", n, retries)
}

func submitAs(t *testing.T, sc *Scheduler, user, pool string, n int, retries int) *model.Regression {
	t.Helper()
	sub := &model.Submission{Name: "t", User: user, Defaults: model.SuiteDefaults{Pool: pool, Retries: &retries}}
	for i := 0; i < n; i++ {
		sub.Suites = append(sub.Suites, model.SuiteSpec{Name: fmt.Sprintf("%s-%s-%d", user, pool, i)})
	}
	reg, err := sc.Submit(context.Background(), sub)
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

// Admission must never exceed the pool's advertised slot count.
func TestAdmissionRespectsSlots(t *testing.T) {
	sc, _, be := testScheduler(t, 2)
	submitSuites(t, sc, 5, 0)
	ctx := context.Background()

	sc.tick(ctx)
	if got := be.count(); got != 2 {
		t.Fatalf("with 2 slots, want 2 dispatched, got %d", got)
	}
	sc.tick(ctx)
	if got := be.count(); got != 2 {
		t.Fatalf("slots still busy, want 2 dispatched, got %d", got)
	}

	// Free one slot; exactly one more suite may start.
	be.finish(t, sc, 0)
	sc.tick(ctx)
	if got := be.count(); got != 3 {
		t.Fatalf("after one slot freed, want 3 dispatched, got %d", got)
	}
}

// A failed suite is retried up to its limit, and passing on a retry is flaky.
func TestRetryAndFlaky(t *testing.T) {
	sc, st, be := testScheduler(t, 4)
	reg := submitSuites(t, sc, 1, 2)
	ctx := context.Background()

	sc.tick(ctx)
	be.mu.Lock()
	run1 := be.dispatched[0]
	be.nodeUse[be.node[run1]]--
	be.mu.Unlock()
	sc.Ingest(ctx, &model.RunEvent{
		RunID: run1, Phase: "finished", State: model.RunFailed,
		Summary: model.Summary{Tests: 4, Passed: 3, Failed: 1},
	})

	sc.tick(ctx)
	if got := be.count(); got != 2 {
		t.Fatalf("failure should queue a retry, dispatched=%d", got)
	}

	be.mu.Lock()
	run2 := be.dispatched[1]
	be.nodeUse[be.node[run2]]--
	be.mu.Unlock()
	sc.Ingest(ctx, &model.RunEvent{
		RunID: run2, Phase: "finished", State: model.RunPassed,
		Summary: model.Summary{Tests: 4, Passed: 4},
	})
	sc.Rollup(ctx, reg.ID)

	got, runs, _ := st.Regression(reg.ID)
	if got.State != model.RegPassed {
		t.Fatalf("want regression passed, got %s", got.State)
	}
	if got.Totals.Flaky != 1 {
		t.Fatalf("want 1 flaky suite, got %d", got.Totals.Flaky)
	}
	if len(runs) != 2 {
		t.Fatalf("want 2 attempts recorded, got %d", len(runs))
	}
}

// Exhausting retries leaves the regression failed, not stuck.
func TestRetriesExhausted(t *testing.T) {
	sc, st, be := testScheduler(t, 4)
	reg := submitSuites(t, sc, 1, 1)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		sc.tick(ctx)
		be.mu.Lock()
		run := be.dispatched[i]
		be.nodeUse[be.node[run]]--
		be.mu.Unlock()
		sc.Ingest(ctx, &model.RunEvent{
			RunID: run, Phase: "finished", State: model.RunFailed,
			Summary: model.Summary{Tests: 2, Passed: 1, Failed: 1},
		})
	}
	sc.tick(ctx)
	if got := be.count(); got != 2 {
		t.Fatalf("want exactly 2 attempts (1 + 1 retry), got %d", got)
	}
	got, _, _ := st.Regression(reg.ID)
	if got.State != model.RegFailed {
		t.Fatalf("want regression failed, got %s", got.State)
	}
	if got.Totals.Flaky != 0 {
		t.Fatalf("a suite that never passed is not flaky, got %d", got.Totals.Flaky)
	}
}

// A pool named by no node has no capacity; work waits instead of erroring.
func TestUnknownPoolRejectedAtSubmit(t *testing.T) {
	sc, _, _ := testScheduler(t, 1)
	_, err := sc.Submit(context.Background(), &model.Submission{
		Suites: []model.SuiteSpec{{Name: "x", Pool: "nope"}},
	})
	if err == nil {
		t.Fatal("expected submission to a nonexistent pool to be rejected")
	}
}

// A virtual pool admits against the union of its members, and a member sees
// the virtual runs that landed on its nodes.
func TestVirtualPoolLedger(t *testing.T) {
	cfg := baseConfig(t,
		config.Pool{Name: "hp"},
		config.Pool{Name: "mid"},
		config.Pool{Name: "global", Spans: []string{"hp", "mid"}},
	)
	sc, st, be := newScheduler(t, cfg, node("hp1", "hp", 2), node("mid1", "mid", 1))
	ctx := context.Background()

	submitAs(t, sc, "", "global", 5, 0)
	sc.tick(ctx)
	if got := be.count(); got != 3 {
		t.Fatalf("global spans 2+1 slots, want 3 dispatched, got %d", got)
	}
	// Every member is full with global work, so pool-specific work waits.
	submitAs(t, sc, "", "hp", 1, 0)
	submitAs(t, sc, "", "mid", 1, 0)
	sc.tick(ctx)
	if got := be.count(); got != 3 {
		t.Fatalf("members are full, want still 3 dispatched, got %d", got)
	}
	pools := sc.Pools()
	byName := map[string]PoolStatus{}
	for _, p := range pools {
		byName[p.Name] = p
	}
	if byName["global"].Slots != 3 || byName["global"].Used != 3 {
		t.Fatalf("global view: want 3/3, got %d/%d", byName["global"].Used, byName["global"].Slots)
	}
	if byName["hp"].Used != 2 || byName["mid"].Used != 1 {
		t.Fatalf("members must see placed global runs: hp %d/2 mid %d/1", byName["hp"].Used, byName["mid"].Used)
	}
	if len(byName["global"].Nodes) != 0 || byName["global"].Spans[0] != "hp" {
		t.Fatalf("virtual pool lists members, not nodes: %+v", byName["global"])
	}

	// Freeing a mid slot lets exactly one more run in; priority/FIFO picks the
	// oldest queued global run, and the fake proves it fits.
	be.finish(t, sc, 2)
	sc.tick(ctx)
	if got := be.count(); got != 4 {
		t.Fatalf("one slot freed, want 4 dispatched, got %d", got)
	}
	queued := 0
	for _, r := range st.AllRuns() {
		if r.State == model.RunQueued {
			queued++
		}
	}
	if queued != 3 {
		t.Fatalf("want 3 still queued, got %d", queued)
	}
}

// The scheduler chooses the node: the least loaded one in the pool that has
// a slot free; an unassigned node is no capacity at all, and assigning it
// through the catalog makes it usable on the next tick without a restart.
func TestPlacementAndAssignment(t *testing.T) {
	cfg := baseConfig(t, config.Pool{Name: "p"})
	sc, st, be := newScheduler(t, cfg, node("a", "p", 2), node("b", "p", 2), node("c", "", 2))
	ctx := context.Background()

	submitAs(t, sc, "", "p", 6, 0)
	sc.tick(ctx)
	if got := be.count(); got != 4 {
		t.Fatalf("a+b offer 4 slots and c is unassigned, want 4 dispatched, got %d", got)
	}
	perNode := map[string]int{}
	for _, id := range be.dispatched {
		r, _ := st.Run(id)
		perNode[r.NodeName]++
	}
	if perNode["a"] != 2 || perNode["b"] != 2 || perNode["c"] != 0 {
		t.Fatalf("work spreads over the assigned nodes: %v", perNode)
	}

	// Assign c: two more runs start; the same assignment again is a no-op.
	next := sc.cfg.RawCatalog().Clone()
	next.SetNode(config.Node{Name: "c", Pool: "p"})
	if err := sc.ApplyCatalog(next); err != nil {
		t.Fatal(err)
	}
	if err := sc.ApplyCatalog(next.Clone()); err != nil {
		t.Fatalf("re-applying the same catalog must be idempotent: %v", err)
	}
	sc.tick(ctx)
	if got := be.count(); got != 6 {
		t.Fatalf("after assigning c, want 6 dispatched, got %d", got)
	}
	for _, n := range sc.Pools()[0].Nodes {
		if n.Pool != "p" || n.Used != 2 || n.Slot != testSlot {
			t.Fatalf("node view: %+v", n)
		}
	}
	if len(sc.Unassigned()) != 0 {
		t.Fatalf("nothing is unassigned any more: %+v", sc.Unassigned())
	}
	// Unassigning a node stops new work landing there; what runs keeps running.
	next = sc.cfg.RawCatalog().Clone()
	next.SetNode(config.Node{Name: "a"})
	if err := sc.ApplyCatalog(next); err != nil {
		t.Fatal(err)
	}
	be.finish(t, sc, 0) // a run on a ends
	submitAs(t, sc, "", "p", 1, 0)
	sc.tick(ctx)
	if got := be.count(); got != 6 {
		t.Fatalf("a is unassigned and b, c are full: nothing may start, got %d", got)
	}
	if u := sc.Unassigned(); len(u) != 1 || u[0].Name != "a" || u[0].Used != 1 {
		t.Fatalf("a is unassigned but still running one: %+v", u)
	}
}

func rule(subject, name string, together bool, scope, target string, n int) config.QuotaRule {
	return config.QuotaRule{Subject: subject, Name: name, Together: together, Scope: scope, Target: target, MaxSlots: n}
}

func quotaConfig(t *testing.T) *testConfig {
	cfg := baseConfig(t, config.Pool{Name: "p"}, config.Pool{Name: "q"})
	cfg.cat.Groups = []config.Group{{Name: "core"}}
	cfg.cat.Users = []config.User{{Name: "alice", Groups: []string{"core"}}, {Name: "bob", Groups: []string{"core"}}}
	cfg.cat.Quotas = []config.QuotaRule{
		rule("global", "", false, "global", "", 1),    // each user: 1 anywhere
		rule("group", "core", false, "global", "", 2), // each member of core: 2 anywhere
		rule("group", "core", true, "global", "", 3),  // core together: 3
		rule("group", "core", false, "pool", "q", 0),  // core may not use q
	}
	return cfg
}

// Each member of a group is capped by the group's per-user rule, the group
// together by its "together" rule, and everyone else by the everyone rule.
func TestQuotas(t *testing.T) {
	sc, st, be := newScheduler(t, quotaConfig(t), node("n1", "p", 10))
	ctx := context.Background()

	submitAs(t, sc, "alice", "p", 4, 0)
	sc.tick(ctx)
	if got := be.count(); got != 2 {
		t.Fatalf("alice is capped at 2 per user, got %d dispatched", got)
	}
	submitAs(t, sc, "bob", "p", 2, 0)
	sc.tick(ctx)
	if got := be.count(); got != 3 {
		t.Fatalf("core together is capped at 3, got %d dispatched", got)
	}
	submitAs(t, sc, "carol", "p", 2, 0)
	sc.tick(ctx)
	if got := be.count(); got != 4 {
		t.Fatalf("carol falls under the everyone rule of 1, got %d dispatched", got)
	}

	// The blocked runs say why, naming the rule.
	var reasons []string
	for _, r := range st.AllRuns() {
		if r.State == model.RunQueued && r.Message != "" {
			reasons = append(reasons, r.Message)
		}
	}
	if len(reasons) != 4 {
		t.Fatalf("want a waiting reason on every rule-blocked run, got %v", reasons)
	}
	for _, m := range reasons {
		if !strings.Contains(m, "(rule ") {
			t.Fatalf("the reason names the rule: %q", m)
		}
	}

	// Finishing one of alice's frees a group slot: alice's next is older than bob's.
	be.finish(t, sc, 0)
	sc.tick(ctx)
	if got := be.count(); got != 5 {
		t.Fatalf("after a group slot freed, want 5 dispatched, got %d", got)
	}
	run, _ := st.Run(be.dispatched[4])
	if run.User != "alice" {
		t.Fatalf("alice's next run is older than bob's, want alice, got %s", run.User)
	}

	q := sc.Quotas()
	byKey := map[string]RuleStatus{}
	for _, r := range q {
		byKey[r.Key] = r
	}
	if len(q) != 4 || byKey["group:core/together@global"].Used != 3 || byKey["group:core@global"].Used != 3 {
		t.Fatalf("unexpected quota view: %+v", q)
	}
	if us := byKey["group:core@global"].Users; len(us) != 2 || us[0].User != "alice" || us[0].Cap != 2 || us[0].Used != 2 {
		t.Fatalf("a group rule lists its members with their usage: %+v", us)
	}
	var carol, alice *UserStatus
	for i, u := range byKey["global@global"].Users {
		switch u.User {
		case "carol":
			carol = &byKey["global@global"].Users[i]
		case "alice":
			alice = &byKey["global@global"].Users[i]
		}
	}
	if carol == nil || carol.Used != 1 || carol.Queued != 1 || carol.Cap != 1 || carol.Via != "" {
		t.Fatalf("the everyone rule lists active unlisted users: %+v", carol)
	}
	if alice == nil || alice.Cap != 2 || alice.Via != "group:core@global" {
		t.Fatalf("under the everyone rule alice shows the more specific rule that governs her: %+v", alice)
	}
	// A pool a rule gives the user 0 slots in is refused at submission.
	_, err := sc.Submit(ctx, &model.Submission{User: "alice", Suites: []model.SuiteSpec{{Name: "x", Pool: "q"}}})
	if err == nil || !strings.Contains(err.Error(), "group:core@pool:q") {
		t.Fatalf("core has 0 slots in q; submission must be refused naming the rule, got %v", err)
	}
	if _, err := sc.Submit(ctx, &model.Submission{User: "carol", Suites: []model.SuiteSpec{{Name: "x", Pool: "q"}}}); err != nil {
		t.Fatalf("no rule keeps carol out of q: %v", err)
	}
}

// Within one priority level the pick rotates towards the user holding the
// fewest slots, so a big submission does not lock out a small one.
func TestFairShareWithinPriority(t *testing.T) {
	sc, st, be := newScheduler(t, baseConfig(t, config.Pool{Name: "p"}), node("n1", "p", 3))
	ctx := context.Background()

	submitAs(t, sc, "alice", "p", 5, 0)
	time.Sleep(2 * time.Millisecond) // bob's queue entries are strictly newer
	submitAs(t, sc, "bob", "p", 2, 0)
	sc.tick(ctx)

	users := map[string]int{}
	for _, id := range be.dispatched {
		r, _ := st.Run(id)
		users[r.User]++
	}
	if users["alice"] != 2 || users["bob"] != 1 {
		t.Fatalf("want alice 2 / bob 1 with 3 slots, got %v", users)
	}

	// Priority still wins outright: an urgent submission goes before either.
	be.finish(t, sc, 0)
	sub := &model.Submission{Name: "hot", User: "carol", Priority: 90,
		Suites: []model.SuiteSpec{{Name: "urgent", Pool: "p"}}}
	if _, err := sc.Submit(context.Background(), sub); err != nil {
		t.Fatal(err)
	}
	sc.tick(ctx)
	r, _ := st.Run(be.dispatched[3])
	if r.User != "carol" {
		t.Fatalf("priority 90 must be dispatched first, got %s", r.User)
	}
}

// A user in two groups gets the most permissive of their groups' per-user
// caps, is bound by both groups' "together" rules, and their own rule beats
// the groups'.
func TestUserInSeveralGroups(t *testing.T) {
	cfg := baseConfig(t, config.Pool{Name: "p"}, config.Pool{Name: "q"})
	cfg.cat.Groups = []config.Group{{Name: "a"}, {Name: "b"}}
	cfg.cat.Users = []config.User{{Name: "dana", Groups: []string{"a", "b"}}, {Name: "eve", Groups: []string{"a"}}, {Name: "fay", Groups: []string{"a"}}}
	cfg.cat.Quotas = []config.QuotaRule{
		rule("group", "a", false, "global", "", 1),
		rule("group", "a", true, "global", "", 4),
		rule("group", "b", false, "global", "", 3),
		rule("user", "fay", false, "global", "", 2),
	}
	sc, st, be := newScheduler(t, cfg, node("n1", "p", 10), node("n2", "q", 10))
	ctx := context.Background()

	// dana may run 3 at once (max of 1 and 3)…
	submitAs(t, sc, "dana", "q", 4, 0)
	sc.tick(ctx)
	if got := be.count(); got != 3 {
		t.Fatalf("dana's cap is the most permissive (3), got %d dispatched", got)
	}
	// …and her runs count toward group a's total (3 of 4): eve (capped at 1)
	// gets the last a slot, then waits.
	submitAs(t, sc, "eve", "p", 2, 0)
	sc.tick(ctx)
	if got := be.count(); got != 4 {
		t.Fatalf("eve takes group a's last slot, got %d dispatched", got)
	}
	be.finish(t, sc, 3)
	sc.tick(ctx)
	if got := be.count(); got != 5 {
		t.Fatalf("eve's second run fits once her first ended, got %d", got)
	}
	reg, _, _ := st.Regression(submitAs(t, sc, "dana", "p", 1, 0).ID)
	if len(reg.Groups) != 2 || reg.Groups[0] != "a" || reg.Groups[1] != "b" {
		t.Fatalf("the regression records the user's groups: %v", reg.Groups)
	}
	// fay's own rule (2) beats a's per-user rule (1); a's total (4) still binds.
	be.finish(t, sc, 0)
	be.finish(t, sc, 1)
	be.finish(t, sc, 2)
	be.finish(t, sc, 4)
	submitAs(t, sc, "fay", "p", 3, 0)
	sc.tick(ctx)
	fays := 0
	for _, id := range be.dispatched {
		if r, _ := st.Run(id); r.User == "fay" {
			fays++
		}
	}
	if fays != 2 {
		t.Fatalf("fay's own rule allows 2, got %d of hers dispatched", fays)
	}
}

// Rules scoped to a pool or a node are checked against where the run would
// land: a pool rule sees work in a virtual pool that spans it (and vice
// versa), and a node rule steers the pick to another node before it makes
// the run wait.
func TestScopedRules(t *testing.T) {
	cfg := baseConfig(t, config.Pool{Name: "hp"}, config.Pool{Name: "mid"}, config.Pool{Name: "global", Spans: []string{"hp", "mid"}})
	cfg.cat.Users = []config.User{{Name: "alice"}}
	cfg.cat.Nodes = []config.Node{{Name: "hp1", Pool: "hp"}, {Name: "mid1", Pool: "mid"}}
	cfg.cat.Quotas = []config.QuotaRule{
		rule("user", "alice", false, "pool", "hp", 1),  // alice: 1 slot in hp, however she gets there
		rule("global", "", true, "node", "mid1", 2),    // mid1: 2 of its 4 slots for everyone together
		rule("global", "", false, "pool", "global", 5), // each user: 5 across the span
	}
	sc, st, be := newScheduler(t, cfg, node("hp1", "hp", 4), node("mid1", "mid", 4))
	ctx := context.Background()

	submitAs(t, sc, "alice", "global", 6, 0)
	sc.tick(ctx)
	// alice: 1 on hp1 (her hp rule), 2 on mid1 (the node rule) = 3 running.
	if got := be.count(); got != 3 {
		t.Fatalf("want 3 dispatched (1 on hp1 + 2 on mid1), got %d", got)
	}
	perNode := map[string]int{}
	for _, id := range be.dispatched {
		r, _ := st.Run(id)
		perNode[r.NodeName]++
	}
	if perNode["hp1"] != 1 || perNode["mid1"] != 2 {
		t.Fatalf("placement under scoped rules: %v", perNode)
	}
	// Direct work in hp is also alice's hp work: refused nowhere at submit
	// (the rule is 1, not 0) but it waits, saying which rule.
	submitAs(t, sc, "alice", "hp", 1, 0)
	sc.tick(ctx)
	if got := be.count(); got != 3 {
		t.Fatalf("alice's hp rule is full, got %d dispatched", got)
	}
	waiting := 0
	for _, r := range st.AllRuns() {
		if r.State == model.RunQueued && strings.Contains(r.Message, "user:alice@pool:hp") {
			waiting++
		}
	}
	if waiting == 0 {
		t.Fatal("a run blocked by a node/pool rule must say so")
	}
	// bob is not bound by alice's rule but is by the node rule: hp1 has room.
	submitAs(t, sc, "bob", "global", 4, 0)
	sc.tick(ctx)
	if got := be.count(); got != 6 {
		t.Fatalf("bob gets hp1's 3 free slots and nothing on the capped mid1, got %d dispatched", got)
	}
	q := sc.Quotas()
	for _, r := range q {
		if r.Key == "global/together@node:mid1" && (r.Used != 2 || r.MaxSlots != 2) {
			t.Fatalf("node rule usage: %+v", r)
		}
		// In use: her global-pool run that landed on hp1. Queued: the hp run
		// plus the 3 global-pool runs that could land there.
		if r.Key == "user:alice@pool:hp" && (r.Users[0].Used != 1 || r.Users[0].Queued != 4) {
			t.Fatalf("alice's hp usage counts her global-pool run on hp1 and what could still land there: %+v", r.Users)
		}
	}
}
