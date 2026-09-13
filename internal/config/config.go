// Package config holds the master's process configuration (listen address,
// Nomad and object-store endpoints, the store) and the types of the catalog
// that lives in the store: pools, which pool each node serves, groups, users
// and the quota rules - with their validation and the rule resolution.
//
//	config.go    the process configuration and the catalog's types
//	catalog.go   the Catalog: normalization, validation, lookups and edits
//	quota.go     quota rules: keys, resolution (UserCap, TogetherRules), validation
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/andrei/distributed-test-platform/internal/model"
)

// Slot is the resource envelope of one test slot. It is declared by each
// node (node.yaml: slots + slot), not by the pool: nodes in one pool may be
// sized differently, and the master pins every suite to a node it chose.
type Slot = model.Slot

// Pool is a set of interchangeable nodes with one runtime abstraction.
type Pool struct {
	Name        string        `json:"name"`
	Description string        `json:"description,omitempty"`
	Runtime     model.Runtime `json:"runtime"`
	// TaskDriver overrides the Nomad driver implied by Runtime
	// ("exec"/"raw_exec" for process, "docker"/"podman" for container).
	TaskDriver string `json:"task_driver,omitempty"`

	// Default.Command is the SUITE command, executed by dtp-runner inside the
	// allocation. Default.Image is the container image for container pools.
	Default struct {
		Image   string   `json:"image,omitempty"`
		Command []string `json:"command,omitempty"`
	} `json:"default"`

	// RunnerCommand is what the Nomad TASK launches: the dtp-runner binary.
	// Leave empty for a container image whose entrypoint is already dtp-runner.
	RunnerCommand []string `json:"runner_command,omitempty"`

	// DockerNetwork attaches container tasks to an existing docker network,
	// which is how a containerized Nomad client reaches the master and the
	// object store by service name.
	DockerNetwork string `json:"docker_network,omitempty"`

	// Constraints merged into every job placed here, on top of the suite's
	// own "requires" map. Keys are Nomad node meta keys.
	Constraints map[string]string `json:"constraints,omitempty"`

	// RunnerURL is where a process-runtime task fetches dtp-runner from when
	// the node image does not already ship it: any URL Nomad's artifact stanza
	// can get. Build it with `go build ./cmd/dtp-runner` and publish it.
	RunnerURL string `json:"runner_url,omitempty"`

	// CacheDir on the node for the content-addressed build cache. For the
	// process runtime this must be a host path shared across allocations.
	CacheDir string `json:"cache_dir,omitempty"`

	// HostVolume, when set, is the Nomad host volume mounted at CacheDir so the
	// build cache survives allocations (container runtime).
	HostVolume string `json:"host_volume,omitempty"`

	// Spans makes this a virtual pool: a suite submitted here may land on any
	// node of the named pools ("global" spanning every production pool is the
	// typical use). Members must share runtime and driver; the other settings
	// are inherited from the first member when left empty.
	Spans []string `json:"spans,omitempty"`
}

// Node is the master's record of a worker: which pool it serves. Everything
// else about a node - its labels, slot count and slot size - comes from the
// node itself (dtp-node publishes node.yaml as meta.dtp.*). A node the master
// has never been told about is "unassigned" and gets no work.
type Node struct {
	Name string `json:"name"`
	Pool string `json:"pool,omitempty"` // "" = unassigned
}

// IsVirtual reports whether the pool is a span over other pools rather than
// one with nodes of its own.
func (p *Pool) IsVirtual() bool { return len(p.Spans) > 0 }

// NodePools lists the concrete pools whose nodes can run this pool's suites:
// the members of a virtual pool, or the pool itself.
func (p *Pool) NodePools() []string {
	if p.IsVirtual() {
		return p.Spans
	}
	return []string{p.Name}
}

// Group is a named set of users. Groups are what quota rules and, later,
// anything else that addresses "these people" refer to; a group itself
// carries no limits.
type Group struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// User is an identity slots are charged to. A user may belong to any number
// of groups.
type User struct {
	Name        string   `json:"name"`
	DisplayName string   `json:"display_name,omitempty"`
	Email       string   `json:"email,omitempty"`
	Groups      []string `json:"groups,omitempty"`
}

