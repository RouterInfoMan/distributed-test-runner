// Package agent is the node side of the platform beyond the per-suite runner:
// dtp-node reads node.yaml, works out what the machine is (os, arch, cores,
// memory, display, Java, …), takes the slot count and slot size the operator
// wrote there (checking that they fit the machine), and pushes the result
// into the local Nomad client as dynamic node metadata - the meta.dtp.*
// labels the master schedules on - without editing HCL or restarting
// anything. As a daemon it keeps those current, heartbeats to the master,
// installs the runner binary the master serves, and prunes the build cache.
//
// Which pool the node serves is not decided here: that is an assignment in
// the master's catalog (dashboard, `dtp nodes assign`, or SQL). node.yaml
// may suggest a pool for the first registration, nothing more.
package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/andrei/distributed-test-platform/internal/model"
	"github.com/andrei/distributed-test-platform/internal/yamlite"
)

// Version is stamped into the "agent" label.
const Version = "0.4"

// Config is node.yaml.
type Config struct {
	Master string `json:"master"` // the master's URL, e.g. http://master:8080
	Nomad  string `json:"nomad"`  // the local Nomad client's HTTP API (default http://127.0.0.1:4646)
	Name   string `json:"name"`   // this node's name as Nomad knows it (default: hostname)
	Pool   string `json:"pool"`   // optional: the pool to join when the master first sees this node

	// Slots is how many suites this node runs at once and Slot is what each
	// of them gets. Both are decisions, not measurements: the operator sets
	// them, the agent only checks they fit the machine.
	Slots int      `json:"slots"`
	Slot  SlotSpec `json:"slot"`

	// Labels become meta.dtp.<key>; they override detected ones.
	Labels map[string]string `json:"labels"`

	CacheDir  string         `json:"cache_dir"`  // build cache to prune (default /var/lib/dtp/cache)
	CacheKeep model.Duration `json:"cache_keep"` // prune payloads unused for longer than this (0 = never)
	RunnerDir string         `json:"runner_dir"` // install dtp-runner from the master here ("" = don't)
	Interval  model.Duration `json:"interval"`   // re-detect, re-apply and heartbeat period (default 60s)

	// For `dtp-node nomad-config`: what a fresh client HCL should say.
	NomadServers []string `json:"nomad_servers"`
	Datacenter   string   `json:"datacenter"`
	DataDir      string   `json:"data_dir"`

	Path string `json:"-"` // where this config was read from, for live reload
}

// SlotSpec is the slot block of node.yaml, keyed by what the numbers mean:
//
//	slot:
//	  cores: 2            # a cpuset of whole cores (or cpu_mhz: 2000 for shares)
//	  memory_mb: 4096     # reserved; the hard ceiling unless memory_max_mb is set
//	  memory_max_mb: 8192 # burst ceiling (Nomad memory oversubscription)
//	  disk_mb: 2048       # ephemeral disk for the task
type SlotSpec struct {
	Cores       int `json:"cores"`
	CPUMHz      int `json:"cpu_mhz"`
	MemoryMB    int `json:"memory_mb"`
	MemoryMaxMB int `json:"memory_max_mb"`
	DiskMB      int `json:"disk_mb"`
}

// Slot is the spec as the master and backends see it.
func (s SlotSpec) Slot() model.Slot {
	return model.Slot{Cores: s.Cores, CPU: s.CPUMHz, Memory: s.MemoryMB, MemoryMax: s.MemoryMaxMB, Disk: s.DiskMB}
}

func (s SlotSpec) String() string { return s.Slot().String() }

