// Package config loads the master's configuration: the pool catalogue, the
// Nomad and object-store endpoints, and the slot sizing that turns "N parallel
// suites per node" into resources Nomad can bin-pack.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/andrei/distributed-test-platform/internal/model"
)

// Slot is the resource envelope of one test slot. A node advertising
// meta.dtp.slots = 4 is expected to be sized at 4x this, so Nomad's own
// bin-packing arrives at the same answer as the master's slot ledger.
type Slot struct {
	CPU    int `json:"cpu"`    // MHz
	Memory int `json:"memory"` // MB
	Disk   int `json:"disk"`   // MB
}

// Pool is a set of interchangeable nodes with one runtime abstraction.
type Pool struct {
	Name        string        `json:"name"`
	Description string        `json:"description,omitempty"`
	Runtime     model.Runtime `json:"runtime"`
	Slot        Slot          `json:"slot"`
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
}

// LocalNode is a simulated node used by the "local" backend, which runs suites
// as child processes of the master. It exists so the platform can be demoed and
// tested without a Nomad cluster.
type LocalNode struct {
	Name  string            `json:"name"`
	Pool  string            `json:"pool"`
	Slots int               `json:"slots"`
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
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
}

type Config struct {
	Listen   string `json:"listen"`
	StateDir string `json:"state_dir"`
	// PublicURL is how a runner on a node reaches this master.
	PublicURL string `json:"public_url"`
	// Backend is "nomad" or "local".
	Backend string `json:"backend"`

	DefaultTimeout model.Duration `json:"default_timeout"`
	DefaultRetries int            `json:"default_retries"`

	Nomad      NomadCfg    `json:"nomad"`
	S3         S3Cfg       `json:"s3"`
	Pools      []Pool      `json:"pools"`
	LocalNodes []LocalNode `json:"local_nodes,omitempty"`
	// LocalRunnerPath is the dtp-runner binary used by the local backend.
	LocalRunnerPath string `json:"local_runner_path,omitempty"`
}

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
	envStr("DTP_PUBLIC_URL", &c.PublicURL)
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
	if len(c.Pools) == 0 {
		return fmt.Errorf("no pools configured")
	}
	names := map[string]bool{}
	for i := range c.Pools {
		p := &c.Pools[i]
		if p.Name == "" {
			return fmt.Errorf("pool %d has no name", i)
		}
		if names[p.Name] {
			return fmt.Errorf("duplicate pool %q", p.Name)
		}
		names[p.Name] = true
		if !p.Runtime.Valid() {
			return fmt.Errorf("pool %q: runtime must be process or container", p.Name)
		}
		if p.Slot.CPU <= 0 {
			p.Slot.CPU = 1000
		}
		if p.Slot.Memory <= 0 {
			p.Slot.Memory = 2048
		}
		if p.Slot.Disk <= 0 {
			p.Slot.Disk = 2048
		}
		if p.CacheDir == "" {
			p.CacheDir = "/var/lib/dtp/cache"
		}
	}
	for _, n := range c.LocalNodes {
		if !names[n.Pool] {
			return fmt.Errorf("local node %q references unknown pool %q", n.Name, n.Pool)
		}
	}
	return nil
}

func (c *Config) Pool(name string) (*Pool, bool) {
	for i := range c.Pools {
		if c.Pools[i].Name == name {
			return &c.Pools[i], true
		}
	}
	return nil, false
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
