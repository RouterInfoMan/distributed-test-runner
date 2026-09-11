// Package api exposes the master's HTTP surface: submission, status, the
// runner callback, artifact proxying, and the embedded dashboard.
package api

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/andrei/distributed-test-platform/internal/config"
	"github.com/andrei/distributed-test-platform/internal/model"
	"github.com/andrei/distributed-test-platform/internal/s3"
	"github.com/andrei/distributed-test-platform/internal/sched"
	"github.com/andrei/distributed-test-platform/internal/store"
)

//go:embed all:web
var webFS embed.FS

type Server struct {
	cfg *config.Config
	st  *store.Store
	sc  *sched.Scheduler
	s3  *s3.Client
	log *slog.Logger
}

func New(cfg *config.Config, st *store.Store, sc *sched.Scheduler, s3c *s3.Client, log *slog.Logger) *Server {
	return &Server{cfg: cfg, st: st, sc: sc, s3: s3c, log: log}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("POST /api/v1/regressions", s.submit)
	mux.HandleFunc("GET /api/v1/regressions", s.listRegressions)
	mux.HandleFunc("GET /api/v1/regressions/{id}", s.getRegression)
	mux.HandleFunc("POST /api/v1/regressions/{id}/cancel", s.cancelRegression)
	mux.HandleFunc("GET /api/v1/pools", s.getPools)
	mux.HandleFunc("GET /api/v1/overview", s.overview)
	mux.HandleFunc("GET /api/v1/runs/{id}", s.getRun)
	mux.HandleFunc("POST /api/v1/runs/{id}/events", s.runEvent)
	mux.HandleFunc("GET /api/v1/runs/{id}/artifacts", s.listArtifacts)
	mux.HandleFunc("GET /api/v1/runs/{id}/artifacts/{path...}", s.getArtifact)

	sub, err := fs.Sub(webFS, "web")
	if err == nil {
		mux.Handle("GET /", http.FileServer(http.FS(sub)))
	}
	return logging(s.log, mux)
}

// ---------------------------------------------------------------------------
// handlers
// ---------------------------------------------------------------------------

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{"status": "ok", "backend": s.sc.Backend().Name()}
	if err := s.sc.Backend().Healthy(r.Context()); err != nil {
		resp["status"] = "degraded"
		resp["backend_error"] = err.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) submit(w http.ResponseWriter, r *http.Request) {
	var sub model.Submission
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&sub); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid submission json: %w", err))
		return
	}
	reg, err := s.sc.Submit(&sub)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusAccepted, reg)
}

func (s *Server) listRegressions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"regressions": s.st.Regressions()})
}

func (s *Server) getRegression(w http.ResponseWriter, r *http.Request) {
	reg, runs, ok := s.st.Regression(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, fmt.Errorf("no such regression"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"regression": reg,
		"runs":       runs,
		"suites":     suiteViews(reg, runs),
	})
}

func (s *Server) cancelRegression(w http.ResponseWriter, r *http.Request) {
	if err := s.sc.Cancel(r.Context(), r.PathValue("id")); err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"canceled": r.PathValue("id")})
}

func (s *Server) getPools(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"pools": s.sc.Pools()})
}

// overview backs the dashboard. With ?since=<revision> it long-polls, so the
// UI updates the moment anything changes instead of on a timer.
func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	rev := s.st.Revision()
	if v := r.URL.Query().Get("since"); v != "" {
		if since, err := strconv.ParseUint(v, 10, 64); err == nil {
			rev = s.st.Wait(since, 25*time.Second)
		}
	}
	health := "ok"
	berr := ""
	if err := s.sc.Backend().Healthy(r.Context()); err != nil {
		health, berr = "degraded", err.Error()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"revision":      rev,
		"backend":       s.sc.Backend().Name(),
		"backend_state": health,
		"backend_error": berr,
		"pools":         s.sc.Pools(),
		"regressions":   s.st.Regressions(),
		"generated_at":  time.Now().UTC(),
	})
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	run, ok := s.st.Run(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, fmt.Errorf("no such run"))
		return
	}
	writeJSON(w, http.StatusOK, run)
}

// runEvent is the runner callback. Authorization is the per-run bearer token
// minted at queue time, so a node can only report on its own attempt.
func (s *Server) runEvent(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	run, ok := s.st.RunByToken(token)
	if !ok || run.ID != runID {
		writeErr(w, http.StatusUnauthorized, fmt.Errorf("invalid run token"))
		return
	}
	var ev model.RunEvent
	if err := json.NewDecoder(io.LimitReader(r.Body, 32<<20)).Decode(&ev); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ev.RunID = runID
	if err := s.sc.Ingest(r.Context(), &ev); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// Roll the regression up immediately so `dtp status -w` reacts at once.
	s.sc.Rollup(r.Context(), run.RegressionID)
	writeJSON(w, http.StatusOK, map[string]any{"accepted": true})
}

