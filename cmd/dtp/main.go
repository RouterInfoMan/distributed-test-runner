// Command dtp is the operator CLI: submit a regression, watch it, inspect
// pools, pull artifacts.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/andrei/distributed-test-platform/internal/model"
	"github.com/andrei/distributed-test-platform/internal/sched"
)

const usage = `dtp - distributed test platform CLI

usage:
  dtp submit <submission.json> [-w] [-id ID] [-priority N]
  dtp status <regression-id> [-w]
  dtp list
  dtp pools
  dtp cancel <regression-id>
  dtp artifacts <run-id> [-get PATH]
  dtp logs <run-id>

global flags:
  -master URL   master address (default $DTP_MASTER or http://127.0.0.1:8080)

'submit -w' and 'status -w' follow the run and exit non-zero if it did not
pass, which is what a CI job wants.
`

var (
	master  string
	posArgs []string
)

func main() {
	// -master is accepted both before and after the subcommand, since it reads
	// naturally either way in a CI script.
	argv, preMaster := extractGlobalMaster(os.Args[1:])
	if len(argv) < 1 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd := argv[0]
	args := argv[1:]

	def := envOr("DTP_MASTER", "http://127.0.0.1:8080")
	if preMaster != "" {
		def = preMaster
	}
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	fs.StringVar(&master, "master", def, "master address")

	var err error
	switch cmd {
	case "submit":
		watch := fs.Bool("w", false, "wait for completion")
		id := fs.String("id", "", "override regression id")
		prio := fs.Int("priority", 0, "override priority")
		parse(fs, args, 1)
		err = doSubmit(arg(0), *id, *prio, *watch)
	case "status":
		watch := fs.Bool("w", false, "follow until complete")
		parse(fs, args, 1)
		err = doStatus(arg(0), *watch)
	case "list":
		parse(fs, args, 0)
		err = doList()
	case "pools":
		parse(fs, args, 0)
		err = doPools()
	case "cancel":
		parse(fs, args, 1)
		err = doCancel(arg(0))
	case "artifacts":
		get := fs.String("get", "", "download this artifact path to stdout")
		parse(fs, args, 1)
		err = doArtifacts(arg(0), *get)
	case "logs":
		parse(fs, args, 1)
		err = doArtifacts(arg(0), "dtp-runner.log")
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "dtp:", err)
		os.Exit(1)
	}
}

// extractGlobalMaster pulls a leading "-master URL" / "-master=URL" out of the
// argument list so it can precede the subcommand.
func extractGlobalMaster(args []string) ([]string, string) {
	val := ""
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		a := strings.TrimLeft(args[0], "-")
		switch {
		case a == "master" && len(args) > 1:
			val, args = args[1], args[2:]
		case strings.HasPrefix(a, "master="):
			val, args = strings.TrimPrefix(a, "master="), args[1:]
		default:
			return args, val // -h and friends fall through to the dispatcher
		}
	}
	return args, val
}

