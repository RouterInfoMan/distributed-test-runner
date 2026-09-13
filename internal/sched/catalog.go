package sched

import (
	"github.com/andrei/distributed-test-platform/internal/config"
)

// The catalog - pools, node assignments, groups, users, quota rules - is edited
// through the scheduler so a change is validated, stored and made live in one
// step, and never lands halfway through a tick.

// ApplyCatalog validates, stores and activates a new catalog: the one write
// path, behind the config API. Applying the same catalog twice is a no-op.
func (s *Scheduler) ApplyCatalog(cat *config.Catalog) error {
	s.catMu.Lock()
	defer s.catMu.Unlock()
	if _, err := s.cfg.CheckCatalog(cat); err != nil {
		return err
	}
	if cat.Equal(s.cfg.RawCatalog()) {
		return nil
	}
	if err := s.st.SaveCatalog(cat); err != nil {
		return err
	}
	if err := s.cfg.SetCatalog(cat); err != nil {
		return err
	}
	s.log.Info("catalog: updated", "pools", len(cat.Pools), "nodes", len(cat.Nodes), "users", len(cat.Users), "groups", len(cat.Groups), "rules", len(cat.Quotas))
	s.st.Notify()
	return nil
}

// ReloadCatalog re-reads the store, for catalogs edited there directly (SQL).
func (s *Scheduler) ReloadCatalog() error {
	cat, err := s.st.LoadCatalog()
	if err != nil {
		return err
	}
	if cat == nil {
		cat = &config.Catalog{}
	}
	s.catMu.Lock()
	defer s.catMu.Unlock()
	if err := s.cfg.SetCatalog(cat); err != nil {
		return err
	}
	s.log.Info("catalog: reloaded from the store", "pools", len(cat.Pools), "nodes", len(cat.Nodes), "rules", len(cat.Quotas))
	s.st.Notify()
	return nil
}
