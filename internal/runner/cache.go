package runner

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/andrei/distributed-test-platform/internal/model"
	"github.com/andrei/distributed-test-platform/internal/s3"
)

// Cache is a content-addressed store of unpacked build payloads on a node.
// Entries are keyed by sha256 (or by URL hash when no sha is declared), so a
// 400MB RCP product is downloaded and unpacked once per node and then reused by
// every suite in every regression that names the same build.
type Cache struct {
	Dir string
	S3  *s3.Client
	Log func(format string, args ...any)
}

// entry layout: <dir>/<key>/{.ready, payload/...}
func (c *Cache) entryDir(key string) string { return filepath.Join(c.Dir, key) }

// Ensure returns the local path of an unpacked artifact, fetching it if needed.
func (c *Cache) Ensure(ctx context.Context, a model.BuildArtifact) (string, error) {
	key := a.SHA256
	if key == "" {
		sum := sha256.Sum256([]byte(a.URL))
		key = "url-" + hex.EncodeToString(sum[:])[:32]
	}
	dir := c.entryDir(key)
	payload := filepath.Join(dir, "payload")
	ready := filepath.Join(dir, ".ready")

	if _, err := os.Stat(ready); err == nil {
		// The marker's mtime is "last used": the node agent prunes entries
		// that have not been touched within its cache_keep window.
		now := time.Now()
		os.Chtimes(ready, now, now)
		c.logf("cache hit  %s (%s)", a.Name, key[:12])
		return payload, nil
	}
	if err := os.MkdirAll(c.Dir, 0o755); err != nil {
		return "", err
	}

	// Cross-process lock: several slots on the same node may want the same
	// build at once. The loser waits for the winner's .ready marker.
	lock := filepath.Join(c.Dir, key+".lock")
	held, err := acquire(lock, 30*time.Minute)
	if err != nil {
		return "", err
	}
	if !held {
		if err := waitReady(ctx, ready, 30*time.Minute); err != nil {
			return "", err
		}
		c.logf("cache hit  %s (after wait)", a.Name)
		return payload, nil
	}
	defer os.Remove(lock)

	// Re-check: the previous holder may have finished while we waited.
	if _, err := os.Stat(ready); err == nil {
		return payload, nil
	}

	c.logf("cache miss %s -> fetching %s", a.Name, a.URL)
	staging := dir + ".staging"
	os.RemoveAll(staging)
	os.RemoveAll(dir)
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return "", err
	}

	tmpFile := filepath.Join(staging, "download")
	sum, err := c.download(ctx, a.URL, tmpFile)
	if err != nil {
		return "", fmt.Errorf("fetch %s: %w", a.URL, err)
	}
	if a.SHA256 != "" && !strings.EqualFold(sum, a.SHA256) {
		return "", fmt.Errorf("sha256 mismatch for %s: got %s want %s", a.Name, sum, a.SHA256)
	}

	dest := filepath.Join(staging, "payload")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return "", err
	}
	format := a.Unpack
	if format == "" || format == "auto" {
		format = detectFormat(a.URL)
	}
	switch format {
	case "zip":
		err = unzip(tmpFile, dest)
	case "tar.gz", "tgz":
		err = untar(tmpFile, dest, true)
	case "tar":
		err = untar(tmpFile, dest, false)
	case "none":
		err = os.Rename(tmpFile, filepath.Join(dest, filepath.Base(a.URL)))
	default:
		return "", fmt.Errorf("unknown unpack format %q", format)
	}
	if err != nil {
		return "", fmt.Errorf("unpack %s: %w", a.Name, err)
	}
	if format != "none" {
		os.Remove(tmpFile)
	}
	if err := os.WriteFile(filepath.Join(staging, ".ready"), []byte(sum), 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(staging, dir); err != nil {
		return "", err
	}
	c.logf("cached     %s at %s", a.Name, payload)
	return payload, nil
}

