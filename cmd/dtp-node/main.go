// Command dtp-node is the node agent: node.yaml + this binary make a machine a
// worker. It detects what the node is, labels the local Nomad client with it
// and with the slot count and slot size from node.yaml, and as a daemon keeps
// that current while reporting to the master, installing the runner and
// pruning the cache. Which pool the node serves is assigned on the master.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/andrei/distributed-test-platform/internal/agent"
)

const usage = `dtp-node - node agent

usage:
  dtp-node [-config /etc/dtp/node.yaml] labels        # what this node would be labelled
  dtp-node [-config …] apply                          # push labels + slots into the local Nomad client, once
  dtp-node [-config …] run                            # daemon: apply, heartbeat, runner install, cache prune
  dtp-node [-config …] nomad-config [-runtime container|process]
                                                      # render a Nomad client HCL for this node
  dtp-node version

node.yaml:
  master: http://master:8080         # required
  nomad: http://127.0.0.1:4646       # the local client's API
  name: rcp-hp-1                     # default: hostname
  slots: 6                           # required: how many suites this node runs at once
  slot:                              # required: what each of them gets
    cores: 2
    memory_mb: 4096
    memory_max_mb: 8192              # optional burst ceiling
    disk_mb: 2048
  pool: high-perf-pool               # optional: the pool to join when the master first sees the node
  labels:                            # meta.dtp.*; override detected ones
    perf: high
  cache_dir: /var/lib/dtp/cache
  cache_keep: 168h
  runner_dir: /usr/local/bin         # install dtp-runner from the master here
  interval: 60s
`

