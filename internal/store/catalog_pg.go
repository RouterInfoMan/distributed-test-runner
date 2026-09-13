package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/andrei/distributed-test-platform/internal/config"
	"github.com/andrei/distributed-test-platform/internal/model"
	"github.com/andrei/distributed-test-platform/internal/pg"
)

// The catalog tables are one row per pool, per membership of a virtual pool,
// per node, per group, per user, per group membership and per quota rule.
// Every field has a column, so the tables are editable by hand and the
// loader is a plain column-to-field mapping; SaveCatalog upserts every row
// in one transaction and deletes what the catalog no longer has.

const poolColumns = `name, description, runtime, task_driver, image, command, runner_command, runner_url,
  docker_network, cache_dir, host_volume, constraints`

func (b *pgBackend) LoadCatalog() (*config.Catalog, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	q := func(sql string) ([]pg.Row, error) {
		res, err := b.conn.Query(ctx, sql)
		if err != nil {
			return nil, fmt.Errorf("postgres: catalog: %w", err)
		}
		return res.Rows, nil
	}

	pools, err := q(`SELECT ` + poolColumns + ` FROM pools ORDER BY position, name`)
	if err != nil {
		return nil, err
	}
	if len(pools) == 0 {
		return nil, nil // never seeded
	}
	cat := &config.Catalog{}
	for _, r := range pools {
		p, err := poolFromRow(r)
		if err != nil {
			return nil, err
		}
		cat.Pools = append(cat.Pools, p)
	}
	members, err := q(`SELECT pool_name, member_name FROM pool_members ORDER BY pool_name, position, member_name`)
	if err != nil {
		return nil, err
	}
	for _, r := range members {
		if p, ok := cat.Pool(text(r[0])); ok {
			p.Spans = append(p.Spans, text(r[1]))
		}
	}

	nodes, err := q(`SELECT name, pool_name FROM nodes ORDER BY name`)
	if err != nil {
		return nil, err
	}
	for _, r := range nodes {
		cat.Nodes = append(cat.Nodes, config.Node{Name: text(r[0]), Pool: text(r[1])})
	}

	groups, err := q(`SELECT name, description FROM groups ORDER BY position, name`)
	if err != nil {
		return nil, err
	}
	for _, r := range groups {
		cat.Groups = append(cat.Groups, config.Group{Name: text(r[0]), Description: text(r[1])})
	}

	users, err := q(`SELECT name, display_name, email FROM users ORDER BY name`)
	if err != nil {
		return nil, err
	}
	memberships, err := q(`SELECT user_name, group_name FROM group_members ORDER BY user_name, group_name`)
	if err != nil {
		return nil, err
	}
	inGroups := map[string][]string{}
	for _, r := range memberships {
		inGroups[text(r[0])] = append(inGroups[text(r[0])], text(r[1]))
	}
	for _, r := range users {
		cat.Users = append(cat.Users, config.User{Name: text(r[0]), DisplayName: text(r[1]), Email: text(r[2]), Groups: inGroups[text(r[0])]})
	}

	rules, err := q(`SELECT subject_kind, user_name, group_name, together, scope_kind, pool_name, node_name, max_slots, note
FROM quota_rules ORDER BY scope_kind, pool_name, node_name, subject_kind, group_name, user_name, together`)
	if err != nil {
		return nil, err
	}
	for _, r := range rules {
		rule := config.QuotaRule{Subject: text(r[0]), Together: text(r[3]) == "t", Scope: text(r[4]),
			MaxSlots: integer(r[7]), Note: text(r[8])}
		rule.Name = text(r[1]) + text(r[2]) // exactly one is set, per the CHECKs
		rule.Target = text(r[5]) + text(r[6])
		cat.Quotas = append(cat.Quotas, rule)
	}
	return cat, nil
}

func poolFromRow(r pg.Row) (config.Pool, error) {
	p := config.Pool{
		Name:          text(r[0]),
		Description:   text(r[1]),
		Runtime:       model.Runtime(text(r[2])),
		TaskDriver:    text(r[3]),
		RunnerURL:     text(r[7]),
		DockerNetwork: text(r[8]),
		CacheDir:      text(r[9]),
		HostVolume:    text(r[10]),
	}
	p.Default.Image = text(r[4])
	p.Default.Command = pg.ParseArray(text(r[5]))
	p.RunnerCommand = pg.ParseArray(text(r[6]))
	if err := jsonInto(r[11], &p.Constraints); err != nil {
		return p, fmt.Errorf("postgres: pool %s constraints: %w", p.Name, err)
	}
	return p, nil
}

