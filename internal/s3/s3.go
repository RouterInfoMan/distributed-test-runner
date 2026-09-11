// Package s3 is a dependency-free S3 client: AWS SigV4 over net/http, path
// style addressing, which is what MinIO wants. Only the verbs the platform
// needs are implemented (PUT, GET, HEAD, list-v2, presign).
package s3

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"
	"time"
)

const emptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// Client talks to one bucket-less endpoint; the bucket is part of each path.
type Client struct {
	Endpoint  string // http://minio:9000
	Region    string
	AccessKey string
	SecretKey string
	HTTP      *http.Client
}

func New(endpoint, region, accessKey, secretKey string) *Client {
	if region == "" {
		region = "us-east-1"
	}
	return &Client{
		Endpoint:  strings.TrimRight(endpoint, "/"),
		Region:    region,
		AccessKey: accessKey,
		SecretKey: secretKey,
		HTTP:      &http.Client{Timeout: 10 * time.Minute},
	}
}

// Object is one entry from ListPrefix.
type Object struct {
	Key  string
	Size int64
}

// EnsureBucket creates the bucket when it does not exist yet.
func (c *Client) EnsureBucket(ctx context.Context, bucket string) error {
	req, err := c.newRequest(ctx, http.MethodHead, "/"+bucket, nil, nil, emptyPayloadHash)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	req, err = c.newRequest(ctx, http.MethodPut, "/"+bucket, nil, nil, emptyPayloadHash)
	if err != nil {
		return err
	}
	resp, err = c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusConflict {
		return httpErr("create bucket", resp)
	}
	return nil
}

// PutFile uploads a local file to bucket/key.
func (c *Client) PutFile(ctx context.Context, bucket, key, localPath, contentType string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		return err
	}
	return c.put(ctx, bucket, key, f, st.Size(), hex.EncodeToString(sum.Sum(nil)), contentType)
}

// PutBytes uploads an in-memory object.
func (c *Client) PutBytes(ctx context.Context, bucket, key string, body []byte, contentType string) error {
	sum := sha256.Sum256(body)
	return c.put(ctx, bucket, key, bytes.NewReader(body), int64(len(body)),
		hex.EncodeToString(sum[:]), contentType)
}

func (c *Client) put(ctx context.Context, bucket, key string, body io.Reader, size int64, payloadHash, contentType string) error {
	// Content-Type is deliberately outside the signed header set, so it can be
	// attached before signing without affecting the canonical request.
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.Endpoint+"/"+bucket+"/"+key, body)
	if err != nil {
		return err
	}
	req.ContentLength = size
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if err := c.sign(req, payloadHash); err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return httpErr("put "+key, resp)
	}
	io.Copy(io.Discard, resp.Body)
	return nil
}

// Get returns the object body; the caller closes it.
func (c *Client) Get(ctx context.Context, bucket, key string) (io.ReadCloser, http.Header, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/"+bucket+"/"+key, nil, nil, emptyPayloadHash)
	if err != nil {
		return nil, nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, nil, httpErr("get "+key, resp)
	}
	return resp.Body, resp.Header, nil
}

type listResult struct {
	XMLName     xml.Name `xml:"ListBucketResult"`
	IsTruncated bool     `xml:"IsTruncated"`
	NextToken   string   `xml:"NextContinuationToken"`
	Contents    []struct {
		Key  string `xml:"Key"`
		Size int64  `xml:"Size"`
	} `xml:"Contents"`
}

