// Package model holds the wire and persistence types shared by the master,
// the CLI and the node-side runner.
package model

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Runtime is the execution abstraction a suite asks for.
//
//	RuntimeProcess   -> isolated directory + process cgroup (Nomad exec/raw_exec driver)
//	RuntimeContainer -> OCI container (Nomad docker driver)
type Runtime string

const (
	RuntimeProcess   Runtime = "process"
	RuntimeContainer Runtime = "container"
)

func (r Runtime) Valid() bool { return r == RuntimeProcess || r == RuntimeContainer }

// RunState is the lifecycle of a single suite attempt.
type RunState string

const (
	RunQueued     RunState = "queued"     // admitted, waiting for a free slot
	RunDispatched RunState = "dispatched" // handed to the backend, not started yet
	RunRunning    RunState = "running"    // runner reported start
	RunPassed     RunState = "passed"
	RunFailed     RunState = "failed"  // tests failed
	RunErrored    RunState = "errored" // infrastructure/harness failure
	RunTimeout    RunState = "timeout"
	RunCanceled   RunState = "canceled"
)

func (s RunState) Terminal() bool {
	switch s {
	case RunPassed, RunFailed, RunErrored, RunTimeout, RunCanceled:
		return true
	}
	return false
}

// Retryable reports whether a failed attempt is worth another slot.
func (s RunState) Retryable() bool {
	return s == RunFailed || s == RunErrored || s == RunTimeout
}

// RegressionState is the rollup of every suite in a submission.
type RegressionState string

const (
	RegPending  RegressionState = "pending"
	RegRunning  RegressionState = "running"
	RegPassed   RegressionState = "passed"
	RegFailed   RegressionState = "failed"
	RegErrored  RegressionState = "errored"
	RegCanceled RegressionState = "canceled"
)

// ---------------------------------------------------------------------------
// Submission (the JSON a user POSTs)
// ---------------------------------------------------------------------------

// BuildArtifact is one payload fetched onto the node and unpacked into the
// content-addressed build cache. Reused across every suite that names the same
// sha256, so a 400MB RCP product is downloaded once per node per build.
type BuildArtifact struct {
	Name   string `json:"name"`             // cache/dir name, e.g. "product"
	URL    string `json:"url"`              // http(s):// or s3://bucket/key
	SHA256 string `json:"sha256,omitempty"` // required for caching; verified on fetch
	Unpack string `json:"unpack,omitempty"` // "", "zip", "tar.gz", "tar", "auto"
}

// SuiteSpec is one Eclipse RCP test suite: the atomic unit of distribution.
// A suite occupies exactly one slot on exactly one node for its whole run.
type SuiteSpec struct {
	Name     string            `json:"name"`
	Pool     string            `json:"pool"`
	Runtime  Runtime           `json:"runtime,omitempty"`
	Image    string            `json:"image,omitempty"`    // container runtime only
	Command  []string          `json:"command,omitempty"`  // overrides the pool default
	Requires map[string]string `json:"requires,omitempty"` // -> Nomad node meta constraints
	Build    []BuildArtifact   `json:"build,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	Timeout  Duration          `json:"timeout,omitempty"`
	Retries  *int              `json:"retries,omitempty"`
}

// SuiteDefaults is merged underneath every suite in the submission.
type SuiteDefaults struct {
	Pool     string            `json:"pool,omitempty"`
	Runtime  Runtime           `json:"runtime,omitempty"`
	Image    string            `json:"image,omitempty"`
	Command  []string          `json:"command,omitempty"`
	Requires map[string]string `json:"requires,omitempty"`
	Build    []BuildArtifact   `json:"build,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	Timeout  Duration          `json:"timeout,omitempty"`
	Retries  *int              `json:"retries,omitempty"`
}

// Submission is the document handed to POST /api/v1/regressions.
type Submission struct {
	RegressionID string            `json:"regression_id,omitempty"` // generated when empty
	Name         string            `json:"name,omitempty"`
	Priority     int               `json:"priority,omitempty"` // higher runs first
	Labels       map[string]string `json:"labels,omitempty"`
	Defaults     SuiteDefaults     `json:"defaults,omitempty"`
	Suites       []SuiteSpec       `json:"suites"`
}

