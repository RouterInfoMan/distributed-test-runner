# Distributed Test Platform (PoC)

A master node that distributes **Eclipse RCP test suites** across pools of worker
nodes using **HashiCorp Nomad**, then aggregates every result and artifact into
`results/<regression_id>/`.

```
                    submission.json  (user, priority, suites)
                          │
                    ┌─────▼─────────────────────────────────────┐
                    │  dtp-master                               │
                    │   • catalog: pools, node→pool, quotas     │
                    │   • per-node slot ledger, picks the node  │
                    │   • quotas per user / group               │
                    │   • admission: priority, fair share, FIFO │
                    │   • Nomad batch job per suite attempt,    │
                    │     pinned to the chosen node             │
                    │   • JUnit rollup, retries, flaky marking  │
                    │   • REST API + dashboard                  │
                    └─────┬─────────────────────────┬───────────┘
                register  │                         │ results
                    ┌─────▼─────┐                   │
                    │   Nomad   │                   │
                    │  servers  │                   │
                    └─────┬─────┘                   │
       ┌──────────────────┼──────────────────┐      │
       │  global = high-perf-pool ∪ mid-pool │      │
   ┌───▼────────────┐ ┌───▼────────────┐ ┌───▼────────────┐
   │ high-perf-pool │ │ mid-pool       │ │ dev-pool       │
   │ node (6 slots) │ │ node (4 slots) │ │ nodes (2 + 1)  │
   │ ▣ ▣ ▢ ▢ ▢ ▢    │ │ ▣ ▢ ▢ ▢        │ │ ▣ ▢   ▢        │
   │ docker driver  │ │ docker driver  │ │ exec driver    │
   │  └ dtp-runner  │ │  └ dtp-runner  │ │  └ dtp-runner  │
   └───────┬────────┘ └───────┬────────┘ └───────┬────────┘
           │      build cache (sha256)   │        │
           └──────────────┬──────────────┴────────┘
                          │ artifacts
                    ┌─────▼───────────────────────┐
                    │  MinIO / S3                 │
                    │  results/<regression_id>/   │
                    │    <suite>/attempt-N/...    │
                    └─────────────────────────────┘
```