// Load reads node.yaml (or JSON) and applies defaults.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c := &Config{Path: path}
	if err := yamlite.Unmarshal([]byte(os.ExpandEnv(string(b))), c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if c.Master == "" {
		return nil, fmt.Errorf("%s: master is required", path)
	}
	if c.Slots <= 0 {
		return nil, fmt.Errorf("%s: slots is required (how many suites this node runs at once)", path)
	}
	if c.Slot.Cores <= 0 && c.Slot.CPUMHz <= 0 {
		return nil, fmt.Errorf("%s: slot.cores (or slot.cpu_mhz) is required (what one suite gets)", path)
	}
	if c.Slot.MemoryMB <= 0 {
		return nil, fmt.Errorf("%s: slot.memory_mb is required", path)
	}
	if c.Slot.DiskMB <= 0 {
		c.Slot.DiskMB = 2048
	}
	if c.Slot.MemoryMaxMB > 0 && c.Slot.MemoryMaxMB < c.Slot.MemoryMB {
		return nil, fmt.Errorf("%s: slot.memory_max_mb (%d) is below slot.memory_mb (%d)", path, c.Slot.MemoryMaxMB, c.Slot.MemoryMB)
	}
	if c.Nomad == "" {
		c.Nomad = "http://127.0.0.1:4646"
	}
	if c.Name == "" {
		c.Name, _ = os.Hostname()
	}
	if c.CacheDir == "" {
		c.CacheDir = "/var/lib/dtp/cache"
	}
	if c.Interval == 0 {
		c.Interval = model.Duration(60 * time.Second)
	}
	if c.Datacenter == "" {
		c.Datacenter = "dc1"
	}
	if c.DataDir == "" {
		c.DataDir = "/opt/nomad/data"
	}
	if c.Labels == nil {
		c.Labels = map[string]string{}
	}
	return c, nil
}

// ---------------------------------------------------------------------------
// what the node is
// ---------------------------------------------------------------------------

// Facts is what the agent found out about the machine.
type Facts struct {
	Hostname string
	OS, Arch string
	Cores    int // usable by Nomad tasks (reservable cores), else the machine's
	MemoryMB int // what Nomad schedules against, else the machine's
	Display  string
	Java     string // major version, "" when absent
	Docker   bool
	Kernel   string
	NodeID   string // Nomad node id, when the local client answered
}

// Detect gathers facts, preferring what the local Nomad client fingerprinted
// (that is what the scheduler will place against) over /proc.
func Detect(ctx context.Context, cfg *Config) Facts {
	f := Facts{OS: runtime.GOOS, Arch: archName(runtime.GOARCH), Cores: runtime.NumCPU(), MemoryMB: machineMemoryMB()}
	f.Hostname, _ = os.Hostname()
	if id, node, err := nomadNode(ctx, cfg.Nomad); err == nil {
		f.NodeID = id
		if n := len(node.NodeResources.Cpu.ReservableCpuCores); n > 0 {
			f.Cores = n
		}
		if node.NodeResources.Memory.MemoryMB > 0 {
			f.MemoryMB = int(node.NodeResources.Memory.MemoryMB)
		}
		if k := node.Attributes["kernel.name"]; k != "" {
			f.OS = k
		}
		if v := node.Attributes["kernel.version"]; v != "" {
			f.Kernel = v
		}
	}
	switch {
	case os.Getenv("DISPLAY") != "":
		f.Display = "x11"
	case lookPath("Xvfb"):
		f.Display = "xvfb"
	default:
		f.Display = "none"
	}
	f.Java = javaMajor()
	if _, err := os.Stat("/var/run/docker.sock"); err == nil || lookPath("docker") {
		f.Docker = true
	}
	return f
}

func archName(goarch string) string {
	switch goarch {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	}
	return goarch
}

func lookPath(bin string) bool { _, err := exec.LookPath(bin); return err == nil }

var javaVersionRE = regexp.MustCompile(`version "(\d+)(?:\.(\d+))?`)

// javaMajor runs `java -version` and returns the major version ("21", "8").
func javaMajor() string {
	out, err := exec.Command("java", "-version").CombinedOutput()
	if err != nil {
		return ""
	}
	m := javaVersionRE.FindSubmatch(out)
	if m == nil {
		return ""
	}
	if string(m[1]) == "1" && len(m[2]) > 0 { // "1.8.0_292"
		return string(m[2])
	}
	return string(m[1])
}