// Normalize applies defaults to every suite and validates the result.
func (s *Submission) Normalize() error {
	if len(s.Suites) == 0 {
		return fmt.Errorf("submission has no suites")
	}
	seen := map[string]bool{}
	for i := range s.Suites {
		su := &s.Suites[i]
		if su.Name == "" {
			return fmt.Errorf("suite %d: name is required", i)
		}
		if seen[su.Name] {
			return fmt.Errorf("duplicate suite name %q", su.Name)
		}
		seen[su.Name] = true

		if su.Pool == "" {
			su.Pool = s.Defaults.Pool
		}
		if su.Pool == "" {
			return fmt.Errorf("suite %q: pool is required", su.Name)
		}
		if su.Runtime == "" {
			su.Runtime = s.Defaults.Runtime
		}
		if su.Image == "" {
			su.Image = s.Defaults.Image
		}
		if len(su.Command) == 0 {
			su.Command = s.Defaults.Command
		}
		if len(su.Build) == 0 {
			su.Build = s.Defaults.Build
		}
		if su.Timeout == 0 {
			su.Timeout = s.Defaults.Timeout
		}
		if su.Retries == nil {
			su.Retries = s.Defaults.Retries
		}
		su.Requires = mergeMap(s.Defaults.Requires, su.Requires)
		su.Env = mergeMap(s.Defaults.Env, su.Env)

		if su.Runtime != "" && !su.Runtime.Valid() {
			return fmt.Errorf("suite %q: unknown runtime %q", su.Name, su.Runtime)
		}
		for j, b := range su.Build {
			if b.URL == "" {
				return fmt.Errorf("suite %q: build[%d] needs a url", su.Name, j)
			}
			if b.Name == "" {
				return fmt.Errorf("suite %q: build[%d] needs a name", su.Name, j)
			}
		}
	}
	return nil
}

func mergeMap(base, over map[string]string) map[string]string {
	if base == nil && over == nil {
		return nil
	}
	out := make(map[string]string, len(base)+len(over))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}

// MaxAttempts is 1 + retries.
func (su SuiteSpec) MaxAttempts() int {
	if su.Retries == nil {
		return 1
	}
	if *su.Retries < 0 {
		return 1
	}
	return *su.Retries + 1
}

// ---------------------------------------------------------------------------
// Stored entities
// ---------------------------------------------------------------------------

// Regression is one submission plus its rollup.
type Regression struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Priority    int               `json:"priority"`
	Labels      map[string]string `json:"labels,omitempty"`
	State       RegressionState   `json:"state"`
	SubmittedAt time.Time         `json:"submitted_at"`
	StartedAt   *time.Time        `json:"started_at,omitempty"`
	FinishedAt  *time.Time        `json:"finished_at,omitempty"`
	Suites      []SuiteSpec       `json:"suites"`
	Totals      Totals            `json:"totals"`
	ArtifactURI string            `json:"artifact_uri"`
}

// Totals is the test-case rollup across every counted attempt.
type Totals struct {
	Suites        int `json:"suites"`
	SuitesPassed  int `json:"suites_passed"`
	SuitesFailed  int `json:"suites_failed"`
	SuitesErrored int `json:"suites_errored"`
	SuitesRunning int `json:"suites_running"`
	SuitesQueued  int `json:"suites_queued"`
	Tests         int `json:"tests"`
	Passed        int `json:"passed"`
	Failed        int `json:"failed"`
	Skipped       int `json:"skipped"`
	Flaky         int `json:"flaky"`
}

// Run is one attempt of one suite.
type Run struct {
	ID           string     `json:"id"`
	RegressionID string     `json:"regression_id"`
	Suite        string     `json:"suite"`
	Attempt      int        `json:"attempt"`
	MaxAttempts  int        `json:"max_attempts"`
	Pool         string     `json:"pool"`
	Runtime      Runtime    `json:"runtime"`
	State        RunState   `json:"state"`
	Priority     int        `json:"priority"`
	QueuedAt     time.Time  `json:"queued_at"`
	DispatchedAt *time.Time `json:"dispatched_at,omitempty"`
	StartedAt    *time.Time `json:"started_at,omitempty"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`

	// Placement, filled in by the backend.
	BackendID string `json:"backend_id,omitempty"` // Nomad job id / local pid tag
	AllocID   string `json:"alloc_id,omitempty"`
	NodeID    string `json:"node_id,omitempty"`
	NodeName  string `json:"node_name,omitempty"`

	ExitCode    *int      `json:"exit_code,omitempty"`
	Message     string    `json:"message,omitempty"`
	Summary     Summary   `json:"summary"`
	Cases       []Case    `json:"cases,omitempty"` // failures/errors only
	ArtifactURI string    `json:"artifact_uri,omitempty"`
	Artifacts   []ArtRef  `json:"artifacts,omitempty"`
	Token       string    `json:"-"` // runner callback bearer token
	Spec        SuiteSpec `json:"-"`
}