**Contents:** [Quick start](#quick-start) · [The submission document](#the-submission-document) · [Pools, nodes and quotas](#pools-nodes-and-quotas) · [CLI](#cli) · [The dashboard](#the-dashboard) · [Where the state lives](#where-the-state-lives) · [The node side: `dtp-node` + `node.yaml`](#the-node-side-dtp-node--nodeyaml) · [The build repository and the node build cache](#the-build-repository-and-the-node-build-cache) · [Result layout](#result-layout) · [Real suites vs the simulated fixture](#real-suites-vs-the-simulated-fixture) · [Running EGit for real](#running-egit-for-real) · [Layout](#layout)

How it works inside — components, the life of a regression, the scheduling
model, what Nomad does here, the catalog, execution, results, failure
handling, the API, adapting it to another product, and the design rationale —
is in **[ARCHITECTURE.md](ARCHITECTURE.md)**.

## What it does

| Concern | Decision |
|---|---|
| Unit of distribution | **One suite = one slot on one node.** No sharding. |
| Node capacity | Each node declares in its `node.yaml` how many slots it runs and what one slot is — `slots: 6`, `slot: {cores: 2, memory_mb: 4096, memory_max_mb: 8192}` — published as `meta.dtp.slots` / `meta.dtp.slot.*`. The demo lab uses **2 dedicated cores / 4 GB on every node type** (Nomad `cores` + memory reservation, with `memory_max` headroom). |
| Execution abstraction | **`process`** (task directory + process cgroup, Nomad `exec`/`raw_exec`) and **`container`** (Nomad `docker`). Same `dtp-runner` binary in both. |
| Placement | Submission names a **pool** plus `requires` → `${meta.dtp.*}`. The **master picks the node** (least loaded in the pool with a free slot and matching labels) and pins the Nomad job to it; Nomad enforces the slot's cores and memory. |
| Pools | `high-perf-pool`, `mid-pool`, `dev-pool` are rows in the master's catalog, and a node belongs to a pool by a row in the `nodes` table (dashboard, `dtp nodes assign`, or SQL) — nothing in Nomad. **`global`** is a virtual pool spanning the container pools — "anywhere with capacity". |
| Quotas | A **rule table**: (user \| group \| everyone) × (node \| pool \| everywhere) → max concurrent slots, each rule limiting every user it covers separately or all of them together. The most specific rule governs a user per scope; 0 forbids. Groups are plain sets of users. |
| Fairness | Priority wins outright; within a priority level the next slot goes to whoever holds the fewest, so a 50-suite submission shares with a 5-suite one. |
| Progress | Long suites report interim JUnit counts every 15s, so the dashboard moves during a 40-minute UI run. |
| Code under test | Fetched per run from a URL/S3, verified by sha256, unpacked into a **content-addressed cache on the node** and reused across suites. |
| Results | Runner parses nothing — it uploads; the **master parses JUnit XML** into per-suite and per-regression rollups. |
| State | **PostgreSQL** (`regressions` + `runs` tables; the queue is `WHERE state = 'queued'`) via a stdlib wire-protocol client, or JSON files for the no-dependency demo. |
| Submitting | `dtp submit file.json`, `POST /api/v1/regressions`, or the dashboard's **New regression** form: pick a template, tick suites, choose pools and targets, submit. |
| Artifacts | Pushed to S3/MinIO under `results/<regression_id>/<suite>/attempt-N/`. |
| Retries | Per-suite `retries`; a suite that goes green only on a retry is reported **flaky**. |

## Quick start

Two ways to run it. Both are the same code; they differ only in the backend
that places work.

### 1. Local backend — no cluster needed

The master forks `dtp-runner` against simulated nodes. Scheduling, slots,
constraints, retries, JUnit parsing, S3 upload and the dashboard are all the
real code path. Needs Go and Docker (for MinIO).

```bash
make demo       # starts MinIO + the master and applies deploy/local.catalog.json (the demo pools) to its store
./bin/dtp submit examples/regression.json -w
make smoke      # 37 end-to-end assertions: placement, retries, quota rules, nodes, rollup, artifacts
```

### 2. Full Nomad stack

One Nomad server, four worker nodes across three pools (13 slots; plus the
virtual `global` pool), MinIO, PostgreSQL and the master — all in compose.

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
| MinIO console | http://localhost:9001 — `dtpadmin` / `dtpadmin123` |
| PostgreSQL | `psql postgres://dtp:dtp-s3cret@localhost:5433/dtp` |

## The submission document

```jsonc
{
  "name": "egit-nightly",
  "user": "alice",                       // charged for the slots; default: dtp submit -user / $USER
  "priority": 50,                        // higher is admitted first
  "labels": { "branch": "master" },

  "defaults": {                          // merged under every suite
    "pool": "global",                    // anywhere the virtual pool spans
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
      "requires": { "perf": "high" },          // → ${meta.dtp.perf} = high
      "timeout": "15m",
      "retries": 2 },

    { "name": "org.eclipse.egit.gitflow.test",
      "pool": "dev-pool" }
  ]
}
```

The build's **version is a property of the payload, not of any node**: it
comes from the build repository's manifest, is recorded on the regression,
and reaches the suite as `DTP_BUILD_EGIT_VERSION`. Any node runs any version.

`requires` keys map onto node meta prefixed with `dtp.`, which `dtp-node`
publishes from what it detects plus the node's `node.yaml` labels:

```yaml
# node.yaml
slots: 4                 # this node runs 4 suites in parallel
slot: {cores: 2, memory_mb: 4096, memory_max_mb: 8192, disk_mb: 2048}   # what each gets
labels:
  perf: standard         # anything you want to place on: perf, gpu, site, …
```

## Pools, nodes and quotas

Pools, the node→pool assignment, groups, users and the quota rules are the
**catalog**, and the catalog lives in the store and nowhere else: the
`pools`, `nodes`, `groups`, `users`, `group_members` and `quota_rules` tables
(plus `pool_members`) in PostgreSQL — `catalog.json` in the state dir when
the master runs on the file store. The master's config file
describes the *process* — listen address, Nomad, object store, store DSN —
and says nothing about the lab.

How it gets there:

* **First init of the database**: [`deploy/sql/010_seed_catalog.sql`](deploy/sql/010_seed_catalog.sql)
  holds the demo lab as rows; the compose stack mounts it (after the schema)
  into Postgres' `docker-entrypoint-initdb.d`, so a fresh volume comes up
  with four pools, four node assignments, two groups, four users and nine
  quota rules, and the master simply loads them. For your own lab, write your own seed the
  same way — or start empty.
* **Any time after**: the dashboard's **⚙ Config**, `dtp config apply
  catalog.json`, `dtp nodes assign <node> <pool>`, `dtp quotas set <rule>
  <n>`, `PUT /api/v1/config` (`…/pools/{name}`, `…/nodes/{name}`,
  `…/groups/{name}`, `…/users/{name}`, `…/quotas/{rule-key}`), or SQL in
  psql followed by `dtp config reload`
  (`POST /api/v1/config/reload`). The API paths write the store and apply in
  one step; there is no polling of the tables — a change made with SQL is
  picked up when you say so.

Every write is **idempotent**: saving what is already there changes nothing
(no row touched, `updated_at` untouched), deleting what is not there is a
no-op, and re-assigning a node to the pool it is in returns the same catalog.
Every change is validated as a whole (a virtual pool spanning an unknown
pool, mixed runtimes in a span, a node assigned to an unknown or virtual
pool, a membership in an unknown group, a rule naming an unknown user, group,
pool or node);
a change that fails is refused with the reason — from the API, or logged and
reported as `store_error` on `GET /api/v1/config` when it came from the
store — and the previous catalog stays in force. With no pools at all the
master still runs: submissions are refused with "unknown pool" until some
exist.

```sql
UPDATE nodes SET pool_name = 'mid-pool' WHERE name = 'rcp-hp-2';
INSERT INTO group_members (group_name, user_name) VALUES ('core-devs', 'carol');
INSERT INTO quota_rules (subject_kind, user_name, scope_kind, max_slots) VALUES ('user', 'carol', 'global', 2);
-- then: dtp config reload
```

**A pool is "which nodes, run how"** — runtime (`process` or `container`),
task driver, image, command, docker network, cache dir, constraints — and
nothing about capacity. The rows for a pool, one column per field (the
schema comments in [`internal/store/sql/schema.sql`](internal/store/sql/schema.sql)
describe each):

```sql
INSERT INTO pools (name, runtime, task_driver, image, command, docker_network, cache_dir, position)
VALUES ('high-perf-pool', 'container', 'docker',
        'dtp/rcp-runner:dev', '{/opt/dtp/run-suite.sh}', 'dtp_default', '/tmp/dtp-nomad/cache', 0),
       ('global', NULL, NULL, '', '{}', '', '', 3);
INSERT INTO pool_members (pool_name, member_name, position) VALUES ('global', 'high-perf-pool', 0);
INSERT INTO nodes (name, pool_name) VALUES ('rcp-hp-1', 'high-perf-pool');
```

**Capacity belongs to the node.** Each worker's `node.yaml` says how many
suites it runs at once and what each of them gets:

```yaml
slots: 6
slot:
  cores: 2            # a cpuset of dedicated cores (or cpu_mhz: 2000 for shares)
  memory_mb: 4096     # reserved, and the hard ceiling unless memory_max_mb is set
  memory_max_mb: 8192 # burst ceiling (Nomad memory oversubscription)
  disk_mb: 2048
```

`dtp-node` publishes these as `meta.dtp.slots` and `meta.dtp.slot.*`; the
master reads them from the node ([`slotsFor`](internal/backend/nomadbackend.go),
`slotOf`), sizes every job it places there from them, and shows them per
node (`dtp nodes`, the Pools table). A 2-core slot is 2 cores on a 2 GHz box
and on a 4 GHz box alike (Nomad's `cores` resource, a cpuset). Under the
docker driver memory is also a hard ceiling, and an EGit UI run (Maven + the
Eclipse test JVM + WebKit helpers + Xvfb) was OOM-killed at 3 GB;
`memory_max_mb` lets a task burst above its reservation up to that ceiling —
Nomad memory oversubscription, enabled by the compose bootstrap — so the JVMs
get headroom without every slot reserving the peak. An OOM kill is reported
as such on the run. Nodes in one pool may declare different slot sizes: each
job gets its own node's.

**A node is in a pool because the catalog says so.** The `nodes` table maps
node name → pool; `dtp-node` only *suggests* a pool (`pool:` in `node.yaml`)
for the first registration — an agent the master has never heard of is
added to that pool if it exists, unassigned otherwise — and after that the
catalog is the only thing that decides. Change it in **⚙ Config → Nodes**,
with `dtp nodes assign rcp-hp-2 mid-pool`, or with SQL; the next scheduler
tick places on the new pool, whatever the node was running keeps running.
A node in no pool (or in a pool it was removed from — deleting a pool
unassigns its nodes) gets no new work and is listed as **unassigned** on the
dashboard and in `dtp nodes`. Nomad itself keeps every client in its default
node pool; pools are the master's, and each job is pinned to the node the
master chose (`${node.unique.id}`), so the assignment never needs a client
restart.

A **virtual pool** (`global`) needs only a name and `spans`; it inherits the
rest from its first member. Its nodes are its members' nodes and its
capacity the sum of theirs; the master picks among them like for any pool.
Members must share one runtime and driver (a job runs under one driver).

**Groups** (`groups` + `group_members`) are plain named sets of users — a
user may be in any number of them — that the quota rules refer to, and that
anything else addressing "these people" can refer to later. A group carries
no limits itself. **Users** (`users`) are the identities slots are charged
to; someone who submits without being listed is simply a user in no group.

**Quota rules** (`quota_rules`) are the limits, one row each:

```
(user NAME | group NAME | everyone)  ×  (node NAME | pool NAME | everywhere)  →  max concurrent slots
```

For a group or for everyone the limit applies to **each user separately**
unless the rule says `together`, in which case it applies to **all of them
added up**. How rules combine:

* Rules of different scopes are all enforced: `carol@global 4` and
  `carol@pool:high-perf-pool 2` mean at most 2 on the fast machines and at
  most 4 anywhere.
* Within one scope, the *per-user* rule that governs a user is the **most
  specific** one: their own rule, else the most permissive of their groups'
  rules, else the everyone rule. So `global@global 6` is the default for
  everybody, `group:core-devs@global 8` raises it for the core team, and
  `user:carol@global 2` lowers it for carol whatever her groups say.
* Every `together` rule that covers the user applies on top: `group:core-devs
  /together@global 10` caps the team's slots added up;
  `global/together@node:rcp-hp-1 5` keeps one of that node's six slots free
  for everyone.
* A pool-scoped rule sees work that lands in the pool however it got there:
  a run submitted to the virtual `global` pool that lands on a
  `high-perf-pool` node counts in both, and so does a run submitted straight
  to `high-perf-pool`. A node-scoped rule steers the master's node choice
  before it makes a run wait.
* `0` forbids: a submission to a pool where the user's governing per-user
  rule is 0 is refused outright, naming the rule; a `together` rule at 0
  drains its scope. No rule means no limit.

A rule is written `subject[:name][/together]@scope[:target]` in the CLI and
the API — `global@global`, `user:carol@global`,
`group:release@pool:high-perf-pool`, `global/together@node:rcp-hp-1` — and
edited as a row in ⚙ Config. The dashboard's **Quota rules** table shows
every rule with what it currently sees: for a `together` rule the slots in
use against the limit, for a per-user rule each user it governs with their
slots in scope and the cap that actually applies to them (which may come from
a more specific rule, shown alongside).

A user at a cap keeps their remaining suites queued — visibly, with the
rule named on the run — while other users keep flowing. There is no
authentication: the `user` field is trusted (this is an internal system),
and `dtp submit` fills it from `-user`, `$DTP_USER` or `$USER`.

```sql
INSERT INTO users (name, display_name) VALUES ('erin', 'Erin Salas');
INSERT INTO group_members (group_name, user_name) VALUES ('release', 'erin'), ('core-devs', 'erin');
INSERT INTO quota_rules (subject_kind, group_name, together, scope_kind, pool_name, max_slots, note)
  VALUES ('group', 'release', true, 'pool', 'mid-pool', 3, 'release shares the mid machines');
```

How the numbers are enforced — the ledger, node choice, what Nomad checks —
is in [ARCHITECTURE.md § 3 and § 6](ARCHITECTURE.md#3-scheduling-model).

## CLI

```
dtp submit <file.json> [-w] [-id ID] [-priority N] [-user NAME]
                                                     # -w follows and exits non-zero on failure
                                                     # flags go after the subcommand;
                                                     # -master URL or $DTP_MASTER selects the master
dtp status <regression-id> [-w]
dtp list
dtp pools                                            # slot ledger, live
dtp nodes                                            # every node: pool, status, slots, slot size, agent
dtp nodes assign <node> [<pool>]                     # move a node (no pool: park it, unassigned)
dtp quotas                                           # the rule table with live usage
dtp quotas set <rule> <max-slots> [note]             # add or change a rule (0 forbids); idempotent
dtp quotas rm <rule>
dtp builds                                           # the build repository: ids, products, versions, suites
dtp cancel <regression-id> [-suite NAME]             # whole regression, or one suite of it
dtp artifacts <run-id> [-get PATH]
dtp logs <run-id>                                    # == artifacts -get dtp-runner.log
dtp discover <reactor-dir> [-pool P] [-build NAME]   # Tycho reactor -> submission JSON
dtp config                                           # the catalog as stored (pools, nodes, groups, users, quota rules)
dtp config apply <catalog.json>                      # replace it (a {"pools", "nodes", "groups", "users", "quotas"} file)
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

## The dashboard

The **＋ New regression** button opens a composer fed by `GET /api/v1/catalog`:
templates (the submission files in `templates_dir` — the repo's `examples/`
in both demo stacks), the pools, every `meta.dtp.*` value present on the
registered nodes as **Targets** dropdowns, suites and users seen before, and
the build payloads published to the `builds` bucket. Pick a template, tick the
suites to run, override pool / targets / timeout / retries per suite, add a
suite by name, and submit. It posts the same JSON `dtp submit` would.

Live regressions have a **cancel** button on their row, and each live suite a
**✕** in the expanded detail (`dtp cancel <id> [-suite NAME]` does the same).

**⚙ Config** opens the catalog editor: pools (runtime, driver, spans,
image, command — or the whole pool as JSON for the rarer fields), **nodes**
(every node the backend sees or the catalog knows, with a pool selector, its
live status, slots and declared slot size; `＋ node` pre-registers one ahead
of its agent), groups (name, description; members shown), users (display
name, email, groups) and **quota rules** (who × where → max slots, each user
or all together, with a note), each row saved on its own through the config
API — editing a rule's subject or scope replaces that rule. Every save is
validated as a whole catalog; a bad one is refused with the reason in the
row and nothing changes; saving an unchanged row changes nothing. **↻ reload
from store** re-reads the tables after an edit made with SQL.
The page updates in place: only the cells whose content changed are touched,
so open details and pool cards stay put while results stream in.

## Where the state lives

The master keeps regressions and runs in memory and mirrors every mutation to
its store. With `"store": {"driver": "postgres", "dsn": "postgres://…"}` (the
compose stack) that is two tables — `regressions` and `runs` — whose columns
are what you query and whose `doc` column is the full JSON document, so the
schema never lags the model. The queue is a query:

```sql
SELECT suite, user_name, pool, priority, queued_at FROM run_queue;
```

The catalog tables sit next to them: `pools`, `pool_members` (a virtual
pool's members), `nodes` (name → pool), `groups`, `users`, `group_members`
and `quota_rules` (subject kind + user/group, together, scope kind +
pool/node, max_slots — with CHECKs that exactly the right name is set for
each kind and a UNIQUE over the whole subject × scope tuple); every field is
a column. The master reads them at start and on `POST
/api/v1/config/reload`; the config API writes them with upserts and deletes
only what the new catalog no longer has.

The schema is one file, [`internal/store/sql/schema.sql`](internal/store/sql/schema.sql),
embedded into the binary: Postgres runs it at first init in the compose
stack, and the master runs it itself when it connects to a database with no
tables. States are **enums** (`run_state`, `regression_state`,
`pool_runtime`, `task_driver`), the invariants the code relies on are **CHECK
constraints** (attempt ≥ 1, priority 0–100, a burst ceiling never below the
reservation, no self-spanning pool), relations are **foreign keys** with
`ON DELETE CASCADE`, every table has `created_at`/`updated_at` kept by a
trigger, every table and non-obvious column carries a `COMMENT` (`\d+ runs`
in psql), and the queue is a **view** (`run_queue`). No migrations: to change
the schema, edit the file and recreate the database (`make stack-stop` drops
the volume).

History stays for as long as you keep it; a restart reloads everything and
re-queues whatever was mid-flight. The client is `internal/pg`, ~400 lines of
the PostgreSQL wire protocol on the standard library (SCRAM-SHA-256, MD5, TLS,
extended queries), so the build still has no third-party modules. Without a
`store` section the master writes one JSON file per regression under
`state_dir`, which is what the local demo uses.

## The node side: `dtp-node` + `node.yaml`

Two things run on a worker. **`dtp-runner`** is not a daemon: Nomad launches
one per suite attempt and it learns everything it needs (master URL, object
store, token, cache dir) from the RunSpec in the task's environment.
**`dtp-node`** is the daemon — the node agent — and it is what makes a machine
a worker: one static binary plus a `node.yaml` beside it.

```yaml
master: http://master:8080         # the master; the agent registers here
nomad: http://127.0.0.1:4646       # the local Nomad client's API
name: rcp-hp-1                     # default: hostname
slots: 6                           # required: how many suites this node runs at once
slot:                              # required: what each of them gets
  cores: 2
  memory_mb: 4096
  memory_max_mb: 8192              # optional burst ceiling
  disk_mb: 2048
pool: high-perf-pool               # optional: the pool to join when the master first sees this node
labels:                            # meta.dtp.*; override the detected ones
  perf: high
cache_dir: /var/lib/dtp/cache
cache_keep: 168h                   # prune payloads unused for a week
runner_dir: /usr/local/bin         # keep dtp-runner here, fetched from the master when its sha256 changes
interval: 60s
```

What it does, once (`dtp-node apply`) or continuously (`dtp-node run`):

* **Detects the node** — os, arch, cores and memory (from what the local Nomad
  client fingerprinted, so the count matches what the scheduler places
  against), display (`Xvfb` present?), Java major version, docker — and
  merges `labels:` on top.
* **Declares the slots** — the count and the slot size in `node.yaml`, as
  `meta.dtp.slots` and `meta.dtp.slot.*`. They are decisions, not
  measurements: the agent *warns* when `slots × slot` exceeds what Nomad
  reports for the node, but never changes the numbers. `node.yaml` is
  re-read every tick, so editing it is enough — no restart.
* **Labels the running Nomad client** via dynamic node metadata
  (`/v1/client/metadata`): no HCL edit, no restart; labels dropped from
  `node.yaml` are removed again. The master schedules on exactly these
  (`requires` → `${meta.dtp.*}`), and `dtp pools` shows them.
* **Heartbeats to the master** (`POST /api/v1/nodes/{name}/heartbeat`), which
  registers a node it has not seen (in the pool `node.yaml` suggests, if it
  exists) and answers with the pool the catalog has the node in and the
  runner's checksum; the agent logs a warning while it is in none. The
  dashboard shows `agent 0.4 ✓` on the node, or a warning when it goes
  silent.
* **Installs `dtp-runner`** into `runner_dir` from `GET /api/v1/runner`
  whenever the master's copy changes, so process-pool nodes never carry a
  stale runner.
* **Prunes the build cache** — entries not used within `cache_keep`.

`dtp-node labels` prints what it would set, where each value came from and
whether the declared slots fit;
`dtp-node nomad-config` renders a Nomad client HCL for a fresh machine from
the same `node.yaml` (servers, drivers, and the labels as a static fallback —
no `node_pool`: which pool the node serves is decided on the master). On a
real node run it as a service next to the Nomad client; in the compose demo
each node has a sidecar (`deploy/nodes/*.yaml`), which is why the client HCL
files carry only `dtp.slots` now.

## The build repository and the node build cache

Builds are published to the `builds` bucket, each payload with a **manifest**
beside it (`builds/<id>.json`) written by whatever produced it:

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
repository (`dtp builds`, `GET /api/v1/builds`, the composer's build picker),
and a submission names a build instead of pasting a URL and a checksum:

| `build` entry | Meaning |
|---|---|
| `{"name": "egit", "id": "latest"}` | the newest payload whose `product` is `egit` (or `"product": "…"` to name it) |
| `{"name": "egit", "id": "egit-b522e135e4"}` | that payload, pinned |
| `{"name": "egit", "url": "s3://…", "sha256": "…"}` | as given; gains the manifest's version when the repository knows the URL |

At submission the master fills in URL, sha256 and unpack from the manifest,
records the resolved payloads on the regression (`dtp status` and the
dashboard show `egit 7.8.0 · egit-b522e135e4`), and the runner exports
`DTP_BUILD_<NAME>_VERSION` and `DTP_BUILD_<NAME>_ID` next to
`DTP_BUILD_<NAME>`. The version therefore travels with the build, never with
a node.

`results` are per-run; **builds are not**. Seed a stand-in product with its
manifest and submit a regression that asks for it:

```bash
./scripts/seed-build.sh                        # 7.7MB "RCP product" rcp-stub 4.30 -> s3://builds/ + manifest
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

One fetch per node, not per suite — and the two suites that started concurrently
on `rcp-hp-1` blocked on the cache lock rather than downloading twice.
The unpacked path reaches the suite as `DTP_BUILD_PRODUCT`.

`${VAR}` in a submission is expanded from the environment, for the cases where
a CI job wants to pin a build it just produced (`BUILD_ID` is printed by the
build scripts) rather than take `latest`.

## Result layout

```
results/<regression_id>/
  manifest.json                       # regression + every attempt, written on completion
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

Artifacts are also reachable through the master, so the browser needs no object
store credentials: `GET /api/v1/runs/{run_id}/artifacts/{path}`.

## Real suites vs the simulated fixture

Two kinds of suite appear in this repository, and they are named so they
cannot be confused:

| | Suites | What runs | Artifacts |
|---|---|---|---|
| **Real** | `org.eclipse.egit.core.test`, `org.eclipse.egit.ui.test`, `org.eclipse.egit.gitflow.test` ([`examples/egit.json`](examples/egit.json)) | EGit's own Tycho test modules against the EGit build: 447 + 615 + 42 JUnit/SWTBot tests, Eclipse workbench under Xvfb | surefire XML per class, the workspace `.log`, **SWTBot screenshots of the workbench at the moment of failure** (JPEG, 1920×1080) |
| **Simulated** | `simulated.*` ([`examples/regression.json`](examples/regression.json), the smoke test) | [`examples/fixtures/rcp-suite.sh`](examples/fixtures/rcp-suite.sh): no product, no tests — it *generates* the shape of a run so the platform can be exercised in seconds without a 800 MB build | surefire XML, an `eclipse.log`, and a rendered mock dialog per failing case, watermarked **SIMULATED FIXTURE** |

The platform does not distinguish them: a simulated run goes through the
same admission, dispatch, runner, upload, parsing and rollup as a real one.
Only the harness differs (`fixture` vs `tycho`/`eclipse`, see below).

The fixture is driven by environment variables set per suite (or in
`defaults`) under `env` in the submission:

| Variable | Default | Effect |
|---|---|---|
| `FIXTURE_TESTS` | 24 | number of test cases in the suite |
| `FIXTURE_FAIL` | 0 | how many of them fail (each gets a screenshot and a stack trace) |
| `FIXTURE_SKIP` | 0 | how many are skipped |
| `FIXTURE_SECONDS` | 20 | wall-clock duration of the run, spread evenly over the cases |
| `FIXTURE_FLAKY` | 0 | `1`: the failures happen only on attempt 1, so a retry goes green — exercises flaky detection |
| `FIXTURE_CRASH` | 0 | `1`: exit 42 with no results at all — exercises "errored" (harness failure) vs "failed" (test failure) |

`examples/regression.json` uses these to stage one of everything: a flaky
suite with `retries: 2`, a crashing suite with `retries: 1`, a suite with two
failures, suites pinned to `dev-pool` and to `perf=high`.
That is what `make smoke` asserts against (25 checks). Nothing simulated is
needed for real work: `make egit` never touches the fixture.

## Running EGit for real

[`scripts/build-egit.sh`](scripts/build-egit.sh) builds EGit from source with
Tycho inside the worker image and publishes the result as a build payload;
[`examples/egit.json`](examples/egit.json) runs its three test modules as
suites:

```bash
make stack
./scripts/build-egit.sh                # ~10 min first time: clone, mvn install, warm the test runtime, tar, publish + manifest
./bin/dtp submit examples/egit.json -w -user jenkins    # "id": "latest" of product egit
```

What the payload is: the checked-out tree, the Maven repository it resolved
against (target platform, Tycho, the surefire OSGi runtime), the JGit p2
repository it built against, and an `mvn.args` file with the flags the
reactor needs — plus the manifest beside it in the repository (version from
the tag, commit, checksum, the test modules it carries). The build script proves the repository is complete by rerunning
a test **offline** before packaging. On a node,
[`run-suite.sh`](deploy/images/run-suite.sh)'s `tycho` harness copies the tree
into the attempt's workspace, starts Xvfb + metacity and runs

```
mvn -o verify -pl org.eclipse.egit.ui.test -Dmaven.repo.local=<cache>/m2 -Ddash.skip=true …
```

so nothing is downloaded at test time and concurrent slots share one Maven
repository read-only. Surefire XML, the workspace `.log`, the Maven transcript
and SWTBot's screenshots (every `screenshots/` directory under the module,
whatever the format — SWTBot defaults to JPEG) come back as artifacts; partial
XML is mirrored every 10 s so the master shows progress mid-run.
`DTP_MAVEN_ARGS` in a suite's env is appended to the Maven command line:
`-Dtest=CommitActionTest` narrows a run, `-Dui.test.vmargs=…` reaches the
test JVM's argLine.

One EGit quirk worth knowing: its `other-os` profile, which sets the UI test
JVM arguments (`-Xmx1024m`, the SWTBot timeout and screenshot directory), only
activates when the OS *name* is literally `not-mac`, so on Linux the argLine
is empty — exactly as it is for a plain `mvn verify`. The harness reproduces
the build faithfully rather than fixing it; pass what you need through
`DTP_MAVEN_ARGS`.

The default ref is the newest EGit release tag; `EGIT_REF=master` works too but
then needs `JGIT_SITE` pointing at a matching JGit snapshot p2 repository,
because EGit's pom expects a sibling JGit checkout.

Any Tycho reactor works the same way — `dtp discover <dir>` prints a submission
listing every `eclipse-test-plugin` module it finds — and the `eclipse` harness
covers products tested with the Eclipse test framework instead
(`DTP_TEST_APPLICATION` + a `product` payload). With neither, the fixture in
`examples/fixtures/rcp-suite.sh` runs, which produces the same surefire XML,
`eclipse.log` and screenshots, so the platform is demoable without a build.

## Layout

```
cmd/dtp-master     control plane: API, scheduler, aggregator, dashboard
cmd/dtp-runner     node-side agent: build cache → run suite → upload → report
cmd/dtp            operator CLI
cmd/dtp-node       node agent: node.yaml → labels, slots + slot size, heartbeat, runner install, cache prune
internal/sched     the control loop: ledger + quota rules, admission and node choice, reconciliation, retries, rollup
internal/config    process config; the catalog types (pools, nodes, groups, users, quota rules) and their validation
internal/backend   Nomad job generation; local process backend
internal/nomad     minimal Nomad API client
internal/pg        minimal PostgreSQL wire-protocol client
internal/agent     node agent logic (detection, Nomad dynamic metadata, heartbeat)
internal/buildrepo the build repository: payload manifests, "latest"/id resolution
internal/yamlite   the YAML subset node.yaml needs
internal/store     in-memory store with JSON-file and PostgreSQL backends
internal/runner    build cache, suite execution, artifact upload
internal/junit     surefire/JUnit XML parser
internal/s3        dependency-free S3 client (SigV4)
internal/api/web   dashboard (single page, no build step)
deploy/            compose stack, Nomad configs, worker images, node.yaml per demo node
examples/          submissions (fixture, build cache, real EGit) + fixture suite
scripts/           smoke test, EGit/Tycho payload builder, local demo
```

No third-party Go modules — `go.mod` has no `require` block. Nomad, S3/SigV4,
PostgreSQL, JUnit and the dashboard are all hand-rolled against the standard
library.

See [ARCHITECTURE.md](ARCHITECTURE.md) for how it all works inside, the
design rationale, and what this PoC deliberately does not do yet.
