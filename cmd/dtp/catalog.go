package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/andrei/distributed-test-platform/internal/config"
	"github.com/andrei/distributed-test-platform/internal/model"
	"github.com/andrei/distributed-test-platform/internal/sched"
)

// The lab: pools, nodes, quota rules, builds and the stored catalog.

func doPools() error {
	var resp struct {
		Pools []sched.PoolStatus `json:"pools"`
	}
	if err := call(http.MethodGet, "/api/v1/pools", nil, &resp); err != nil {
		return err
	}
	for _, p := range resp.Pools {
		fmt.Printf("%s  [%s/%s]  %d/%d slots used", p.Name, p.Runtime, p.Driver, p.Used, p.Slots)
		if p.Queued > 0 {
			fmt.Printf("  (%d queued)", p.Queued)
		}
		fmt.Println()
		if p.Error != "" {
			fmt.Printf("  ! %s\n", p.Error)
		}
		if len(p.Spans) > 0 {
			fmt.Printf("  spans %s\n", strings.Join(p.Spans, ", "))
		}
		for _, n := range p.Nodes {
			status := "ready"
			if !n.Ready {
				status = n.Status
			}
			meta := make([]string, 0, len(n.Meta))
			for k, v := range n.Meta {
				if k != "slots" {
					meta = append(meta, k+"="+v)
				}
			}
			sort.Strings(meta)
			if n.Slots == 0 {
				meta = append([]string{"(no dtp.slots declared: set slots in node.yaml)"}, meta...)
			}
			meta = filterSlotMeta(meta)
			fmt.Printf("  %-22s %-8s %d/%d  %s  %s\n", n.Name, status, n.Used, n.Slots, slotText(n.Slot), strings.Join(meta, " "))
			for _, s := range n.Running {
				fmt.Printf("      ▸ %s\n", s)
			}
		}
		fmt.Println()
	}
	return nil
}

// slotText renders a node's declared slot size, or says it has none.
func slotText(s model.Slot) string {
	if s.Cores == 0 && s.CPU == 0 && s.Memory == 0 {
		return "(no slot declared: set slot in node.yaml)"
	}
	return s.String()
}

// filterSlotMeta hides the slot.* labels, shown as the slot column instead.
func filterSlotMeta(meta []string) []string {
	out := meta[:0]
	for _, m := range meta {
		if !strings.HasPrefix(m, "slot.") {
			out = append(out, m)
		}
	}
	return out
}

func doNodes(args []string) error {
	if len(args) >= 1 && args[0] == "assign" {
		if len(args) < 2 {
			return fmt.Errorf("usage: dtp nodes assign <node> [<pool>]   (no pool = unassign)")
		}
		pool := ""
		if len(args) >= 3 {
			pool = args[2]
		}
		var out map[string]any
		if err := call(http.MethodPut, "/api/v1/config/nodes/"+args[1], map[string]string{"name": args[1], "pool": pool}, &out); err != nil {
			return err
		}
		if pool == "" {
			fmt.Printf("%s unassigned\n", args[1])
		} else {
			fmt.Printf("%s -> %s\n", args[1], pool)
		}
		return nil
	}
	var resp struct {
		Pools      []sched.PoolStatus `json:"pools"`
		Unassigned []sched.NodeStatus `json:"unassigned"`
	}
	if err := call(http.MethodGet, "/api/v1/nodes", nil, &resp); err != nil {
		return err
	}
	fmt.Printf("%-22s %-16s %-8s %-7s %-22s %s\n", "NODE", "POOL", "STATUS", "SLOTS", "SLOT", "AGENT")
	row := func(n sched.NodeStatus, pool string) {
		status := "ready"
		if !n.Ready {
			status = n.Status
		}
		agent := "—"
		if n.Agent != nil {
			agent = n.Agent.AgentVersion
			if n.Agent.Stale {
				agent += " (silent)"
			}
		}
		fmt.Printf("%-22s %-16s %-8s %-7s %-22s %s\n", n.Name, pool, status, fmt.Sprintf("%d/%d", n.Used, n.Slots), slotText(n.Slot), agent)
	}
	for _, p := range resp.Pools {
		for _, n := range p.Nodes {
			row(n, p.Name)
		}
	}
	for _, n := range resp.Unassigned {
		row(n, "(unassigned)")
	}
	return nil
}

