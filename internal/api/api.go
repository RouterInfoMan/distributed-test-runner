// Package api exposes the master's HTTP surface: submission, status, the
// runner callback, artifact proxying, the catalog, and the embedded
// dashboard.
//
//	api.go            the server, routes, the dashboard page, helpers
//	regressions.go    regressions, runs, the runner callback, artifacts
//	catalog_api.go    the catalog and its edits
//	nodes_api.go      nodes, agent heartbeats, the runner binary
//	views_api.go      pools, quotas, builds, the composer's catalog, the overview
package api

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/andrei/distributed-test-platform/internal/config"
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

	runnerMu  sync.Mutex
	runnerKey string // path|size|mtime of the checksummed runner
	runnerSum string

	pageOnce  sync.Once
	pageID    string
	pageBytes []byte
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
	mux.HandleFunc("POST /api/v1/regressions/{id}/suites/{suite}/cancel", s.cancelSuite)
	mux.HandleFunc("GET /api/v1/pools", s.getPools)
	mux.HandleFunc("GET /api/v1/quotas", s.getQuotas)
	mux.HandleFunc("GET /api/v1/catalog", s.getCatalog)
	mux.HandleFunc("GET /api/v1/builds", s.getBuilds)
	mux.HandleFunc("GET /api/v1/nodes", s.getNodes)
	mux.HandleFunc("POST /api/v1/nodes/{name}/heartbeat", s.heartbeat)
	mux.HandleFunc("GET /api/v1/runner", s.getRunner)
	mux.HandleFunc("GET /api/v1/runner/sha256", s.getRunnerSHA)
	mux.HandleFunc("GET /api/v1/config", s.getConfig)
	mux.HandleFunc("PUT /api/v1/config", s.putConfig)
	mux.HandleFunc("PUT /api/v1/config/pools/{name}", s.putPool)
	mux.HandleFunc("DELETE /api/v1/config/pools/{name}", s.deletePool)
	mux.HandleFunc("PUT /api/v1/config/groups/{name}", s.putGroup)
	mux.HandleFunc("DELETE /api/v1/config/groups/{name}", s.deleteGroup)
	mux.HandleFunc("PUT /api/v1/config/quotas/{key...}", s.putRule)
	mux.HandleFunc("DELETE /api/v1/config/quotas/{key...}", s.deleteRule)
	mux.HandleFunc("PUT /api/v1/config/users/{name}", s.putUser)
	mux.HandleFunc("DELETE /api/v1/config/users/{name}", s.deleteUser)
	mux.HandleFunc("PUT /api/v1/config/nodes/{name}", s.putNode)
	mux.HandleFunc("DELETE /api/v1/config/nodes/{name}", s.deleteNode)
	mux.HandleFunc("POST /api/v1/config/reload", s.reloadConfig)
	mux.HandleFunc("GET /api/v1/overview", s.overview)
	mux.HandleFunc("GET /api/v1/runs/{id}", s.getRun)
	mux.HandleFunc("POST /api/v1/runs/{id}/events", s.runEvent)
	mux.HandleFunc("GET /api/v1/runs/{id}/artifacts", s.listArtifacts)
	mux.HandleFunc("GET /api/v1/runs/{id}/artifacts/{path...}", s.getArtifact)

	sub, err := fs.Sub(webFS, "web")
	if err == nil {
		static := http.FileServer(http.FS(sub))
		mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
			// The dashboard stays open for days on a screen and the master
			// gets rebuilt under it: never let a browser cache it, and stamp
			// the page with this build so it can notice a newer one.
			w.Header().Set("Cache-Control", "no-cache")
			if r.URL.Path == "/" || r.URL.Path == "/index.html" {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.Write(s.page())
				return
			}
			static.ServeHTTP(w, r)
		})
	}
	return logging(s.log, mux)
}

// page returns index.html stamped with this build's id.
func (s *Server) page() []byte {
	s.pageOnce.Do(func() {
		b, _ := webFS.ReadFile("web/index.html")
		s.pageID = buildID()
		s.pageBytes = []byte(strings.Replace(string(b), `<meta name="dtp-build" content="">`,
			`<meta name="dtp-build" content="`+s.pageID+`">`, 1))
	})
	return s.pageBytes
}

// buildID identifies this master binary: the hash of the embedded dashboard
// plus the process start, so a rebuilt master or a restarted one both count
// as new to an open page.
func buildID() string {
	b, _ := webFS.ReadFile("web/index.html")
	sum := sha256.Sum256(append(b, []byte(startedAt.Format(time.RFC3339Nano))...))
	return hex.EncodeToString(sum[:])[:12]
}

var startedAt = time.Now()

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{"status": "ok", "backend": s.sc.Backend().Name()}
	if err := s.sc.Backend().Healthy(r.Context()); err != nil {
		resp["status"] = "degraded"
		resp["backend_error"] = err.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

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
