-- schema.sql - the master's schema.
--
-- Applied once, either by PostgreSQL itself at first init (the compose stack
-- mounts this file into docker-entrypoint-initdb.d) or by dtp-master at
-- startup when it finds no tables. There is no migration machinery: to change
-- the schema, edit this file and recreate the database (make stack-stop drops
-- the volume). Conventions: snake_case, plural tables, enums for closed sets,
-- timestamptz everywhere, CHECK constraints for the invariants the code
-- relies on, and a comment on everything a DBA might wonder about.

-- ---------------------------------------------------------------------------
-- Enumerations: the closed sets the platform reasons about.
-- ---------------------------------------------------------------------------

CREATE TYPE regression_state AS ENUM ('pending', 'running', 'passed', 'failed', 'errored', 'canceled');
CREATE TYPE run_state        AS ENUM ('queued', 'dispatched', 'running', 'passed', 'failed', 'errored', 'timeout', 'canceled');
CREATE TYPE pool_runtime     AS ENUM ('process', 'container');
CREATE TYPE task_driver      AS ENUM ('exec', 'raw_exec', 'docker', 'podman');
CREATE TYPE quota_subject    AS ENUM ('user', 'group', 'global');
CREATE TYPE quota_scope      AS ENUM ('node', 'pool', 'global');

COMMENT ON TYPE regression_state IS 'The fold over a regression''s suites: errored if any suite errored, else failed if any failed, else passed.';
COMMENT ON TYPE run_state IS 'Lifecycle of one suite attempt. failed = tests failed; errored = the harness or infrastructure failed; timeout = killed at its deadline.';
COMMENT ON TYPE pool_runtime IS 'How suites in a pool are isolated: a task directory + process cgroup, or an OCI container.';
COMMENT ON TYPE task_driver IS 'The Nomad task driver a pool uses; NULL on a pool means the one implied by its runtime.';
COMMENT ON TYPE quota_subject IS 'Who a quota rule limits: one user, the members of a group, or everyone (global).';
COMMENT ON TYPE quota_scope IS 'Where a quota rule counts slots: on one node, in one pool, or everywhere (global).';

-- updated_at is maintained by a trigger so no writer can forget it.
CREATE FUNCTION set_updated_at() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  NEW.updated_at := now();
  RETURN NEW;
END;
$$;

-- ---------------------------------------------------------------------------
-- The catalog: pools, node assignments, groups, users, quota rules. Lives only here;
-- edited through the config API / dashboard / dtp (applied at once) or by SQL
-- (then POST /api/v1/config/reload or `dtp config reload`).
-- ---------------------------------------------------------------------------

CREATE TABLE pools (
  name               TEXT PRIMARY KEY CHECK (name ~ '^[a-z0-9][a-z0-9._-]*$'),
  description        TEXT NOT NULL DEFAULT '',
  runtime            pool_runtime,                 -- NULL on a virtual pool: inherited from its first member
  task_driver        task_driver,                  -- NULL: implied by runtime (exec / docker)
  image              TEXT NOT NULL DEFAULT '',     -- container pools: the worker image
  command            TEXT[] NOT NULL DEFAULT '{}', -- the suite command, e.g. {/opt/dtp/run-suite.sh}
  runner_command     TEXT[] NOT NULL DEFAULT '{}', -- process pools: how the task launches dtp-runner
  runner_url         TEXT NOT NULL DEFAULT '',
  docker_network     TEXT NOT NULL DEFAULT '',
  cache_dir          TEXT NOT NULL DEFAULT '',
  host_volume        TEXT NOT NULL DEFAULT '',
  constraints        JSONB NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(constraints) = 'object'),
  position           INT NOT NULL DEFAULT 0,       -- display order
  created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TRIGGER pools_updated_at BEFORE UPDATE ON pools FOR EACH ROW EXECUTE FUNCTION set_updated_at();
COMMENT ON TABLE pools IS 'A pool groups nodes (see nodes.pool_name) that run suites the same way, or - with rows in pool_members - is a virtual pool spanning others. Slot sizes belong to nodes, not pools.';
COMMENT ON COLUMN pools.constraints IS 'meta.dtp.* constraints added to every job placed here, as {"key": "value"}.';

