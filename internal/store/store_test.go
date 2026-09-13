package store

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/andrei/distributed-test-platform/internal/config"
	"github.com/andrei/distributed-test-platform/internal/model"
)

// roundTrip creates, mutates and reopens through a backend factory, checking
// that everything the scheduler relies on survives a restart.
func roundTrip(t *testing.T, open func() Backend) {
	t.Helper()
	st, err := OpenWith(open(), nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	reg := &model.Regression{ID: "reg-1", Name: "n", User: "alice", Groups: []string{"core"}, Priority: 5,
		State: model.RegPending, SubmittedAt: now,
		Suites: []model.SuiteSpec{{Name: "s1", Pool: "p", Env: map[string]string{"K": "v"}}}}
	run := &model.Run{ID: "run-1", RegressionID: "reg-1", Suite: "s1", Attempt: 1, MaxAttempts: 2,
		Pool: "p", State: model.RunQueued, User: "alice", Groups: []string{"core"}, Priority: 5, QueuedAt: now,
		Token: "tok-1", Spec: reg.Suites[0]}
	if err := st.Create(reg, []*model.Run{run}); err != nil {
		t.Fatal(err)
	}
	if err := st.Create(reg, nil); err == nil {
		t.Fatal("duplicate regression id must be refused")
	}
	st.UpdateRun("run-1", func(r *model.Run) {
		r.State = model.RunRunning
		r.NodeName = "n1"
		r.StartedAt = &now
		r.Summary = model.Summary{Tests: 3, Passed: 2, Failed: 1}
		r.Cases = []model.Case{{Class: "C", Name: "m", Status: "failed", Message: "boom"}}
	})
	st.AddRun(&model.Run{ID: "run-2", RegressionID: "reg-1", Suite: "s1", Attempt: 2, MaxAttempts: 2,
		Pool: "p", State: model.RunQueued, QueuedAt: now.Add(time.Second), Token: "tok-2"})
	st.UpdateRegression("reg-1", func(r *model.Regression, runs []*model.Run) {
		r.State = model.RegRunning
		r.StartedAt = &now
		r.Totals.Suites = 1
	})
	st.Close()

	// Reopen: the mid-flight run is re-queued, everything else is intact.
	st, err = OpenWith(open(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	got, runs, ok := st.Regression("reg-1")
	if !ok {
		t.Fatal("regression not reloaded")
	}
	if got.User != "alice" || len(got.Groups) != 1 || got.State != model.RegRunning || got.Totals.Suites != 1 ||
		got.StartedAt == nil || !got.StartedAt.Equal(now) || got.Suites[0].Env["K"] != "v" {
		t.Fatalf("regression not intact: %+v", got)
	}
	if len(runs) != 2 {
		t.Fatalf("want 2 runs, got %d", len(runs))
	}
	r1, _ := st.Run("run-1")
	if r1.State != model.RunQueued || r1.StartedAt != nil {
		t.Fatalf("a run left running must be re-queued on restart, got %s", r1.State)
	}
	if r1.NodeName != "n1" || r1.Summary.Failed != 1 || len(r1.Cases) != 1 || r1.Cases[0].Message != "boom" {
		t.Fatalf("run detail lost: %+v", r1)
	}
	if r1.Spec.Env["K"] != "v" {
		t.Fatalf("spec not restored from the regression: %+v", r1.Spec)
	}
	if byTok, ok := st.RunByToken("tok-2"); !ok || byTok.ID != "run-2" {
		t.Fatalf("token index not restored")
	}
	if all := st.AllRuns(); len(all) != 2 || all[0].ID != "run-1" {
		t.Fatalf("AllRuns order/content: %v", all)
	}
}

func TestFileBackendRoundTrip(t *testing.T) {
	dir := t.TempDir()
	roundTrip(t, func() Backend {
		b, err := NewFileBackend(dir)
		if err != nil {
			t.Fatal(err)
		}
		return b
	})
}

// DTP_TEST_PG_DSN=postgres://… go test ./internal/store
//
// Point it at a throwaway database: these tests delete rows and leave their
// own behind.
func TestPostgresBackendRoundTrip(t *testing.T) {
	dsn := os.Getenv("DTP_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("DTP_TEST_PG_DSN not set")
	}
	ctx := context.Background()
	b, err := NewPostgresBackend(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	pb := b.(*pgBackend)
	// Start from a clean slate and leave one behind.
	for _, q := range []string{`DELETE FROM runs`, `DELETE FROM regressions`} {
		if err := pb.exec(q); err != nil {
			t.Fatal(err)
		}
	}
	b.Close()
	roundTrip(t, func() Backend {
		b, err := NewPostgresBackend(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		return b
	})
	// The queue is a plain query.
	b, _ = NewPostgresBackend(ctx, dsn)
	defer b.Close()
	res, err := b.(*pgBackend).conn.Query(ctx, `SELECT id, user_name, pool FROM run_queue`)
	if err != nil {
		t.Fatal(err)
	}
	// run-1 was persisted as running; the store re-queues on load but does not
	// write that back until the scheduler touches it, so one row is queued.
	if len(res.Rows) != 1 || *res.Rows[0][0] != "run-2" {
		t.Fatalf("queue query: %+v", res.Rows)
	}
}

func sampleCatalog() *config.Catalog {
	cat := &config.Catalog{Pools: []config.Pool{
		{Name: "hp", Description: "fast", Runtime: model.RuntimeContainer, TaskDriver: "docker",
			DockerNetwork: "dtp_default", CacheDir: "/tmp/cache", Constraints: map[string]string{"os": "linux"}},
		{Name: "dev", Runtime: model.RuntimeProcess, TaskDriver: "raw_exec",
			RunnerCommand: []string{"/usr/local/bin/dtp-runner"}},
		{Name: "global", Spans: []string{"hp"}},
	}}
	cat.Nodes = []config.Node{{Name: "rcp-hp-1", Pool: "hp"}, {Name: "rcp-dev-1", Pool: "dev"}, {Name: "spare"}}
	cat.Pools[0].Default.Image = "dtp/rcp-runner:dev"
	cat.Pools[0].Default.Command = []string{"/opt/dtp/run-suite.sh"}
	cat.Pools[1].Default.Command = []string{"/opt/dtp/run-suite.sh"}
	cat.Groups = []config.Group{{Name: "core", Description: "the core team"}, {Name: "release"}}
	cat.Users = []config.User{
		{Name: "alice", DisplayName: "Alice", Email: "alice@example.com", Groups: []string{"core"}},
		{Name: "bob", Groups: []string{"core", "release"}},
		{Name: "jenkins", Groups: []string{"release"}},
		{Name: "guest"},
	}
	cat.Quotas = []config.QuotaRule{
		{Subject: "global", Scope: "global", MaxSlots: 4, Note: "each user, anywhere"},
		{Subject: "global", Together: true, Scope: "node", Target: "rcp-hp-1", MaxSlots: 3},
		{Subject: "group", Name: "core", Scope: "global", MaxSlots: 4},
		{Subject: "group", Name: "core", Together: true, Scope: "global", MaxSlots: 6},
		{Subject: "group", Name: "release", Scope: "pool", Target: "hp", MaxSlots: 8},
		{Subject: "group", Name: "release", Scope: "pool", Target: "dev", MaxSlots: 0},
		{Subject: "user", Name: "bob", Scope: "global", MaxSlots: 2},
		{Subject: "user", Name: "guest", Scope: "pool", Target: "global", MaxSlots: 1},
	}
	return cat
}

// catalogRoundTrip stores a catalog as authored and reads it back unchanged:
// no defaults filled in, no inherited values, same order.
func catalogRoundTrip(t *testing.T, be Backend) {
	t.Helper()
	if got, err := be.LoadCatalog(); err != nil || got != nil {
		t.Fatalf("an empty store must report no catalog, got %v %v", got, err)
	}
	want := sampleCatalog()
	if err := be.SaveCatalog(want); err != nil {
		t.Fatal(err)
	}
	got, err := be.LoadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(want) {
		a, _ := json.MarshalIndent(want, "", " ")
		b, _ := json.MarshalIndent(got, "", " ")
		t.Fatalf("catalog changed in the round trip:\nwant %s\ngot  %s", a, b)
	}
	if _, err := got.Normalize(); err != nil {
		t.Fatalf("stored catalog must still validate: %v", err)
	}
	// A second save replaces, not appends.
	want.RemovePool("dev")
	want.RemoveGroup("release")
	if err := be.SaveCatalog(want); err != nil {
		t.Fatal(err)
	}
	got, _ = be.LoadCatalog()
	if len(got.Pools) != 2 || len(got.Groups) != 1 {
		t.Fatalf("save must replace: %+v", got)
	}
	// Removing the pool unassigned its node; the node itself stays known.
	if got.NodePool("rcp-dev-1") != "" || len(got.Nodes) != 3 {
		t.Fatalf("nodes after removing dev: %+v", got.Nodes)
	}
	// Removing a group removed its memberships and rules; the users stayed;
	// the pool's rules went with the pool.
	if u, ok := got.User("bob"); !ok || strings.Join(u.Groups, ",") != "core" || len(got.Users) != 4 {
		t.Fatalf("memberships after removing release: %+v", got.Users)
	}
	var keys []string
	for _, r := range got.Clone().Quotas { // canonical order
		keys = append(keys, r.Key())
	}
	if strings.Join(keys, " ") != "global@global group:core@global group:core/together@global user:bob@global user:guest@pool:global global/together@node:rcp-hp-1" {
		t.Fatalf("rules after the removals: %v", keys)
	}
}

func TestFileCatalogRoundTrip(t *testing.T) {
	be, err := NewFileBackend(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	catalogRoundTrip(t, be)
}

func TestPostgresCatalogRoundTrip(t *testing.T) {
	dsn := os.Getenv("DTP_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("DTP_TEST_PG_DSN not set")
	}
	ctx := context.Background()
	b, err := NewPostgresBackend(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	pb := b.(*pgBackend)
	for _, q := range []string{`DELETE FROM quota_rules`, `DELETE FROM group_members`, `DELETE FROM users`, `DELETE FROM groups`,
		`DELETE FROM nodes`, `DELETE FROM pool_members`, `DELETE FROM pools`} {
		if err := pb.exec(q); err != nil {
			t.Fatal(err)
		}
	}
	catalogRoundTrip(t, b)

	// The whole point: an edit made in SQL is what the master reads next.
	for _, q := range []string{
		`UPDATE pools SET description = 'edited in sql' WHERE name = 'hp'`,
		`UPDATE nodes SET pool_name = 'hp' WHERE name = 'spare'`,
		`INSERT INTO users (name, display_name) VALUES ('carol', 'Carol')`,
		`INSERT INTO group_members (group_name, user_name) VALUES ('core', 'carol')`,
		`INSERT INTO quota_rules (subject_kind, user_name, scope_kind, pool_name, max_slots, note) VALUES ('user', 'carol', 'pool', 'global', 3, 'sql')`,
		`UPDATE quota_rules SET max_slots = 5 WHERE subject_kind = 'global' AND scope_kind = 'global'`,
	} {
		if err := pb.exec(q); err != nil {
			t.Fatal(err)
		}
	}
	got, err := b.LoadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	hp, _ := got.Pool("hp")
	carol := got.UserCap("carol", config.PoolScope("global"))
	everyone := got.UserCap("nobody", config.Everywhere)
	if hp.Description != "edited in sql" || got.NodePool("spare") != "hp" || strings.Join(got.UserGroups("carol"), ",") != "core" ||
		len(got.Members("core")) != 3 || carol == nil || carol.MaxSlots != 3 || carol.Note != "sql" || everyone == nil || everyone.MaxSlots != 5 {
		t.Fatalf("SQL edits not reflected: hp=%+v spare=%q carol=%+v everyone=%+v members=%v", hp, got.NodePool("spare"), carol, everyone, got.Members("core"))
	}
	// Saves are upserts: created_at survives a save, updated_at moves only for
	// rows whose values changed.
	before, _ := pb.conn.Query(ctx, `SELECT name, created_at, updated_at FROM pools ORDER BY name`)
	cat, _ := b.LoadCatalog()
	hp2, _ := cat.Pool("hp")
	hp2.Description = "edited"
	if err := b.SaveCatalog(cat); err != nil {
		t.Fatal(err)
	}
	after, _ := pb.conn.Query(ctx, `SELECT name, created_at, updated_at FROM pools ORDER BY name`)
	for i := range before.Rows {
		name, c0, u0 := *before.Rows[i][0], *before.Rows[i][1], *before.Rows[i][2]
		c1, u1 := *after.Rows[i][1], *after.Rows[i][2]
		if c1 != c0 {
			t.Fatalf("%s: created_at must survive a save (%s -> %s)", name, c0, c1)
		}
		if (name == "hp") != (u1 != u0) {
			t.Fatalf("%s: updated_at must move only when the row changed (before %s, after %s)", name, u0, u1)
		}
	}

	// The constraints the schema promises hold.
	if err := pb.exec(`INSERT INTO group_members (group_name, user_name) VALUES ('core', 'nobody')`); err == nil {
		t.Fatal("a membership for an unknown user must be refused by the FK")
	}
	if err := pb.exec(`UPDATE runs SET state = 'flying' WHERE false`); err == nil {
		t.Fatal("an unknown run state must be refused by the enum")
	}
	if err := pb.exec(`UPDATE nodes SET pool_name = 'nope' WHERE name = 'spare'`); err == nil {
		t.Fatal("assigning a node to an unknown pool must be refused by the FK")
	}
	if err := pb.exec(`UPDATE quota_rules SET max_slots = -1 WHERE user_name = 'carol'`); err == nil {
		t.Fatal("a negative limit must be refused by the CHECK")
	}
	if err := pb.exec(`INSERT INTO quota_rules (subject_kind, user_name, together, scope_kind, max_slots) VALUES ('user', 'carol', true, 'global', 1)`); err == nil {
		t.Fatal("\"together\" on a user rule must be refused by the CHECK")
	}
	if err := pb.exec(`INSERT INTO quota_rules (subject_kind, group_name, scope_kind, max_slots) VALUES ('user', 'core', 'global', 1)`); err == nil {
		t.Fatal("a user rule naming a group must be refused by the CHECK")
	}
	if err := pb.exec(`INSERT INTO quota_rules (subject_kind, scope_kind, max_slots) VALUES ('global', 'global', 9)`); err == nil {
		t.Fatal("a second everyone-anywhere rule must be refused by the UNIQUE constraint")
	}
	if err := pb.exec(`SELECT 1`); err != nil {
		t.Fatalf("connection must survive refused statements: %v", err)
	}
}