func main() {
	cfgPath := flag.String("config", envOr("DTP_NODE_CONFIG", "/etc/dtp/node.yaml"), "node.yaml")
	rt := flag.String("runtime", "container", "nomad-config: container or process")
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	// The subcommand may come before or after the flags ("run -config x" reads
	// as naturally as "-config x run"); Go's flag package stops at the first
	// non-flag word, so lift the command out before parsing.
	cmd, rest := "", []string{}
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			rest = append(rest, a)
			// A value-taking flag written as "-config x" owns the next word.
			name := strings.TrimLeft(a, "-")
			if f := flag.Lookup(name); f != nil && !strings.Contains(a, "=") && i+1 < len(args) {
				if b, isBool := f.Value.(interface{ IsBoolFlag() bool }); !isBool || !b.IsBoolFlag() {
					i++
					rest = append(rest, args[i])
				}
			}
			continue
		}
		if cmd == "" {
			cmd = a
			continue
		}
		rest = append(rest, a)
	}
	flag.CommandLine.Parse(rest)
	if cmd == "version" {
		fmt.Println("dtp-node", agent.Version)
		return
	}
	if cmd == "" || cmd == "help" {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cfg, err := agent.Load(*cfgPath)
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var runErr error
	switch cmd {
	case "labels":
		labels, facts, warn := compute(ctx, cfg)
		keys := make([]string, 0, len(labels))
		for k := range labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Printf("node %s  suggested pool %s  nomad node %s\n", cfg.Name, orDash(cfg.Pool), orDash(facts.NodeID))
		fmt.Printf("slots %d × %s (node.yaml)  node: %d cores, %d MB\n", cfg.Slots, cfg.Slot, facts.Cores, facts.MemoryMB)
		if warn != "" {
			fmt.Printf("warning: %s\n", warn)
		}
		fmt.Println()
		for _, k := range keys {
			src := "detected"
			if _, ok := cfg.Labels[k]; ok {
				src = "node.yaml"
			}
			fmt.Printf("  meta.dtp.%-14s = %-24q %s\n", k, labels[k], src)
		}
	case "apply":
		labels, _, warn := compute(ctx, cfg)
		if warn != "" {
			log.Warn("slots do not fit the node", "detail", warn)
		}
		keys, err := agent.ApplyLabels(ctx, cfg.Nomad, labels)
		if err != nil {
			runErr = err
			break
		}
		fmt.Printf("applied %d labels to %s: %s\n", len(keys), cfg.Nomad, strings.Join(keys, " "))
	case "nomad-config":
		labels, _, _ := compute(ctx, cfg)
		fmt.Print(agent.NomadConfig(cfg, labels, *rt))
	case "run":
		runErr = run(ctx, cfg, log)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if runErr != nil {
		log.Error(cmd, "err", runErr)
		os.Exit(1)
	}
}

// compute detects the node and builds its labels. Slots and slot size are
// what node.yaml says; the fingerprint is used only to warn when they do not
// fit the machine.
func compute(ctx context.Context, cfg *agent.Config) (map[string]string, agent.Facts, string) {
	facts := agent.Detect(ctx, cfg)
	return agent.Labels(cfg, facts), facts, agent.CheckCapacity(cfg, facts)
}

// run is the daemon loop: every interval, re-read node.yaml (an edited slot
// count or label takes effect without a restart), re-detect and re-apply (a
// JDK installed after boot shows up), heartbeat, keep the runner current,
// and prune the cache once an hour.
func run(ctx context.Context, cfg *agent.Config, log *slog.Logger) error {
	log.Info("dtp-node starting", "version", agent.Version, "node", cfg.Name, "slots", cfg.Slots, "slot", cfg.Slot.String(),
		"master", cfg.Master, "nomad", cfg.Nomad)
	var lastLabels, lastWarn, lastPool string
	lastPrune := time.Time{}
	tick := func() {
		if fresh, err := agent.Load(cfg.Path); err != nil {
			log.Warn("node.yaml: not reloaded", "err", err) // keep running on the last good config
		} else if enc, _ := json.Marshal(fresh); string(enc) != mustJSON(cfg) {
			log.Info("node.yaml changed; applying", "slots", fresh.Slots)
			*cfg = *fresh
		}
		facts := agent.Detect(ctx, cfg)
		labels := agent.Labels(cfg, facts)
		if warn := agent.CheckCapacity(cfg, facts); warn != lastWarn {
			lastWarn = warn
			if warn != "" {
				log.Warn("slots do not fit the node; Nomad will place fewer than declared", "detail", warn)
			}
		}

		if enc, _ := json.Marshal(labels); string(enc) != lastLabels {
			if keys, err := agent.ApplyLabels(ctx, cfg.Nomad, labels); err != nil {
				log.Warn("nomad: apply labels", "err", err)
			} else {
				lastLabels = string(enc)
				log.Info("labels applied", "keys", len(keys), "slots", cfg.Slots, "node_id", orDash(facts.NodeID))
			}
		}

		entries, bytes := agent.CacheStats(cfg.CacheDir)
		reply, err := agent.SendHeartbeat(ctx, cfg, agent.Heartbeat{
			Name: cfg.Name, Pool: cfg.Pool, NodeID: facts.NodeID, Labels: labels, Slots: cfg.Slots, Slot: cfg.Slot.Slot(),
			AgentVersion: agent.Version, CacheEntries: entries, CacheBytes: bytes, At: time.Now().UTC(),
		})
		if err != nil {
			log.Warn("master: heartbeat", "err", err)
		} else {
			if reply.Pool != lastPool {
				lastPool = reply.Pool
				if reply.Pool == "" {
					log.Warn("master has this node in no pool; assign it (dashboard ⚙ Config, or: dtp nodes assign " + cfg.Name + " <pool>)")
				} else {
					log.Info("master assignment", "pool", reply.Pool)
				}
			}
			if changed, err := agent.EnsureRunner(ctx, cfg, reply.RunnerSHA256); err != nil {
				log.Warn("runner install", "err", err)
			} else if changed {
				log.Info("dtp-runner installed from master", "dir", cfg.RunnerDir, "sha256", reply.RunnerSHA256[:12])
			}
		}

		if cfg.CacheKeep > 0 && time.Since(lastPrune) > time.Hour {
			lastPrune = time.Now()
			if removed, err := agent.PruneCache(cfg.CacheDir, cfg.CacheKeep.Duration()); err != nil {
				log.Warn("cache prune", "err", err)
			} else if len(removed) > 0 {
				log.Info("cache pruned", "removed", len(removed), "keep", cfg.CacheKeep.String())
			}
		}
	}
	tick()
	t := time.NewTicker(cfg.Interval.Duration())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Info("dtp-node stopping")
			return nil
		case <-t.C:
			tick()
		}
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
