# Distributed Test Platform (PoC)

A master that runs **Eclipse RCP test suites** on pools of worker nodes
through **HashiCorp Nomad**, collects every result and artifact under
`results/<regression_id>/`, and shows it all on a dashboard.

```mermaid
%%{init: {"flowchart": {"rankSpacing": 35, "nodeSpacing": 25}}}%%
flowchart TB
  SUB["submission: user, priority, suites"] --> M
  M["dtp-master<br/>queue · slot ledger · quota rules · node choice<br/>retries · JUnit rollup · API · dashboard"]
  M -->|"one Nomad job per suite attempt,<br/>pinned to the chosen node"| NS["Nomad"]
  NS --> HP["high-perf-pool<br/>rcp-hp-1 · 6 slots"]
  NS --> MID["mid-pool<br/>rcp-mid-1 · 4 slots"]
  NS --> DEV["dev-pool<br/>rcp-dev-1, rcp-dev-2 · 3 slots"]
  HP & MID & DEV <--> S3[("MinIO / S3<br/>builds/ in, results/ out")]
```

The virtual pool `global` spans `high-perf-pool` and `mid-pool`: "anywhere
with capacity".

**Contents:**
[What it does](#what-it-does) ·
[Quick start](#quick-start) ·
[The submission document](#the-submission-document) ·
[Pools, nodes and quotas](#pools-nodes-and-quotas) ·
[CLI](#cli) ·
[The dashboard](#the-dashboard) ·
[Where the state lives](#where-the-state-lives) ·
[The node side](#the-node-side-dtp-node--nodeyaml) ·
[Builds and the node cache](#the-build-repository-and-the-node-build-cache) ·
[Result layout](#result-layout) ·
[Real suites vs the simulated fixture](#real-suites-vs-the-simulated-fixture) ·
[Running EGit for real](#running-egit-for-real) ·
[Layout](#layout)

How it works inside (components, state machines, scheduling, what Nomad does
here, the catalog, execution, results, failure handling, the API, how to
adapt it to another product, and the design rationale) is in
**[ARCHITECTURE.md](ARCHITECTURE.md)**.

## What it does

| Topic | Decision |
|---|---|
| Unit of work | **One suite is one slot on one node.** No sharding below the suite. |
| Node capacity | Each node declares in its `node.yaml` how many slots it runs and what one slot is: `slots: 6`, `slot: {cores: 2, memory_mb: 4096, memory_max_mb: 8192}`. The demo lab uses 2 dedicated cores and 4 GB per slot on every node type. |
| Execution | `process` (a task directory and a process cgroup, Nomad `exec` / `raw_exec`) or `container` (Nomad `docker`). The same `dtp-runner` binary runs in both. |
| Placement | A submission names a **pool** and optional `requires` labels. The **master picks the node**: the least loaded node in the pool with a free slot and matching labels. The Nomad job is pinned to that node; Nomad enforces the slot's cores and memory. |
| Pools | `high-perf-pool`, `mid-pool` and `dev-pool` are rows in the master's catalog. A node belongs to a pool through a row in the `nodes` table (dashboard, `dtp nodes assign`, or SQL). Nomad knows nothing about pools. `global` is a virtual pool that spans the two container pools. |
| Quotas | A **rule table**: (user, group or everyone) × (node, pool or everywhere) → max concurrent slots. A rule limits each user it covers separately, or all of them together. The most specific rule wins per scope. 0 forbids. Groups are plain sets of users. |
| Fairness | Higher priority always goes first. Within one priority, the next slot goes to the user holding the fewest slots, so a 50-suite submission shares with a 5-suite one. |
| Progress | Long suites report interim JUnit counts every 15 seconds, so the dashboard moves during a 40-minute UI run. |
| Code under test | Fetched per run from a URL or S3, verified by sha256, unpacked into a cache on the node keyed by that sha256, and reused across suites. |
| Results | The runner uploads; the **master parses the JUnit XML** into per-suite and per-regression totals. |
| State | **PostgreSQL** (`regressions` and `runs` tables; the queue is `WHERE state = 'queued'`), or JSON files for the dependency-free demo. |
| Submitting | `dtp submit file.json`, `POST /api/v1/regressions`, or the dashboard's **New regression** form. |
| Artifacts | Uploaded to S3 / MinIO under `results/<regression_id>/<suite>/attempt-N/`. |
| Retries | `retries` per suite. A suite that passes only on a retry is reported as **flaky**. |

## Quick start

There are two ways to run it. The code is the same. Only the backend that
runs the work differs.

### 1. Local backend, no cluster

The master starts `dtp-runner` as child processes against simulated nodes.
Scheduling, slots, labels, retries, JUnit parsing, S3 upload and the
dashboard all use the real code. Needs Go and Docker (for MinIO).

```bash
make demo       # starts MinIO and the master, and applies deploy/local.catalog.json (the demo lab)
./bin/dtp submit examples/regression.json -w
make smoke      # 37 end-to-end checks: placement, retries, quota rules, nodes, totals, artifacts
```

### 2. Full Nomad stack

One Nomad server, four worker nodes in three pools (13 slots, plus the
virtual `global` pool), MinIO, PostgreSQL and the master, all in Docker
Compose.

```bash
make stack                      # docker compose up -d --build
make build                      # the CLI talks to the master over HTTP
./bin/dtp pools                 # or: DTP_MASTER=http://host:8080 ./bin/dtp pools
./bin/dtp submit examples/regression.json -w
```

| | |
|---|---|
| Dashboard | http://localhost:8080 |
| Nomad UI | http://localhost:4646 |
| MinIO console | http://localhost:9001 (`dtpadmin` / `dtpadmin123`) |
| PostgreSQL | `psql postgres://dtp:dtp-s3cret@localhost:5433/dtp` |

## The submission document

```jsonc
{
  "name": "egit-nightly",
  "user": "alice",                       // charged for the slots; default: dtp submit -user, or $USER
  "priority": 50,                        // higher is admitted first
  "labels": { "branch": "master" },

  "defaults": {                          // merged into every suite
    "pool": "global",                    // any node the virtual pool spans
    "timeout": "10m",
    "retries": 0,
    "requires": { "os": "linux", "display": "xvfb" },
    "build": [
      { "name": "egit", "id": "latest" }       // the newest egit payload in the build repository;
    ]                                          // or "id": "egit-b522e135e4", or "url" + "sha256"
  },

  "suites": [
    { "name": "org.eclipse.egit.core.test" },

    { "name": "org.eclipse.egit.ui.test",
      "pool": "high-perf-pool",
      "requires": { "perf": "high" },          // only nodes labelled perf=high
      "timeout": "15m",
      "retries": 2 },

    { "name": "org.eclipse.egit.gitflow.test",
      "pool": "dev-pool" }
  ]
}
```

The build version belongs to the payload, not to a node. It comes from the
build repository's manifest, is recorded on the regression, and reaches the
suite as `DTP_BUILD_EGIT_VERSION`. Any node can run any version.

`requires` keys match node labels. `dtp-node` publishes the labels it detects
plus the ones in the node's `node.yaml`:

```yaml
# node.yaml
slots: 4                 # this node runs 4 suites at once
slot: {cores: 2, memory_mb: 4096, memory_max_mb: 8192, disk_mb: 2048}   # what each suite gets
labels:
  perf: standard         # anything you want to select on: perf, gpu, site, ...
```

## Pools, nodes and quotas

Pools, node assignments, groups, users and quota rules together are the
**catalog**. The catalog lives in the store and nowhere else: the `pools`,
`nodes`, `groups`, `users`, `group_members` and `quota_rules` tables (plus
`pool_members`) in PostgreSQL, or `catalog.json` in the state directory when
the master uses the file store. The master's configuration file describes
the process only: listen address, Nomad, object store, store DSN.

How the catalog gets there:

* **When the database is first created**,
  [`deploy/sql/010_seed_catalog.sql`](deploy/sql/010_seed_catalog.sql)
  inserts the demo lab: four pools, four node assignments, two groups, four
  users and nine quota rules. The compose stack mounts it into Postgres'
  `docker-entrypoint-initdb.d`, after the schema. For your own lab, write a
  seed the same way, or start empty.
* **Any time after that**, use the dashboard's **Config** editor,
  `dtp config apply catalog.json`, `dtp nodes assign <node> <pool>`,
  `dtp quotas set <rule> <n>`, the config API (`PUT /api/v1/config`,
  `…/pools/{name}`, `…/nodes/{name}`, `…/groups/{name}`, `…/users/{name}`,
  `…/quotas/{rule-key}`), or SQL followed by `dtp config reload`
  (`POST /api/v1/config/reload`). The API writes the store and applies the
  change in one step. The master does not poll the tables. A change made
  with SQL is picked up when you reload.

Every write is **idempotent**: saving what is already there changes nothing
(no row is touched), deleting what is not there is a no-op, and assigning a
node to the pool it is already in returns the same catalog. Every change is
validated as a whole: a virtual pool spanning an unknown pool, mixed
runtimes in a virtual pool, a node assigned to an unknown or virtual pool, a
user in an unknown group, a rule naming an unknown user, group, pool or
node. A change that fails is refused with the reason, and the previous
catalog stays in force. (A bad catalog in the store itself is logged and
shown as `store_error` on `GET /api/v1/config`.) The master runs with no
pools at all; submissions are refused with "unknown pool" until some exist.

```sql
UPDATE nodes SET pool_name = 'mid-pool' WHERE name = 'rcp-hp-2';
INSERT INTO group_members (group_name, user_name) VALUES ('core-devs', 'carol');
INSERT INTO quota_rules (subject_kind, user_name, scope_kind, max_slots) VALUES ('user', 'carol', 'global', 2);
-- then: dtp config reload
```

### Pools

A pool says which nodes serve it and how suites run there: runtime
(`process` or `container`), task driver, image, command, docker network,
cache directory, constraints. It says nothing about capacity. There is one
column per field, and the comments in
[`internal/store/sql/schema.sql`](internal/store/sql/schema.sql) describe
each one:

```sql
INSERT INTO pools (name, runtime, task_driver, image, command, docker_network, cache_dir, position)
VALUES ('high-perf-pool', 'container', 'docker',
        'dtp/rcp-runner:dev', '{/opt/dtp/run-suite.sh}', 'dtp_default', '/tmp/dtp-nomad/cache', 0),
       ('global', NULL, NULL, '', '{}', '', '', 3);
INSERT INTO pool_members (pool_name, member_name, position) VALUES ('global', 'high-perf-pool', 0);
INSERT INTO nodes (name, pool_name) VALUES ('rcp-hp-1', 'high-perf-pool');
```

A **virtual pool** (`global`) needs only a name and the pools it spans. It
inherits everything else from its first member. Its nodes are its members'
nodes and its capacity is the sum of theirs. All members must use the same
runtime and driver, because a job runs under one driver.

### Nodes and slots

Capacity belongs to the node. Each worker's `node.yaml` says how many suites
it runs at once and what each of them gets:

```yaml
slots: 6
slot:
  cores: 2            # dedicated cores, a cpuset (or cpu_mhz: 2000 for shares)
  memory_mb: 4096     # reserved; also the hard limit unless memory_max_mb is set
  memory_max_mb: 8192 # burst limit (Nomad memory oversubscription)
  disk_mb: 2048
```

`dtp-node` publishes these as `meta.dtp.slots` and `meta.dtp.slot.*`. The
master reads them from the node, sizes every job it places there from them,
and shows them per node (`dtp nodes`, the Pools table). A 2-core slot is 2
cores on a slow machine and on a fast one alike. Under the docker driver the
memory reservation is also the hard limit; an EGit UI run (Maven, the
Eclipse test JVM, WebKit helpers and Xvfb) was OOM-killed at 3 GB.
`memory_max_mb` lets a task use more memory than it reserved, up to that
limit, so the JVMs have headroom without every slot reserving the peak. An
OOM kill is reported as such on the run. Nodes in one pool may declare
different slot sizes. Each job is sized by the node it runs on.

A node is in a pool because the catalog says so. The `nodes` table maps a
node name to a pool. The `pool:` entry in `node.yaml` is only a suggestion
for the first registration: an agent the master has never seen is added to
that pool if it exists, otherwise it is added unassigned. After that, the
catalog decides. Change it in **Config → Nodes**, with
`dtp nodes assign rcp-hp-2 mid-pool`, or with SQL. The next scheduler tick
places work by the new assignment. Whatever the node was running keeps
running. A node in no pool (including a pool that was deleted) gets no new
work and is listed as **unassigned** on the dashboard and in `dtp nodes`.
Nomad keeps every client in its default node pool, and each job is pinned
to the node the master chose, so reassigning a node never needs a client
restart.

### Groups, users and quota rules

**Groups** are named sets of users. A user may be in any number of them. A
group carries no limits itself. Quota rules refer to groups, and other
features can refer to them later. **Users** are the identities slots are
charged to. Someone who submits without being listed is simply a user in no
group.

**Quota rules** are the limits, one row each:

```
(user NAME | group NAME | everyone)  ×  (node NAME | pool NAME | everywhere)  →  max concurrent slots
```

A group rule or an everyone rule limits **each user separately**, unless it
says `together`, in which case it limits **all of them added up**. The rules
combine like this:

* Rules for different scopes are all enforced. `carol@global 4` and
  `carol@pool:high-perf-pool 2` mean at most 2 on the fast machines and at
  most 4 anywhere.
* Within one scope, the per-user rule that applies to a user is the most
  specific one: the user's own rule, else the most permissive of their
  groups' rules, else the everyone rule. So `global@global 6` is the default
  for everybody, `group:core-devs@global 8` raises it for the core team, and
  `user:carol@global 2` lowers it for carol regardless of her groups.
* Every `together` rule that covers the user applies as well.
  `group:core-devs/together@global 10` caps the team's slots added up.
  `global/together@node:rcp-hp-1 5` keeps one of that node's six slots free.
* A pool rule sees work that lands in the pool by any route. A run submitted
  to the virtual `global` pool that lands on a `high-perf-pool` node counts
  in both, and so does a run submitted directly to `high-perf-pool`. A node
  rule steers the master's node choice before it makes a run wait.
* `0` forbids. A submission to a pool where the user's per-user rule is 0 is
  refused, naming the rule. A `together` rule at 0 drains its scope. No rule
  means no limit.

A rule is written as `subject[:name][/together]@scope[:target]` in the CLI
and the API: `global@global`, `user:carol@global`,
`group:release@pool:high-perf-pool`, `global/together@node:rcp-hp-1`. In the
dashboard, rules are rows in the Config editor, and the **Quota rules** table
shows every rule with its current usage: for a `together` rule the slots in
use against the limit, for a per-user rule each user it covers with their
slots in that scope and the cap that applies to them (which may come from a
more specific rule, shown next to it).

A user at a cap keeps their remaining suites queued, with the rule named on
the run, while other users keep running. There is no authentication: the
`user` field is trusted (this is an internal system), and `dtp submit` fills
it from `-user`, `$DTP_USER` or `$USER`.

```sql
INSERT INTO users (name, display_name) VALUES ('erin', 'Erin Salas');
INSERT INTO group_members (group_name, user_name) VALUES ('release', 'erin'), ('core-devs', 'erin');
INSERT INTO quota_rules (subject_kind, group_name, together, scope_kind, pool_name, max_slots, note)
  VALUES ('group', 'release', true, 'pool', 'mid-pool', 3, 'release shares the mid machines');
```

How the numbers are enforced (the ledger, node choice, what Nomad checks) is
in [ARCHITECTURE.md, sections 4 and 7](ARCHITECTURE.md#4-scheduling).

## CLI

```
dtp submit <file.json> [-w] [-id ID] [-priority N] [-user NAME]
                                                     # -w follows the run and exits non-zero on failure
                                                     # flags go after the subcommand;
                                                     # -master URL or $DTP_MASTER selects the master
dtp status <regression-id> [-w]
dtp list
dtp pools                                            # the slot ledger, live
dtp nodes                                            # every node: pool, status, slots, slot size, agent
dtp nodes assign <node> [<pool>]                     # move a node (no pool: park it, unassigned)
dtp quotas                                           # the rule table with live usage
dtp quotas set <rule> <max-slots> [note]             # add or change a rule (0 forbids); idempotent
dtp quotas rm <rule>
dtp builds                                           # the build repository: ids, products, versions, suites
dtp cancel <regression-id> [-suite NAME]             # the whole regression, or one suite of it
dtp artifacts <run-id> [-get PATH]
dtp logs <run-id>                                    # same as: artifacts -get dtp-runner.log
dtp discover <reactor-dir> [-pool P] [-build NAME]   # Tycho reactor -> submission JSON
dtp config                                           # the catalog as stored
dtp config apply <catalog.json>                      # replace it ({"pools", "nodes", "groups", "users", "quotas"})
dtp config reload                                    # re-read the store after editing it with SQL
```

```
$ dtp pools
high-perf-pool  [container/docker]  1/6 slots used
  rcp-hp-1               ready    1/6  2 cores/4096MB  agent=0.4 arch=x86_64 cores=12 display=xvfb java=21 memory_mb=24576 os=linux perf=high
      ▸ org.eclipse.egit.ui.test

mid-pool  [container/docker]  2/4 slots used
  rcp-mid-1              ready    2/4  2 cores/4096MB  agent=0.4 arch=x86_64 cores=8 display=xvfb java=21 memory_mb=16384 os=linux perf=standard
      ▸ org.eclipse.egit.core.test
      ▸ org.eclipse.egit.gitflow.test

dev-pool  [process/raw_exec]  0/3 slots used
  rcp-dev-1              ready    0/2  2 cores/4096MB  agent=0.4 arch=x86_64 cores=4 display=xvfb java=21 memory_mb=8192 os=linux perf=standard
  rcp-dev-2              ready    0/1  2 cores/4096MB  agent=0.4 arch=x86_64 cores=2 display=xvfb java=21 memory_mb=4096 os=linux perf=standard

global  [container/docker]  3/10 slots used
  spans high-perf-pool, mid-pool

$ dtp nodes
NODE                   POOL             STATUS   SLOTS   SLOT                   AGENT
rcp-hp-1               high-perf-pool   ready    1/6     2 cores/4096MB         0.4
rcp-mid-1              mid-pool         ready    2/4     2 cores/4096MB         0.4
rcp-dev-1              dev-pool         ready    0/2     2 cores/4096MB         0.4
rcp-dev-2              (unassigned)     ready    0/1     2 cores/4096MB         0.4

$ dtp nodes assign rcp-dev-2 dev-pool
rcp-dev-2 -> dev-pool

$ dtp quotas
RULE                                           LIMIT  IN USE           KEY
each user anywhere                                 6  3                global@global
    alice                                          8  0/8             governed by group:core-devs@global
    bob                                            8  0/8             governed by group:core-devs@global
    carol                                          2  0/2             governed by user:carol@global
    jenkins                                       12  3/12            governed by user:jenkins@global
each member of core-devs anywhere                  8  0                group:core-devs@global
    alice                                          8  0/8
    bob                                            8  0/8
    carol                                          2  0/2             governed by user:carol@global
core-devs together anywhere                       10  0/10             group:core-devs/together@global
each member of release in pool high-perf-pool      4  3                group:release@pool:high-perf-pool
    bob                                            4  0/4
    jenkins                                        4  3/4
jenkins in pool dev-pool                           0  0/0              user:jenkins@pool:dev-pool
    · CI stays off the developer boxes
everyone together on node rcp-hp-1                 5  3/5              global/together@node:rcp-hp-1
    · one slot of rcp-hp-1 stays free for manual runs

$ dtp quotas set group:release/together@pool:mid-pool 3 "release shares mid"
group:release/together@pool:mid-pool = 3 slots
```

```
$ dtp status reg-20260912-103645-simulated-nightly-b6d648
reg-20260912-103645-simulated-nightly-b6d648  errored  simulated-nightly
suites 9/9 done · 0 running · 0 queued   tests 164 pass / 2 fail / 0 skip   1 flaky
s3://dtp/results/reg-20260912-103645-simulated-nightly-b6d648/

  SUITE                                  STATE     TRY    TESTS        TIME    NODE
  simulated.core.test                    passed    1/1    30/30        20.3s   rcp-mid-1
  simulated.flaky.ui.test                passed    2/3*   14/14        12.1s   rcp-dev-1
  simulated.crash.test                   errored   2/2    —            0.1s    rcp-hp-1
      suite exited 42 and produced no JUnit results
  simulated.ui.test                      failed    1/1    24/26 (2✗)   30.3s   rcp-hp-1
      2 of 26 tests failed
```

### States you will see

Each suite attempt is a **run**. `dtp status` and the dashboard show its
state:

```mermaid
stateDiagram-v2
  direction LR
  [*] --> queued
  queued --> dispatched: slot free
  dispatched --> running: started
  running --> passed
  running --> failed
  running --> errored
  running --> timeout
  queued --> canceled
  dispatched --> canceled
  running --> canceled
```

`failed`, `errored` and `timeout` are retried as a new run when the suite has
`retries` left. A suite that passes only on a retry is marked **flaky** (the
`2/3*` in the `TRY` column above). The regression is `errored` if any suite
ended in `errored`, `timeout` or `canceled`, else `failed` if any suite
failed, else `passed`. A queued run's message says why it waits: a full
pool, or the quota rule that is at its limit. The full state machines are in
[ARCHITECTURE.md, section 3](ARCHITECTURE.md#3-state-machines).

## The dashboard

**New regression** opens a form fed by `GET /api/v1/catalog`: the templates
(the submission files in `templates_dir`, which is the repo's `examples/` in
both demo setups), the pools, every `meta.dtp.*` value present on the nodes
as **Targets** dropdowns, suites and users seen before, and the build
payloads in the `builds` bucket. Pick a template, tick the suites to run,
change pool, targets, timeout or retries per suite, add a suite by name, and
submit. It posts the same JSON `dtp submit` would.

A running regression has a **cancel** button on its row, and each running
suite has a **✕** in the expanded detail (`dtp cancel <id> [-suite NAME]`
does the same).

**Config** opens the catalog editor:

* **Pools**: runtime, driver, spans, image, command, description, or the
  whole pool as JSON for the rarer fields.
* **Nodes**: every node the backend sees or the catalog knows, with a pool
  selector, its live status, slots and declared slot size. **＋ node**
  registers a node by name before its agent starts.
* **Groups**: name and description; members are shown.
* **Users**: display name, email, groups.
* **Quota rules**: who × where → max slots, each user or all together, with
  a note. Editing a rule's subject or scope replaces that rule.

Each row is saved on its own through the config API. Every save is validated
as a whole catalog. A bad one is refused, with the reason in the row, and
nothing changes. Saving an unchanged row changes nothing. **↻ reload from
store** re-reads the tables after an edit made with SQL.

The page updates in place. Only cells whose content changed are touched, so
open details stay where they are while results stream in.

## Where the state lives

The master keeps regressions and runs in memory and writes every change to
its store. With `"store": {"driver": "postgres", "dsn": "postgres://…"}` (the
compose stack) the store is two tables, `regressions` and `runs`. Their
columns are what you query; their `doc` column holds the full JSON document,
so the schema never lags the model. The queue is a query:

```sql
SELECT suite, user_name, pool, priority, queued_at FROM run_queue;
```

The catalog tables sit next to them: `pools`, `pool_members`, `nodes`,
`groups`, `users`, `group_members` and `quota_rules`. Every field is a
column. The master reads them at start and on `POST /api/v1/config/reload`.
The config API writes them with upserts and deletes only what the new
catalog no longer has.

The schema is one file,
[`internal/store/sql/schema.sql`](internal/store/sql/schema.sql), embedded
in the binary. Postgres runs it at first init in the compose stack, and the
master runs it itself when it connects to a database with no tables. States
are enums (`run_state`, `regression_state`, `pool_runtime`, `task_driver`,
`quota_subject`, `quota_scope`). The invariants the code relies on are CHECK
constraints. Relations are foreign keys with `ON DELETE CASCADE`. Every
table has `created_at` and `updated_at`, kept by a trigger. Every table and
non-obvious column has a `COMMENT` (`\d+ runs` in psql). The queue is a view
(`run_queue`). There are no migrations: to change the schema, edit the file
and recreate the database (`make stack-stop` drops the volume).

History stays as long as you keep it. A restart reloads everything and
re-queues whatever was in flight. The client is `internal/pg`, about 400
lines of the PostgreSQL wire protocol on the standard library (SCRAM-SHA-256,
MD5, TLS, extended queries), so the build has no third-party modules.
Without a `store` section the master writes one JSON file per regression
under `state_dir`; that is what the local demo uses.

## The node side: `dtp-node` + `node.yaml`

Two programs run on a worker. **`dtp-runner`** is not a daemon: Nomad starts
one per suite attempt, and it learns everything it needs (master URL, object
store, token, cache directory) from the RunSpec in the task's environment.
**`dtp-node`** is the daemon, the node agent. It is what makes a machine a
worker: one static binary plus a `node.yaml` next to it.

```yaml
master: http://master:8080         # the master; the agent registers here
nomad: http://127.0.0.1:4646       # the local Nomad client's API
name: rcp-hp-1                     # default: hostname
slots: 6                           # required: how many suites this node runs at once
slot:                              # required: what each of them gets
  cores: 2
  memory_mb: 4096
  memory_max_mb: 8192              # optional burst limit
  disk_mb: 2048
pool: high-perf-pool               # optional: the pool to join when the master first sees this node
labels:                            # meta.dtp.*; override the detected ones
  perf: high
cache_dir: /var/lib/dtp/cache
cache_keep: 168h                   # delete payloads unused for a week
runner_dir: /usr/local/bin         # keep dtp-runner here, fetched from the master when its sha256 changes
interval: 60s
```

What it does, once (`dtp-node apply`) or continuously (`dtp-node run`):

* **Detects the node**: OS, architecture, cores and memory (from what the
  local Nomad client reports, so the numbers match what the scheduler
  places against), display (`Xvfb` present?), Java major version, docker.
  Then it merges `labels:` on top.
* **Declares the slots**: the count and the slot size from `node.yaml`, as
  `meta.dtp.slots` and `meta.dtp.slot.*`. These are decisions, not
  measurements. The agent warns when `slots × slot` exceeds what Nomad
  reports for the node, but never changes the numbers. `node.yaml` is
  re-read every tick, so editing it is enough. No restart is needed.
* **Labels the running Nomad client** through dynamic node metadata
  (`/v1/client/metadata`): no HCL edit, no restart. Labels removed from
  `node.yaml` are removed from the node. The master selects nodes by exactly
  these labels (`requires`), and `dtp pools` shows them.
* **Sends heartbeats to the master** (`POST /api/v1/nodes/{name}/heartbeat`).
  The master registers a node it has not seen (in the pool `node.yaml`
  suggests, if it exists) and answers with the pool the catalog has the node
  in and the runner's checksum. The agent logs a warning while it is in no
  pool. The dashboard shows `agent 0.4 ✓` on the node, or a warning when it
  goes silent.
* **Installs `dtp-runner`** into `runner_dir` from `GET /api/v1/runner`
  whenever the master's copy changes, so process-pool nodes never run a
  stale runner.
* **Prunes the build cache**: entries not used within `cache_keep`.

`dtp-node labels` prints what it would set, where each value came from, and
whether the declared slots fit. `dtp-node nomad-config` renders a Nomad
client HCL for a new machine from the same `node.yaml` (servers, drivers,
and the labels as a static fallback; no `node_pool`, because the master
decides which pool the node serves). On a real node, run it as a service
next to the Nomad client. In the compose demo each node has a sidecar
(`deploy/nodes/*.yaml`), which is why the client HCL files carry only
`dtp.slots`.

## The build repository and the node build cache

Builds are published to the `builds` bucket. Each payload has a **manifest**
next to it (`builds/<id>.json`), written by whatever produced the build:

```json
{
  "id": "egit-b522e135e4", "product": "egit", "version": "7.8.0",
  "ref": "v7.8.0.202609011348-r", "commit": "b522e135…", "built": "2026-09-12T16:58:44Z",
  "url": "s3://builds/egit-b522e135e4.tar.gz", "sha256": "c2484f23…", "size": 845024458,
  "unpack": "tar.gz", "harness": "tycho",
  "suites": ["org.eclipse.egit.core.test", "org.eclipse.egit.gitflow.test", "org.eclipse.egit.ui.test"]
}
```

The manifest is what makes a payload more than a URL. The master reads the
repository (`dtp builds`, `GET /api/v1/builds`, the form's build picker),
and a submission names a build instead of pasting a URL and a checksum:

| `build` entry | Meaning |
|---|---|
| `{"name": "egit", "id": "latest"}` | the newest payload whose `product` is `egit` (or `"product": "…"` to name it) |
| `{"name": "egit", "id": "egit-b522e135e4"}` | that payload, pinned |
| `{"name": "egit", "url": "s3://…", "sha256": "…"}` | as given; gains the manifest's version when the repository knows the URL |

At submission the master fills in the URL, sha256 and unpack method from the
manifest and records the resolved payloads on the regression (`dtp status`
and the dashboard show `egit 7.8.0 · egit-b522e135e4`). The runner exports
`DTP_BUILD_<NAME>_VERSION` and `DTP_BUILD_<NAME>_ID` next to
`DTP_BUILD_<NAME>`. The version travels with the build, never with a node.

Results are stored per run. Builds are shared. To try it, seed a stand-in
product with its manifest and submit a regression that asks for it:

```bash
./scripts/seed-build.sh                        # a 7.7 MB "RCP product" rcp-stub 4.30 -> s3://builds/ + manifest
./bin/dtp submit examples/regression-with-build.json -w      # "id": "latest" of product rcp-stub
```

Each runner reports what it did with the payload:

```
$ for r in $(...run ids...); do ./bin/dtp logs $r | grep cache; done
simulated.jgit.test                  rcp-hp-1        cache miss product -> fetching s3://builds/rcp-4.30-linux.tar.gz
simulated.core.test                  rcp-hp-1        cache hit  product (after wait)
simulated.ui.test                    rcp-hp-1        cache hit  product (after wait)
simulated.gitflow.test               rcp-dev-2       cache miss product -> fetching s3://builds/rcp-4.30-linux.tar.gz
simulated.smartimport.test           rcp-dev-1       cache miss product -> fetching s3://builds/rcp-4.30-linux.tar.gz
```

One download per node, not per suite. The two suites that started at the
same time on `rcp-hp-1` waited for the cache lock instead of downloading
twice. The unpacked path reaches the suite as `DTP_BUILD_PRODUCT`.

`${VAR}` in a submission is expanded from the environment. That is how a CI
job pins the build it just produced (`BUILD_ID` is printed by the build
scripts) instead of taking `latest`.

## Result layout

```
results/<regression_id>/
  manifest.json                       # the regression and every attempt, written on completion
  org.eclipse.egit.ui.test/
    attempt-1/
      TEST-org.eclipse.egit.ui.wizards.share.SharingWizardTest.xml   # one per test class (Tycho)
      TEST-….xml
      eclipse.log                     # the workbench's .metadata/.log
      mvn.log                         # the Maven/Tycho transcript
      dtp-runner.log                  # the runner's own transcript
      screenshots/shareProjectWithExternalRepo(….SharingWizardTest).jpeg   # SWTBot, on failure
    attempt-2/…
```

Artifacts are also served through the master, so the browser needs no
object-store credentials: `GET /api/v1/runs/{run_id}/artifacts/{path}`.

## Real suites vs the simulated fixture

Two kinds of suite appear in this repository. They are named so that they
cannot be confused:

| | Suites | What runs | Artifacts |
|---|---|---|---|
| **Real** | `org.eclipse.egit.core.test`, `org.eclipse.egit.ui.test`, `org.eclipse.egit.gitflow.test` ([`examples/egit.json`](examples/egit.json)) | EGit's own Tycho test modules against the EGit build: 447 + 615 + 42 JUnit and SWTBot tests, with the Eclipse workbench under Xvfb | surefire XML per class, the workspace `.log`, SWTBot screenshots of the workbench at the moment of failure (JPEG, 1920×1080) |
| **Simulated** | `simulated.*` ([`examples/regression.json`](examples/regression.json), the smoke test) | [`examples/fixtures/rcp-suite.sh`](examples/fixtures/rcp-suite.sh): no product, no tests. It generates the shape of a run so the platform can be exercised in seconds without an 800 MB build. | surefire XML, an `eclipse.log`, and a rendered mock dialog per failing case, watermarked **SIMULATED FIXTURE** |

The platform does not distinguish them. A simulated run goes through the
same admission, dispatch, runner, upload, parsing and rollup as a real one.
Only the harness differs (`fixture` instead of `tycho` or `eclipse`).

The fixture is driven by environment variables, set per suite (or in
`defaults`) under `env` in the submission:

| Variable | Default | Effect |
|---|---|---|
| `FIXTURE_TESTS` | 24 | number of test cases in the suite |
| `FIXTURE_FAIL` | 0 | how many of them fail (each gets a screenshot and a stack trace) |
| `FIXTURE_SKIP` | 0 | how many are skipped |
| `FIXTURE_SECONDS` | 20 | wall-clock duration of the run, spread evenly over the cases |
| `FIXTURE_FLAKY` | 0 | `1`: the failures happen only on attempt 1, so a retry passes; exercises flaky detection |
| `FIXTURE_CRASH` | 0 | `1`: exit 42 with no results at all; exercises "errored" (harness failure) versus "failed" (test failure) |

`examples/regression.json` uses these to stage one of everything: a flaky
suite with `retries: 2`, a crashing suite with `retries: 1`, a suite with two
failures, suites pinned to `dev-pool` and to `perf=high`. That is what
`make smoke` checks (37 checks). Nothing simulated is needed for real work.
`make egit` never touches the fixture.

## Running EGit for real

[`scripts/build-egit.sh`](scripts/build-egit.sh) builds EGit from source with
Tycho inside the worker image and publishes the result as a build payload.
[`examples/egit.json`](examples/egit.json) runs its three test modules as
suites:

```bash
make stack
./scripts/build-egit.sh                # ~10 min the first time: clone, mvn install, warm the test runtime, tar, publish + manifest
./bin/dtp submit examples/egit.json -w -user jenkins    # "id": "latest" of product egit
```

The payload contains the checked-out tree, the Maven repository it resolved
against (target platform, Tycho, the surefire OSGi runtime), the JGit p2
repository it was built against, and an `mvn.args` file with the flags the
reactor needs. The manifest next to it records the version (from the tag),
the commit, the checksum and the test modules. The build script proves the
repository is complete by re-running a test offline before packaging.

On a node, the `tycho` harness in [`run-suite.sh`](deploy/images/run-suite.sh)
copies the tree into the attempt's workspace, starts Xvfb and metacity, and
runs

```
mvn -o verify -pl org.eclipse.egit.ui.test -Dmaven.repo.local=<cache>/m2 -Ddash.skip=true …
```

Nothing is downloaded at test time, and concurrent slots share one Maven
repository read-only. Surefire XML, the workspace `.log`, the Maven
transcript and SWTBot's screenshots (every `screenshots/` directory under
the module, in any image format; SWTBot defaults to JPEG) come back as
artifacts. Partial XML is mirrored every 10 seconds so the master shows
progress during the run. `DTP_MAVEN_ARGS` in a suite's `env` is appended to
the Maven command line: `-Dtest=CommitActionTest` narrows a run,
`-Dui.test.vmargs=…` reaches the test JVM's argLine.

One EGit quirk is worth knowing. Its `other-os` profile, which sets the UI
test JVM arguments (`-Xmx1024m`, the SWTBot timeout and screenshot
directory), only activates when the OS name is literally `not-mac`, so on
Linux the argLine is empty, exactly as it is for a plain `mvn verify`. The
harness reproduces the build as it is rather than fixing it; pass what you
need through `DTP_MAVEN_ARGS`.

The default ref is the newest EGit release tag. `EGIT_REF=master` works too,
but then `JGIT_SITE` must point at a matching JGit snapshot p2 repository,
because EGit's pom expects a sibling JGit checkout.

Any Tycho reactor works the same way: `dtp discover <dir>` prints a
submission listing every `eclipse-test-plugin` module it finds. The `eclipse`
harness covers products tested with the Eclipse test framework instead
(`DTP_TEST_APPLICATION` plus a `product` payload). With neither, the fixture
in `examples/fixtures/rcp-suite.sh` runs. It produces the same surefire XML,
`eclipse.log` and screenshots, so the platform can be demonstrated without a
build.

## Layout

```
cmd/dtp-master     control plane: API, scheduler, rollup, dashboard
cmd/dtp-runner     runs on the node, one per suite attempt: build cache -> run the suite -> upload -> report
cmd/dtp            operator CLI
cmd/dtp-node       node agent: node.yaml -> labels, slots and slot size, heartbeat, runner install, cache prune
internal/sched     the control loop: ledger and quota rules, admission and node choice, reconciliation, retries, rollup
internal/config    process configuration; the catalog types (pools, nodes, groups, users, quota rules) and their validation
internal/backend   Nomad job generation; local process backend
internal/nomad     minimal Nomad API client
internal/pg        minimal PostgreSQL wire-protocol client
internal/agent     node agent logic (detection, Nomad dynamic metadata, heartbeat)
internal/buildrepo the build repository: payload manifests, "latest" and id resolution
internal/yamlite   the YAML subset node.yaml needs
internal/store     in-memory store with JSON-file and PostgreSQL backends
internal/runner    build cache, suite execution, artifact upload
internal/junit     surefire/JUnit XML parser
internal/s3        dependency-free S3 client (SigV4)
internal/api/web   dashboard (single page, no build step)
deploy/            compose stack, Nomad configs, worker images, node.yaml per demo node
examples/          submissions (fixture, build cache, real EGit) and the fixture suite
scripts/           smoke test, EGit/Tycho payload builder, local demo
```

There are no third-party Go modules: `go.mod` has no `require` block. The
Nomad client, S3 (SigV4), PostgreSQL, JUnit parsing and the dashboard are all
written against the standard library.

See [ARCHITECTURE.md](ARCHITECTURE.md) for how it works inside, the design
rationale, and what this PoC deliberately does not do yet.