func (r *Run) Duration() time.Duration {
	if r.StartedAt == nil {
		return 0
	}
	end := time.Now()
	if r.FinishedAt != nil {
		end = *r.FinishedAt
	}
	return end.Sub(*r.StartedAt)
}

// ArtRef is one file stored under the run's result prefix.
type ArtRef struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// Summary is the parsed JUnit rollup for one attempt.
type Summary struct {
	Tests    int     `json:"tests"`
	Passed   int     `json:"passed"`
	Failed   int     `json:"failed"`
	Errors   int     `json:"errors"`
	Skipped  int     `json:"skipped"`
	Duration float64 `json:"duration_seconds"`
}

// Case is a single test case result.
type Case struct {
	Class    string  `json:"class"`
	Name     string  `json:"name"`
	Status   string  `json:"status"` // passed|failed|error|skipped
	Duration float64 `json:"duration_seconds"`
	Message  string  `json:"message,omitempty"`
	Details  string  `json:"details,omitempty"`
}

// ---------------------------------------------------------------------------
// Runner contract
// ---------------------------------------------------------------------------

// S3Target tells the runner where to push artifacts.
type S3Target struct {
	Endpoint  string `json:"endpoint"`
	Region    string `json:"region"`
	Bucket    string `json:"bucket"`
	Prefix    string `json:"prefix"`
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
}

// RunSpec is the self-contained job description handed to dtp-runner on the
// node. It is passed as base64 JSON in DTP_RUN_SPEC (or a Nomad dispatch
// payload file) so the runner needs no other configuration.
type RunSpec struct {
	RunID        string            `json:"run_id"`
	RegressionID string            `json:"regression_id"`
	Suite        string            `json:"suite"`
	Attempt      int               `json:"attempt"`
	Build        []BuildArtifact   `json:"build,omitempty"`
	Command      []string          `json:"command"`
	Env          map[string]string `json:"env,omitempty"`
	Timeout      Duration          `json:"timeout"`
	CacheDir     string            `json:"cache_dir"`
	S3           S3Target          `json:"s3"`
	MasterURL    string            `json:"master_url"`
	Token        string            `json:"token"`
}

// RunEvent is what the runner POSTs back to the master.
type RunEvent struct {
	RunID     string    `json:"run_id"`
	Phase     string    `json:"phase"` // "started" | "finished"
	State     RunState  `json:"state,omitempty"`
	NodeID    string    `json:"node_id,omitempty"`
	NodeName  string    `json:"node_name,omitempty"`
	AllocID   string    `json:"alloc_id,omitempty"`
	ExitCode  *int      `json:"exit_code,omitempty"`
	Message   string    `json:"message,omitempty"`
	Summary   Summary   `json:"summary"`
	Cases     []Case    `json:"cases,omitempty"`
	Artifacts []ArtRef  `json:"artifacts,omitempty"`
	At        time.Time `json:"at"`
}

// ---------------------------------------------------------------------------
// Duration: a time.Duration that marshals as "20m" in JSON.
// ---------------------------------------------------------------------------

type Duration time.Duration

func (d Duration) Duration() time.Duration { return time.Duration(d) }
func (d Duration) String() string          { return time.Duration(d).String() }

func (d Duration) MarshalJSON() ([]byte, error) {
	if d == 0 {
		return []byte(`""`), nil
	}
	return json.Marshal(time.Duration(d).String())
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	switch x := v.(type) {
	case float64: // bare number == seconds
		*d = Duration(time.Duration(x) * time.Second)
	case string:
		if strings.TrimSpace(x) == "" {
			*d = 0
			return nil
		}
		p, err := time.ParseDuration(x)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", x, err)
		}
		*d = Duration(p)
	case nil:
		*d = 0
	default:
		return fmt.Errorf("invalid duration %v", v)
	}
	return nil
}
