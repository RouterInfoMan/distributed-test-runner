package sched

import (
	"context"
	"fmt"
	"path"
	"sort"
	"time"

	"github.com/andrei/distributed-test-platform/internal/model"
)

// Admission: which queued run starts next, on which node, and what the node
// is told to do.

// dispatchGrace is how long a run may sit in "dispatched" before the master
// stops waiting for the backend to place it and reports the delay.
const dispatchGrace = 10 * time.Minute

// admit dispatches queued runs while their pool has free slots and their user
// is under quota. Priority wins outright; within one priority level the next
// pick goes to whoever holds the fewest slots right now, oldest first on a
// tie, so a user who queued fifty suites shares the cluster with one who
// queued five instead of locking them out until the fifty are done. A run
// blocked by quota or a full pool is skipped, not a barrier.
func (s *Scheduler) admit(ctx context.Context) {
	all := s.st.AllRuns()
	l := s.newLedger(all)

	var queued []*model.Run
	for _, r := range all {
		if r.State == model.RunQueued {
			queued = append(queued, r)
		}
	}
	sort.SliceStable(queued, func(i, j int) bool {
		if queued[i].Priority != queued[j].Priority {
			return queued[i].Priority > queued[j].Priority
		}
		return queued[i].QueuedAt.Before(queued[j].QueuedAt)
	})

	eligible := func(r *model.Run) bool {
		pool, ok := l.cat.Pool(r.Pool)
		if !ok || l.block(r, nil) != "" {
			return false
		}
		n, _ := l.pick(pool, r)
		return n != nil
	}
	for {
		best := -1
		for i, r := range queued {
			if r == nil || !eligible(r) {
				continue
			}
			if best >= 0 && r.Priority < queued[best].Priority {
				break // the list is priority-ordered; nothing below outranks the pick
			}
			if best < 0 || l.byUser[r.User] < l.byUser[queued[best].User] {
				best = i
			}
		}
		if best < 0 {
			break
		}
		r := queued[best]
		queued[best] = nil
		pool, _ := l.cat.Pool(r.Pool)
		node, _ := l.pick(pool, r)
		r.NodeID, r.NodeName = node.info.ID, node.info.Name
		if err := s.dispatch(ctx, r, node.info.Slot); err != nil {
			s.log.Warn("dispatch failed", "run", r.ID, "suite", r.Suite, "node", node.info.Name, "err", err)
			continue
		}
		l.charge(r)
	}
	// Say why what is still queued waits: a rule, by submission or by the
	// nodes it could have landed on; nothing when the pool is just full.
	for _, r := range queued {
		if r == nil {
			continue
		}
		why := l.block(r, nil)
		if why == "" {
			if pool, ok := l.cat.Pool(r.Pool); ok {
				_, why = l.pick(pool, r)
			}
		}
		s.note(r, why)
	}
}

// note records why a queued run is waiting, without churning the store when
// nothing changed.
func (s *Scheduler) note(r *model.Run, msg string) {
	if r.Message == msg {
		return
	}
	s.st.UpdateRun(r.ID, func(run *model.Run) {
		if run.State == model.RunQueued {
			run.Message = msg
		}
	})
}

func (s *Scheduler) dispatch(ctx context.Context, r *model.Run, slot model.Slot) error {
	spec, err := s.buildRunSpec(r, slot)
	if err != nil {
		s.fail(r.ID, model.RunErrored, "build run spec: "+err.Error())
		return err
	}
	pl, derr := s.be.Dispatch(ctx, r, spec)
	if derr != nil {
		return derr
	}
	now := time.Now().UTC()
	s.st.UpdateRun(r.ID, func(run *model.Run) {
		run.State = model.RunDispatched
		run.DispatchedAt = &now
		run.Message = ""
		run.BackendID = pl.BackendID
		run.NodeID, run.NodeName = r.NodeID, r.NodeName
		run.ArtifactURI = fmt.Sprintf("s3://%s/%s", s.cfg.S3.Bucket, spec.S3.Prefix)
	})
	s.log.Info("dispatched", "run", r.ID, "suite", r.Suite, "pool", r.Pool, "node", r.NodeName,
		"attempt", r.Attempt, "backend_id", pl.BackendID)
	return nil
}

// buildRunSpec produces the self-contained instruction set the node-side runner
// executes: what to fetch, what to run, where to put the results.
func (s *Scheduler) buildRunSpec(r *model.Run, slot model.Slot) (model.RunSpec, error) {
	pool, ok := s.cfg.Pool(r.Pool)
	if !ok {
		return model.RunSpec{}, fmt.Errorf("unknown pool %q", r.Pool)
	}
	cmd := r.Spec.Command
	if len(cmd) == 0 {
		cmd = pool.Default.Command
	}
	if len(cmd) == 0 {
		return model.RunSpec{}, fmt.Errorf("pool %q has no default command and suite sets none", pool.Name)
	}
	timeout := r.Spec.Timeout
	if timeout == 0 {
		timeout = s.cfg.DefaultTimeout
	}

	env := map[string]string{}
	for k, v := range r.Spec.Env {
		env[k] = v
	}
	env["DTP_SUITE"] = r.Suite
	env["DTP_REGRESSION_ID"] = r.RegressionID
	env["DTP_ATTEMPT"] = fmt.Sprint(r.Attempt)

	return model.RunSpec{
		RunID:        r.ID,
		RegressionID: r.RegressionID,
		Suite:        r.Suite,
		Attempt:      r.Attempt,
		Build:        r.Spec.Build,
		Command:      cmd,
		Env:          env,
		Timeout:      timeout,
		Slot:         slot,
		CacheDir:     pool.CacheDir,
		S3: model.S3Target{
			Endpoint:  s.cfg.S3.Endpoint,
			Region:    s.cfg.S3.Region,
			Bucket:    s.cfg.S3.Bucket,
			Prefix:    runPrefix(r),
			AccessKey: s.cfg.S3.AccessKey,
			SecretKey: s.cfg.S3.SecretKey,
		},
		MasterURL: s.cfg.PublicURL,
		Token:     r.Token,
	}, nil
}

// resultPrefix is the object-store layout: results/<regression_id>/...
func resultPrefix(regID string) string { return path.Join("results", regID) + "/" }

func runPrefix(r *model.Run) string {
	return path.Join("results", r.RegressionID, r.Suite, fmt.Sprintf("attempt-%d", r.Attempt)) + "/"
}
