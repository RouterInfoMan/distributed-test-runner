// Package runner is the node-side agent. It receives a self-contained RunSpec,
// materializes the build from the node's content-addressed cache, executes one
// Eclipse RCP test suite, uploads every artifact to the object store under
// results/<regression_id>/<suite>/attempt-<n>/, and reports the parsed result
// back to the master.
package runner

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/andrei/distributed-test-platform/internal/junit"
	"github.com/andrei/distributed-test-platform/internal/model"
	"github.com/andrei/distributed-test-platform/internal/s3"
)

// ConsoleLog is the runner's own transcript, always uploaded with the results
// so a suite that produced no JUnit XML is still debuggable.
const ConsoleLog = "dtp-runner.log"

type Runner struct {
	Spec model.RunSpec

	workspace string
	results   string
	console   *os.File
	s3        *s3.Client
	http      *http.Client
	node      nodeIdentity
}

type nodeIdentity struct {
	ID      string
	Name    string
	AllocID string
}

// LoadSpec reads the RunSpec from DTP_RUN_SPEC (base64 JSON), from a Nomad
// dispatch payload file, or from a path given on the command line.
func LoadSpec(path string) (model.RunSpec, error) {
	var raw []byte
	switch {
	case path != "":
		b, err := os.ReadFile(path)
		if err != nil {
			return model.RunSpec{}, err
		}
		raw = b
	case os.Getenv("DTP_RUN_SPEC") != "":
		b, err := base64.StdEncoding.DecodeString(os.Getenv("DTP_RUN_SPEC"))
		if err != nil {
			// Tolerate a plain-JSON spec for hand-driven debugging.
			b = []byte(os.Getenv("DTP_RUN_SPEC"))
		}
		raw = b
	case os.Getenv("NOMAD_META_dtp_payload") != "":
		b, err := os.ReadFile(os.Getenv("NOMAD_META_dtp_payload"))
		if err != nil {
			return model.RunSpec{}, err
		}
		raw = b
	default:
		return model.RunSpec{}, fmt.Errorf("no run spec: set DTP_RUN_SPEC or pass -spec <file>")
	}
	var spec model.RunSpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return model.RunSpec{}, fmt.Errorf("parse run spec: %w", err)
	}
	if spec.RunID == "" {
		return model.RunSpec{}, fmt.Errorf("run spec has no run_id")
	}
	return spec, nil
}

func New(spec model.RunSpec) *Runner {
	r := &Runner{
		Spec: spec,
		http: &http.Client{Timeout: 60 * time.Second},
		node: detectNode(),
	}
	if spec.S3.Endpoint != "" {
		r.s3 = s3.New(spec.S3.Endpoint, spec.S3.Region, spec.S3.AccessKey, spec.S3.SecretKey)
	}
	return r
}

