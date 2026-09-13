-- The demo lab's catalog: pools, node assignments, groups, users and their
-- memberships, and the quota rules, as rows. PostgreSQL runs this once when the data volume is
-- first created (docker-entrypoint-initdb.d, after the schema in
-- schema.sql); from then on the tables are the only place the catalog lives.
-- Edit them with SQL (then `dtp config reload`), the dashboard's ⚙ Config
-- editor, or `dtp config apply`. Every column is described by a COMMENT in
-- the schema (\d+ pools in psql).
--
-- What a pool does NOT hold: slot sizes. Cores, memory and disk per suite
-- are declared by each node in its node.yaml (slot: {...}) and published as
-- meta.dtp.slot.*; a pool is only "which nodes, run how".

INSERT INTO pools (name, description, runtime, task_driver,
                   image, command, runner_command, docker_network, cache_dir, position)
VALUES
  ('high-perf-pool', 'Fast machines: release regressions, long UI suites',
   'container', 'docker',
   'dtp/rcp-runner:dev', '{/opt/dtp/run-suite.sh}', '{}', 'dtp_default', '/tmp/dtp-nomad/cache', 0),
  ('mid-pool', 'Standard machines: day-to-day regressions',
   'container', 'docker',
   'dtp/rcp-runner:dev', '{/opt/dtp/run-suite.sh}', '{}', 'dtp_default', '/tmp/dtp-nomad/cache', 1),
  ('dev-pool', 'Provisioned developer boxes: task directory + process cgroup',
   'process', 'raw_exec',
   '', '{/opt/dtp/run-suite.sh}', '{/usr/local/bin/dtp-runner}', '', '/var/lib/dtp/cache', 2),
  -- A virtual pool: a name and its members (below); runtime, driver and the
  -- rest are inherited from the first member.
  ('global', 'Anywhere with capacity: spans the container pools',
   NULL, NULL, '', '{}', '{}', '', '', 3);

INSERT INTO pool_members (pool_name, member_name, position) VALUES
  ('global', 'high-perf-pool', 0),
  ('global', 'mid-pool',       1);

-- Which pool each worker serves. A node the master has never heard of is
-- added here on its first heartbeat (in the pool its node.yaml suggests, if
-- that pool exists); after that this table is the only thing that decides.
-- Set pool_name to NULL to park a node: it finishes what it runs, gets
-- nothing new.
INSERT INTO nodes (name, pool_name) VALUES
  ('rcp-hp-1',  'high-perf-pool'),
  ('rcp-mid-1', 'mid-pool'),
  ('rcp-dev-1', 'dev-pool'),
  ('rcp-dev-2', 'dev-pool');

-- Groups are just sets of users; the quota rules below (and whatever else
-- addresses "these people" later) refer to them.
INSERT INTO groups (name, description, position) VALUES
  ('core-devs', 'The core team', 0),
  ('release',   'Release engineering and CI', 1);

INSERT INTO users (name, display_name, email) VALUES
  ('alice',   'Alice Lindqvist', 'alice@example.internal'),
  ('bob',     'Bob Okafor',      'bob@example.internal'),
  ('carol',   'Carol Meyer',     'carol@example.internal'),
  ('jenkins', 'CI',              '');

-- A user may be in several groups.
INSERT INTO group_members (group_name, user_name) VALUES
  ('core-devs', 'alice'),
  ('core-devs', 'bob'),
  ('core-devs', 'carol'),
  ('release',   'bob'),
  ('release',   'jenkins');

-- The quota rules: (user | group | everyone) x (node | pool | everywhere)
-- -> max concurrent slots. Per scope, the per-user rule that governs a user
-- is the most specific one (own > their groups', most permissive > the
-- everyone rule); "together" rules add up everyone they cover and apply on
-- top; rules of different scopes are all enforced. 0 forbids.
INSERT INTO quota_rules (subject_kind, user_name, group_name, together, scope_kind, pool_name, node_name, max_slots, note) VALUES
  -- everyone: 6 slots each, anywhere
  ('global', NULL,      NULL,        false, 'global', NULL,             NULL,       6, 'each user, anywhere'),
  -- core-devs: 8 each, 10 together
  ('group',  NULL,      'core-devs', false, 'global', NULL,             NULL,       8, 'each core dev'),
  ('group',  NULL,      'core-devs', true,  'global', NULL,             NULL,      10, 'the core team added up'),
  -- release: 8 each anywhere, but at most 4 each on the fast machines
  ('group',  NULL,      'release',   false, 'global', NULL,             NULL,       8, ''),
  ('group',  NULL,      'release',   false, 'pool',   'high-perf-pool', NULL,       4, 'leave the fast machines to the devs'),
  -- carol: 2, whatever core-devs allows; jenkins: 12, and never the dev boxes
  ('user',   'carol',   NULL,        false, 'global', NULL,             NULL,       2, 'part-time'),
  ('user',   'jenkins', NULL,        false, 'global', NULL,             NULL,      12, 'CI runs a lot'),
  ('user',   'jenkins', NULL,        false, 'pool',   'dev-pool',       NULL,       0, 'CI stays off the developer boxes'),
  -- rcp-hp-1: 5 of its 6 slots for everyone added up, one stays free for manual use
  ('global', NULL,      NULL,        true,  'node',   NULL,             'rcp-hp-1', 5, 'one slot of rcp-hp-1 stays free for manual runs');
