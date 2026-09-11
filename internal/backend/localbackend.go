package backend

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/andrei/distributed-test-platform/internal/config"
	"github.com/andrei/distributed-test-platform/internal/model"
)

// LocalBackend runs each suite as a child process of the master against a set
// of simulated nodes from the config. It implements the same contract as the
// Nomad backend (including per-node slot accounting) so the scheduler, the
// runner protocol and the dashboard can be exercised on one machine.
type LocalBackend struct {
	cfg     *config.Config
	logDir  string
	mu      sync.Mutex
	procs   map[string]*localProc
	nodeUse map[string]int // node name -> slots in use
}

type localProc struct {
	cmd      *exec.Cmd
	nodeID   string
	nodeName string
	done     bool
	exitCode int
	failed   bool
	message  string
}

func NewLocal(cfg *config.Config) *LocalBackend {
	return &LocalBackend{
		cfg:     cfg,
		logDir:  filepath.Join(cfg.StateDir, "local-logs"),
		procs:   map[string]*localProc{},
		nodeUse: map[string]int{},
	}
}

func (b *LocalBackend) Name() string                    { return "local" }
func (b *LocalBackend) Healthy(_ context.Context) error { return nil }

func (b *LocalBackend) Inventory(_ context.Context) ([]NodeInfo, error) {
	out := make([]NodeInfo, 0, len(b.cfg.LocalNodes))
	for _, n := range b.cfg.LocalNodes {
		slots := n.Slots
		if slots <= 0 {
			slots = 1
		}
		out = append(out, NodeInfo{
			ID:     "local-" + n.Name,
			Name:   n.Name,
			Pool:   n.Pool,
			Slots:  slots,
			Ready:  true,
			Status: "ready",
			Meta:   n.Meta,
		})
	}
	return out, nil
}

// pickNode does what Nomad's scheduler would: first node in the pool that has
// a free slot and satisfies the suite's required meta.
func (b *LocalBackend) pickNode(run *model.Run) (string, string, error) {
	pool, _ := b.cfg.Pool(run.Pool)
	for _, n := range b.cfg.LocalNodes {
		if n.Pool != run.Pool {
			continue
		}
		if !metaSatisfies(n.Meta, pool, run.Spec.Requires) {
			continue
		}
		slots := n.Slots
		if slots <= 0 {
			slots = 1
		}
		if b.nodeUse[n.Name] < slots {
			b.nodeUse[n.Name]++
			return "local-" + n.Name, n.Name, nil
		}
	}
	return "", "", fmt.Errorf("no free slot in pool %q matching %v", run.Pool, run.Spec.Requires)
}

func metaSatisfies(meta map[string]string, pool *config.Pool, requires map[string]string) bool {
	merged := map[string]string{}
	if pool != nil {
		for k, v := range pool.Constraints {
			merged[k] = v
		}
	}
	for k, v := range requires {
		merged[k] = v
	}
	for k, want := range merged {
		if meta[k] != want {
			return false
		}
	}
	return true
}

func (b *LocalBackend) Dispatch(_ context.Context, run *model.Run, spec model.RunSpec) (Placement, error) {
	pool, ok := b.cfg.Pool(run.Pool)
	if !ok {
		return Placement{}, fmt.Errorf("unknown pool %q", run.Pool)
	}
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return Placement{}, err
	}

	b.mu.Lock()
	nodeID, nodeName, err := b.pickNode(run)
	b.mu.Unlock()
	if err != nil {
		return Placement{}, err
	}
	release := func() {
		b.mu.Lock()
		if b.nodeUse[nodeName] > 0 {
			b.nodeUse[nodeName]--
		}
		b.mu.Unlock()
	}

	runner := b.cfg.LocalRunnerPath
	if runner == "" {
		runner = "dtp-runner"
	}
	cmd := exec.Command(runner)
	cmd.Env = append(os.Environ(),
		"DTP_RUN_SPEC="+base64.StdEncoding.EncodeToString(specJSON),
		"DTP_RUN_ID="+run.ID,
		"DTP_NODE_ID="+nodeID,
		"DTP_NODE_NAME="+nodeName,
		"DTP_LOCAL_POOL="+pool.Name,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := os.MkdirAll(b.logDir, 0o755); err != nil {
		release()
		return Placement{}, err
	}
	logFile, err := os.Create(filepath.Join(b.logDir, run.ID+".log"))
	if err != nil {
		release()
		return Placement{}, err
	}
	cmd.Stdout, cmd.Stderr = logFile, logFile

	if err := cmd.Start(); err != nil {
		logFile.Close()
		release()
		return Placement{}, fmt.Errorf("start runner: %w", err)
	}

	p := &localProc{cmd: cmd, nodeID: nodeID, nodeName: nodeName}
	b.mu.Lock()
	b.procs[run.ID] = p
	b.mu.Unlock()

	go func() {
		werr := cmd.Wait()
		logFile.Close()
		release()
		b.mu.Lock()
		defer b.mu.Unlock()
		p.done = true
		if werr != nil {
			p.failed = true
			p.message = werr.Error()
		}
		p.exitCode = cmd.ProcessState.ExitCode()
	}()

	return Placement{BackendID: "local:" + run.ID, NodeID: nodeID, NodeName: nodeName}, nil
}

func (b *LocalBackend) Poll(_ context.Context, run *model.Run) (Status, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, ok := b.procs[run.ID]
	if !ok {
		return Status{Phase: PhaseUnknown}, nil
	}
	st := Status{NodeID: p.nodeID, NodeName: p.nodeName, AllocID: "local:" + run.ID}
	if !p.done {
		st.Phase = PhaseRunning
		return st, nil
	}
	code := p.exitCode
	st.ExitCode = &code
	st.Message = p.message
	if p.failed || code != 0 {
		st.Phase = PhaseFailed
	} else {
		st.Phase = PhaseComplete
	}
	return st, nil
}

func (b *LocalBackend) Stop(_ context.Context, run *model.Run) error {
	b.mu.Lock()
	p, ok := b.procs[run.ID]
	b.mu.Unlock()
	if !ok || p.done || p.cmd.Process == nil {
		return nil
	}
	// Kill the whole process group: the runner may have an Eclipse child.
	return syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
}

// LogPath is where the local backend captured a run's stdout.
func (b *LocalBackend) LogPath(runID string) string {
	return filepath.Join(b.logDir, runID+".log")
}