func (c *Cache) logf(format string, args ...any) {
	if c.Log != nil {
		c.Log(format, args...)
	}
}

// download writes url to dst and returns the payload's sha256.
func (c *Cache) download(ctx context.Context, rawURL, dst string) (string, error) {
	f, err := os.Create(dst)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	w := io.MultiWriter(f, h)

	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "s3":
		if c.S3 == nil {
			return "", fmt.Errorf("s3:// build url but no object store configured")
		}
		bucket := u.Host
		key := strings.TrimPrefix(u.Path, "/")
		body, _, err := c.S3.Get(ctx, bucket, key)
		if err != nil {
			return "", err
		}
		defer body.Close()
		if _, err := io.Copy(w, body); err != nil {
			return "", err
		}
	case "http", "https":
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return "", err
		}
		resp, err := (&http.Client{Timeout: 30 * time.Minute}).Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			return "", fmt.Errorf("http %s", resp.Status)
		}
		if _, err := io.Copy(w, resp.Body); err != nil {
			return "", err
		}
	case "file", "":
		src, err := os.Open(u.Path)
		if err != nil {
			return "", err
		}
		defer src.Close()
		if _, err := io.Copy(w, src); err != nil {
			return "", err
		}
	default:
		return "", fmt.Errorf("unsupported scheme %q", u.Scheme)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func detectFormat(rawURL string) string {
	l := strings.ToLower(rawURL)
	if i := strings.IndexByte(l, '?'); i >= 0 {
		l = l[:i]
	}
	switch {
	case strings.HasSuffix(l, ".zip"):
		return "zip"
	case strings.HasSuffix(l, ".tar.gz"), strings.HasSuffix(l, ".tgz"):
		return "tar.gz"
	case strings.HasSuffix(l, ".tar"):
		return "tar"
	}
	return "none"
}

// acquire takes an exclusive lock file, treating a stale one as free.
func acquire(path string, staleAfter time.Duration) (bool, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err == nil {
		fmt.Fprintf(f, "%d", os.Getpid())
		f.Close()
		return true, nil
	}
	if !os.IsExist(err) {
		return false, err
	}
	if st, serr := os.Stat(path); serr == nil && time.Since(st.ModTime()) > staleAfter {
		os.Remove(path)
		return acquire(path, staleAfter)
	}
	return false, nil
}

func waitReady(ctx context.Context, ready string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(ready); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("timed out waiting for another slot to populate the build cache")
}

// ---------------------------------------------------------------------------
// archive extraction (with path-traversal guards)
// ---------------------------------------------------------------------------

func safeJoin(root, name string) (string, error) {
	p := filepath.Join(root, filepath.Clean("/"+name))
	if !strings.HasPrefix(p, filepath.Clean(root)+string(os.PathSeparator)) && p != filepath.Clean(root) {
		return "", fmt.Errorf("archive entry escapes destination: %q", name)
	}
	return p, nil
}

func unzip(src, dest string) error {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, f := range zr.File {
		target, err := safeJoin(dest, f.Name)
		if err != nil {
			return err
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		mode := f.Mode()
		if mode == 0 {
			mode = 0o644
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode.Perm())
		if err != nil {
			rc.Close()
			return err
		}
		_, cerr := io.Copy(out, rc)
		out.Close()
		rc.Close()
		if cerr != nil {
			return cerr
		}
	}
	return nil
}

func untar(src, dest string, gz bool) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	var r io.Reader = f
	if gz {
		zr, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer zr.Close()
		r = zr
	}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target, err := safeJoin(dest, h.Name)
		if err != nil {
			return err
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeSymlink:
			os.MkdirAll(filepath.Dir(target), 0o755)
			os.Remove(target)
			if err := os.Symlink(h.Linkname, target); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(h.Mode).Perm())
			if err != nil {
				return err
			}
			_, cerr := io.Copy(out, tr)
			out.Close()
			if cerr != nil {
				return cerr
			}
		}
	}
}