func machineMemoryMB() int {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				kb, _ := strconv.Atoi(fields[1])
				return kb / 1024
			}
		}
	}
	return 0
}

// ---------------------------------------------------------------------------
// labels and slots
// ---------------------------------------------------------------------------

// Labels merges detected facts with the configured labels (configured wins).
// Keys are without the meta.dtp. prefix.
func Labels(cfg *Config, f Facts) map[string]string {
	l := map[string]string{
		"os":        f.OS,
		"arch":      f.Arch,
		"cores":     strconv.Itoa(f.Cores),
		"memory_mb": strconv.Itoa(f.MemoryMB),
		"display":   f.Display,
		"agent":     Version,
		"slots":     strconv.Itoa(cfg.Slots),
	}
	// The slot size travels as labels too (meta.dtp.slot.*): the master reads
	// it from the node, the same way it reads the count.
	if cfg.Slot.Cores > 0 {
		l["slot.cores"] = strconv.Itoa(cfg.Slot.Cores)
	}
	if cfg.Slot.CPUMHz > 0 {
		l["slot.cpu_mhz"] = strconv.Itoa(cfg.Slot.CPUMHz)
	}
	l["slot.memory_mb"] = strconv.Itoa(cfg.Slot.MemoryMB)
	if cfg.Slot.MemoryMaxMB > 0 {
		l["slot.memory_max_mb"] = strconv.Itoa(cfg.Slot.MemoryMaxMB)
	}
	l["slot.disk_mb"] = strconv.Itoa(cfg.Slot.DiskMB)
	if f.Java != "" {
		l["java"] = f.Java
	}
	if f.Docker {
		l["docker"] = "true"
	}
	for k, v := range cfg.Labels {
		l[k] = v
	}
	return l
}

// CheckCapacity reports whether slots × slot fits the node's fingerprint. It
// never changes either; it gives the operator a reason to revisit node.yaml.
func CheckCapacity(cfg *Config, f Facts) string {
	slot := cfg.Slot.Slot()
	var problems []string
	if slot.Cores > 0 && f.Cores > 0 && cfg.Slots*slot.Cores > f.Cores {
		problems = append(problems, fmt.Sprintf("%d slots × %d cores = %d, but Nomad reports %d reservable cores",
			cfg.Slots, slot.Cores, cfg.Slots*slot.Cores, f.Cores))
	}
	if slot.Memory > 0 && f.MemoryMB > 0 && cfg.Slots*slot.Memory > f.MemoryMB {
		problems = append(problems, fmt.Sprintf("%d slots × %d MB = %d MB, but Nomad reports %d MB",
			cfg.Slots, slot.Memory, cfg.Slots*slot.Memory, f.MemoryMB))
	}
	return strings.Join(problems, "; ")
}

// ---------------------------------------------------------------------------
// the master
// ---------------------------------------------------------------------------

// Heartbeat is what the agent reports; the master answers with the pool it
// assigned the node to and the runner checksum.
type Heartbeat struct {
	Name         string            `json:"name"`
	Pool         string            `json:"pool,omitempty"` // node.yaml's suggestion, used once
	NodeID       string            `json:"node_id,omitempty"`
	Labels       map[string]string `json:"labels"`
	Slots        int               `json:"slots"`
	Slot         model.Slot        `json:"slot"`
	AgentVersion string            `json:"agent_version"`
	CacheEntries int               `json:"cache_entries"`
	CacheBytes   int64             `json:"cache_bytes"`
	At           time.Time         `json:"at"`
}

type HeartbeatReply struct {
	Pool         string `json:"pool"`          // the pool the catalog has this node in ("" = unassigned)
	RunnerSHA256 string `json:"runner_sha256"` // "" when the master serves no runner
}

var httpc = &http.Client{Timeout: 30 * time.Second}

