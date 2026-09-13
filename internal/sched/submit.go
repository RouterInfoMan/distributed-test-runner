package sched

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/andrei/distributed-test-platform/internal/config"
	"github.com/andrei/distributed-test-platform/internal/model"
)

// Submission: a regression and the first attempt of each of its suites, and
// cancellation.

// Submit validates a submission, resolves its build payloads against the
// repository, materializes the first attempt of every suite and queues them.
func (s *Scheduler) Submit(ctx context.Context, sub *model.Submission) (*model.Regression, error) {
	if err := sub.Normalize(); err != nil {
		return nil, err
	}
	cat := s.cfg.Catalog()
	groups := cat.UserGroups(sub.User)
	if r := cat.Forbidden(sub.User, config.Everywhere); r != nil {
		return nil, fmt.Errorf("user %q may not run anything: rule %s (%s) allows 0 slots", sub.User, r.Key(), r.Describe())
	}
	// Builds: an id (or "latest") is looked up in the repository; a URL is
	// taken as is, gaining a version when the repository knows it.
	var builds []model.BuildArtifact
	seen := map[string]bool{}
	for i := range sub.Suites {
		su := &sub.Suites[i]
		for j := range su.Build {
			b := &su.Build[j]
			if s.repo == nil {
				if b.ID != "" {
					return nil, fmt.Errorf("suite %q: build %q names id %q but no build repository is configured", su.Name, b.Name, b.ID)
				}
			} else if err := s.repo.Resolve(ctx, b); err != nil {
				return nil, fmt.Errorf("suite %q: %w", su.Name, err)
			}
			key := b.Name + "|" + b.URL
			if !seen[key] {
				seen[key] = true
				builds = append(builds, *b)
			}
		}
	}
	for _, su := range sub.Suites {
		pool, ok := s.cfg.Pool(su.Pool)
		if !ok {
			return nil, fmt.Errorf("suite %q: unknown pool %q", su.Name, su.Pool)
		}
		if su.Runtime != "" && su.Runtime != pool.Runtime {
			return nil, fmt.Errorf("suite %q: pool %q provides runtime %q, suite asked for %q",
				su.Name, pool.Name, pool.Runtime, su.Runtime)
		}
		if r := cat.Forbidden(sub.User, config.PoolScope(su.Pool)); r != nil {
			return nil, fmt.Errorf("suite %q: user %q may not use pool %q: rule %s (%s) allows 0 slots",
				su.Name, sub.User, su.Pool, r.Key(), r.Describe())
		}
	}

	id := SanitizeID(sub.RegressionID)
	if id == "" {
		id = NewRegressionID(sub.Name)
	}
	now := time.Now().UTC()

	reg := &model.Regression{
		ID:          id,
		Name:        sub.Name,
		User:        sub.User,
		Groups:      groups,
		Priority:    sub.Priority,
		Labels:      sub.Labels,
		State:       model.RegPending,
		SubmittedAt: now,
		Suites:      sub.Suites,
		Builds:      builds,
		ArtifactURI: fmt.Sprintf("s3://%s/%s", s.cfg.S3.Bucket, resultPrefix(id)),
	}
	reg.Totals.Suites = len(sub.Suites)
	reg.Totals.SuitesQueued = len(sub.Suites)

	runs := make([]*model.Run, 0, len(sub.Suites))
	for _, su := range sub.Suites {
		runs = append(runs, s.newRun(reg, su, 1))
	}
	if err := s.st.Create(reg, runs); err != nil {
		return nil, err
	}
	s.log.Info("regression submitted", "regression", reg.ID, "user", reg.User, "suites", len(runs), "builds", buildSummary(builds))
	return reg, nil
}

// buildSummary renders "egit 7.8.0 (egit-b522e135e4)" for logs and the CLI.
func buildSummary(builds []model.BuildArtifact) string {
	parts := make([]string, 0, len(builds))
	for _, b := range builds {
		p := b.Name
		if b.Version != "" {
			p += " " + b.Version
		}
		if b.ID != "" {
			p += " (" + b.ID + ")"
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, ", ")
}

func (s *Scheduler) newRun(reg *model.Regression, su model.SuiteSpec, attempt int) *model.Run {
	pool, _ := s.cfg.Pool(su.Pool)
	rt := su.Runtime
	if rt == "" && pool != nil {
		rt = pool.Runtime
	}
	maxAttempts := su.MaxAttempts()
	if su.Retries == nil && s.cfg.DefaultRetries > 0 {
		maxAttempts = s.cfg.DefaultRetries + 1
	}
	return &model.Run{
		ID:           NewRunID(),
		RegressionID: reg.ID,
		Suite:        su.Name,
		Attempt:      attempt,
		MaxAttempts:  maxAttempts,
		Pool:         su.Pool,
		Runtime:      rt,
		State:        model.RunQueued,
		Priority:     reg.Priority,
		User:         reg.User,
		Groups:       reg.Groups,
		QueuedAt:     time.Now().UTC(),
		Token:        NewToken(),
		Spec:         su,
	}
}

// Cancel stops every non-terminal run in a regression.
func (s *Scheduler) Cancel(ctx context.Context, regID string) error {
	_, runs, ok := s.st.Regression(regID)
	if !ok {
		return fmt.Errorf("unknown regression %q", regID)
	}
	for _, r := range runs {
		if r.State.Terminal() {
			continue
		}
		if r.State != model.RunQueued {
			if err := s.be.Stop(ctx, r); err != nil {
				s.log.Warn("stop failed", "run", r.ID, "err", err)
			}
		}
		now := time.Now().UTC()
		s.st.UpdateRun(r.ID, func(run *model.Run) {
			run.State = model.RunCanceled
			run.FinishedAt = &now
			run.Message = "canceled"
		})
	}
	s.st.UpdateRegression(regID, func(reg *model.Regression, _ []*model.Run) {
		reg.State = model.RegCanceled
		if reg.FinishedAt == nil {
			now := time.Now().UTC()
			reg.FinishedAt = &now
		}
	})
	return nil
}

// CancelSuite stops the newest attempt of one suite and marks it canceled; no
// retry is queued. The rest of the regression carries on.
func (s *Scheduler) CancelSuite(ctx context.Context, regID, suite string) error {
	_, runs, ok := s.st.Regression(regID)
	if !ok {
		return fmt.Errorf("unknown regression %q", regID)
	}
	latest := LatestPerSuite(runs)[suite]
	if latest == nil {
		return fmt.Errorf("no suite %q in regression %s", suite, regID)
	}
	if latest.State.Terminal() {
		return fmt.Errorf("suite %q already finished (%s)", suite, latest.State)
	}
	if latest.State != model.RunQueued {
		if err := s.be.Stop(ctx, latest); err != nil {
			s.log.Warn("stop failed", "run", latest.ID, "err", err)
		}
	}
	now := time.Now().UTC()
	s.st.UpdateRun(latest.ID, func(run *model.Run) {
		if run.State.Terminal() {
			return
		}
		run.State = model.RunCanceled
		run.FinishedAt = &now
		run.Message = "canceled"
	})
	s.Rollup(ctx, regID)
	return nil
}
