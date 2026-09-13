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
		out = append(out, NodeInfo{
			ID:     "local-" + n.Name,
			Name:   n.Name,
			Slots:  n.Slots,
			Slot:   n.Slot,
			Ready:  true,
			Status: "ready",
			Meta:   n.Meta,
		})
	}
	return out, nil
}

// claim reserves one of the chosen node's slots, the way Nomad would refuse
// a placement that does not fit.
func (b *LocalBackend) claim(run *model.Run) (string, error) {
	for _, n := range b.cfg.LocalNodes {
		if "local-"+n.Name != run.NodeID {
			continue
		}
		if b.nodeUse[n.Name] >= n.Slots {
			return "", fmt.Errorf("node %s has no free slot (%d/%d)", n.Name, b.nodeUse[n.Name], n.Slots)
		}
		b.nodeUse[n.Name]++
		return n.Name, nil
	}
	return "", fmt.Errorf("unknown local node %q", run.NodeID)
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

	if run.NodeID == "" {
		return Placement{}, fmt.Errorf("run %s has no node chosen", run.ID)
	}
	b.mu.Lock()
	nodeName, err := b.claim(run)
	b.mu.Unlock()
	if err != nil {
		return Placement{}, err
	}
	nodeID := run.NodeID
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