CREATE TABLE pool_members (
  pool_name   TEXT NOT NULL REFERENCES pools(name) ON DELETE CASCADE,
  member_name TEXT NOT NULL REFERENCES pools(name) ON DELETE CASCADE,
  position    INT  NOT NULL DEFAULT 0,
  PRIMARY KEY (pool_name, member_name),
  CHECK (pool_name <> member_name)
);
COMMENT ON TABLE pool_members IS 'Members of a virtual pool. Jobs placed in the virtual pool may land on any member''s nodes; members must share runtime and driver.';

CREATE TABLE nodes (
  name       TEXT PRIMARY KEY CHECK (name <> ''),
  pool_name  TEXT REFERENCES pools(name) ON DELETE SET NULL,   -- NULL = unassigned: the node gets no work
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TRIGGER nodes_updated_at BEFORE UPDATE ON nodes FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE INDEX nodes_pool_idx ON nodes (pool_name);
COMMENT ON TABLE nodes IS 'Which pool each worker serves. A node''s labels, slot count and slot size come from the node itself (dtp-node publishes node.yaml as meta.dtp.*); nodes register on their first heartbeat, assigned to the pool node.yaml suggests when it exists.';

CREATE TABLE groups (
  name        TEXT PRIMARY KEY CHECK (name ~ '^[a-z0-9][a-z0-9._-]*$'),
  description TEXT NOT NULL DEFAULT '',
  position    INT NOT NULL DEFAULT 0,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TRIGGER groups_updated_at BEFORE UPDATE ON groups FOR EACH ROW EXECUTE FUNCTION set_updated_at();
COMMENT ON TABLE groups IS 'Named sets of users (see group_members). A group carries nothing itself; quota rules - and whatever else addresses "these people" later - refer to it.';

CREATE TABLE users (
  name         TEXT PRIMARY KEY CHECK (name <> ''),
  display_name TEXT NOT NULL DEFAULT '',
  email        TEXT NOT NULL DEFAULT '',
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TRIGGER users_updated_at BEFORE UPDATE ON users FOR EACH ROW EXECUTE FUNCTION set_updated_at();
COMMENT ON TABLE users IS 'Identities slots are charged to. There is no authentication: a submission''s user field is trusted, and a user not listed here is simply in no group.';

CREATE TABLE group_members (
  group_name TEXT NOT NULL REFERENCES groups(name) ON DELETE CASCADE,
  user_name  TEXT NOT NULL REFERENCES users(name) ON DELETE CASCADE,
  PRIMARY KEY (group_name, user_name)
);
CREATE INDEX group_members_user_idx ON group_members (user_name);
COMMENT ON TABLE group_members IS 'Who is in which group; a user may be in several.';

-- One row per rule: subject × scope -> max_slots. The subject is a user, a
-- group or everyone; for a group or everyone the limit is on each user
-- separately unless together is set. The scope is a node, a pool or
-- everywhere. 0 forbids; no row means no limit. Within one scope the
-- per-user rule that governs a user is the most specific one (own > group,
-- most permissive of their groups > global); every "together" rule that
-- covers them applies on top; rules of different scopes are all enforced.
CREATE TABLE quota_rules (
  subject_kind quota_subject NOT NULL,
  user_name    TEXT REFERENCES users(name)  ON DELETE CASCADE,   -- when subject_kind = 'user'
  group_name   TEXT REFERENCES groups(name) ON DELETE CASCADE,   -- when subject_kind = 'group'
  together     BOOLEAN NOT NULL DEFAULT false,                   -- the limit is on all of them added up, not on each user
  scope_kind   quota_scope NOT NULL,
  pool_name    TEXT REFERENCES pools(name) ON DELETE CASCADE,    -- when scope_kind = 'pool'
  node_name    TEXT REFERENCES nodes(name) ON DELETE CASCADE,    -- when scope_kind = 'node'
  max_slots    INT NOT NULL CHECK (max_slots >= 0),
  note         TEXT NOT NULL DEFAULT '',
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT quota_rules_user_iff_user_subject   CHECK ((subject_kind = 'user')  = (user_name  IS NOT NULL)),
  CONSTRAINT quota_rules_group_iff_group_subject CHECK ((subject_kind = 'group') = (group_name IS NOT NULL)),
  CONSTRAINT quota_rules_together_not_for_users  CHECK (subject_kind <> 'user' OR NOT together),
  CONSTRAINT quota_rules_pool_iff_pool_scope     CHECK ((scope_kind = 'pool') = (pool_name IS NOT NULL)),
  CONSTRAINT quota_rules_node_iff_node_scope     CHECK ((scope_kind = 'node') = (node_name IS NOT NULL)),
  CONSTRAINT quota_rules_one_per_subject_scope   UNIQUE NULLS NOT DISTINCT (subject_kind, user_name, group_name, together, scope_kind, pool_name, node_name)
);
CREATE TRIGGER quota_rules_updated_at BEFORE UPDATE ON quota_rules FOR EACH ROW EXECUTE FUNCTION set_updated_at();
COMMENT ON TABLE quota_rules IS 'Concurrent-slot limits: (user | group | everyone) x (node | pool | everywhere) -> max_slots. See the comment above the table for how rules combine.';
COMMENT ON COLUMN quota_rules.together IS 'For a group or global subject: true limits the members'' slots added together, false limits each member separately.';
COMMENT ON COLUMN quota_rules.max_slots IS 'The limit; 0 forbids the subject any slot in the scope (a submission there is refused).';

-- ---------------------------------------------------------------------------
-- Regressions and their suite attempts. The columns are what people query;
-- doc is the full JSON document the master works with, so the schema never
-- lags the model.
-- ---------------------------------------------------------------------------

CREATE TABLE regressions (
  id           TEXT PRIMARY KEY,
  name         TEXT NOT NULL DEFAULT '',
  user_name    TEXT NOT NULL DEFAULT '',            -- who is charged; not a FK: unlisted users are allowed (they are in no group)
  groups       TEXT[] NOT NULL DEFAULT '{}',        -- the user's groups at submission
  priority     INT NOT NULL DEFAULT 0 CHECK (priority BETWEEN 0 AND 100),
  state        regression_state NOT NULL,
  submitted_at TIMESTAMPTZ NOT NULL,
  started_at   TIMESTAMPTZ,
  finished_at  TIMESTAMPTZ CHECK (finished_at IS NULL OR started_at IS NULL OR finished_at >= started_at),
  doc          JSONB NOT NULL
);
CREATE INDEX regressions_submitted_idx ON regressions (submitted_at DESC);
CREATE INDEX regressions_user_idx ON regressions (user_name, submitted_at DESC);
COMMENT ON TABLE regressions IS 'One submission: a set of suites run against a build. doc holds the submission, the resolved builds and the totals.';

CREATE TABLE runs (
  id            TEXT PRIMARY KEY,
  regression_id TEXT NOT NULL REFERENCES regressions(id) ON DELETE CASCADE,
  suite         TEXT NOT NULL,
  attempt       INT NOT NULL CHECK (attempt >= 1),
  pool          TEXT NOT NULL,                     -- not a FK: a pool may be renamed or dropped after the fact
  state         run_state NOT NULL,
  user_name     TEXT NOT NULL DEFAULT '',
  groups        TEXT[] NOT NULL DEFAULT '{}',
  priority      INT NOT NULL DEFAULT 0 CHECK (priority BETWEEN 0 AND 100),
  node_name     TEXT NOT NULL DEFAULT '',
  queued_at     TIMESTAMPTZ NOT NULL,
  dispatched_at TIMESTAMPTZ,
  started_at    TIMESTAMPTZ,
  finished_at   TIMESTAMPTZ,
  token         TEXT NOT NULL DEFAULT '',          -- the runner''s callback bearer token
  doc           JSONB NOT NULL,
  UNIQUE (regression_id, suite, attempt)
);
CREATE INDEX runs_queue_idx ON runs (state, pool, priority DESC, queued_at);
CREATE INDEX runs_user_idx ON runs (user_name, state);
COMMENT ON TABLE runs IS 'One attempt of one suite. A retry is a new row with attempt + 1. The queue is the rows in state queued.';

CREATE VIEW run_queue AS
  SELECT id, regression_id, suite, pool, user_name, groups, priority, queued_at
  FROM runs
  WHERE state = 'queued'
  ORDER BY priority DESC, queued_at;
COMMENT ON VIEW run_queue IS 'What the master will admit next, in priority order (fair share between users is applied at admission time).';
