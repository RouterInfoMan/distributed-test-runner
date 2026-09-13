package backend

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/andrei/distributed-test-platform/internal/config"
	"github.com/andrei/distributed-test-platform/internal/model"
	"github.com/andrei/distributed-test-platform/internal/nomad"
)

func testCfg() *config.Config {
	c := config.Default()
	c.Backend = "nomad"
	c.Nomad.Datacenters = []string{"dc1"}
	cat := &config.Catalog{}
	cat.Pools = []config.Pool{
		{
			Name: "linux-container", Runtime: model.RuntimeContainer,
			CacheDir: "/var/lib/dtp/cache", DockerNetwork: "dtp_default",
			Constraints: map[string]string{"os": "linux"},
		},
		{
			Name: "linux-process", Runtime: model.RuntimeProcess, TaskDriver: "raw_exec",
			CacheDir:      "/var/lib/dtp/cache",
			RunnerCommand: []string{"/usr/local/bin/dtp-runner"},
		},
	}
	cat.Nodes = []config.Node{{Name: "node-a", Pool: "linux-container"}, {Name: "node-b", Pool: "linux-process"}}
	cat.Pools[0].Default.Image = "dtp/rcp-runner:dev"
	if err := c.Validate(); err != nil {
		panic(err)
	}
	if err := c.SetCatalog(cat); err != nil {
		panic(err)
	}
	return c
}

// nodeSlot is what the chosen node declared (meta.dtp.slot.*); the scheduler
// hands it to Dispatch as spec.Slot.
var nodeSlot = model.Slot{CPU: 500, Memory: 512, Disk: 512}

// run is a run the scheduler has already placed on node-a.
func run(pool string, spec model.SuiteSpec) *model.Run {
	return &model.Run{
		ID: "run-1", RegressionID: "reg-1", Suite: spec.Name, Attempt: 2,
		Pool: pool, Priority: 70, Spec: spec, NodeID: "8f2c1d3e-node-a", NodeName: "node-a",
	}
}

func runSpec(r *model.Run, slot model.Slot) model.RunSpec {
	return model.RunSpec{RunID: r.ID, Suite: r.Suite, Attempt: r.Attempt, Slot: slot}
}

func TestContainerJobSpec(t *testing.T) {
	cfg := testCfg()
	b := NewNomad(cfg)
	r := run("linux-container", model.SuiteSpec{
		Name:     "org.eclipse.egit.ui.test",
		Requires: map[string]string{"rcp_version": "4.30", "display": "xvfb"},
	})
	pool, _ := cfg.Pool("linux-container")

	job, err := b.buildJob(r, runSpec(r, nodeSlot), pool)
	if err != nil {
		t.Fatal(err)
	}
	if job["NodePool"] != "all" {
		t.Fatalf("pools are the master's, not Nomad's: the job runs in Nomad's all pool, got %v", job["NodePool"])
	}
	if job["Type"] != "batch" {
		t.Fatalf("want a batch job, got %v", job["Type"])
	}
	if job["Priority"] != 70 {
		t.Fatalf("submission priority must reach Nomad, got %v", job["Priority"])
	}
	if got := job["ID"].(string); got != "dtp-reg-1-org.eclipse.egit.ui.test-2" {
		t.Fatalf("unexpected job id %q", got)
	}

	group := job["TaskGroups"].([]map[string]any)[0]
	task := group["Tasks"].([]map[string]any)[0]
	if task["Driver"] != "docker" {
		t.Fatalf("container pool must use the docker driver, got %v", task["Driver"])
	}
	tc := task["Config"].(map[string]any)
	if tc["image"] != "dtp/rcp-runner:dev" {
		t.Fatalf("image not applied: %v", tc["image"])
	}
	if tc["network_mode"] != "dtp_default" {
		t.Fatalf("docker network not applied: %v", tc["network_mode"])
	}
	if vols, ok := tc["volumes"].([]string); !ok || vols[0] != "/var/lib/dtp/cache:/var/lib/dtp/cache" {
		t.Fatalf("build cache not mounted: %v", tc["volumes"])
	}

	res := task["Resources"].(map[string]any)
	if res["CPU"] != 500 || res["MemoryMB"] != 512 {
		t.Fatalf("a task must reserve exactly one of the node's slots, got %v", res)
	}
	if disk := group["EphemeralDisk"].(map[string]any)["SizeMB"]; disk != 512 {
		t.Fatalf("ephemeral disk comes from the node's slot, got %v", disk)
	}

	// Pool baseline plus suite requirements as node-meta constraints, and the
	// pin to the node the master chose.
	cons := group["Constraints"].([]map[string]any)
	want := map[string]string{
		"${meta.dtp.display}":     "xvfb",
		"${meta.dtp.os}":          "linux",
		"${meta.dtp.rcp_version}": "4.30",
		"${node.unique.id}":       "8f2c1d3e-node-a",
	}
	if len(cons) != len(want) {
		t.Fatalf("want %d constraints, got %d: %v", len(want), len(cons), cons)
	}
	for _, c := range cons {
		if want[c["LTarget"].(string)] != c["RTarget"] {
			t.Fatalf("unexpected constraint %v", c)
		}
		if c["Operand"] != "=" {
			t.Fatalf("unexpected operand %v", c["Operand"])
		}
	}

	// Retries must not be delegated to Nomad: the master owns retry policy.
	if rp := group["RestartPolicy"].(map[string]any); rp["Attempts"] != 0 {
		t.Fatalf("nomad must not restart the task: %v", rp)
	}
	if rp := group["ReschedulePolicy"].(map[string]any); rp["Attempts"] != 0 {
		t.Fatalf("nomad must not reschedule the task: %v", rp)
	}
}

