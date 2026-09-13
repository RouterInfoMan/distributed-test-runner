package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/andrei/distributed-test-platform/internal/buildrepo"
	"github.com/andrei/distributed-test-platform/internal/model"
)

// Read-only views: pools, quotas, builds, the composer's catalog, templates,
// and the dashboard's overview.

func (s *Server) getPools(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"pools": s.sc.Pools()})
}

func (s *Server) getQuotas(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"quotas": s.sc.Quotas()})
}

// Template is a submission file the dashboard offers as a starting point.
type Template struct {
	Name       string           `json:"name"`
	File       string           `json:"file"`
	Submission model.Submission `json:"submission"`
}

// getBuilds lists the build repository: every payload, with its manifest
// (product, version, suites) when the publisher wrote one.
func (s *Server) getBuilds(w http.ResponseWriter, r *http.Request) {
	repo := s.sc.Builds()
	if repo == nil {
		writeJSON(w, http.StatusOK, map[string]any{"builds": []any{}, "bucket": ""})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	builds, err := repo.List(ctx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"builds": builds, "bucket": repo.Bucket()})
}

// getCatalog is everything the dashboard's "new regression" form needs to
// offer choices instead of free text: pools, the node meta values actually
// present on the cluster (for "requires"), suites and users seen before,
// templates and published builds.
func (s *Server) getCatalog(w http.ResponseWriter, r *http.Request) {
	type poolInfo struct {
		Name    string        `json:"name"`
		Runtime model.Runtime `json:"runtime"`
		Virtual bool          `json:"virtual"`
		Slots   int           `json:"slots"`
	}
	var pools []poolInfo
	meta := map[string]map[string]bool{}
	for _, p := range s.sc.Pools() {
		pools = append(pools, poolInfo{Name: p.Name, Runtime: p.Runtime, Virtual: len(p.Spans) > 0, Slots: p.Slots})
		for _, n := range p.Nodes {
			for k, v := range n.Meta {
				// Capacity labels are not targets a suite would require.
				if k == "slots" || k == "agent" || strings.HasPrefix(k, "slot.") {
					continue
				}
				if meta[k] == nil {
					meta[k] = map[string]bool{}
				}
				meta[k][v] = true
			}
		}
	}
	metaOut := map[string][]string{}
	for k, vs := range meta {
		for v := range vs {
			metaOut[k] = append(metaOut[k], v)
		}
		sort.Strings(metaOut[k])
	}

	suites, users := map[string]bool{}, map[string]bool{}
	for _, reg := range s.st.Regressions() {
		for _, su := range reg.Suites {
			suites[su.Name] = true
		}
		if reg.User != "" {
			users[reg.User] = true
		}
	}
	for _, u := range s.cfg.Catalog().Users {
		users[u.Name] = true
	}

	templates := []Template{}
	if dir := s.cfg.TemplatesDir; dir != "" {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
				continue
			}
			b, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				continue
			}
			var sub model.Submission
			if json.Unmarshal(b, &sub) != nil || len(sub.Suites) == 0 {
				continue
			}
			for _, su := range sub.Suites {
				suites[su.Name] = true
			}
			templates = append(templates, Template{Name: strings.TrimSuffix(e.Name(), ".json"), File: e.Name(), Submission: sub})
		}
	}

	builds := []buildrepo.Payload{}
	if repo := s.sc.Builds(); repo != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if list, err := repo.List(ctx); err == nil {
			builds = list
			for _, p := range list {
				if p.Manifest != nil {
					for _, su := range p.Manifest.Suites {
						suites[su] = true
					}
				}
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"pools":     pools,
		"meta":      metaOut,
		"suites":    sortedKeys(suites),
		"users":     sortedKeys(users),
		"harnesses": []string{"fixture", "tycho", "eclipse"},
		"templates": templates,
		"builds":    builds,
	})
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
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
	s.page()
	writeJSON(w, http.StatusOK, map[string]any{
		"revision":      rev,
		"build":         s.pageID,
		"backend":       s.sc.Backend().Name(),
		"backend_state": health,
		"backend_error": berr,
		"pools":         s.sc.Pools(),
		"unassigned":    s.sc.Unassigned(),
		"quotas":        s.sc.Quotas(),
		"regressions":   s.st.Regressions(),
		"generated_at":  time.Now().UTC(),
	})
}