// LocalNode is a simulated node used by the "local" backend, which runs suites
// as child processes of the master. It exists so the platform can be demoed and
// tested without a Nomad cluster.
type LocalNode struct {
	Name  string            `json:"name"`
	Slots int               `json:"slots"`
	Slot  Slot              `json:"slot"`
	Meta  map[string]string `json:"meta,omitempty"`
}

type NomadCfg struct {
	Address   string `json:"address"`
	Region    string `json:"region,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Token     string `json:"token,omitempty"`
	// Datacenters placed into every generated job.
	Datacenters []string `json:"datacenters,omitempty"`
}

type S3Cfg struct {
	Endpoint  string `json:"endpoint"`
	PublicURL string `json:"public_url,omitempty"` // browser-reachable endpoint
	Region    string `json:"region,omitempty"`
	Bucket    string `json:"bucket"`
	// BuildsBucket is where build payloads are published (scripts/build-egit.sh,
	// scripts/seed-build.sh); the dashboard lists it when composing a submission.
	BuildsBucket string `json:"builds_bucket,omitempty"`
	AccessKey    string `json:"access_key"`
	SecretKey    string `json:"secret_key"`
}

// StoreCfg selects where regressions and runs are persisted: "file" (one JSON
// per regression under state_dir, the default) or "postgres".
type StoreCfg struct {
	Driver string `json:"driver,omitempty"`
	DSN    string `json:"dsn,omitempty"` // postgres://user:pass@host:5432/db?sslmode=disable
}

type Config struct {
	Listen   string   `json:"listen"`
	StateDir string   `json:"state_dir"`
	Store    StoreCfg `json:"store,omitempty"`
	// TemplatesDir holds submission JSON files the dashboard offers as
	// starting points (the repo's examples/ in the demo stacks).
	TemplatesDir string `json:"templates_dir,omitempty"`
	// PublicURL is how a runner on a node reaches this master.
	PublicURL string `json:"public_url"`
	// RunnerBinary is the dtp-runner the master hands to node agents
	// (GET /api/v1/runner). Default: a dtp-runner next to the master binary.
	RunnerBinary string `json:"runner_binary,omitempty"`
	// Backend is "nomad" or "local".
	Backend string `json:"backend"`

	DefaultTimeout model.Duration `json:"default_timeout"`
	DefaultRetries int            `json:"default_retries"`

	Nomad NomadCfg `json:"nomad"`
	S3    S3Cfg    `json:"s3"`

	LocalNodes []LocalNode `json:"local_nodes,omitempty"`
	// LocalRunnerPath is the dtp-runner binary used by the local backend.
	LocalRunnerPath string `json:"local_runner_path,omitempty"`

	// The catalog - pools, nodes, groups, users, quota rules - is not in this file. It lives in
	// the store (PostgreSQL tables, or catalog.json for the file store), is
	// loaded at startup and reloaded on request, and is edited through the
	// API, the dashboard or SQL. Read it through Catalog() and Pool().
	raw  atomic.Pointer[Catalog] // as authored (what the store holds)
	live atomic.Pointer[Catalog] // normalized: defaults filled, virtual pools inherited
}

// Catalog returns the live, normalized pools and quotas. The snapshot is
// immutable; a tick or a request should take it once and use it throughout.
func (c *Config) Catalog() *Catalog { return c.live.Load() }

// RawCatalog returns the catalog as authored, for editing and for change
// detection against the store.
func (c *Config) RawCatalog() *Catalog { return c.raw.Load() }

// SetCatalog validates a new catalog and, if it passes, makes it live. A bad
// catalog is rejected and the previous one stays in force.
func (c *Config) SetCatalog(raw *Catalog) error {
	live, err := c.CheckCatalog(raw)
	if err != nil {
		return err
	}
	c.raw.Store(raw.Clone())
	c.live.Store(live)
	return nil
}

// CheckCatalog normalizes a catalog without making it live. (Simulated local
// nodes that name a pool the catalog lacks are simply not inventoried, so a
// catalog may be applied before or after the nodes that use it.)
func (c *Config) CheckCatalog(raw *Catalog) (*Catalog, error) {
	return raw.Normalize()
}

// Pool looks a pool up in the live catalog.
func (c *Config) Pool(name string) (*Pool, bool) { return c.Catalog().Pool(name) }

func Default() *Config {
	c := &Config{
		Listen:          ":8080",
		StateDir:        "/var/lib/dtp",
		PublicURL:       "http://127.0.0.1:8080",
		Backend:         "local",
		DefaultTimeout:  model.Duration(30 * time.Minute),
		DefaultRetries:  0,
		LocalRunnerPath: "dtp-runner",
	}
	c.Nomad.Address = "http://127.0.0.1:4646"
	c.Nomad.Datacenters = []string{"dc1"}
	c.S3.Region = "us-east-1"
	c.S3.Bucket = "dtp"
	c.S3.BuildsBucket = "builds"
	return c
}

// Load reads a JSON config file (when path is non-empty) and then applies
// DTP_* environment overrides, which is what the compose stack uses.
func Load(path string) (*Config, error) {
	c := Default()
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		// ${VAR} references are expanded from the environment, so one config
		// file works across hosts (paths, endpoints, credentials).
		if err := json.Unmarshal([]byte(os.ExpandEnv(string(b))), c); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	envStr("DTP_LISTEN", &c.Listen)
	envStr("DTP_STATE_DIR", &c.StateDir)
	envStr("DTP_STORE_DRIVER", &c.Store.Driver)
	if v := os.Getenv("DTP_STORE_DSN"); v != "" {
		c.Store.Driver, c.Store.DSN = "postgres", v
	}
	envStr("DTP_PUBLIC_URL", &c.PublicURL)
	envStr("DTP_RUNNER_BINARY", &c.RunnerBinary)
	envStr("DTP_BACKEND", &c.Backend)
	envStr("DTP_LOCAL_RUNNER", &c.LocalRunnerPath)
	envStr("DTP_NOMAD_ADDR", &c.Nomad.Address)
	envStr("DTP_NOMAD_TOKEN", &c.Nomad.Token)
	envStr("DTP_NOMAD_NAMESPACE", &c.Nomad.Namespace)
	if v := os.Getenv("DTP_NOMAD_DATACENTERS"); v != "" {
		c.Nomad.Datacenters = strings.Split(v, ",")
	}
	envStr("DTP_S3_ENDPOINT", &c.S3.Endpoint)
	envStr("DTP_S3_PUBLIC_URL", &c.S3.PublicURL)
	envStr("DTP_S3_REGION", &c.S3.Region)
	envStr("DTP_S3_BUCKET", &c.S3.Bucket)
	envStr("DTP_S3_BUILDS_BUCKET", &c.S3.BuildsBucket)
	envStr("DTP_TEMPLATES_DIR", &c.TemplatesDir)
	envStr("DTP_S3_ACCESS_KEY", &c.S3.AccessKey)
	envStr("DTP_S3_SECRET_KEY", &c.S3.SecretKey)
	if v := os.Getenv("DTP_DEFAULT_RETRIES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.DefaultRetries = n
		}
	}
	return c, c.Validate()
}

func envStr(key string, dst *string) {
	if v := os.Getenv(key); v != "" {
		*dst = v
	}
}

func (c *Config) Validate() error {
	if c.Backend != "nomad" && c.Backend != "local" {
		return fmt.Errorf("backend must be nomad or local, got %q", c.Backend)
	}
	switch c.Store.Driver {
	case "", "file":
		c.Store.Driver = "file"
	case "postgres":
		if c.Store.DSN == "" {
			return fmt.Errorf("store.driver is postgres but store.dsn is empty")
		}
	default:
		return fmt.Errorf("store.driver must be file or postgres, got %q", c.Store.Driver)
	}
	// Start with an empty catalog; the store fills it in.
	return c.SetCatalog(&Catalog{})
}

// RunnerCmd is the command Nomad launches for a task in this pool.
func (p *Pool) RunnerCmd() []string {
	if len(p.RunnerCommand) > 0 {
		return p.RunnerCommand
	}
	if p.Runtime == model.RuntimeProcess {
		if p.RunnerURL != "" {
			return []string{"local/dtp-runner"} // fetched by the artifact stanza
		}
		return []string{"dtp-runner"}
	}
	return nil // container: rely on the image entrypoint
}

// Driver returns the Nomad task driver for this pool. Operators may pin it
// explicitly (exec vs raw_exec, podman vs docker); otherwise the runtime
// abstraction picks the obvious one.
func (p *Pool) Driver() string {
	if p.TaskDriver != "" {
		return p.TaskDriver
	}
	if p.Runtime == model.RuntimeContainer {
		return "docker"
	}
	return "exec"
}