// detectNode resolves which worker this attempt landed on. The master injects
// DTP_NODE_* (interpolated by Nomad at placement time, or set directly by the
// local backend); the hostname is the last resort.
func detectNode() nodeIdentity {
	n := nodeIdentity{
		ID:      firstEnv("DTP_NODE_ID", "NOMAD_NODE_ID"),
		Name:    firstEnv("DTP_NODE_NAME", "NOMAD_NODE_NAME"),
		AllocID: os.Getenv("NOMAD_ALLOC_ID"),
	}
	if n.Name == "" {
		n.Name, _ = os.Hostname()
	}
	if n.ID == "" {
		n.ID = n.Name
	}
	return n
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// Execute performs the whole attempt and returns the process exit code the
// task should exit with.
func (r *Runner) Execute(ctx context.Context) int {
	if err := r.setup(); err != nil {
		r.report(ctx, model.RunErrored, "runner setup failed: "+err.Error(), nil, model.Summary{}, nil, nil)
		return 1
	}
	defer r.console.Close()

	r.logf("dtp-runner starting: suite=%s attempt=%d run=%s node=%s",
		r.Spec.Suite, r.Spec.Attempt, r.Spec.RunID, r.node.Name)
	r.notifyStarted(ctx)

	env, err := r.prepareBuild(ctx)
	if err != nil {
		r.logf("ERROR preparing build: %v", err)
		arts := r.uploadArtifacts(ctx)
		r.report(ctx, model.RunErrored, "build preparation failed: "+err.Error(), nil, model.Summary{}, nil, arts)
		return 1
	}

	exitCode, timedOut, runErr := r.runSuite(ctx, env)
	if runErr != nil {
		r.logf("ERROR launching suite: %v", runErr)
	}

	res, perr := junit.ParseDir(r.results)
	if perr != nil {
		r.logf("WARN scanning results: %v", perr)
	}
	r.logf("suite finished: exit=%d tests=%d passed=%d failed=%d errors=%d skipped=%d",
		exitCode, res.Summary.Tests, res.Summary.Passed, res.Summary.Failed,
		res.Summary.Errors, res.Summary.Skipped)

	state, msg := classify(exitCode, timedOut, runErr, res.Summary)
	arts := r.uploadArtifacts(ctx)
	r.report(ctx, state, msg, &exitCode, res.Summary, res.Problems(), arts)

	// The task's own exit status mirrors the verdict so Nomad's view agrees
	// with the master's.
	if state == model.RunPassed {
		return 0
	}
	return 1
}

// classify turns process outcome plus parsed results into a run state. A
// non-zero exit with parsed failures is a test failure; a non-zero exit with no
// results at all is an infrastructure error.
func classify(exitCode int, timedOut bool, runErr error, sum model.Summary) (model.RunState, string) {
	switch {
	case timedOut:
		return model.RunTimeout, "suite exceeded its timeout and was killed"
	case runErr != nil:
		return model.RunErrored, runErr.Error()
	case sum.Tests == 0:
		if exitCode == 0 {
			return model.RunErrored, "suite exited cleanly but produced no JUnit results"
		}
		return model.RunErrored, fmt.Sprintf("suite exited %d and produced no JUnit results", exitCode)
	case sum.Failed+sum.Errors > 0:
		return model.RunFailed, fmt.Sprintf("%d of %d tests failed", sum.Failed+sum.Errors, sum.Tests)
	case exitCode != 0:
		return model.RunFailed, fmt.Sprintf("all reported tests passed but the suite exited %d", exitCode)
	default:
		return model.RunPassed, ""
	}
}

// ---------------------------------------------------------------------------
// workspace + build
// ---------------------------------------------------------------------------

func (r *Runner) setup() error {
	base := firstEnv("NOMAD_TASK_DIR", "DTP_WORKSPACE_ROOT")
	if base == "" {
		d, err := os.MkdirTemp("", "dtp-"+r.Spec.RunID+"-")
		if err != nil {
			return err
		}
		base = d
	}
	r.workspace = filepath.Join(base, "workspace")
	r.results = filepath.Join(base, "results")
	for _, d := range []string{r.workspace, r.results} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	f, err := os.Create(filepath.Join(r.results, ConsoleLog))
	if err != nil {
		return err
	}
	r.console = f
	return nil
}

func (r *Runner) logf(format string, args ...any) {
	line := fmt.Sprintf("%s  %s\n", time.Now().UTC().Format(time.RFC3339), fmt.Sprintf(format, args...))
	if r.console != nil {
		r.console.WriteString(line)
	}
	os.Stdout.WriteString(line)
}

// prepareBuild populates the node's build cache and returns the environment
// additions that point the suite command at the unpacked payloads.
func (r *Runner) prepareBuild(ctx context.Context) (map[string]string, error) {
	env := map[string]string{
		"DTP_WORKSPACE":   r.workspace,
		"DTP_RESULTS_DIR": r.results,
		"DTP_SUITE":       r.Spec.Suite,
		"DTP_RUN_ID":      r.Spec.RunID,
		"DTP_ATTEMPT":     fmt.Sprint(r.Spec.Attempt),
		"DTP_NODE_NAME":   r.node.Name,
	}
	// The slot is a hard ceiling under the docker driver, so a harness can
	// size its JVM heaps to fit rather than guess.
	if r.Spec.Slot.Memory > 0 {
		env["DTP_SLOT_MEMORY_MB"] = fmt.Sprint(r.Spec.Slot.Memory)
	}
	if r.Spec.Slot.MemoryMax > 0 {
		env["DTP_SLOT_MEMORY_MAX_MB"] = fmt.Sprint(r.Spec.Slot.MemoryMax)
	}
	if r.Spec.Slot.CPU > 0 {
		env["DTP_SLOT_CPU"] = fmt.Sprint(r.Spec.Slot.CPU)
	}
	if r.Spec.Slot.Disk > 0 {
		env["DTP_SLOT_DISK_MB"] = fmt.Sprint(r.Spec.Slot.Disk)
	}
	if len(r.Spec.Build) == 0 {
		return env, nil
	}
	cache := &Cache{Dir: r.Spec.CacheDir, S3: r.s3, Log: r.logf}
	for _, a := range r.Spec.Build {
		path, err := cache.Ensure(ctx, a)
		if err != nil {
			return nil, err
		}
		// A single-directory archive is flattened so DTP_BUILD_PRODUCT points
		// at the product root rather than its wrapper directory. The version
		// and id come from the build's manifest, resolved by the master.
		key := "DTP_BUILD_" + strings.ToUpper(sanitizeEnv(a.Name))
		env[key] = flatten(path)
		if a.Version != "" {
			env[key+"_VERSION"] = a.Version
		}
		if a.ID != "" {
			env[key+"_ID"] = a.ID
		}
	}
	return env, nil
}

func flatten(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || !entries[0].IsDir() {
		return dir
	}
	return filepath.Join(dir, entries[0].Name())
}

func sanitizeEnv(s string) string {
	var b strings.Builder
	for _, ch := range s {
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9':
			b.WriteRune(ch)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// execution
// ---------------------------------------------------------------------------

func (r *Runner) runSuite(ctx context.Context, extraEnv map[string]string) (exitCode int, timedOut bool, err error) {
	timeout := r.Spec.Timeout.Duration()
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, r.Spec.Command[0], r.Spec.Command[1:]...)
	cmd.Dir = r.workspace
	cmd.Env = os.Environ()
	for k, v := range r.Spec.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	for k, v := range extraEnv {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	// Own the process group so a wedged Eclipse and its children die together.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }

	out := io.MultiWriter(r.console, os.Stdout)
	cmd.Stdout, cmd.Stderr = out, out

	r.logf("exec: %s (timeout %s, cwd %s)", strings.Join(r.Spec.Command, " "), timeout, r.workspace)
	start := time.Now()
	stopProgress := r.reportProgress(runCtx)
	runErr := cmd.Run()
	stopProgress()
	r.logf("exec completed in %s", time.Since(start).Round(time.Millisecond))

	if runCtx.Err() == context.DeadlineExceeded {
		return -1, true, nil
	}
	if cmd.ProcessState != nil {
		return cmd.ProcessState.ExitCode(), false, nil
	}
	if runErr != nil {
		return -1, false, fmt.Errorf("could not execute %q: %w", r.Spec.Command[0], runErr)
	}
	return 0, false, nil
}

// ProgressInterval is how often the results directory is re-parsed while the
// suite runs. Surefire writes one XML per finished test class, so the master
// sees a long suite advance instead of a 40-minute silence.
const ProgressInterval = 15 * time.Second

// reportProgress posts interim results until the returned stop function is
// called. Only changes are sent.
func (r *Runner) reportProgress(ctx context.Context) func() {
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		t := time.NewTicker(ProgressInterval)
		defer t.Stop()
		last := model.Summary{}
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
			}
			res, err := junit.ParseDir(r.results)
			if err != nil || res.Summary == last {
				continue
			}
			last = res.Summary
			r.post(ctx, &model.RunEvent{
				RunID:    r.Spec.RunID,
				Phase:    "progress",
				NodeID:   r.node.ID,
				NodeName: r.node.Name,
				AllocID:  r.node.AllocID,
				Message:  fmt.Sprintf("%d tests so far, %d failed", res.Summary.Tests, res.Summary.Failed+res.Summary.Errors),
				Summary:  res.Summary,
				Cases:    res.Problems(),
				At:       time.Now().UTC(),
			})
		}
	}()
	return func() { close(done); <-finished }
}