func (s *Server) listArtifacts(w http.ResponseWriter, r *http.Request) {
	run, ok := s.st.Run(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, fmt.Errorf("no such run"))
		return
	}
	if s.s3 == nil {
		writeJSON(w, http.StatusOK, map[string]any{"artifacts": run.Artifacts})
		return
	}
	prefix := artifactPrefix(run)
	objs, err := s.s3.ListPrefix(r.Context(), s.cfg.S3.Bucket, prefix)
	if err != nil {
		// Fall back to what the runner reported.
		writeJSON(w, http.StatusOK, map[string]any{"artifacts": run.Artifacts, "warning": err.Error()})
		return
	}
	out := make([]model.ArtRef, 0, len(objs))
	for _, o := range objs {
		out = append(out, model.ArtRef{Path: strings.TrimPrefix(o.Key, prefix), Size: o.Size})
	}
	writeJSON(w, http.StatusOK, map[string]any{"artifacts": out, "prefix": prefix, "uri": run.ArtifactURI})
}

// getArtifact streams an object through the master so the browser never needs
// credentials for, or network access to, the object store.
func (s *Server) getArtifact(w http.ResponseWriter, r *http.Request) {
	run, ok := s.st.Run(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, fmt.Errorf("no such run"))
		return
	}
	if s.s3 == nil {
		writeErr(w, http.StatusServiceUnavailable, fmt.Errorf("no object store configured"))
		return
	}
	rel := strings.TrimPrefix(r.PathValue("path"), "/")
	if strings.Contains(rel, "..") {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("bad path"))
		return
	}
	body, hdr, err := s.s3.Get(r.Context(), s.cfg.S3.Bucket, artifactPrefix(run)+rel)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	defer body.Close()
	ct := hdr.Get("Content-Type")
	if ct == "" || ct == "application/octet-stream" {
		ct = s3.GuessContentType(rel)
	}
	w.Header().Set("Content-Type", ct)
	if cl := hdr.Get("Content-Length"); cl != "" {
		w.Header().Set("Content-Length", cl)
	}
	io.Copy(w, body)
}

func artifactPrefix(run *model.Run) string {
	return fmt.Sprintf("results/%s/%s/attempt-%d/", run.RegressionID, run.Suite, run.Attempt)
}

// ---------------------------------------------------------------------------
// views
// ---------------------------------------------------------------------------

// SuiteView folds a suite's attempts into one row for the CLI and dashboard.
type SuiteView struct {
	Suite    string         `json:"suite"`
	Pool     string         `json:"pool"`
	State    model.RunState `json:"state"`
	Attempts int            `json:"attempts"`
	Max      int            `json:"max_attempts"`
	Flaky    bool           `json:"flaky"`
	Node     string         `json:"node,omitempty"`
	Summary  model.Summary  `json:"summary"`
	Duration float64        `json:"duration_seconds"`
	Message  string         `json:"message,omitempty"`
	RunID    string         `json:"run_id"`
	Runs     []*model.Run   `json:"runs,omitempty"`
}

func suiteViews(reg *model.Regression, runs []*model.Run) []SuiteView {
	bySuite := map[string][]*model.Run{}
	for _, r := range runs {
		bySuite[r.Suite] = append(bySuite[r.Suite], r)
	}
	out := make([]SuiteView, 0, len(reg.Suites))
	for _, su := range reg.Suites {
		attempts := bySuite[su.Name]
		v := SuiteView{Suite: su.Name, Pool: su.Pool, State: model.RunQueued, Runs: attempts}
		if len(attempts) == 0 {
			out = append(out, v)
			continue
		}
		latest := attempts[0]
		for _, a := range attempts {
			if a.Attempt > latest.Attempt {
				latest = a
			}
		}
		v.State = latest.State
		v.Attempts = latest.Attempt
		v.Max = latest.MaxAttempts
		v.Node = latest.NodeName
		v.Summary = latest.Summary
		v.Message = latest.Message
		v.RunID = latest.ID
		v.Duration = latest.Duration().Seconds()
		v.Flaky = latest.State == model.RunPassed && latest.Attempt > 1
		out = append(out, v)
	}
	return out
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func logging(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: 200}
		next.ServeHTTP(sw, r)
		// The dashboard long-poll is expected to be slow; don't log it as noise.
		if strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Query().Get("since") == "" {
			log.Debug("http", "method", r.Method, "path", r.URL.Path,
				"status", sw.code, "dur", time.Since(start).Round(time.Millisecond).String())
		}
	})
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(c int) {
	w.code = c
	w.ResponseWriter.WriteHeader(c)
}
