package backend

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/andrei/distributed-test-platform/internal/config"
	"github.com/andrei/distributed-test-platform/internal/model"
	"github.com/andrei/distributed-test-platform/internal/nomad"
)

// MetaPrefix is the node-meta namespace the platform reads and constrains on.
// dtp-node publishes a worker's node.yaml there, e.g.
//
//	dtp.slots = 6   dtp.slot.cores = 2   dtp.slot.memory_mb = 4096   dtp.perf = high
//
// and a suite's "requires": {"perf": "high"} becomes the Nomad constraint
// ${meta.dtp.perf} = high.
const MetaPrefix = "dtp."

// SlotsMeta is the node meta key holding a node's parallel-suite capacity;
// the slot.* keys hold the size of one slot.
const SlotsMeta = MetaPrefix + "slots"

type NomadBackend struct {
	cli *nomad.Client
	cfg *config.Config
	mu  sync.Mutex
	// nodeCache avoids a per-node GET on every scheduler tick.
	nodeCache   []NodeInfo
	nodeCacheAt time.Time
}

func NewNomad(cfg *config.Config) *NomadBackend {
	return &NomadBackend{
		cli: nomad.New(cfg.Nomad.Address, cfg.Nomad.Token, cfg.Nomad.Namespace, cfg.Nomad.Region),
		cfg: cfg,
	}
}

func (b *NomadBackend) Name() string { return "nomad" }

func (b *NomadBackend) Healthy(ctx context.Context) error { return b.cli.Ping(ctx) }

// Inventory reads the node list and then each node's meta. Results are cached
// briefly because the scheduler ticks far faster than nodes change.
func (b *NomadBackend) Inventory(ctx context.Context) ([]NodeInfo, error) {
	b.mu.Lock()
	if time.Since(b.nodeCacheAt) < 5*time.Second && b.nodeCache != nil {
		out := b.nodeCache
		b.mu.Unlock()
		return out, nil
	}
	b.mu.Unlock()

	stubs, err := b.cli.Nodes(ctx)
	if err != nil {
		return nil, err
	}

	// Every Nomad client is a candidate; which pool it serves is the
	// catalog's decision, and a node that declares no slots contributes none.
	var out []NodeInfo
	for _, st := range stubs {
		n, err := b.cli.Node(ctx, st.ID)
		if err != nil {
			continue
		}
		info := NodeInfo{
			ID:     st.ID,
			Name:   st.Name,
			Ready:  n.Ready(),
			Status: st.Status,
			Meta:   filterMeta(n.Meta),
		}
		info.Slots = slotsFor(n)
		info.Slot = slotOf(n)
		out = append(out, info)
	}

	b.mu.Lock()
	b.nodeCache, b.nodeCacheAt = out, time.Now()
	b.mu.Unlock()
	return out, nil
}

// slotsFor is the node's declared meta.dtp.slots (set by dtp-node from
// node.yaml, or statically in the client HCL). A node that declares nothing
// has no slots: capacity is an operator's decision, never inferred from the
// hardware.
func slotsFor(n *nomad.Node) int {
	return metaInt(n.Meta, SlotsMeta)
}

// slotOf is the node's declared slot size (meta.dtp.slot.*).
func slotOf(n *nomad.Node) model.Slot {
	return model.Slot{
		Cores:     metaInt(n.Meta, MetaPrefix+"slot.cores"),
		CPU:       metaInt(n.Meta, MetaPrefix+"slot.cpu_mhz"),
		Memory:    metaInt(n.Meta, MetaPrefix+"slot.memory_mb"),
		MemoryMax: metaInt(n.Meta, MetaPrefix+"slot.memory_max_mb"),
		Disk:      metaInt(n.Meta, MetaPrefix+"slot.disk_mb"),
	}
}

func metaInt(meta map[string]string, key string) int {
	if v, ok := meta[key]; ok {
		if i, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && i > 0 {
			return i
		}
	}
	return 0
}