func doQuotas(args []string) error {
	if len(args) >= 1 && (args[0] == "set" || args[0] == "rm") {
		if len(args) < 2 || (args[0] == "set" && len(args) < 3) {
			return fmt.Errorf("usage: dtp quotas set <rule> <max-slots> [note]   |   dtp quotas rm <rule>\n" +
				"  rule = subject[:name][/together]@scope[:target], e.g. user:carol@global, group:release@pool:high-perf-pool, global/together@node:rcp-hp-1")
		}
		key := args[1]
		if _, err := config.ParseRuleKey(key); err != nil {
			return err
		}
		var out map[string]any
		if args[0] == "rm" {
			if err := call(http.MethodDelete, "/api/v1/config/quotas/"+key, nil, &out); err != nil {
				return err
			}
			fmt.Printf("%s removed\n", key)
			return nil
		}
		n, err := strconv.Atoi(args[2])
		if err != nil || n < 0 {
			return fmt.Errorf("max-slots must be a number >= 0 (0 forbids)")
		}
		body := map[string]any{"max_slots": n}
		if len(args) > 3 {
			body["note"] = strings.Join(args[3:], " ")
		}
		if err := call(http.MethodPut, "/api/v1/config/quotas/"+key, body, &out); err != nil {
			return err
		}
		fmt.Printf("%s = %d slots\n", key, n)
		return nil
	}
	var resp struct {
		Quotas []sched.RuleStatus `json:"quotas"`
	}
	if err := call(http.MethodGet, "/api/v1/quotas", nil, &resp); err != nil {
		return err
	}
	if len(resp.Quotas) == 0 {
		fmt.Println("no quota rules: nobody is limited (dtp quotas set <rule> <n>)")
		return nil
	}
	fmt.Printf("%-46s %5s  %-16s %s\n", "RULE", "LIMIT", "IN USE", "KEY")
	for _, q := range resp.Quotas {
		use := fmt.Sprintf("%d", q.Used)
		if q.Together || q.Subject == config.SubjectUser {
			use = fmt.Sprintf("%d/%d", q.Used, q.MaxSlots)
		}
		if q.Queued > 0 {
			use += fmt.Sprintf(" +%dq", q.Queued)
		}
		fmt.Printf("%-46s %5d  %-16s %s\n", q.Describe, q.MaxSlots, use, q.Key)
		if q.Subject != config.SubjectUser {
			for _, u := range q.Users {
				line := fmt.Sprintf("    %-42s %5d  %-16s", displayUser(u.User), u.Cap, fmt.Sprintf("%d/%d", u.Used, u.Cap))
				if u.Via != "" {
					line += "governed by " + u.Via
				}
				if u.Queued > 0 {
					line += fmt.Sprintf("  %d queued", u.Queued)
				}
				fmt.Println(strings.TrimRight(line, " "))
			}
		}
		if q.Note != "" {
			fmt.Printf("    · %s\n", q.Note)
		}
	}
	return nil
}

func displayUser(u string) string {
	if u == "" {
		return "(anonymous)"
	}
	return u
}

func doBuilds() error {
	var resp struct {
		Bucket string `json:"bucket"`
		Builds []struct {
			URL      string `json:"url"`
			Size     int64  `json:"size"`
			Manifest *struct {
				ID      string    `json:"id"`
				Product string    `json:"product"`
				Version string    `json:"version"`
				Ref     string    `json:"ref"`
				Built   time.Time `json:"built"`
				Harness string    `json:"harness"`
				Suites  []string  `json:"suites"`
			} `json:"manifest"`
		} `json:"builds"`
	}
	if err := call(http.MethodGet, "/api/v1/builds", nil, &resp); err != nil {
		return err
	}
	if len(resp.Builds) == 0 {
		fmt.Println("no builds in the repository")
		return nil
	}
	fmt.Printf("%-26s %-10s %-10s %-17s %-8s %-8s %s\n", "ID", "PRODUCT", "VERSION", "BUILT", "SIZE", "HARNESS", "SUITES")
	for _, b := range resp.Builds {
		size := fmt.Sprintf("%dM", b.Size/1048576)
		m := b.Manifest
		if m == nil {
			fmt.Printf("%-26s %-10s %-10s %-17s %-8s %-8s %s\n", "—", "—", "—", "—", size, "—", "(no manifest) "+b.URL)
			continue
		}
		fmt.Printf("%-26s %-10s %-10s %-17s %-8s %-8s %s\n", m.ID, m.Product, m.Version,
			m.Built.Local().Format("2006-01-02 15:04"), size, m.Harness, strings.Join(m.Suites, " "))
	}
	return nil
}

func doConfig() error {
	var out json.RawMessage
	if err := call(http.MethodGet, "/api/v1/config", nil, &out); err != nil {
		return err
	}
	var buf bytes.Buffer
	json.Indent(&buf, out, "", "  ")
	fmt.Println(buf.String())
	return nil
}

func doConfigApply(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var cat struct {
		Pools  json.RawMessage `json:"pools"`
		Nodes  json.RawMessage `json:"nodes"`
		Groups json.RawMessage `json:"groups"`
		Users  json.RawMessage `json:"users"`
		Quotas json.RawMessage `json:"quotas"`
	}
	if err := json.Unmarshal([]byte(os.ExpandEnv(string(raw))), &cat); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	var out struct {
		Pools  []json.RawMessage `json:"pools"`
		Nodes  []json.RawMessage `json:"nodes"`
		Groups []json.RawMessage `json:"groups"`
		Users  []json.RawMessage `json:"users"`
		Quotas []json.RawMessage `json:"quotas"`
	}
	if err := call(http.MethodPut, "/api/v1/config", cat, &out); err != nil {
		return err
	}
	fmt.Printf("applied: %d pools, %d nodes, %d groups, %d users, %d quota rules\n", len(out.Pools), len(out.Nodes), len(out.Groups), len(out.Users), len(out.Quotas))
	return nil
}