// ---------------------------------------------------------------------------
// artifacts
// ---------------------------------------------------------------------------

// uploadArtifacts pushes everything under the results dir to
// <bucket>/<prefix>, preserving relative paths.
func (r *Runner) uploadArtifacts(ctx context.Context) []model.ArtRef {
	var refs []model.ArtRef
	if r.console != nil {
		r.console.Sync()
	}
	if r.s3 == nil {
		r.logf("WARN no object store configured; artifacts stay in %s", r.results)
		return refs
	}
	prefix := strings.TrimSuffix(r.Spec.S3.Prefix, "/") + "/"
	count := 0
	err := filepath.WalkDir(r.results, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(r.results, p)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		upCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		if uerr := r.s3.PutFile(upCtx, r.Spec.S3.Bucket, prefix+rel, p, s3.GuessContentType(rel)); uerr != nil {
			r.logf("WARN upload %s: %v", rel, uerr)
			return nil
		}
		count++
		refs = append(refs, model.ArtRef{Path: rel, Size: info.Size()})
		return nil
	})
	if err != nil {
		r.logf("WARN walking results: %v", err)
	}
	r.logf("uploaded %d artifacts to s3://%s/%s", count, r.Spec.S3.Bucket, prefix)
	return refs
}

// ---------------------------------------------------------------------------
// master callbacks
// ---------------------------------------------------------------------------

