package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/andrei/distributed-test-platform/internal/sched"
)

// Nodes: what the scheduler sees, the agents' heartbeats, and the runner
// binary they install.

func (s *Server) getNodes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"agents":     s.sc.Agents(),
		"pools":      s.sc.Pools(),
		"unassigned": s.sc.Unassigned(),
		"assigned":   s.cfg.RawCatalog().Nodes,
	})
}

// heartbeat takes a dtp-node report (registering a new node) and answers
// with the pool the catalog has it in and the runner's checksum, so the
// agent can say where it serves and self-update.
func (s *Server) heartbeat(w http.ResponseWriter, r *http.Request) {
	var a sched.AgentInfo
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&a); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid heartbeat json: %w", err))
		return
	}
	a.Name = r.PathValue("name")
	if err := s.sc.Heartbeat(a); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	sum, _, _ := s.runner()
	writeJSON(w, http.StatusOK, map[string]any{"runner_sha256": sum, "pool": s.cfg.Catalog().NodePool(a.Name)})
}

// runner locates the runner binary and its checksum, cached by mtime+size so
// a rebuilt binary is picked up without a restart.
func (s *Server) runner() (sum, path string, err error) {
	path = s.cfg.RunnerBinary
	if path == "" {
		exe, _ := os.Executable()
		path = filepath.Join(filepath.Dir(exe), "dtp-runner")
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", path, err
	}
	key := fmt.Sprintf("%s|%d|%d", path, info.Size(), info.ModTime().UnixNano())
	s.runnerMu.Lock()
	defer s.runnerMu.Unlock()
	if s.runnerKey == key {
		return s.runnerSum, path, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return "", path, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", path, err
	}
	s.runnerKey, s.runnerSum = key, hex.EncodeToString(h.Sum(nil))
	return s.runnerSum, path, nil
}

func (s *Server) getRunner(w http.ResponseWriter, r *http.Request) {
	sum, path, err := s.runner()
	if err != nil {
		writeErr(w, http.StatusNotFound, fmt.Errorf("no runner binary to serve: %w", err))
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Checksum-Sha256", sum)
	http.ServeFile(w, r, path)
}

func (s *Server) getRunnerSHA(w http.ResponseWriter, r *http.Request) {
	sum, _, err := s.runner()
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprintln(w, sum)
}
