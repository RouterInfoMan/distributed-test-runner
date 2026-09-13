package sched

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/andrei/distributed-test-platform/internal/model"
)

// Rollup: per-regression totals, state and the manifest written next to the
// artifacts.

func (s *Scheduler) rollupAll(ctx context.Context) {
	for _, reg := range s.st.Regressions() {
		if reg.State == model.RegPassed || reg.State == model.RegFailed ||
			reg.State == model.RegErrored || reg.State == model.RegCanceled {
			continue
		}
		s.Rollup(ctx, reg.ID)
	}
}

// LatestPerSuite returns the newest attempt of each suite.
func LatestPerSuite(runs []*model.Run) map[string]*model.Run {
	out := map[string]*model.Run{}
	for _, r := range runs {
		if cur, ok := out[r.Suite]; !ok || r.Attempt > cur.Attempt {
			out[r.Suite] = r
		}
	}
	return out
}

// Rollup recomputes a regression's totals and state, and writes the final
// manifest to the object store once everything is terminal.
func (s *Scheduler) Rollup(ctx context.Context, regID string) {
	var completed bool
	var snapshot *model.Regression
	var snapRuns []*model.Run

	s.st.UpdateRegression(regID, func(reg *model.Regression, runs []*model.Run) {
		latest := LatestPerSuite(runs)
		t := model.Totals{Suites: len(reg.Suites)}
		anyStarted, allTerminal := false, true

		for _, su := range reg.Suites {
			r, ok := latest[su.Name]
			if !ok {
				t.SuitesQueued++
				allTerminal = false
				continue
			}
			switch r.State {
			case model.RunQueued:
				t.SuitesQueued++
				allTerminal = false
			case model.RunDispatched, model.RunRunning:
				t.SuitesRunning++
				anyStarted, allTerminal = true, false
			case model.RunPassed:
				t.SuitesPassed++
				anyStarted = true
			case model.RunFailed:
				t.SuitesFailed++
				anyStarted = true
			default: // errored, timeout, canceled
				t.SuitesErrored++
				anyStarted = true
			}
			t.Tests += r.Summary.Tests
			t.Passed += r.Summary.Passed
			t.Failed += r.Summary.Failed + r.Summary.Errors
			t.Skipped += r.Summary.Skipped
			if r.State == model.RunPassed && r.Attempt > 1 {
				t.Flaky++ // suite only went green after a retry
			}
		}

		reg.Totals = t
		switch {
		case allTerminal && anyStarted:
			switch {
			case t.SuitesErrored > 0:
				reg.State = model.RegErrored
			case t.SuitesFailed > 0:
				reg.State = model.RegFailed
			default:
				reg.State = model.RegPassed
			}
			if reg.FinishedAt == nil {
				now := time.Now().UTC()
				reg.FinishedAt = &now
			}
			completed = true
		case anyStarted:
			reg.State = model.RegRunning
			if reg.StartedAt == nil {
				now := time.Now().UTC()
				reg.StartedAt = &now
			}
		default:
			reg.State = model.RegPending
		}

		cp := *reg
		snapshot = &cp
		snapRuns = runs
	})

	if completed && snapshot != nil {
		s.writeManifest(ctx, snapshot, snapRuns)
	}
}

// Manifest is the machine-readable summary written to
// results/<regression_id>/manifest.json when a regression finishes.
type Manifest struct {
	Regression  *model.Regression `json:"regression"`
	Runs        []*model.Run      `json:"runs"`
	GeneratedAt time.Time         `json:"generated_at"`
}

func (s *Scheduler) writeManifest(ctx context.Context, reg *model.Regression, runs []*model.Run) {
	if s.s3 == nil {
		return
	}
	m := Manifest{Regression: reg, Runs: runs, GeneratedAt: time.Now().UTC()}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return
	}
	key := resultPrefix(reg.ID) + "manifest.json"
	// The rollup often runs inside the runner's own "finished" request, and
	// the runner exits as soon as it is answered; the upload must outlive
	// that connection.
	c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := s.s3.PutBytes(c, s.cfg.S3.Bucket, key, b, "application/json"); err != nil {
		s.log.Warn("manifest upload failed", "regression", reg.ID, "err", err)
		return
	}
	s.log.Info("regression complete", "regression", reg.ID, "state", string(reg.State),
		"tests", reg.Totals.Tests, "failed", reg.Totals.Failed,
		"manifest", fmt.Sprintf("s3://%s/%s", s.cfg.S3.Bucket, key))
}
