package store

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"time"

	"github.com/andrei/distributed-test-platform/internal/model"
	"github.com/andrei/distributed-test-platform/internal/pg"
)

// pgBackend keeps regressions and runs in two PostgreSQL tables. The columns
// are what people query - who, which pool, what state, since when - and the
// full document rides along as JSONB, so the schema never lags the model.
// The queue is `SELECT … FROM runs WHERE state = 'queued'`.
type pgBackend struct {
	dsn  string
	conn *pg.Conn
}

// schemaSQL is the whole schema, one file, also what a DBA reads or runs by
// hand (the compose stack mounts it into Postgres' initdb directory).
//
//go:embed sql/schema.sql
var schemaSQL string

// Schema returns the SQL the master applies to an empty database.
func Schema() string { return schemaSQL }

// NewPostgresBackend connects and, when the database has no tables yet,
// applies the schema in one transaction. An existing database is used as is;
// changing the schema means recreating the database.
func NewPostgresBackend(ctx context.Context, dsn string) (Backend, error) {
	b := &pgBackend{dsn: dsn}
	if err := b.connect(ctx); err != nil {
		return nil, err
	}
	res, err := b.conn.Query(ctx, `SELECT to_regclass('runs') IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("postgres: %w", err)
	}
	if len(res.Rows) == 1 && res.Rows[0][0] != nil && *res.Rows[0][0] == "t" {
		return b, nil
	}
	if _, err := b.conn.Exec(ctx, `BEGIN`); err != nil {
		return nil, err
	}
	for _, stmt := range pg.SplitStatements(schemaSQL) {
		if _, err := b.conn.Exec(ctx, stmt); err != nil {
			b.conn.Exec(ctx, `ROLLBACK`)
			return nil, fmt.Errorf("postgres: apply schema: %w", err)
		}
	}
	if _, err := b.conn.Exec(ctx, `COMMIT`); err != nil {
		return nil, err
	}
	return b, nil
}

func (b *pgBackend) connect(ctx context.Context) error {
	c, err := pg.Connect(ctx, b.dsn)
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	b.conn = c
	return nil
}

func (b *pgBackend) Close() error { return b.conn.Close() }

// exec runs a statement, reconnecting once on a broken connection. A server
// error (bad SQL, constraint) is returned as is.
func (b *pgBackend) exec(sql string, args ...any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := b.conn.Exec(ctx, sql, args...)
	if err == nil {
		return nil
	}
	if _, server := err.(*pg.Error); server {
		return err
	}
	b.conn.Close()
	if rerr := b.connect(ctx); rerr != nil {
		return fmt.Errorf("%v (reconnect: %w)", err, rerr)
	}
	_, err = b.conn.Exec(ctx, sql, args...)
	return err
}

func (b *pgBackend) Load() ([]*record, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	regs, err := b.conn.Query(ctx, `SELECT doc FROM regressions ORDER BY submitted_at`)
	if err != nil {
		return nil, fmt.Errorf("postgres: load regressions: %w", err)
	}
	byID := map[string]*record{}
	var out []*record
	for _, row := range regs.Rows {
		var reg model.Regression
		if err := json.Unmarshal([]byte(*row[0]), &reg); err != nil {
			return nil, fmt.Errorf("postgres: regression doc: %w", err)
		}
		rec := &record{Regression: &reg, Tokens: map[string]string{}}
		byID[reg.ID] = rec
		out = append(out, rec)
	}
	runs, err := b.conn.Query(ctx, `SELECT regression_id, token, doc FROM runs ORDER BY queued_at`)
	if err != nil {
		return nil, fmt.Errorf("postgres: load runs: %w", err)
	}
	for _, row := range runs.Rows {
		rec, ok := byID[*row[0]]
		if !ok {
			continue
		}
		var run model.Run
		if err := json.Unmarshal([]byte(*row[2]), &run); err != nil {
			return nil, fmt.Errorf("postgres: run doc: %w", err)
		}
		rec.Runs = append(rec.Runs, &run)
		if tok := *row[1]; tok != "" {
			rec.Tokens[tok] = run.ID
		}
	}
	return out, nil
}

func (b *pgBackend) SaveRecord(rec *record) error {
	if err := b.SaveRegression(rec); err != nil {
		return err
	}
	for _, r := range rec.Runs {
		if err := b.SaveRun(rec, r); err != nil {
			return err
		}
	}
	return nil
}

func (b *pgBackend) SaveRegression(rec *record) error {
	reg := rec.Regression
	doc, err := json.Marshal(reg)
	if err != nil {
		return err
	}
	return b.exec(`
INSERT INTO regressions (id, name, user_name, groups, priority, state, submitted_at, started_at, finished_at, doc)
VALUES ($1, $2, $3, $4::text[], $5, $6::regression_state, $7, $8, $9, $10::jsonb)
ON CONFLICT (id) DO UPDATE SET
  name = EXCLUDED.name, user_name = EXCLUDED.user_name, groups = EXCLUDED.groups, priority = EXCLUDED.priority,
  state = EXCLUDED.state, started_at = EXCLUDED.started_at, finished_at = EXCLUDED.finished_at, doc = EXCLUDED.doc`,
		reg.ID, reg.Name, reg.User, pg.Array(reg.Groups), clampPriority(reg.Priority), string(reg.State),
		reg.SubmittedAt, reg.StartedAt, reg.FinishedAt, string(doc))
}

// clampPriority keeps a submission's priority inside the column's CHECK.
func clampPriority(p int) int {
	if p < 0 {
		return 0
	}
	if p > 100 {
		return 100
	}
	return p
}

func (b *pgBackend) SaveRun(_ *record, r *model.Run) error {
	doc, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return b.exec(`
INSERT INTO runs (id, regression_id, suite, attempt, pool, state, user_name, groups, priority, node_name,
                  queued_at, dispatched_at, started_at, finished_at, token, doc)
VALUES ($1, $2, $3, $4, $5, $6::run_state, $7, $8::text[], $9, $10, $11, $12, $13, $14, $15, $16::jsonb)
ON CONFLICT (id) DO UPDATE SET
  state = EXCLUDED.state, node_name = EXCLUDED.node_name, dispatched_at = EXCLUDED.dispatched_at,
  started_at = EXCLUDED.started_at, finished_at = EXCLUDED.finished_at, token = EXCLUDED.token, doc = EXCLUDED.doc`,
		r.ID, r.RegressionID, r.Suite, r.Attempt, r.Pool, string(r.State), r.User, pg.Array(r.Groups), clampPriority(r.Priority), r.NodeName,
		r.QueuedAt, r.DispatchedAt, r.StartedAt, r.FinishedAt, r.Token, string(doc))
}
