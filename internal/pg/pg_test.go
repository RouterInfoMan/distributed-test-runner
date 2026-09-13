package pg

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// The SCRAM-SHA-256 exchange from RFC 7677 section 3, with the user name
// present in client-first as in the RFC (PostgreSQL ignores it either way).
func TestSCRAMReferenceExchange(t *testing.T) {
	s := newScram("pencil")
	s.nonce = "rOprNGfwEbeRWgbNEkqO"
	s.firstBare = "n=user,r=" + s.nonce
	serverFirst := "r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096"
	final, err := s.clientFinal(serverFirst)
	if err != nil {
		t.Fatal(err)
	}
	want := "c=biws,r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,p=dHzbZapWIk4jUhN+Ute9ytag9zjfMHgsqmmiz7AndVQ="
	if final != want {
		t.Fatalf("client-final:\n got %s\nwant %s", final, want)
	}
	if err := s.verifyServer("v=6rriTRBi23WpRR/wtup+mMhUZUn/dB5nLTJRsjl95G4="); err != nil {
		t.Fatal(err)
	}
	if err := s.verifyServer("v=AAAA"); err == nil {
		t.Fatal("a wrong server signature must be rejected")
	}
}

func TestParseDSN(t *testing.T) {
	c, err := ParseDSN("postgres://dtp:s3cret@db.internal:6432/dtpdb?sslmode=require")
	if err != nil {
		t.Fatal(err)
	}
	if c.User != "dtp" || c.Password != "s3cret" || c.Host != "db.internal" || c.Port != "6432" ||
		c.Database != "dtpdb" || c.SSLMode != "require" {
		t.Fatalf("unexpected parse: %+v", c)
	}
	c, _ = ParseDSN("postgresql://u@localhost/x")
	if c.Port != "5432" || c.SSLMode != "disable" || c.Database != "x" {
		t.Fatalf("defaults not applied: %+v", c)
	}
	if _, err := ParseDSN("mysql://u@h/x"); err == nil {
		t.Fatal("non-postgres scheme must be rejected")
	}
}

func TestWireParsers(t *testing.T) {
	// RowDescription with two columns.
	var m msg
	m.int16(2)
	for _, name := range []string{"id", "doc"} {
		m.cstring(name)
		m.int32(0)
		m.int16(0)
		m.int32(25)
		m.int16(-1)
		m.int32(-1)
		m.int16(0)
	}
	cols := parseRowDescription(m.bytes())
	if len(cols) != 2 || cols[0] != "id" || cols[1] != "doc" {
		t.Fatalf("columns: %v", cols)
	}
	// DataRow with a value and a NULL.
	m = msg{}
	m.int16(2)
	m.int32(3)
	m.raw([]byte("abc"))
	m.int32(-1)
	row := parseDataRow(m.bytes())
	if row[0] == nil || *row[0] != "abc" || row[1] != nil {
		t.Fatalf("row: %v", row)
	}
	// ErrorResponse.
	e := parseError([]byte("SERROR\x00C42P01\x00Mrelation \"x\" does not exist\x00\x00"))
	if e.Code != "42P01" || e.Severity != "ERROR" || e.Message == "" {
		t.Fatalf("error: %+v", e)
	}
	if got := (Result{Tag: "UPDATE 7"}).Affected(); got != 7 {
		t.Fatalf("affected: %d", got)
	}
}

// TestLive talks to a real server: DTP_TEST_PG_DSN=postgres://… go test ./internal/pg
func TestLive(t *testing.T) {
	dsn := os.Getenv("DTP_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("DTP_TEST_PG_DSN not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if _, err := c.Exec(ctx, `CREATE TEMP TABLE t (id TEXT PRIMARY KEY, n INT, at TIMESTAMPTZ, doc JSONB)`); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	res, err := c.Exec(ctx, `INSERT INTO t VALUES ($1, $2, $3, $4)`, "a", 7, now, `{"k":"v"}`)
	if err != nil {
		t.Fatal(err)
	}
	if res.Affected() != 1 {
		t.Fatalf("affected %d", res.Affected())
	}
	if _, err := c.Exec(ctx, `INSERT INTO t (id, n) VALUES ($1, $2)`, "b", nil); err != nil {
		t.Fatal(err)
	}
	// Upsert path the store relies on.
	if _, err := c.Exec(ctx, `INSERT INTO t VALUES ($1, $2) ON CONFLICT (id) DO UPDATE SET n = EXCLUDED.n`, "a", 8); err != nil {
		t.Fatal(err)
	}
	rows, err := c.Query(ctx, `SELECT id, n, at, doc->>'k' FROM t ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows.Rows) != 2 || rows.Columns[3] != "?column?" {
		t.Fatalf("rows: %+v", rows)
	}
	a, b := rows.Rows[0], rows.Rows[1]
	if *a[0] != "a" || *a[1] != "8" || *a[3] != "v" || a[2] == nil {
		t.Fatalf("row a: %v %v %v %v", *a[0], *a[1], a[2], *a[3])
	}
	if *b[0] != "b" || b[1] != nil || b[2] != nil || b[3] != nil {
		t.Fatalf("row b must carry NULLs: %v", b)
	}
	// A server error comes back typed, and the connection stays usable.
	if _, err := c.Exec(ctx, `SELECT * FROM no_such_table`); err == nil {
		t.Fatal("expected an error")
	} else if pe, ok := err.(*Error); !ok || pe.Code != "42P01" {
		t.Fatalf("want a typed 42P01 error, got %v", err)
	}
	if r, err := c.Query(ctx, `SELECT 1 + 1`); err != nil || *r.Rows[0][0] != "2" {
		t.Fatalf("connection unusable after an error: %v %v", r, err)
	}
}

func TestSplitStatements(t *testing.T) {
	script := `-- a comment; with a semicolon
CREATE TABLE t (s TEXT DEFAULT 'a;b'); /* block; comment */
CREATE FUNCTION f() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  NEW.updated_at := now(); RETURN NEW;
END;
$$;
CREATE FUNCTION g() RETURNS text AS $body$ SELECT ';' $body$ LANGUAGE sql;
INSERT INTO t VALUES ('it''s;fine')`
	stmts := SplitStatements(script)
	if len(stmts) != 4 {
		t.Fatalf("want 4 statements, got %d: %q", len(stmts), stmts)
	}
	if !strings.Contains(stmts[1], "RETURN NEW;") || !strings.HasSuffix(stmts[1], "$$") {
		t.Fatalf("dollar-quoted body must stay whole: %q", stmts[1])
	}
	if !strings.Contains(stmts[2], "SELECT ';'") || !strings.Contains(stmts[3], "it''s;fine") {
		t.Fatalf("quoted semicolons must survive: %q %q", stmts[2], stmts[3])
	}
}

func TestArrayCodec(t *testing.T) {
	vals := []string{"/opt/dtp/run-suite.sh", `with "quote"`, `back\slash`, "", "a,b"}
	lit := Array(vals)
	got := ParseArray(lit)
	if len(got) != len(vals) {
		t.Fatalf("round trip lost elements: %q -> %q", lit, got)
	}
	for i := range vals {
		if got[i] != vals[i] {
			t.Fatalf("element %d: want %q got %q (literal %s)", i, vals[i], got[i], lit)
		}
	}
	if got := ParseArray("{}"); len(got) != 0 || got == nil {
		t.Fatalf("empty array: %v", got)
	}
	if got := ParseArray(`{a,"b c",d}`); len(got) != 3 || got[1] != "b c" {
		t.Fatalf("server-style literal: %v", got)
	}
}