// SendHeartbeat reports to the master.
func SendHeartbeat(ctx context.Context, cfg *Config, hb Heartbeat) (HeartbeatReply, error) {
	var reply HeartbeatReply
	body, _ := json.Marshal(hb)
	url := strings.TrimRight(cfg.Master, "/") + "/api/v1/nodes/" + hb.Name + "/heartbeat"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return reply, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpc.Do(req)
	if err != nil {
		return reply, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return reply, fmt.Errorf("master: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return reply, json.NewDecoder(resp.Body).Decode(&reply)
}

// EnsureRunner installs or updates <runner_dir>/dtp-runner from the master
// when its sha256 differs. Returns true when it changed something.
func EnsureRunner(ctx context.Context, cfg *Config, want string) (bool, error) {
	if cfg.RunnerDir == "" || want == "" {
		return false, nil
	}
	dst := filepath.Join(cfg.RunnerDir, "dtp-runner")
	if have, _ := fileSHA256(dst); have == want {
		return false, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(cfg.Master, "/")+"/api/v1/runner", nil)
	if err != nil {
		return false, err
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("master: %s", resp.Status)
	}
	if err := os.MkdirAll(cfg.RunnerDir, 0o755); err != nil {
		return false, err
	}
	tmp := dst + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return false, err
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return false, err
	}
	f.Close()
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		os.Remove(tmp)
		return false, fmt.Errorf("runner download sha256 %s, master announced %s", got[:12], want[:12])
	}
	return true, os.Rename(tmp, dst)
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ---------------------------------------------------------------------------
// the local Nomad client
// ---------------------------------------------------------------------------

type nomadNodeView struct {
	Attributes    map[string]string `json:"Attributes"`
	NodeResources struct {
		Cpu struct {
			ReservableCpuCores []int `json:"ReservableCpuCores"`
		} `json:"Cpu"`
		Memory struct {
			MemoryMB int64 `json:"MemoryMB"`
		} `json:"Memory"`
	} `json:"NodeResources"`
}

// nomadNode resolves the local client's node id (agent/self) and reads the
// node as the servers see it.
func nomadNode(ctx context.Context, addr string) (string, *nomadNodeView, error) {
	var self struct {
		Stats struct {
			Client map[string]string `json:"client"`
		} `json:"stats"`
	}
	if err := getJSON(ctx, strings.TrimRight(addr, "/")+"/v1/agent/self", &self); err != nil {
		return "", nil, err
	}
	id := self.Stats.Client["node_id"]
	if id == "" {
		return "", nil, errors.New("nomad agent is not a client")
	}
	var node nomadNodeView
	if err := getJSON(ctx, strings.TrimRight(addr, "/")+"/v1/node/"+id, &node); err != nil {
		return id, nil, err
	}
	return id, &node, nil
}

// managedKey records which labels the agent owns, so a label dropped from
// node.yaml is removed from the node instead of lingering. It sits outside
// the dtp.* namespace so it is bookkeeping, not a label.
const managedKey = "dtp_agent.keys"

// ApplyLabels writes meta.dtp.* into the running client as dynamic node
// metadata (Nomad ≥ 1.5). Labels the agent set before but no longer sets are
// cleared. Returns the keys written.
func ApplyLabels(ctx context.Context, addr string, labels map[string]string) ([]string, error) {
	var current struct {
		Dynamic map[string]string `json:"Dynamic"`
	}
	base := strings.TrimRight(addr, "/") + "/v1/client/metadata"
	if err := getJSON(ctx, base, &current); err != nil {
		return nil, fmt.Errorf("nomad: %w", err)
	}
	meta := map[string]*string{}
	// Keys managed by this or an earlier agent are cleared unless set again
	// below; the pre-0.3 bookkeeping key itself is retired.
	for _, mk := range []string{managedKey, "dtp.agent.keys"} {
		for _, k := range strings.Split(current.Dynamic[mk], ",") {
			if k != "" {
				meta["dtp."+k] = nil
			}
		}
	}
	if _, legacy := current.Dynamic["dtp.agent.keys"]; legacy {
		meta["dtp.agent.keys"] = nil
	}
	keys := make([]string, 0, len(labels))
	for k, v := range labels {
		v := v
		meta["dtp."+k] = &v
		keys = append(keys, k)
	}
	sort.Strings(keys)
	managed := strings.Join(keys, ",")
	meta[managedKey] = &managed

	body, _ := json.Marshal(map[string]any{"Meta": meta})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("nomad: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("nomad: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return keys, nil
}

func getJSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s: %s: %s", url, resp.Status, strings.TrimSpace(string(b)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// ---------------------------------------------------------------------------
// the build cache
// ---------------------------------------------------------------------------

// CacheStats sizes the cache for the heartbeat.
func CacheStats(dir string) (entries int, bytes int64) {
	items, err := os.ReadDir(dir)
	if err != nil {
		return 0, 0
	}
	for _, e := range items {
		if !e.IsDir() {
			continue
		}
		entries++
		filepath.WalkDir(filepath.Join(dir, e.Name()), func(_ string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				if info, err := d.Info(); err == nil {
					bytes += info.Size()
				}
			}
			return nil
		})
	}
	return entries, bytes
}

// PruneCache removes entries whose .ready marker (touched on every use) is
// older than keep. Entries still being fetched have no marker and are left.
func PruneCache(dir string, keep time.Duration) ([]string, error) {
	if keep <= 0 {
		return nil, nil
	}
	items, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var removed []string
	for _, e := range items {
		if !e.IsDir() {
			continue
		}
		ready := filepath.Join(dir, e.Name(), ".ready")
		info, err := os.Stat(ready)
		if err != nil || time.Since(info.ModTime()) < keep {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err == nil {
			removed = append(removed, e.Name())
		}
	}
	return removed, nil
}

// ---------------------------------------------------------------------------
// nomad client config for a fresh node
// ---------------------------------------------------------------------------

// NomadConfig renders a Nomad client HCL for this node from node.yaml: the
// static part the agent cannot set at runtime (data dir, servers, drivers)
// plus the labels as a static fallback until the agent applies them
// dynamically. Every client stays in Nomad's default node pool; the dtp pool
// is a catalog assignment and the master pins each job to its node.
func NomadConfig(cfg *Config, labels map[string]string, runtimeKind string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Generated by dtp-node nomad-config from node.yaml; the agent keeps meta current at runtime.\n")
	fmt.Fprintf(&b, "datacenter = %q\ndata_dir   = %q\nname       = %q\nbind_addr  = \"0.0.0.0\"\n\n", cfg.Datacenter, cfg.DataDir, cfg.Name)
	b.WriteString("server { enabled = false }\n\nclient {\n  enabled   = true\n")
	if len(cfg.NomadServers) > 0 {
		quoted := make([]string, len(cfg.NomadServers))
		for i, s := range cfg.NomadServers {
			quoted[i] = strconv.Quote(s)
		}
		fmt.Fprintf(&b, "  servers   = [%s]\n", strings.Join(quoted, ", "))
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	b.WriteString("\n  meta {\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "    %-24s = %q\n", strconv.Quote("dtp."+k), labels[k])
	}
	b.WriteString("  }\n")
	if runtimeKind == "container" {
		fmt.Fprintf(&b, "\n  host_volume \"dtp-cache\" {\n    path      = %q\n    read_only = false\n  }\n", cfg.CacheDir)
	}
	b.WriteString("}\n\n")
	if runtimeKind == "container" {
		b.WriteString("plugin \"docker\" {\n  config {\n    volumes { enabled = true }\n    allow_privileged = false\n    gc { image = false }\n  }\n}\n")
	} else {
		b.WriteString("plugin \"raw_exec\" {\n  config { enabled = true }\n}\n")
	}
	return b.String()
}
