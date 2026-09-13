package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/andrei/distributed-test-platform/internal/config"
)

// The catalog as stored - pools, nodes, groups, users, quota rules - and the
// edits to it. Every write goes through applyCatalog: validated as a whole,
// written to the store, applied live; repeating a write changes nothing.

func (s *Server) getConfig(w http.ResponseWriter, r *http.Request) {
	cat := s.cfg.RawCatalog()
	writeJSON(w, http.StatusOK, map[string]any{
		"pools":  cat.Pools,
		"nodes":  cat.Nodes,
		"groups": cat.Groups,
		"users":  cat.Users,
		"quotas": cat.Quotas,
		"source": s.cfg.Store.Driver,
	})
}

// reloadConfig re-reads the catalog from the store, for edits made there
// directly (psql). The API paths never need it: they write and apply.
func (s *Server) reloadConfig(w http.ResponseWriter, r *http.Request) {
	if err := s.sc.ReloadCatalog(); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	s.getConfig(w, nil)
}

func (s *Server) putNode(w http.ResponseWriter, r *http.Request) {
	var n config.Node
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&n); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid node json: %w", err))
		return
	}
	n.Name = r.PathValue("name")
	cat := s.cfg.RawCatalog().Clone()
	cat.SetNode(n)
	s.applyCatalog(w, cat)
}

// deleteNode forgets a node (it comes back unassigned on its next heartbeat
// if the agent still runs). Deleting an unknown node is a no-op.
func (s *Server) deleteNode(w http.ResponseWriter, r *http.Request) {
	cat := s.cfg.RawCatalog().Clone()
	cat.RemoveNode(r.PathValue("name"))
	s.applyCatalog(w, cat)
}

// putConfig replaces the whole catalog. The body is {"pools": …, "quotas": …};
// a full master config file works too, its other keys are ignored.
func (s *Server) putConfig(w http.ResponseWriter, r *http.Request) {
	var cat config.Catalog
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&cat); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid catalog json: %w", err))
		return
	}
	s.applyCatalog(w, &cat)
}

func (s *Server) putPool(w http.ResponseWriter, r *http.Request) {
	var p config.Pool
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&p); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid pool json: %w", err))
		return
	}
	p.Name = r.PathValue("name")
	cat := s.cfg.RawCatalog().Clone()
	cat.SetPool(p)
	s.applyCatalog(w, cat)
}

// Deletes are idempotent: removing what is already absent succeeds.
func (s *Server) deletePool(w http.ResponseWriter, r *http.Request) {
	cat := s.cfg.RawCatalog().Clone()
	cat.RemovePool(r.PathValue("name"))
	s.applyCatalog(w, cat)
}

func (s *Server) putGroup(w http.ResponseWriter, r *http.Request) {
	var g config.Group
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&g); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid group json: %w", err))
		return
	}
	g.Name = r.PathValue("name")
	cat := s.cfg.RawCatalog().Clone()
	cat.SetGroup(g)
	s.applyCatalog(w, cat)
}

func (s *Server) deleteGroup(w http.ResponseWriter, r *http.Request) {
	cat := s.cfg.RawCatalog().Clone()
	cat.RemoveGroup(r.PathValue("name"))
	s.applyCatalog(w, cat)
}

// putRule sets the quota rule at {key} (subject[:name][/together]@scope
// [:target], e.g. user:carol@global). The body carries max_slots and note,
// and may also carry a different subject/scope: then the rule at {key} is
// replaced by the one in the body, which is how the dashboard edits a rule
// in place.
func (s *Server) putRule(w http.ResponseWriter, r *http.Request) {
	at, err := config.ParseRuleKey(r.PathValue("key"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var rule config.QuotaRule
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&rule); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid quota rule json: %w", err))
		return
	}
	if rule.Subject == "" && rule.Scope == "" {
		rule.Subject, rule.Name, rule.Together, rule.Scope, rule.Target = at.Subject, at.Name, at.Together, at.Scope, at.Target
	}
	cat := s.cfg.RawCatalog().Clone()
	if rule.Key() != at.Key() {
		cat.RemoveRule(at.Key())
	}
	cat.SetRule(rule)
	s.applyCatalog(w, cat)
}

func (s *Server) deleteRule(w http.ResponseWriter, r *http.Request) {
	cat := s.cfg.RawCatalog().Clone()
	cat.RemoveRule(r.PathValue("key"))
	s.applyCatalog(w, cat)
}

func (s *Server) putUser(w http.ResponseWriter, r *http.Request) {
	var u config.User
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&u); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid user json: %w", err))
		return
	}
	u.Name = r.PathValue("name")
	cat := s.cfg.RawCatalog().Clone()
	cat.SetUser(u)
	s.applyCatalog(w, cat)
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request) {
	cat := s.cfg.RawCatalog().Clone()
	cat.RemoveUser(r.PathValue("name"))
	s.applyCatalog(w, cat)
}

func (s *Server) applyCatalog(w http.ResponseWriter, cat *config.Catalog) {
	if err := s.sc.ApplyCatalog(cat); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	s.getConfig(w, nil)
}
