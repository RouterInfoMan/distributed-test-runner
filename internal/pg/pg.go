// Package pg is a small PostgreSQL client on the standard library: the v3
// wire protocol with SCRAM-SHA-256, MD5 and cleartext authentication, TLS,
// and the extended query protocol with text-format parameters. It covers
// what the master's store needs - parameterized statements against a few
// tables - and nothing else, so the build stays free of third-party modules.
package pg

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Config is a parsed connection URL: postgres://user:pass@host:port/db?sslmode=…
type Config struct {
	Host, Port, User, Password, Database string
	SSLMode                              string // disable (default) | require | verify-full
	AppName                              string
}

// ParseDSN accepts postgres:// and postgresql:// URLs.
func ParseDSN(dsn string) (Config, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return Config{}, fmt.Errorf("pg: bad dsn: %w", err)
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return Config{}, fmt.Errorf("pg: dsn scheme must be postgres://, got %q", u.Scheme)
	}
	c := Config{
		Host:     u.Hostname(),
		Port:     u.Port(),
		Database: strings.TrimPrefix(u.Path, "/"),
		SSLMode:  u.Query().Get("sslmode"),
		AppName:  u.Query().Get("application_name"),
	}
	if u.User != nil {
		c.User = u.User.Username()
		c.Password, _ = u.User.Password()
	}
	if c.Host == "" {
		c.Host = "localhost"
	}
	if c.Port == "" {
		c.Port = "5432"
	}
	if c.SSLMode == "" {
		c.SSLMode = "disable"
	}
	if c.AppName == "" {
		c.AppName = "dtp-master"
	}
	if c.User == "" {
		return Config{}, errors.New("pg: dsn has no user")
	}
	return c, nil
}

// Conn is one connection. Calls are serialized; the master's store holds its
// own lock around every mutation, so one connection is all it needs.
type Conn struct {
	cfg Config
	mu  sync.Mutex
	nc  net.Conn
	r   *bufio.Reader
	w   *bufio.Writer
}

// Error is a server-reported error (ErrorResponse).
type Error struct {
	Severity, Code, Message, Detail string
}

func (e *Error) Error() string { return fmt.Sprintf("pg: %s %s: %s", e.Severity, e.Code, e.Message) }

// Connect dials, negotiates TLS when asked, and authenticates.
func Connect(ctx context.Context, dsn string) (*Conn, error) {
	cfg, err := ParseDSN(dsn)
	if err != nil {
		return nil, err
	}
	d := net.Dialer{Timeout: 10 * time.Second}
	nc, err := d.DialContext(ctx, "tcp", net.JoinHostPort(cfg.Host, cfg.Port))
	if err != nil {
		return nil, fmt.Errorf("pg: dial %s:%s: %w", cfg.Host, cfg.Port, err)
	}
	c := &Conn{cfg: cfg, nc: nc}
	if cfg.SSLMode != "disable" {
		if err := c.startTLS(); err != nil {
			nc.Close()
			return nil, err
		}
	}
	c.r = bufio.NewReader(c.nc)
	c.w = bufio.NewWriter(c.nc)
	if dl, ok := ctx.Deadline(); ok {
		c.nc.SetDeadline(dl)
		defer c.nc.SetDeadline(time.Time{})
	}
	if err := c.startup(); err != nil {
		nc.Close()
		return nil, err
	}
	return c, nil
}

func (c *Conn) Close() error { return c.nc.Close() }

// ---------------------------------------------------------------------------
// startup + authentication
// ---------------------------------------------------------------------------

func (c *Conn) startTLS() error {
	// SSLRequest: length 8, code 80877103; the server answers one byte.
	var req [8]byte
	binary.BigEndian.PutUint32(req[0:], 8)
	binary.BigEndian.PutUint32(req[4:], 80877103)
	if _, err := c.nc.Write(req[:]); err != nil {
		return err
	}
	var ans [1]byte
	if _, err := io.ReadFull(c.nc, ans[:]); err != nil {
		return err
	}
	if ans[0] != 'S' {
		return errors.New("pg: server refused TLS")
	}
	tc := &tls.Config{ServerName: c.cfg.Host, InsecureSkipVerify: c.cfg.SSLMode != "verify-full"}
	c.nc = tls.Client(c.nc, tc)
	return nil
}

