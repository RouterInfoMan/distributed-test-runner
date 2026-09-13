package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/andrei/distributed-test-platform/internal/config"
	"github.com/andrei/distributed-test-platform/internal/model"
)

// fileBackend mirrors each regression to <dir>/<id>.json (atomic write +
// rename). Enough durability for a demo or a small lab, no dependencies.
type fileBackend struct{ dir string }

func NewFileBackend(dir string) (Backend, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &fileBackend{dir: dir}, nil
}

func (f *fileBackend) Load() ([]*record, error) {
	entries, err := os.ReadDir(f.dir)
	if err != nil {
		return nil, err
	}
	var out []*record
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(f.dir, e.Name()))
		if err != nil {
			return nil, err
		}
		var rec record
		if err := json.Unmarshal(b, &rec); err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		out = append(out, &rec)
	}
	return out, nil
}

// The file is the whole record, so every save rewrites it.
func (f *fileBackend) SaveRecord(rec *record) error            { return f.write(rec) }
func (f *fileBackend) SaveRegression(rec *record) error        { return f.write(rec) }
func (f *fileBackend) SaveRun(rec *record, _ *model.Run) error { return f.write(rec) }
func (f *fileBackend) Close() error                            { return nil }

// The catalog lives next to the regressions as catalog.json.
func (f *fileBackend) catalogPath() string { return filepath.Join(f.dir, "catalog.json") }

func (f *fileBackend) LoadCatalog() (*config.Catalog, error) {
	b, err := os.ReadFile(f.catalogPath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cat config.Catalog
	if err := json.Unmarshal(b, &cat); err != nil {
		return nil, fmt.Errorf("catalog.json: %w", err)
	}
	return &cat, nil
}

func (f *fileBackend) SaveCatalog(cat *config.Catalog) error {
	b, err := json.MarshalIndent(cat, "", "  ")
	if err != nil {
		return err
	}
	tmp := f.catalogPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, f.catalogPath())
}

func (f *fileBackend) write(rec *record) error {
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	id := rec.Regression.ID
	tmp := filepath.Join(f.dir, id+".json.tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(f.dir, id+".json"))
}
