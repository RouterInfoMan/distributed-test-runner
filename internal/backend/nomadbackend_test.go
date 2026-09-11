package backend

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/andrei/distributed-test-platform/internal/config"
	"github.com/andrei/distributed-test-platform/internal/model"
	"github.com/andrei/distributed-test-platform/internal/nomad"
)

func testCfg() *config.Config {
	c := config.Default()
	c.Backend = "nomad"
	c.Nomad.Datacenters = []string{"dc1"}
	c.Pools = []config.Pool{
		{
			Name: "linux-container", Runtime: model.RuntimeContainer,
			Slot:     config.Slot{CPU: 500, Memory: 512, Disk: 512},
			CacheDir: "/var/lib/dtp/cache", DockerNetwork: "dtp_default",
			Constraints: map[string]string{"os": "linux"},
		},
		{
			Name: "linux-process", Runtime: model.RuntimeProcess, TaskDriver: "raw_exec",
			Slot:          config.Slot{CPU: 500, Memory: 512, Disk: 512},
			CacheDir:      "/var/lib/dtp/cache",
			RunnerCommand: []string{"/usr/local/bin/dtp-runner"},
		},
	}
	c.Pools[0].Default.Image = "dtp/rcp-runner:dev"
	if err := c.Validate(); err != nil {
		panic(err)
	}
	return c
}

func run(pool string, spec model.SuiteSpec) *model.Run {
	return &model.Run{
		ID: "run-1", RegressionID: "reg-1", Suite: spec.Name, Attempt: 2,
		Pool: pool, Priority: 70, Spec: spec,
	}
}

func TestContainerJobSpec(t *testing.T) {
	cfg := testCfg()
	b := NewNomad(cfg)
	r := run("linux-container", model.SuiteSpec{
		Name:     "org.eclipse.egit.ui.test",
		Requires: map[string]string{"rcp_version": "4.30", "display": "xvfb"},
	})
	pool, _ := cfg.Pool("linux-container")

	job, err := b.buildJob(r, model.RunSpec{RunID: r.ID, Suite: r.Suite}, pool)
	if err != nil {
		t.Fatal(err)
	}
	if job["NodePool"] != "linux-container" {
		t.Fatalf("job must target the pool, got %v", job["NodePool"])
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
		t.Fatalf("a task must reserve exactly one slot, got %v", res)
	}

	// Pool baseline plus suite requirements, as node-meta constraints.
	cons := group["Constraints"].([]map[string]any)
	want := map[string]string{
		"${meta.dtp.display}":     "xvfb",
		"${meta.dtp.os}":          "linux",
		"${meta.dtp.rcp_version}": "4.30",
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

	spec := model.RunSpec{RunID: "run-1", Suite: "org.eclipse.jgit.test", Attempt: 2}
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

func TestSlotsFromNodeMeta(t *testing.T) {
	pool := &config.Pool{Slot: config.Slot{CPU: 500}}
	if got := slotsFor(nodeWithMeta(map[string]string{SlotsMeta: "4"}, 8000), pool); got != 4 {
		t.Fatalf("declared meta.dtp.slots must win, got %d", got)
	}
	// No declaration: derive from CPU against the pool's slot size.
	if got := slotsFor(nodeWithMeta(nil, 2500), pool); got != 5 {
		t.Fatalf("want 5 derived slots, got %d", got)
	}
}

// nodeWithMeta builds the minimum of a Nomad node for slot derivation.
func nodeWithMeta(meta map[string]string, cpuShares int64) *nomad.Node {
	n := &nomad.Node{Meta: meta}
	n.NodeResources = &struct {
		Cpu struct {
			CpuShares int64 `json:"CpuShares"`
		} `json:"Cpu"`
		Memory struct {
			MemoryMB int64 `json:"MemoryMB"`
		} `json:"Memory"`
	}{}
	n.NodeResources.Cpu.CpuShares = cpuShares
	return n
}
