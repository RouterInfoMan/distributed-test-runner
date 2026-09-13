package main

import (
	"encoding/json"
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

// Regressions: submit, follow, list, cancel, and the artifacts of a run.

func doSubmit(path, id string, prio int, user string, watch bool) error {
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
	if sub.User == "" {
		sub.User = user
	}
	var reg model.Regression
	if err := call(http.MethodPost, "/api/v1/regressions", sub, &reg); err != nil {
		return err
	}
	fmt.Printf("%s  submitted  %d suites  as %s\n", reg.ID, len(reg.Suites), reg.User)
	for _, b := range reg.Builds {
		fmt.Printf("build      %s %s (%s)\n", b.Name, b.Version, b.ID)
	}
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
	fmt.Printf("\n%s\n", r.ArtifactURI)
	for _, b := range r.Builds {
		fmt.Printf("build %s", b.Name)
		if b.Version != "" {
			fmt.Printf(" %s", b.Version)
		}
		if b.ID != "" {
			fmt.Printf(" (%s)", b.ID)
		}
		fmt.Printf("  %s\n", b.URL)
	}
	fmt.Println()

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
	fmt.Printf("%-38s %-10s %-12s %-18s %-12s %s\n", "ID", "STATE", "SUITES", "TESTS", "USER", "SUBMITTED")
	for _, r := range resp.Regressions {
		t := r.Totals
		fmt.Printf("%-38s %s %-12s %-18s %-12s %s\n", r.ID, padState(string(r.State), 10),
			fmt.Sprintf("%d/%d", t.SuitesPassed, t.Suites),
			fmt.Sprintf("%d pass %d fail", t.Passed, t.Failed),
			trunc(r.User, 12),
			r.SubmittedAt.Local().Format("2006-01-02 15:04:05"))
	}
	return nil
}

func doCancel(id, suite string) error {
	var out map[string]any
	path := "/api/v1/regressions/" + id + "/cancel"
	if suite != "" {
		path = "/api/v1/regressions/" + id + "/suites/" + suite + "/cancel"
	}
	if err := call(http.MethodPost, path, nil, &out); err != nil {
		return err
	}
	if suite != "" {
		fmt.Printf("%s: suite %s canceled\n", id, suite)
	} else {
		fmt.Printf("%s canceled\n", id)
	}
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