// parse permutes flags and positional arguments so "dtp artifacts RUN -get X"
// works as naturally as "dtp artifacts -get X RUN"; Go's flag package stops at
// the first non-flag argument on its own.
func parse(fs *flag.FlagSet, args []string, want int) {
	posArgs = nil
	for {
		if err := fs.Parse(args); err != nil {
			os.Exit(2)
		}
		if fs.NArg() == 0 {
			break
		}
		posArgs = append(posArgs, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(posArgs) < want {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
}

// arg returns the nth positional argument, or "".
func arg(n int) string {
	if n < len(posArgs) {
		return posArgs[n]
	}
	return ""
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

// ---------------------------------------------------------------------------
// commands
// ---------------------------------------------------------------------------

func doSubmit(path, id string, prio int, watch bool) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	// ${VAR} is expanded from the environment, matching the master's config
	// loader; it is how a CI job injects the build URL and its sha256.
	var sub model.Submission
	if err := json.Unmarshal([]byte(os.ExpandEnv(string(raw))), &sub); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if id != "" {
		sub.RegressionID = id
	}
	if prio != 0 {
		sub.Priority = prio
	}
	var reg model.Regression
	if err := call(http.MethodPost, "/api/v1/regressions", sub, &reg); err != nil {
		return err
	}
	fmt.Printf("%s  submitted  %d suites\n", reg.ID, len(reg.Suites))
	fmt.Printf("dashboard  %s/#%s\n", strings.TrimRight(master, "/"), reg.ID)
	fmt.Printf("artifacts  %s\n\n", reg.ArtifactURI)
	if !watch {
		return nil
	}
	return doStatus(reg.ID, true)
}

type regDetail struct {
	Regression *model.Regression `json:"regression"`
	Runs       []*model.Run      `json:"runs"`
	Suites     []suiteView       `json:"suites"`
}

type suiteView struct {
	Suite    string         `json:"suite"`
	Pool     string         `json:"pool"`
	State    model.RunState `json:"state"`
	Attempts int            `json:"attempts"`
	Max      int            `json:"max_attempts"`
	Flaky    bool           `json:"flaky"`
	Node     string         `json:"node"`
	Summary  model.Summary  `json:"summary"`
	Duration float64        `json:"duration_seconds"`
	Message  string         `json:"message"`
	RunID    string         `json:"run_id"`
}

func doStatus(id string, watch bool) error {
	for {
		var d regDetail
		if err := call(http.MethodGet, "/api/v1/regressions/"+id, nil, &d); err != nil {
			return err
		}
		printRegression(&d, watch)
		if !watch || isTerminal(d.Regression.State) {
			if isTerminal(d.Regression.State) && d.Regression.State != model.RegPassed {
				os.Exit(1)
			}
			return nil
		}
		time.Sleep(2 * time.Second)
	}
}

func printRegression(d *regDetail, live bool) {
	r := d.Regression
	if live {
		fmt.Print("\033[H\033[2J") // redraw in place
	}
	t := r.Totals
	fmt.Printf("%s  %s  %s\n", r.ID, stateTag(string(r.State)), r.Name)
	fmt.Printf("suites %d/%d done · %d running · %d queued   tests %d pass / %d fail / %d skip",
		t.SuitesPassed+t.SuitesFailed+t.SuitesErrored, t.Suites,
		t.SuitesRunning, t.SuitesQueued, t.Passed, t.Failed, t.Skipped)
	if t.Flaky > 0 {
		fmt.Printf("   %d flaky", t.Flaky)
	}
	fmt.Printf("\n%s\n\n", r.ArtifactURI)

	rows := append([]suiteView(nil), d.Suites...)
	sort.Slice(rows, func(i, j int) bool { return rows[i].Suite < rows[j].Suite })
	fmt.Printf("  %-44s %-11s %-7s %-14s %-9s %s\n", "SUITE", "STATE", "TRY", "TESTS", "TIME", "NODE")
	for _, s := range rows {
		tests := "—"
		if s.Summary.Tests > 0 {
			tests = fmt.Sprintf("%d/%d", s.Summary.Passed, s.Summary.Tests)
			if f := s.Summary.Failed + s.Summary.Errors; f > 0 {
				tests += fmt.Sprintf(" (%d✗)", f)
			}
		}
		try := fmt.Sprintf("%d/%d", s.Attempts, max(s.Max, 1))
		if s.Flaky {
			try += "*"
		}
		fmt.Printf("  %-44s %s %-7s %-14s %-9s %s\n",
			trunc(s.Suite, 44), padState(string(s.State), 11), try, tests,
			fmtDur(s.Duration), s.Node)
		if s.Message != "" && s.State != model.RunPassed {
			fmt.Printf("      %s\n", truncHead(s.Message, 110))
		}
	}

	// Failing test cases from the latest attempt of each suite.
	latest := sched.LatestPerSuite(d.Runs)
	first := true
	for _, s := range rows {
		run := latest[s.Suite]
		if run == nil || len(run.Cases) == 0 {
			continue
		}
		if first {
			fmt.Println("\nfailures:")
			first = false
		}
		for _, c := range run.Cases {
			fmt.Printf("  ✗ %s.%s\n", c.Class, c.Name)
			if c.Message != "" {
				fmt.Printf("      %s\n", truncHead(strings.ReplaceAll(c.Message, "\n", " "), 110))
			}
		}
	}
	fmt.Println()
}

func doList() error {
	var resp struct {
		Regressions []*model.Regression `json:"regressions"`
	}
	if err := call(http.MethodGet, "/api/v1/regressions", nil, &resp); err != nil {
		return err
	}
	if len(resp.Regressions) == 0 {
		fmt.Println("no regressions yet")
		return nil
	}
	fmt.Printf("%-38s %-10s %-12s %-18s %s\n", "ID", "STATE", "SUITES", "TESTS", "SUBMITTED")
	for _, r := range resp.Regressions {
		t := r.Totals
		fmt.Printf("%-38s %s %-12s %-18s %s\n", r.ID, padState(string(r.State), 10),
			fmt.Sprintf("%d/%d", t.SuitesPassed, t.Suites),
			fmt.Sprintf("%d pass %d fail", t.Passed, t.Failed),
			r.SubmittedAt.Local().Format("2006-01-02 15:04:05"))
	}
	return nil
}

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
			fmt.Printf("  %-22s %-8s %d/%d  %s\n", n.Name, status, n.Used, n.Slots, strings.Join(meta, " "))
			for _, s := range n.Running {
				fmt.Printf("      ▸ %s\n", s)
			}
		}
		fmt.Println()
	}
	return nil
}