func (c *Conn) startup() error {
	var b msg
	b.int32(196608) // protocol 3.0
	for _, kv := range [][2]string{{"user", c.cfg.User}, {"database", c.cfg.Database},
		{"application_name", c.cfg.AppName}, {"client_encoding", "UTF8"}} {
		if kv[1] != "" {
			b.cstring(kv[0])
			b.cstring(kv[1])
		}
	}
	b.byte(0)
	if err := c.sendUntyped(b.bytes()); err != nil {
		return err
	}

	var scram *scramClient
	for {
		typ, body, err := c.read()
		if err != nil {
			return err
		}
		switch typ {
		case 'E':
			return parseError(body)
		case 'R':
			code := binary.BigEndian.Uint32(body[:4])
			switch code {
			case 0: // AuthenticationOk
			case 3: // cleartext
				if err := c.send('p', cstr(c.cfg.Password)); err != nil {
					return err
				}
			case 5: // md5
				salt := body[4:8]
				inner := md5hex([]byte(c.cfg.Password + c.cfg.User))
				outer := md5hex(append([]byte(inner), salt...))
				if err := c.send('p', cstr("md5"+outer)); err != nil {
					return err
				}
			case 10: // SASL: list of mechanisms
				if !strings.Contains(string(body[4:]), "SCRAM-SHA-256") {
					return errors.New("pg: server offers no SCRAM-SHA-256")
				}
				scram = newScram(c.cfg.Password)
				first := scram.clientFirst()
				var m msg
				m.cstring("SCRAM-SHA-256")
				m.int32(int32(len(first)))
				m.raw([]byte(first))
				if err := c.send('p', m.bytes()); err != nil {
					return err
				}
			case 11: // SASLContinue
				final, err := scram.clientFinal(string(body[4:]))
				if err != nil {
					return err
				}
				if err := c.send('p', []byte(final)); err != nil {
					return err
				}
			case 12: // SASLFinal
				if err := scram.verifyServer(string(body[4:])); err != nil {
					return err
				}
			default:
				return fmt.Errorf("pg: unsupported authentication method %d", code)
			}
		case 'S', 'K', 'N': // ParameterStatus, BackendKeyData, Notice
		case 'Z':
			return nil
		default:
			return fmt.Errorf("pg: unexpected message %q during startup", typ)
		}
	}
}