func (r *Runner) notifyStarted(ctx context.Context) {
	r.post(ctx, &model.RunEvent{
		RunID:    r.Spec.RunID,
		Phase:    "started",
		NodeID:   r.node.ID,
		NodeName: r.node.Name,
		AllocID:  r.node.AllocID,
		At:       time.Now().UTC(),
	})
}

func (r *Runner) report(ctx context.Context, state model.RunState, msg string, exit *int,
	sum model.Summary, cases []model.Case, arts []model.ArtRef) {
	r.post(ctx, &model.RunEvent{
		RunID:     r.Spec.RunID,
		Phase:     "finished",
		State:     state,
		NodeID:    r.node.ID,
		NodeName:  r.node.Name,
		AllocID:   r.node.AllocID,
		ExitCode:  exit,
		Message:   msg,
		Summary:   sum,
		Cases:     cases,
		Artifacts: arts,
		At:        time.Now().UTC(),
	})
}

// post retries briefly: losing a result because the master was restarting
// would cost a whole suite run.
func (r *Runner) post(ctx context.Context, ev *model.RunEvent) {
	if r.Spec.MasterURL == "" {
		return
	}
	body, err := json.Marshal(ev)
	if err != nil {
		return
	}
	url := strings.TrimRight(r.Spec.MasterURL, "/") + "/api/v1/runs/" + ev.RunID + "/events"
	var lastErr error
	for attempt := 0; attempt < 6; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(attempt*attempt) * time.Second):
			}
		}
		req, rerr := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if rerr != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+r.Spec.Token)
		resp, derr := r.http.Do(req)
		if derr != nil {
			lastErr = derr
			continue
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if resp.StatusCode < 300 {
			return
		}
		lastErr = fmt.Errorf("master returned %s", resp.Status)
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusNotFound {
			break // not worth retrying
		}
	}
	r.logf("WARN could not report %s to master: %v", ev.Phase, lastErr)
}