func TestProcessJobSpecCarriesRunSpec(t *testing.T) {
	cfg := testCfg()
	b := NewNomad(cfg)
	r := run("linux-process", model.SuiteSpec{Name: "org.eclipse.jgit.test"})
	pool, _ := cfg.Pool("linux-process")

	spec := runSpec(r, nodeSlot)
	job, err := b.buildJob(r, spec, pool)
	if err != nil {
		t.Fatal(err)
	}
	task := job["TaskGroups"].([]map[string]any)[0]["Tasks"].([]map[string]any)[0]
	if task["Driver"] != "raw_exec" {
		t.Fatalf("pool must be able to pin the driver, got %v", task["Driver"])
	}
	if cmd := task["Config"].(map[string]any)["command"]; cmd != "/usr/local/bin/dtp-runner" {
		t.Fatalf("the task must launch dtp-runner, got %v", cmd)
	}

	env := task["Env"].(map[string]string)
	raw, err := base64.StdEncoding.DecodeString(env["DTP_RUN_SPEC"])
	if err != nil {
		t.Fatalf("DTP_RUN_SPEC is not base64: %v", err)
	}
	var back model.RunSpec
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("DTP_RUN_SPEC is not a RunSpec: %v", err)
	}
	if back.RunID != "run-1" || back.Suite != "org.eclipse.jgit.test" {
		t.Fatalf("run spec did not survive the round trip: %+v", back)
	}
}

// A node's slot count and slot size are what it declares (meta.dtp.slots,
// meta.dtp.slot.*); nothing is inferred from hardware.
func TestSlotsFromNodeMeta(t *testing.T) {
	if got := slotsFor(nodeWithMeta(map[string]string{SlotsMeta: "4"}, 8000)); got != 4 {
		t.Fatalf("declared meta.dtp.slots must be used, got %d", got)
	}
	if got := slotsFor(nodeWithMeta(nil, 2500)); got != 0 {
		t.Fatalf("an undeclared node has no slots, got %d", got)
	}
	if got := slotsFor(nodeWithMeta(map[string]string{SlotsMeta: "many"}, 2500)); got != 0 {
		t.Fatalf("a malformed declaration counts as none, got %d", got)
	}
	n := nodeWithMeta(map[string]string{
		SlotsMeta: "3", "dtp.slot.cores": "2", "dtp.slot.memory_mb": "4096", "dtp.slot.memory_max_mb": "8192", "dtp.slot.disk_mb": "2048",
	}, 8000)
	if got := slotOf(n); got != (model.Slot{Cores: 2, Memory: 4096, MemoryMax: 8192, Disk: 2048}) {
		t.Fatalf("slot from meta: %+v", got)
	}
	if got := slotOf(nodeWithMeta(map[string]string{"dtp.slot.cpu_mhz": "1000", "dtp.slot.memory_mb": "x"}, 0)); got != (model.Slot{CPU: 1000}) {
		t.Fatalf("a malformed dimension counts as unset: %+v", got)
	}
}

// nodeWithMeta builds the minimum of a Nomad node for slot derivation.
func nodeWithMeta(meta map[string]string, cpuShares int64) *nomad.Node {
	n := &nomad.Node{Meta: meta, NodeResources: &nomad.NodeResources{}}
	n.NodeResources.Cpu.CpuShares = cpuShares
	return n
}

