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
// A worker declares e.g.
//
//	meta { "dtp.slots" = "4"  "dtp.os" = "linux"  "dtp.rcp_version" = "4.30" }
//
// and a suite's "requires": {"rcp_version": "4.30"} becomes the Nomad
// constraint ${meta.dtp.rcp_version} = 4.30.
const MetaPrefix = "dtp."

// SlotsMeta is the node meta key holding a node's parallel-suite capacity.
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
	pools := map[string]*config.Pool{}
	for i := range b.cfg.Pools {
		pools[b.cfg.Pools[i].Name] = &b.cfg.Pools[i]
	}

	var out []NodeInfo
	for _, st := range stubs {
		pool := st.NodePool
		if pool == "" {
			pool = "default"
		}
		if _, known := pools[pool]; !known {
			continue // node belongs to a pool this platform does not manage
		}
		n, err := b.cli.Node(ctx, st.ID)
		if err != nil {
			continue
		}
		info := NodeInfo{
			ID:     st.ID,
			Name:   st.Name,
			Pool:   pool,
			Ready:  n.Ready(),
			Status: st.Status,
			Meta:   filterMeta(n.Meta),
		}
		info.Slots = slotsFor(n, pools[pool])
		out = append(out, info)
	}

	b.mu.Lock()
	b.nodeCache, b.nodeCacheAt = out, time.Now()
	b.mu.Unlock()
	return out, nil
}

// slotsFor prefers the node's declared meta.dtp.slots and otherwise derives the
// count from the node's CPU against the pool's slot size, so an operator can
// declare capacity either way.
func slotsFor(n *nomad.Node, pool *config.Pool) int {
	if v, ok := n.Meta[SlotsMeta]; ok {
		if i, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && i > 0 {
			return i
		}
	}
	if n.NodeResources != nil && pool != nil && pool.Slot.CPU > 0 {
		if s := int(n.NodeResources.Cpu.CpuShares) / pool.Slot.CPU; s > 0 {
			return s
		}
	}
	return 1
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
	job, err := b.buildJob(run, spec, pool)
	if err != nil {
		return Placement{}, err
	}
	if _, err := b.cli.RegisterJob(ctx, job); err != nil {
		return Placement{}, err
	}
	return Placement{BackendID: job["ID"].(string)}, nil
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

	// One slot's worth of resources, so Nomad's bin-packer lands exactly
	// meta.dtp.slots suites on a node sized for that many slots.
	resources := map[string]any{
		"CPU":      pool.Slot.CPU,
		"MemoryMB": pool.Slot.Memory,
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
		"EphemeralDisk":    map[string]any{"SizeMB": pool.Slot.Disk},
		"Tasks":            []map[string]any{task},
	}
	if cs := constraints(run, pool); len(cs) > 0 {
		group["Constraints"] = cs
	}
	if pool.HostVolume != "" {
		group["Volumes"] = map[string]any{
			"cache": map[string]any{"Type": "host", "Source": pool.HostVolume, "ReadOnly": false},
		}
		task["VolumeMounts"] = []map[string]any{
			{"Volume": "cache", "Destination": pool.CacheDir, "ReadOnly": false},
		}
	}

	job := map[string]any{
		"ID":          JobID(run),
		"Name":        JobID(run),
		"Type":        "batch",
		"Priority":    nomadPriority(run.Priority),
		"Datacenters": b.cfg.Nomad.Datacenters,
		"NodePool":    run.Pool,
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
	a := allocs[len(allocs)-1]
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
	return st, nil
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