// SaveCatalog replaces every catalog row in one transaction.
func (b *pgBackend) SaveCatalog(cat *config.Catalog) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := b.conn.Exec(ctx, `BEGIN`); err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	if err := b.saveCatalogTx(ctx, cat); err != nil {
		b.conn.Exec(ctx, `ROLLBACK`)
		return fmt.Errorf("postgres: save catalog: %w", err)
	}
	if _, err := b.conn.Exec(ctx, `COMMIT`); err != nil {
		return fmt.Errorf("postgres: commit catalog: %w", err)
	}
	return nil
}

func (b *pgBackend) saveCatalogTx(ctx context.Context, cat *config.Catalog) error {
	exec := func(sql string, args ...any) error {
		_, err := b.conn.Exec(ctx, sql, args...)
		return err
	}
	// Entities are upserted so created_at survives and the updated_at trigger
	// fires only for rows whose values changed; rows no longer in the catalog
	// are deleted afterwards (cascading to their memberships). The pure join
	// tables are simply rewritten.
	keep := func(names []string) string {
		if len(names) == 0 {
			return "''"
		}
		quoted := make([]string, len(names))
		for i, n := range names {
			quoted[i] = "'" + strings.ReplaceAll(n, "'", "''") + "'"
		}
		return strings.Join(quoted, ", ")
	}

	var poolNames []string
	for i, p := range cat.Pools {
		poolNames = append(poolNames, p.Name)
		if err := exec(`INSERT INTO pools (`+poolColumns+`, position) VALUES
  ($1, $2, $3::pool_runtime, $4::task_driver, $5, $6::text[], $7::text[], $8, $9, $10, $11, $12::jsonb, $13)
ON CONFLICT (name) DO UPDATE SET
  description = EXCLUDED.description, runtime = EXCLUDED.runtime, task_driver = EXCLUDED.task_driver,
  image = EXCLUDED.image, command = EXCLUDED.command, runner_command = EXCLUDED.runner_command,
  runner_url = EXCLUDED.runner_url, docker_network = EXCLUDED.docker_network, cache_dir = EXCLUDED.cache_dir,
  host_volume = EXCLUDED.host_volume, constraints = EXCLUDED.constraints, position = EXCLUDED.position
WHERE (pools.description, pools.runtime, pools.task_driver, pools.image, pools.command, pools.runner_command,
       pools.runner_url, pools.docker_network, pools.cache_dir, pools.host_volume, pools.constraints, pools.position)
  IS DISTINCT FROM
      (EXCLUDED.description, EXCLUDED.runtime, EXCLUDED.task_driver, EXCLUDED.image, EXCLUDED.command, EXCLUDED.runner_command,
       EXCLUDED.runner_url, EXCLUDED.docker_network, EXCLUDED.cache_dir, EXCLUDED.host_volume, EXCLUDED.constraints, EXCLUDED.position)`,
			p.Name, p.Description, nullIfEmpty(string(p.Runtime)), nullIfEmpty(p.TaskDriver),
			p.Default.Image, pg.Array(p.Default.Command), pg.Array(p.RunnerCommand), p.RunnerURL, p.DockerNetwork,
			p.CacheDir, p.HostVolume, jsonOf(p.Constraints, "{}"), i); err != nil {
			return err
		}
	}
	if err := exec(`DELETE FROM pools WHERE name NOT IN (` + keep(poolNames) + `)`); err != nil {
		return err
	}
	if err := exec(`DELETE FROM pool_members`); err != nil {
		return err
	}
	for _, p := range cat.Pools {
		for i, m := range p.Spans {
			if err := exec(`INSERT INTO pool_members (pool_name, member_name, position) VALUES ($1, $2, $3)`, p.Name, m, i); err != nil {
				return err
			}
		}
	}

	var nodeNames []string
	for _, n := range cat.Nodes {
		nodeNames = append(nodeNames, n.Name)
		if err := exec(`INSERT INTO nodes (name, pool_name) VALUES ($1, $2)
ON CONFLICT (name) DO UPDATE SET pool_name = EXCLUDED.pool_name
WHERE nodes.pool_name IS DISTINCT FROM EXCLUDED.pool_name`, n.Name, nullIfEmpty(n.Pool)); err != nil {
			return err
		}
	}
	if err := exec(`DELETE FROM nodes WHERE name NOT IN (` + keep(nodeNames) + `)`); err != nil {
		return err
	}

	var groupNames []string
	for i, g := range cat.Groups {
		groupNames = append(groupNames, g.Name)
		if err := exec(`INSERT INTO groups (name, description, position) VALUES ($1, $2, $3)
ON CONFLICT (name) DO UPDATE SET description = EXCLUDED.description, position = EXCLUDED.position
WHERE (groups.description, groups.position) IS DISTINCT FROM (EXCLUDED.description, EXCLUDED.position)`,
			g.Name, g.Description, i); err != nil {
			return err
		}
	}
	if err := exec(`DELETE FROM groups WHERE name NOT IN (` + keep(groupNames) + `)`); err != nil {
		return err
	}

	var userNames []string
	for _, u := range cat.Users {
		userNames = append(userNames, u.Name)
		if err := exec(`INSERT INTO users (name, display_name, email) VALUES ($1, $2, $3)
ON CONFLICT (name) DO UPDATE SET display_name = EXCLUDED.display_name, email = EXCLUDED.email
WHERE (users.display_name, users.email) IS DISTINCT FROM (EXCLUDED.display_name, EXCLUDED.email)`,
			u.Name, u.DisplayName, u.Email); err != nil {
			return err
		}
	}
	if err := exec(`DELETE FROM users WHERE name NOT IN (` + keep(userNames) + `)`); err != nil {
		return err
	}
	if err := exec(`DELETE FROM group_members`); err != nil {
		return err
	}
	for _, u := range cat.Users {
		for _, g := range u.Groups {
			if err := exec(`INSERT INTO group_members (group_name, user_name) VALUES ($1, $2)`, g, u.Name); err != nil {
				return err
			}
		}
	}

	// Rules are identified by their whole subject × scope tuple (the UNIQUE
	// constraint); an unchanged rule is left alone, a changed limit or note
	// is updated in place, and rules the catalog no longer has are deleted.
	for _, r := range cat.Quotas {
		var userName, groupName, poolName, nodeName any
		switch r.Subject {
		case config.SubjectUser:
			userName = r.Name
		case config.SubjectGroup:
			groupName = r.Name
		}
		switch r.Scope {
		case config.ScopePool:
			poolName = r.Target
		case config.ScopeNode:
			nodeName = r.Target
		}
		if err := exec(`INSERT INTO quota_rules (subject_kind, user_name, group_name, together, scope_kind, pool_name, node_name, max_slots, note)
VALUES ($1::quota_subject, $2, $3, $4, $5::quota_scope, $6, $7, $8, $9)
ON CONFLICT (subject_kind, user_name, group_name, together, scope_kind, pool_name, node_name)
DO UPDATE SET max_slots = EXCLUDED.max_slots, note = EXCLUDED.note
WHERE (quota_rules.max_slots, quota_rules.note) IS DISTINCT FROM (EXCLUDED.max_slots, EXCLUDED.note)`,
			r.Subject, userName, groupName, r.Together, r.Scope, poolName, nodeName, r.MaxSlots, r.Note); err != nil {
			return err
		}
	}
	var keys []string
	for _, r := range cat.Quotas {
		keys = append(keys, r.Key())
	}
	if err := exec(`DELETE FROM quota_rules WHERE
  subject_kind::text || COALESCE(':' || user_name, '') || COALESCE(':' || group_name, '') || CASE WHEN together THEN '/together' ELSE '' END
  || '@' || scope_kind::text || COALESCE(':' || pool_name, '') || COALESCE(':' || node_name, '')
  NOT IN (` + keep(keys) + `)`); err != nil {
		return err
	}
	return nil
}

// --- column helpers ---------------------------------------------------------

func text(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

func integer(v *string) int {
	if v == nil {
		return 0
	}
	n, _ := strconv.Atoi(*v)
	return n
}

// nullIfEmpty maps "" to SQL NULL for nullable enum columns.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func jsonInto(v *string, into any) error {
	if v == nil || *v == "" {
		return nil
	}
	return json.Unmarshal([]byte(*v), into)
}

// jsonOf renders a map, with an explicit empty value for nil so the column
// never holds SQL NULL.
func jsonOf(v any, empty string) string {
	b, err := json.Marshal(v)
	if err != nil || string(b) == "null" {
		return empty
	}
	return string(b)
}
