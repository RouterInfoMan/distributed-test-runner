package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andrei/distributed-test-platform/internal/model"
)

const nodeYAML = `
master: http://master:8080
name: rcp-hp-1
slots: 6
slot:
  cores: 2
  memory_mb: 4096
  memory_max_mb: 8192
pool: high-perf-pool
labels:
  perf: high
`

func load(t *testing.T, body string) (*Config, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "node.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return Load(p)
}

// node.yaml declares the slot count and the slot size; both are published as
// labels the master reads, and the pool is only a suggestion.
func TestLoadAndLabels(t *testing.T) {
	cfg, err := load(t, nodeYAML)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Slots != 6 || cfg.Slot.Slot() != (model.Slot{Cores: 2, Memory: 4096, MemoryMax: 8192, Disk: 2048}) {
		t.Fatalf("slots/slot: %d %+v", cfg.Slots, cfg.Slot)
	}
	if cfg.Pool != "high-perf-pool" || cfg.Nomad != "http://127.0.0.1:4646" {
		t.Fatalf("defaults: %+v", cfg)
	}
	l := Labels(cfg, Facts{OS: "linux", Arch: "x86_64", Cores: 12, MemoryMB: 24576, Display: "xvfb", Java: "21"})
	want := map[string]string{"slots": "6", "slot.cores": "2", "slot.memory_mb": "4096", "slot.memory_max_mb": "8192",
		"slot.disk_mb": "2048", "perf": "high", "os": "linux", "java": "21", "agent": Version}
	for k, v := range want {
		if l[k] != v {
			t.Fatalf("label %s: want %q got %q (all: %v)", k, v, l[k], l)
		}
	}
	if _, has := l["slot.cpu_mhz"]; has {
		t.Fatalf("a core-based slot publishes no cpu_mhz: %v", l)
	}
}

func TestLoadRejectsIncompleteSlot(t *testing.T) {
	for name, body := range map[string]string{
		"no slot":         "master: http://m\nslots: 2\n",
		"no memory":       "master: http://m\nslots: 2\nslot:\n  cores: 2\n",
		"no cores":        "master: http://m\nslots: 2\nslot:\n  memory_mb: 4096\n",
		"max below":       "master: http://m\nslots: 2\nslot:\n  cores: 2\n  memory_mb: 4096\n  memory_max_mb: 1024\n",
		"no slots":        "master: http://m\nslot:\n  cores: 2\n  memory_mb: 4096\n",
		"pool not needed": "",
	} {
		if name == "pool not needed" {
			if _, err := load(t, "master: http://m\nslots: 1\nslot:\n  cpu_mhz: 1000\n  memory_mb: 512\n"); err != nil {
				t.Fatalf("pool is optional: %v", err)
			}
			continue
		}
		if _, err := load(t, body); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// The agent never changes the declared numbers; it warns when they do not
// fit what Nomad fingerprinted.
func TestCheckCapacity(t *testing.T) {
	cfg, _ := load(t, nodeYAML)
	if w := CheckCapacity(cfg, Facts{Cores: 12, MemoryMB: 24576}); w != "" {
		t.Fatalf("6 × 2 cores / 4096 MB fits 12 cores / 24 GB: %q", w)
	}
	w := CheckCapacity(cfg, Facts{Cores: 8, MemoryMB: 16384})
	if !strings.Contains(w, "12, but Nomad reports 8") || !strings.Contains(w, "24576 MB, but Nomad reports 16384") {
		t.Fatalf("want both dimensions reported, got %q", w)
	}
	if cfg.Slots != 6 {
		t.Fatal("the check must not change the declared count")
	}
}

// The rendered client HCL has no node_pool: pools are the master's.
func TestNomadConfigHasNoNodePool(t *testing.T) {
	cfg, _ := load(t, nodeYAML)
	hcl := NomadConfig(cfg, Labels(cfg, Facts{OS: "linux"}), "container")
	if strings.Contains(hcl, "node_pool") {
		t.Fatalf("node_pool must not be rendered:\n%s", hcl)
	}
	if !strings.Contains(hcl, `"dtp.slot.cores"         = "2"`) || !strings.Contains(hcl, `"dtp.slots"`) {
		t.Fatalf("labels must be rendered as static meta:\n%s", hcl)
	}
}
