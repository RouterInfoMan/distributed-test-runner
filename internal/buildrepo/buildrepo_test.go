package buildrepo

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/andrei/distributed-test-platform/internal/model"
	"github.com/andrei/distributed-test-platform/internal/s3"
)

// fakeStore is a bucket in a map.
type fakeStore map[string]string

func (f fakeStore) ListPrefix(_ context.Context, _, prefix string) ([]s3.Object, error) {
	var out []s3.Object
	for k, v := range f {
		if strings.HasPrefix(k, prefix) {
			out = append(out, s3.Object{Key: k, Size: int64(len(v))})
		}
	}
	return out, nil
}

func (f fakeStore) Get(_ context.Context, _, key string) (io.ReadCloser, http.Header, error) {
	v, ok := f[key]
	if !ok {
		return nil, nil, io.ErrUnexpectedEOF
	}
	return io.NopCloser(strings.NewReader(v)), http.Header{}, nil
}

func repo() *Repo {
	return New(fakeStore{
		"egit-aaa.tar.gz": "old payload",
		"egit-aaa.json":   `{"id":"egit-aaa","product":"egit","version":"7.7.0","built":"2026-08-01T00:00:00Z","url":"s3://builds/egit-aaa.tar.gz","sha256":"aaa","unpack":"tar.gz","suites":["org.eclipse.egit.core.test"]}`,
		"egit-bbb.tar.gz": "new payload",
		"egit-bbb.json":   `{"id":"egit-bbb","product":"egit","version":"7.8.0","ref":"v7.8.0","built":"2026-09-01T00:00:00Z","url":"s3://builds/egit-bbb.tar.gz","sha256":"bbb","unpack":"tar.gz"}`,
		"rcp-4.30.tar.gz": "a payload nobody described",
		"broken.json":     "not json",
	}, "builds")
}

func TestListOrdersNewestManifestFirst(t *testing.T) {
	list, err := repo().List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 || list[0].Manifest == nil || list[0].Manifest.ID != "egit-bbb" ||
		list[1].Manifest == nil || list[1].Manifest.ID != "egit-aaa" || list[2].Manifest != nil {
		t.Fatalf("unexpected listing: %+v", list)
	}
	if list[0].Manifest.Size != int64(len("new payload")) {
		t.Fatalf("size must come from the archive when the manifest omits it: %d", list[0].Manifest.Size)
	}
}

func TestResolve(t *testing.T) {
	r := repo()
	ctx := context.Background()

	latest := model.BuildArtifact{Name: "egit", ID: "latest"}
	if err := r.Resolve(ctx, &latest); err != nil {
		t.Fatal(err)
	}
	if latest.ID != "egit-bbb" || latest.Version != "7.8.0" || latest.URL != "s3://builds/egit-bbb.tar.gz" ||
		latest.SHA256 != "bbb" || latest.Unpack != "tar.gz" || latest.Ref != "v7.8.0" || latest.Product != "egit" {
		t.Fatalf("latest of egit: %+v", latest)
	}

	byID := model.BuildArtifact{Name: "reactor", ID: "egit-aaa", Unpack: "auto"}
	if err := r.Resolve(ctx, &byID); err != nil {
		t.Fatal(err)
	}
	if byID.Version != "7.7.0" || byID.SHA256 != "aaa" || byID.Unpack != "auto" {
		t.Fatalf("by id keeps the entry's own unpack: %+v", byID)
	}

	byURL := model.BuildArtifact{Name: "egit", URL: "s3://builds/egit-aaa.tar.gz", SHA256: "pinned"}
	if err := r.Resolve(ctx, &byURL); err != nil {
		t.Fatal(err)
	}
	if byURL.Version != "7.7.0" || byURL.SHA256 != "pinned" || byURL.ID != "egit-aaa" {
		t.Fatalf("by url gains the version but keeps its checksum: %+v", byURL)
	}

	outside := model.BuildArtifact{Name: "x", URL: "https://ci.example/x.zip"}
	if err := r.Resolve(ctx, &outside); err != nil || outside.Version != "" {
		t.Fatalf("a foreign url is left alone: %v %+v", err, outside)
	}

	for _, bad := range []model.BuildArtifact{{Name: "egit", ID: "nope"}, {Name: "other", ID: "latest"}, {Name: "none"}} {
		if err := r.Resolve(ctx, &bad); err == nil {
			t.Fatalf("expected an error for %+v", bad)
		}
	}
	other := model.BuildArtifact{Name: "tree", ID: "latest", Product: "egit"}
	if err := r.Resolve(ctx, &other); err != nil || other.Version != "7.8.0" {
		t.Fatalf("product overrides the name for latest: %v %+v", err, other)
	}
}
