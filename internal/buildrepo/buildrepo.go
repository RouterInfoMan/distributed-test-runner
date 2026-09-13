// Package buildrepo reads the build repository: the bucket where build
// payloads are published, each described by a manifest (builds/<id>.json)
// written by the script that produced it. The manifest is what makes a
// payload more than a URL: it says which product and version it is, how it
// unpacks, which suites it carries. Submissions can name a build ("id":
// "egit-b522e135e4", or "latest" for a product) instead of pasting a URL and
// a checksum, and the version travels with the regression and into the
// suite's environment.
package buildrepo

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/andrei/distributed-test-platform/internal/model"
	"github.com/andrei/distributed-test-platform/internal/s3"
)

// Manifest is builds/<id>.json.
type Manifest struct {
	ID      string    `json:"id"`
	Product string    `json:"product"`
	Version string    `json:"version"`
	Ref     string    `json:"ref,omitempty"`    // tag or branch
	Commit  string    `json:"commit,omitempty"` // full commit id
	Built   time.Time `json:"built"`
	URL     string    `json:"url"`    // s3://builds/<id>.tar.gz
	SHA256  string    `json:"sha256"` // of the payload
	Size    int64     `json:"size,omitempty"`
	Unpack  string    `json:"unpack,omitempty"`
	Harness string    `json:"harness,omitempty"` // tycho | eclipse | fixture
	Suites  []string  `json:"suites,omitempty"`  // suites the payload can run
}

// Payload is one archive in the repository, with its manifest when it has one.
type Payload struct {
	URL      string    `json:"url"`
	Size     int64     `json:"size"`
	Manifest *Manifest `json:"manifest,omitempty"`
}

// ObjectStore is the slice of the S3 client the repository needs.
type ObjectStore interface {
	ListPrefix(ctx context.Context, bucket, prefix string) ([]s3.Object, error)
	Get(ctx context.Context, bucket, key string) (io.ReadCloser, http.Header, error)
}

// Repo is a cached view of the bucket.
type Repo struct {
	s3     ObjectStore
	bucket string

	mu       sync.Mutex
	cached   []Payload
	cachedAt time.Time
}

// CacheTTL bounds how stale a listing may be; a new build shows up within it.
const CacheTTL = 15 * time.Second

func New(store ObjectStore, bucket string) *Repo { return &Repo{s3: store, bucket: bucket} }

// Bucket is the repository's bucket name.
func (r *Repo) Bucket() string { return r.bucket }

// List returns every payload, newest manifest first, unmanifested archives
// last.
func (r *Repo) List(ctx context.Context) ([]Payload, error) {
	r.mu.Lock()
	if r.cached != nil && time.Since(r.cachedAt) < CacheTTL {
		out := r.cached
		r.mu.Unlock()
		return out, nil
	}
	r.mu.Unlock()

	objs, err := r.s3.ListPrefix(ctx, r.bucket, "")
	if err != nil {
		return nil, fmt.Errorf("build repository: %w", err)
	}
	manifests := map[string]*Manifest{} // by url
	var archives []Payload
	for _, o := range objs {
		url := "s3://" + r.bucket + "/" + o.Key
		switch {
		case strings.HasSuffix(o.Key, ".json"):
			m, err := r.read(ctx, o.Key)
			if err != nil {
				continue // a manifest that does not parse describes nothing
			}
			if m.URL == "" {
				m.URL = "s3://" + r.bucket + "/" + strings.TrimSuffix(o.Key, ".json") + ".tar.gz"
			}
			if m.ID == "" {
				m.ID = strings.TrimSuffix(path.Base(o.Key), ".json")
			}
			manifests[m.URL] = m
		case isArchive(o.Key):
			archives = append(archives, Payload{URL: url, Size: o.Size})
		}
	}
	out := make([]Payload, 0, len(archives))
	for _, a := range archives {
		a.Manifest = manifests[a.URL]
		if a.Manifest != nil && a.Manifest.Size == 0 {
			a.Manifest.Size = a.Size
		}
		out = append(out, a)
	}
	sort.SliceStable(out, func(i, j int) bool {
		mi, mj := out[i].Manifest, out[j].Manifest
		switch {
		case mi != nil && mj != nil:
			return mi.Built.After(mj.Built)
		case mi != nil:
			return true
		default:
			return false
		}
	})
	r.mu.Lock()
	r.cached, r.cachedAt = out, time.Now()
	r.mu.Unlock()
	return out, nil
}

func (r *Repo) read(ctx context.Context, key string) (*Manifest, error) {
	body, _, err := r.s3.Get(ctx, r.bucket, key)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	b, err := io.ReadAll(io.LimitReader(body, 1<<20))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func isArchive(key string) bool {
	for _, ext := range []string{".tar.gz", ".tgz", ".tar", ".zip"} {
		if strings.HasSuffix(key, ext) {
			return true
		}
	}
	return false
}

// Resolve completes a submission's build entry from the repository:
//
//	{"name": "egit", "id": "egit-b522e135e4"}   -> that build
//	{"name": "egit", "id": "latest"}            -> the newest build of product "egit" (or "product": …)
//	{"name": "egit", "url": "s3://…"}           -> as given, plus the manifest's version when one exists
//
// URL, sha256 and unpack are filled in when the entry leaves them empty; the
// entry's own values win when set.
func (r *Repo) Resolve(ctx context.Context, a *model.BuildArtifact) error {
	if a.ID == "" && a.URL == "" {
		return fmt.Errorf("build %q needs an id or a url", a.Name)
	}
	payloads, err := r.List(ctx)
	if err != nil {
		return err
	}
	var m *Manifest
	switch {
	case a.ID == "latest":
		product := a.Product
		if product == "" {
			product = a.Name
		}
		for _, p := range payloads { // newest first
			if p.Manifest != nil && p.Manifest.Product == product {
				m = p.Manifest
				break
			}
		}
		if m == nil {
			return fmt.Errorf("build %q: no build of product %q in the repository", a.Name, product)
		}
	case a.ID != "":
		for _, p := range payloads {
			if p.Manifest != nil && p.Manifest.ID == a.ID {
				m = p.Manifest
				break
			}
		}
		if m == nil {
			return fmt.Errorf("build %q: no build %q in the repository", a.Name, a.ID)
		}
	default:
		for _, p := range payloads {
			if p.URL == a.URL && p.Manifest != nil {
				m = p.Manifest
				break
			}
		}
		if m == nil {
			return nil // a plain URL, possibly outside the repository: nothing to add
		}
	}
	a.ID = m.ID
	a.Product, a.Version, a.Ref = m.Product, m.Version, m.Ref
	if a.URL == "" {
		a.URL = m.URL
	}
	if a.SHA256 == "" {
		a.SHA256 = m.SHA256
	}
	if a.Unpack == "" {
		a.Unpack = m.Unpack
	}
	return nil
}