func md5hex(b []byte) string {
	sum := md5.Sum(b)
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// SCRAM-SHA-256 (RFC 7677) client side
// ---------------------------------------------------------------------------

type scramClient struct {
	password  string
	nonce     string
	firstBare string
	authMsg   string
	salted    []byte
}

func newScram(password string) *scramClient {
	var raw [18]byte
	rand.Read(raw[:])
	return &scramClient{password: password, nonce: base64.StdEncoding.EncodeToString(raw[:])}
}

// clientFirst omits the user name: PostgreSQL takes it from the startup
// message and ignores the SASL one.
func (s *scramClient) clientFirst() string {
	s.firstBare = "n=,r=" + s.nonce
	return "n,," + s.firstBare
}

func (s *scramClient) clientFinal(serverFirst string) (string, error) {
	var nonce, salt string
	iters := 0
	for _, part := range strings.Split(serverFirst, ",") {
		if len(part) < 2 || part[1] != '=' {
			continue
		}
		switch part[0] {
		case 'r':
			nonce = part[2:]
		case 's':
			salt = part[2:]
		case 'i':
			iters, _ = strconv.Atoi(part[2:])
		}
	}
	if !strings.HasPrefix(nonce, s.nonce) || salt == "" || iters <= 0 {
		return "", errors.New("pg: malformed SCRAM server-first message")
	}
	saltBytes, err := base64.StdEncoding.DecodeString(salt)
	if err != nil {
		return "", fmt.Errorf("pg: SCRAM salt: %w", err)
	}
	s.salted = pbkdf2SHA256([]byte(s.password), saltBytes, iters, sha256.Size)
	clientKey := hmacSHA256(s.salted, []byte("Client Key"))
	storedKey := sha256.Sum256(clientKey)
	withoutProof := "c=biws,r=" + nonce // biws = base64("n,,")
	s.authMsg = s.firstBare + "," + serverFirst + "," + withoutProof
	sig := hmacSHA256(storedKey[:], []byte(s.authMsg))
	proof := make([]byte, len(clientKey))
	for i := range clientKey {
		proof[i] = clientKey[i] ^ sig[i]
	}
	return withoutProof + ",p=" + base64.StdEncoding.EncodeToString(proof), nil
}

func (s *scramClient) verifyServer(serverFinal string) error {
	if strings.HasPrefix(serverFinal, "e=") {
		return fmt.Errorf("pg: SCRAM: %s", serverFinal[2:])
	}
	want := strings.TrimPrefix(serverFinal, "v=")
	serverKey := hmacSHA256(s.salted, []byte("Server Key"))
	got := base64.StdEncoding.EncodeToString(hmacSHA256(serverKey, []byte(s.authMsg)))
	if !hmac.Equal([]byte(got), []byte(want)) {
		return errors.New("pg: SCRAM server signature mismatch")
	}
	return nil
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

// pbkdf2SHA256 is RFC 8018 PBKDF2 with HMAC-SHA-256, the "Hi" of SCRAM.
func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	var out []byte
	for block := 1; len(out) < keyLen; block++ {
		h := hmac.New(sha256.New, password)
		h.Write(salt)
		h.Write([]byte{byte(block >> 24), byte(block >> 16), byte(block >> 8), byte(block)})
		u := h.Sum(nil)
		t := append([]byte(nil), u...)
		for i := 1; i < iter; i++ {
			h = hmac.New(sha256.New, password)
			h.Write(u)
			u = h.Sum(nil)
			for j := range t {
				t[j] ^= u[j]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}

// ---------------------------------------------------------------------------
// queries (extended protocol, unnamed statement/portal, text format)
// ---------------------------------------------------------------------------

// Row is one result row; a nil entry is SQL NULL. Values are the server's
// text representation.
type Row []*string

// Result is the outcome of one statement.
type Result struct {
	Columns []string
	Rows    []Row
	Tag     string // CommandComplete tag, e.g. "INSERT 0 1"
}

// Affected parses the row count out of the command tag.
func (r Result) Affected() int64 {
	f := strings.Fields(r.Tag)
	if len(f) == 0 {
		return 0
	}
	n, _ := strconv.ParseInt(f[len(f)-1], 10, 64)
	return n
}

// Exec runs one statement with positional parameters ($1, $2, …).
func (c *Conn) Exec(ctx context.Context, sql string, args ...any) (Result, error) {
	return c.Query(ctx, sql, args...)
}

// Query runs one statement and returns its rows.
func (c *Conn) Query(ctx context.Context, sql string, args ...any) (Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if dl, ok := ctx.Deadline(); ok {
		c.nc.SetDeadline(dl)
		defer c.nc.SetDeadline(time.Time{})
	}

	params := make([][]byte, len(args))
	for i, a := range args {
		p, err := encode(a)
		if err != nil {
			return Result{}, fmt.Errorf("pg: parameter %d: %w", i+1, err)
		}
		params[i] = p
	}

	var m msg
	// Parse: unnamed statement, let the server infer parameter types.
	m.cstring("")
	m.cstring(sql)
	m.int16(0)
	c.frame('P', m.bytes())
	// Bind: unnamed portal, all parameters text, all results text.
	m = msg{}
	m.cstring("")
	m.cstring("")
	m.int16(0)
	m.int16(int16(len(params)))
	for _, p := range params {
		if p == nil {
			m.int32(-1)
			continue
		}
		m.int32(int32(len(p)))
		m.raw(p)
	}
	m.int16(0)
	c.frame('B', m.bytes())
	c.frame('D', append([]byte{'P'}, 0))
	m = msg{}
	m.cstring("")
	m.int32(0)
	c.frame('E', m.bytes())
	c.frame('S', nil)
	if err := c.w.Flush(); err != nil {
		return Result{}, fmt.Errorf("pg: write: %w", err)
	}

	var res Result
	var firstErr error
	for {
		typ, body, err := c.read()
		if err != nil {
			return Result{}, err
		}
		switch typ {
		case '1', '2', 'n', 'N', 'S', 's': // parse/bind complete, no data, notice, param status, portal suspended
		case 'T':
			res.Columns = parseRowDescription(body)
		case 'D':
			res.Rows = append(res.Rows, parseDataRow(body))
		case 'C':
			res.Tag = strings.TrimRight(string(body), "\x00")
		case 'E':
			if firstErr == nil {
				firstErr = parseError(body)
			}
		case 'Z':
			return res, firstErr
		default:
			return Result{}, fmt.Errorf("pg: unexpected message %q", typ)
		}
	}
}

// encode renders a parameter in text format.
func encode(a any) ([]byte, error) {
	switch v := a.(type) {
	case nil:
		return nil, nil
	case string:
		return []byte(v), nil
	case []byte:
		return v, nil
	case json.RawMessage:
		return []byte(v), nil
	case int:
		return []byte(strconv.Itoa(v)), nil
	case int64:
		return []byte(strconv.FormatInt(v, 10)), nil
	case bool:
		if v {
			return []byte("true"), nil
		}
		return []byte("false"), nil
	case time.Time:
		return []byte(v.UTC().Format(time.RFC3339Nano)), nil
	case *time.Time:
		if v == nil {
			return nil, nil
		}
		return []byte(v.UTC().Format(time.RFC3339Nano)), nil
	case fmt.Stringer:
		return []byte(v.String()), nil
	}
	return nil, fmt.Errorf("unsupported type %T", a)
}

func parseRowDescription(b []byte) []string {
	n := int(binary.BigEndian.Uint16(b[:2]))
	b = b[2:]
	cols := make([]string, 0, n)
	for i := 0; i < n; i++ {
		end := indexNul(b)
		cols = append(cols, string(b[:end]))
		b = b[end+1+18:] // name NUL + tableoid(4) attnum(2) typoid(4) typlen(2) typmod(4) format(2)
	}
	return cols
}

func parseDataRow(b []byte) Row {
	n := int(binary.BigEndian.Uint16(b[:2]))
	b = b[2:]
	row := make(Row, n)
	for i := 0; i < n; i++ {
		l := int32(binary.BigEndian.Uint32(b[:4]))
		b = b[4:]
		if l < 0 {
			continue
		}
		s := string(b[:l])
		row[i] = &s
		b = b[l:]
	}
	return row
}

func parseError(b []byte) *Error {
	e := &Error{}
	for len(b) > 0 && b[0] != 0 {
		code := b[0]
		end := indexNul(b[1:]) + 1
		val := string(b[1:end])
		switch code {
		case 'S':
			e.Severity = val
		case 'C':
			e.Code = val
		case 'M':
			e.Message = val
		case 'D':
			e.Detail = val
		}
		b = b[end+1:]
	}
	return e
}

func indexNul(b []byte) int {
	for i, c := range b {
		if c == 0 {
			return i
		}
	}
	return len(b)
}

// ---------------------------------------------------------------------------
// framing
// ---------------------------------------------------------------------------

type msg struct{ b []byte }

func (m *msg) byte(v byte)      { m.b = append(m.b, v) }
func (m *msg) raw(v []byte)     { m.b = append(m.b, v...) }
func (m *msg) cstring(s string) { m.b = append(append(m.b, s...), 0) }
func (m *msg) int16(v int16)    { m.b = binary.BigEndian.AppendUint16(m.b, uint16(v)) }
func (m *msg) int32(v int32)    { m.b = binary.BigEndian.AppendUint32(m.b, uint32(v)) }
func (m *msg) bytes() []byte    { return m.b }

func cstr(s string) []byte { return append([]byte(s), 0) }

// frame buffers one typed message: type byte, int32 length (self-inclusive), body.
func (c *Conn) frame(typ byte, body []byte) {
	var hdr [5]byte
	hdr[0] = typ
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(body)+4))
	c.w.Write(hdr[:])
	c.w.Write(body)
}

func (c *Conn) send(typ byte, body []byte) error {
	c.frame(typ, body)
	return c.w.Flush()
}

// sendUntyped is for the startup message, which has no type byte.
func (c *Conn) sendUntyped(body []byte) error {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(body)+4))
	c.w.Write(hdr[:])
	c.w.Write(body)
	return c.w.Flush()
}

func (c *Conn) read() (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(c.r, hdr[:]); err != nil {
		return 0, nil, fmt.Errorf("pg: read: %w", err)
	}
	n := int(binary.BigEndian.Uint32(hdr[1:])) - 4
	if n < 0 || n > 64<<20 {
		return 0, nil, fmt.Errorf("pg: bad message length %d", n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(c.r, body); err != nil {
		return 0, nil, fmt.Errorf("pg: read: %w", err)
	}
	return hdr[0], body, nil
}