// A virtual pool's job looks like its member's: the master already chose a
// node in one of the members, so the job is pinned there like any other.
func TestVirtualPoolJobSpec(t *testing.T) {
	cfg := testCfg()
	cat := cfg.RawCatalog().Clone()
	cat.SetPool(config.Pool{Name: "global", Spans: []string{"linux-container"}})
	if err := cfg.SetCatalog(cat); err != nil {
		t.Fatal(err)
	}
	b := NewNomad(cfg)
	r := run("global", model.SuiteSpec{Name: "org.eclipse.egit.core.test", Requires: map[string]string{"perf": "high"}})
	pool, _ := cfg.Pool("global")

	job, err := b.buildJob(r, runSpec(r, nodeSlot), pool)
	if err != nil {
		t.Fatal(err)
	}
	if job["NodePool"] != "all" {
		t.Fatalf("the job runs in Nomad's built-in all pool, got %v", job["NodePool"])
	}
	group := job["TaskGroups"].([]map[string]any)[0]
	cs := group["Constraints"].([]map[string]any)
	var pin map[string]any
	for _, c := range cs {
		if c["LTarget"] == "${node.pool}" {
			t.Fatalf("no Nomad node-pool constraint any more, the node is pinned: %v", cs)
		}
		if c["LTarget"] == "${node.unique.id}" {
			pin = c
		}
	}
	if pin == nil || pin["RTarget"] != "8f2c1d3e-node-a" || pin["Operand"] != "=" {
		t.Fatalf("want the job pinned to the chosen node, got %v", cs)
	}
	// The suite's own requires still apply, and the inherited image is used.
	if cs[0]["LTarget"] != "${meta.dtp.os}" && cs[0]["LTarget"] != "${meta.dtp.perf}" {
		t.Fatalf("suite/pool meta constraints missing: %v", cs)
	}
	task := group["Tasks"].([]map[string]any)[0]
	if task["Driver"] != "docker" || task["Config"].(map[string]any)["image"] != "dtp/rcp-runner:dev" {
		t.Fatalf("virtual pool must inherit driver and image from its member: %v", task)
	}
}

// memory_max reaches Nomad as MemoryMaxMB; the reservation stays one slot.
func TestSlotMemoryMax(t *testing.T) {
	cfg := testCfg()
	b := NewNomad(cfg)
	pool, _ := cfg.Pool("linux-container")
	r := run("linux-container", model.SuiteSpec{Name: "s"})
	job, err := b.buildJob(r, runSpec(r, model.Slot{CPU: 500, Memory: 512, MemoryMax: 2048, Disk: 512}), pool)
	if err != nil {
		t.Fatal(err)
	}
	res := job["TaskGroups"].([]map[string]any)[0]["Tasks"].([]map[string]any)[0]["Resources"].(map[string]any)
	if res["MemoryMB"] != 512 || res["MemoryMaxMB"] != 2048 {
		t.Fatalf("want MemoryMB 512 / MemoryMaxMB 2048, got %v", res)
	}
}

// A core-based slot reserves a cpuset.
func TestSlotCores(t *testing.T) {
	cfg := testCfg()
	b := NewNomad(cfg)
	pool, _ := cfg.Pool("linux-container")
	r := run("linux-container", model.SuiteSpec{Name: "s"})
	job, err := b.buildJob(r, runSpec(r, model.Slot{Cores: 2, Memory: 4096, Disk: 1024}), pool)
	if err != nil {
		t.Fatal(err)
	}
	res := job["TaskGroups"].([]map[string]any)[0]["Tasks"].([]map[string]any)[0]["Resources"].(map[string]any)
	if res["Cores"] != 2 || res["MemoryMB"] != 4096 {
		t.Fatalf("want Cores 2 / MemoryMB 4096, got %v", res)
	}
	if _, has := res["CPU"]; has {
		t.Fatalf("a core-based slot must not also request MHz: %v", res)
	}
}

// An OOM kill is reported as such, not as a generic failed task.
func TestFailureMessageOOM(t *testing.T) {
	a := nomad.Alloc{ClientDescription: "Failed tasks", TaskStates: map[string]*nomad.State{
		"suite": {Failed: true, Events: []nomad.Event{
			{Type: "Started"},
			{Type: "Terminated", ExitCode: 137, Details: map[string]string{"oom_killed": "true", "exit_code": "137"}},
		}}}}
	if got := a.FailureMessage(); !strings.Contains(got, "OOM killed") {
		t.Fatalf("want an OOM message, got %q", got)
	}
}

// Dispatch refuses a run the scheduler did not place: the node is the
// master's choice, never Nomad's.
func TestDispatchNeedsANode(t *testing.T) {
	b := NewNomad(testCfg())
	r := run("linux-container", model.SuiteSpec{Name: "s"})
	r.NodeID = ""
	if _, err := b.Dispatch(context.Background(), r, runSpec(r, nodeSlot)); err == nil || !strings.Contains(err.Error(), "no node") {
		t.Fatalf("want a 'no node chosen' error, got %v", err)
	}
}