// ListPrefix returns every object under prefix.
func (c *Client) ListPrefix(ctx context.Context, bucket, prefix string) ([]Object, error) {
	var out []Object
	token := ""
	for {
		q := url.Values{}
		q.Set("list-type", "2")
		q.Set("prefix", prefix)
		if token != "" {
			q.Set("continuation-token", token)
		}
		req, err := c.newRequest(ctx, http.MethodGet, "/"+bucket, q, nil, emptyPayloadHash)
		if err != nil {
			return nil, err
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode >= 300 {
			defer resp.Body.Close()
			return nil, httpErr("list "+prefix, resp)
		}
		var lr listResult
		err = xml.NewDecoder(resp.Body).Decode(&lr)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		for _, o := range lr.Contents {
			out = append(out, Object{Key: o.Key, Size: o.Size})
		}
		if !lr.IsTruncated || lr.NextToken == "" {
			break
		}
		token = lr.NextToken
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// Presign builds a time-limited GET URL (query-string SigV4), used by the
// dashboard when the browser can reach the object store directly.
func (c *Client) Presign(bucket, key string, ttl time.Duration) (string, error) {
	u, err := url.Parse(c.Endpoint + "/" + bucket + "/" + key)
	if err != nil {
		return "", err
	}
	now := time.Now().UTC()
	stamp := now.Format("20060102T150405Z")
	datestamp := now.Format("20060102")
	scope := datestamp + "/" + c.Region + "/s3/aws4_request"

	q := u.Query()
	q.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	q.Set("X-Amz-Credential", c.AccessKey+"/"+scope)
	q.Set("X-Amz-Date", stamp)
	q.Set("X-Amz-Expires", fmt.Sprintf("%d", int(ttl.Seconds())))
	q.Set("X-Amz-SignedHeaders", "host")
	u.RawQuery = encodeQuery(q)

	canonical := strings.Join([]string{
		http.MethodGet,
		escapePath(u.Path),
		u.RawQuery,
		"host:" + u.Host + "\n",
		"host",
		"UNSIGNED-PAYLOAD",
	}, "\n")
	sts := stringToSign(stamp, scope, canonical)
	sig := hex.EncodeToString(hmacSHA256(signingKey(c.SecretKey, datestamp, c.Region), sts))
	return u.String() + "&X-Amz-Signature=" + sig, nil
}

// ---------------------------------------------------------------------------
// signing
// ---------------------------------------------------------------------------

func (c *Client) newRequest(ctx context.Context, method, p string, q url.Values, body io.Reader, payloadHash string) (*http.Request, error) {
	u := c.Endpoint + p
	if q != nil {
		u += "?" + encodeQuery(q)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if err := c.sign(req, payloadHash); err != nil {
		return nil, err
	}
	return req, nil
}

func (c *Client) sign(req *http.Request, payloadHash string) error {
	now := time.Now().UTC()
	stamp := now.Format("20060102T150405Z")
	datestamp := now.Format("20060102")
	scope := datestamp + "/" + c.Region + "/s3/aws4_request"

	req.Header.Set("X-Amz-Date", stamp)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if req.Host == "" {
		req.Host = req.URL.Host
	}

	signed := "host;x-amz-content-sha256;x-amz-date"
	canonicalHeaders := "host:" + req.URL.Host + "\n" +
		"x-amz-content-sha256:" + payloadHash + "\n" +
		"x-amz-date:" + stamp + "\n"

	canonical := strings.Join([]string{
		req.Method,
		escapePath(req.URL.Path),
		req.URL.RawQuery,
		canonicalHeaders,
		signed,
		payloadHash,
	}, "\n")

	sts := stringToSign(stamp, scope, canonical)
	sig := hex.EncodeToString(hmacSHA256(signingKey(c.SecretKey, datestamp, c.Region), sts))
	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		c.AccessKey, scope, signed, sig))
	return nil
}

func stringToSign(stamp, scope, canonical string) string {
	h := sha256.Sum256([]byte(canonical))
	return strings.Join([]string{"AWS4-HMAC-SHA256", stamp, scope, hex.EncodeToString(h[:])}, "\n")
}

func signingKey(secret, datestamp, region string) []byte {
	k := hmacSHA256([]byte("AWS4"+secret), datestamp)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, "s3")
	return hmacSHA256(k, "aws4_request")
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// escapePath URI-encodes each path segment the way SigV4 expects (slashes kept).
func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = awsEscape(s)
	}
	return strings.Join(parts, "/")
}

// encodeQuery sorts keys and uses AWS escaping rules.
func encodeQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		vals := append([]string(nil), q[k]...)
		sort.Strings(vals)
		for _, v := range vals {
			if b.Len() > 0 {
				b.WriteByte('&')
			}
			b.WriteString(awsEscape(k))
			b.WriteByte('=')
			b.WriteString(awsEscape(v))
		}
	}
	return b.String()
}

func awsEscape(s string) string {
	const unreserved = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_.~"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if strings.IndexByte(unreserved, ch) >= 0 {
			b.WriteByte(ch)
		} else {
			fmt.Fprintf(&b, "%%%02X", ch)
		}
	}
	return b.String()
}

func httpErr(op string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	return fmt.Errorf("s3 %s: %s: %s", op, resp.Status, strings.TrimSpace(string(body)))
}

// GuessContentType maps a filename to a content type for browser-friendly
// artifact viewing.
func GuessContentType(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".xml":
		return "application/xml"
	case ".json":
		return "application/json"
	case ".log", ".txt":
		return "text/plain; charset=utf-8"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".html":
		return "text/html; charset=utf-8"
	case ".zip":
		return "application/zip"
	case ".gz", ".tgz":
		return "application/gzip"
	}
	return "application/octet-stream"
}