func filterMeta(meta map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range meta {
		if strings.HasPrefix(k, MetaPrefix) {
			out[strings.TrimPrefix(k, MetaPrefix)] = v
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// dispatch
// ---------------------------------------------------------------------------

var jobIDSanitizer = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// JobID is deterministic so a master restart can re-attach to a live job.
func JobID(run *model.Run) string {
	suite := jobIDSanitizer.ReplaceAllString(run.Suite, "-")
	if len(suite) > 60 {
		suite = suite[:60]
	}
	return fmt.Sprintf("dtp-%s-%s-%d", run.RegressionID, suite, run.Attempt)
}

func (b *NomadBackend) Dispatch(ctx context.Context, run *model.Run, spec model.RunSpec) (Placement, error) {
	pool, ok := b.cfg.Pool(run.Pool)
	if !ok {
		return Placement{}, fmt.Errorf("unknown pool %q", run.Pool)
	}
	if run.NodeID == "" {
		return Placement{}, fmt.Errorf("run %s has no node chosen", run.ID)
	}
	job, err := b.buildJob(run, spec, pool)
	if err != nil {
		return Placement{}, err
	}
	if _, err := b.cli.RegisterJob(ctx, job); err != nil {
		return Placement{}, err
	}
	return Placement{BackendID: job["ID"].(string), NodeID: run.NodeID, NodeName: run.NodeName}, nil
}

// buildJob renders the Nomad batch job for one suite attempt.
func (b *NomadBackend) buildJob(run *model.Run, spec model.RunSpec, pool *config.Pool) (map[string]any, error) {
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	env := map[string]string{
		"DTP_RUN_SPEC":      base64.StdEncoding.EncodeToString(specJSON),
		"DTP_RUN_ID":        run.ID,
		"DTP_REGRESSION_ID": run.RegressionID,
		"DTP_SUITE":         run.Suite,
		// Nomad interpolates these at placement time; without them the runner
		// would have to guess which node it landed on.
		"DTP_NODE_ID":   "${node.unique.id}",
		"DTP_NODE_NAME": "${node.unique.name}",
	}
	for k, v := range spec.Env {
		env[k] = v
	}

	// One of the chosen node's slots: the node declared the envelope, so
	// Nomad's own accounting lands exactly meta.dtp.slots suites there. Cores
	// are a cpuset reservation, the same whatever the node's clock speed.
	resources := map[string]any{"MemoryMB": spec.Slot.Memory}
	if spec.Slot.Cores > 0 {
		resources["Cores"] = spec.Slot.Cores
	} else {
		resources["CPU"] = spec.Slot.CPU
	}
	if spec.Slot.MemoryMax > spec.Slot.Memory {
		resources["MemoryMaxMB"] = spec.Slot.MemoryMax
	}

	task := map[string]any{
		"Name":        "suite",
		"Env":         env,
		"Resources":   resources,
		"KillTimeout": int64(60 * time.Second),
		"LogConfig":   map[string]any{"MaxFiles": 2, "MaxFileSizeMB": 20},
	}

	driver := pool.Driver()
	task["Driver"] = driver

	switch pool.Runtime {
	case model.RuntimeContainer:
		image := run.Spec.Image
		if image == "" {
			image = pool.Default.Image
		}
		if image == "" {
			return nil, fmt.Errorf("pool %q: container runtime needs an image", pool.Name)
		}
		cfgMap := map[string]any{"image": image}
		if cmd := pool.RunnerCmd(); len(cmd) > 0 {
			cfgMap["command"] = cmd[0]
			if len(cmd) > 1 {
				cfgMap["args"] = cmd[1:]
			}
		}
		if pool.DockerNetwork != "" {
			cfgMap["network_mode"] = pool.DockerNetwork
		}
		// The build cache is a host path so a 400MB product zip is fetched
		// once per node, not once per suite.
		if pool.HostVolume == "" && pool.CacheDir != "" {
			cfgMap["volumes"] = []string{pool.CacheDir + ":" + pool.CacheDir}
		}
		task["Config"] = cfgMap

	case model.RuntimeProcess:
		cmd := pool.RunnerCmd()
		if len(cmd) == 0 {
			return nil, fmt.Errorf("pool %q: process runtime needs a runner_command", pool.Name)
		}
		cfgMap := map[string]any{"command": cmd[0]}
		if len(cmd) > 1 {
			cfgMap["args"] = cmd[1:]
		}
		task["Config"] = cfgMap
		if pool.RunnerURL != "" {
			task["Artifacts"] = []map[string]any{{
				"GetterSource": pool.RunnerURL,
				"RelativeDest": "local/",
			}}
		}
	}

	group := map[string]any{
		"Name":             "suite",
		"Count":            1,
		"RestartPolicy":    map[string]any{"Attempts": 0, "Mode": "fail"},
		"ReschedulePolicy": map[string]any{"Attempts": 0, "Unlimited": false},
		"EphemeralDisk":    map[string]any{"SizeMB": spec.Slot.Disk},
		"Tasks":            []map[string]any{task},
	}
	// The master chose the node; the meta constraints are kept as well so the
	// job documents why that node qualified.
	cs := append(constraints(run, pool), map[string]any{
		"LTarget": "${node.unique.id}", "RTarget": run.NodeID, "Operand": "=",
	})
	group["Constraints"] = cs
	if pool.HostVolume != "" {
		group["Volumes"] = map[string]any{
			"cache": map[string]any{"Type": "host", "Source": pool.HostVolume, "ReadOnly": false},
		}
		task["VolumeMounts"] = []map[string]any{
			{"Volume": "cache", "Destination": pool.CacheDir, "ReadOnly": false},
		}
	}

	// Pools are the master's, not Nomad's: the job may run in any Nomad node
	// pool ("all") and is pinned to the chosen node.
	job := map[string]any{
		"ID":          JobID(run),
		"Name":        JobID(run),
		"Type":        "batch",
		"Priority":    nomadPriority(run.Priority),
		"Datacenters": b.cfg.Nomad.Datacenters,
		"NodePool":    "all",
		"TaskGroups":  []map[string]any{group},
		"Meta": map[string]string{
			"dtp.regression": run.RegressionID,
			"dtp.suite":      run.Suite,
			"dtp.run":        run.ID,
			"dtp.attempt":    strconv.Itoa(run.Attempt),
		},
	}
	if ns := b.cfg.Nomad.Namespace; ns != "" {
		job["Namespace"] = ns
	}
	return job, nil
}

// constraints translates the suite's "requires" map plus the pool's baseline
// into Nomad node-meta constraints.
func constraints(run *model.Run, pool *config.Pool) []map[string]any {
	merged := map[string]string{}
	for k, v := range pool.Constraints {
		merged[k] = v
	}
	for k, v := range run.Spec.Requires {
		merged[k] = v
	}
	keys := make([]string, 0, len(merged))
	for k := range merged {
		keys = append(keys, k)
	}
	sortStrings(keys)

	out := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		out = append(out, map[string]any{
			"LTarget": "${meta." + MetaPrefix + k + "}",
			"RTarget": merged[k],
			"Operand": "=",
		})
	}
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// nomadPriority maps the submission priority (0..100, higher first) onto
// Nomad's job priority range.
func nomadPriority(p int) int {
	if p <= 0 {
		return 50
	}
	if p > 100 {
		return 100
	}
	return p
}

// ---------------------------------------------------------------------------
// poll / stop
// ---------------------------------------------------------------------------

func (b *NomadBackend) Poll(ctx context.Context, run *model.Run) (Status, error) {
	if run.BackendID == "" {
		return Status{Phase: PhaseUnknown}, nil
	}
	allocs, err := b.cli.JobAllocations(ctx, run.BackendID)
	if err != nil {
		if strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), "not found") {
			return Status{Phase: PhaseLost, Message: "job not found in Nomad"}, nil
		}
		return Status{}, err
	}
	if len(allocs) == 0 {
		return Status{Phase: PhasePending, Message: "waiting for placement"}, nil
	}
	return statusOf(allocs[len(allocs)-1]), nil
}

// PollAll lists every allocation once and matches them to runs by job ID. A
// run whose job has no allocation yet is left out, so the scheduler falls back
// to Poll for it, which can tell "not placed yet" from "job gone".
func (b *NomadBackend) PollAll(ctx context.Context, runs []*model.Run) (map[string]Status, error) {
	allocs, err := b.cli.Allocations(ctx, "")
	if err != nil {
		return nil, err
	}
	latest := map[string]nomad.Alloc{} // job id -> newest allocation
	for _, a := range allocs {
		if !strings.HasPrefix(a.JobID, "dtp-") {
			continue
		}
		if cur, ok := latest[a.JobID]; !ok || a.CreateIndex > cur.CreateIndex {
			latest[a.JobID] = a
		}
	}
	out := make(map[string]Status, len(runs))
	for _, r := range runs {
		if a, ok := latest[r.BackendID]; ok && r.BackendID != "" {
			out[r.ID] = statusOf(a)
		}
	}
	return out, nil
}

// statusOf maps an allocation's client status onto the backend phase.
func statusOf(a nomad.Alloc) Status {
	st := Status{AllocID: a.ID, NodeID: a.NodeID, NodeName: a.NodeName}
	if code, ok := a.ExitCode(); ok {
		c := code
		st.ExitCode = &c
	}
	switch a.ClientStatus {
	case "pending":
		st.Phase = PhasePending
	case "running":
		st.Phase = PhaseRunning
	case "complete":
		st.Phase = PhaseComplete
	case "failed":
		st.Phase = PhaseFailed
		st.Message = a.FailureMessage()
	case "lost":
		st.Phase = PhaseLost
		st.Message = "allocation lost"
	default:
		st.Phase = PhaseUnknown
	}
	return st
}

func (b *NomadBackend) Stop(ctx context.Context, run *model.Run) error {
	if run.BackendID == "" {
		return nil
	}
	return b.cli.StopJob(ctx, run.BackendID, true)
}

// Logs returns the task's stdout for a run, used by `dtp logs`.
func (b *NomadBackend) Logs(ctx context.Context, run *model.Run, stream string) ([]byte, error) {
	if run.AllocID == "" {
		return nil, fmt.Errorf("run has no allocation yet")
	}
	return b.cli.AllocLogs(ctx, run.AllocID, "suite", stream, 0)
}