func doCancel(id string) error {
	var out map[string]any
	if err := call(http.MethodPost, "/api/v1/regressions/"+id+"/cancel", nil, &out); err != nil {
		return err
	}
	fmt.Printf("%s canceled\n", id)
	return nil
}

func doArtifacts(runID, get string) error {
	if get != "" {
		resp, err := http.Get(strings.TrimRight(master, "/") +
			"/api/v1/runs/" + runID + "/artifacts/" + get)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			b, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
		}
		_, err = io.Copy(os.Stdout, resp.Body)
		return err
	}
	var resp struct {
		Artifacts []model.ArtRef `json:"artifacts"`
		Prefix    string         `json:"prefix"`
		URI       string         `json:"uri"`
	}
	if err := call(http.MethodGet, "/api/v1/runs/"+runID+"/artifacts", nil, &resp); err != nil {
		return err
	}
	fmt.Printf("%s\n", resp.URI)
	for _, a := range resp.Artifacts {
		fmt.Printf("  %8.1fk  %s\n", float64(a.Size)/1024, a.Path)
	}
	return nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func call(method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, strings.TrimRight(master, "/")+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("contacting master at %s: %w", master, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			return fmt.Errorf("%s", e.Error)
		}
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func isTerminal(s model.RegressionState) bool {
	switch s {
	case model.RegPassed, model.RegFailed, model.RegErrored, model.RegCanceled:
		return true
	}
	return false
}

// padState pads before coloring, so ANSI escapes do not skew column widths.
func padState(s string, width int) string {
	if n := width - len(s); n > 0 {
		return stateTag(s) + strings.Repeat(" ", n)
	}
	return stateTag(s)
}

// stateTag colors state names when stdout is a terminal.
func stateTag(s string) string {
	if os.Getenv("NO_COLOR") != "" {
		return s
	}
	var code string
	switch s {
	case "passed":
		code = "32"
	case "failed":
		code = "31"
	case "errored", "timeout":
		code = "33"
	case "running", "dispatched":
		code = "36"
	default:
		code = "90"
	}
	return "\033[" + code + "m" + s + "\033[0m"
}

func fmtDur(sec float64) string {
	if sec <= 0 {
		return "—"
	}
	d := time.Duration(sec * float64(time.Second))
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return d.Round(time.Second).String()
}

// truncHead keeps the start of a message; trunc keeps the tail, which is the
// distinctive part of a Java package name.
func truncHead(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n < 4 {
		return s[:n]
	}
	return "…" + s[len(s)-n+1:]
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
