# Architecture

How the platform works inside and why it is built this way. The
[README](README.md) says how to run and use it; this is the walkthrough for
someone changing it or adapting it to another RCP product.

**Contents:** [1. Components](#1-components) · [2. Life of a regression](#2-life-of-a-regression) · [3. Scheduling model](#3-scheduling-model) · [4. What Nomad does here](#4-what-nomad-does-here--and-what-it-no-longer-does) · [5. The catalog](#5-the-catalog-pools-nodes-and-quotas-as-data) · [6. Nodes](#6-nodes) · [7. Execution on the node](#7-execution-on-the-node) · [8. Results](#8-results) · [9. Failure handling](#9-failure-handling) · [10. API surface](#10-api-surface) · [11. Adapting it to your product](#11-adapting-it-to-your-product) · [Design notes](#design-notes) · [What this PoC does not do yet](#what-this-poc-does-not-do-yet)

## 1. Components

| Component | Runs on | Owns |
|---|---|---|
| **`dtp-master`** (Go, one process) | one machine, or as a Nomad service job | the queue and per-node slot ledger, admission and node choice, Nomad job generation, reconciliation, retries, JUnit rollup, the catalog (pools / node assignments / users / quotas), the REST API and dashboard |
| **PostgreSQL** | next to the master | regressions, runs (the queue is `runs WHERE state='queued'`), the catalog tables; JSON files replace it in the local demo |
| **Nomad servers** | cluster | node inventory, resource enforcement (cgroups), allocation lifecycle; placement is the master's (each job is pinned to a node) |
| **Nomad client** | every worker | fingerprints cores/memory, runs one task per suite attempt with the `docker` or `exec`/`raw_exec` driver; knows nothing about pools |
| **`dtp-node`** | every worker, as a service | labels the node (dynamic node metadata) with its declared slots and slot size, heartbeats, installs `dtp-runner`, prunes the build cache |
| **`dtp-runner`** | every worker, one per suite attempt (the Nomad task) | build cache → harness → artifact upload → result callback |
| **`run-suite.sh`** | inside the task | the harness: `tycho`, `eclipse` or `fixture` |
| **MinIO / S3** | cluster | artifacts under `results/<regression>/<suite>/attempt-N/`, build payloads under `builds/` |
| **`dtp`** (CLI) | operator / CI | submit, follow, cancel, inspect pools/quotas/config, discover suites |

Two contracts hold it together, both plain JSON:

* **RunSpec** (master → node): everything one attempt needs — build
  payloads, command, env, timeout, slot size, cache dir, object-store target
  and prefix, master URL, a per-run bearer token. Handed over base64-encoded
  in the task environment (`DTP_RUN_SPEC`), so a runner needs no config and
  no cluster lookups.
* **RunEvent** (node → master): `started` (node identity), `progress`
  (interim JUnit rollup every 15 s), `finished` (state, exit code, summary,
  failing cases, artifact list), posted to `/api/v1/runs/{id}/events` with
  the run's token. The master keeps no test logic; the runner keeps no
  scheduling logic.

## 2. Life of a regression

```
 dtp submit / dashboard / POST /api/v1/regressions
   │  Normalize: defaults merged under every suite; user → their groups;
   │  pools must exist and match the suite's runtime; no 0-slot rule for the user there.
   ▼
 regression + one Run per suite (attempt 1, state queued) → store
   │
   │  every second, the scheduler tick:
   │    refreshInventory Nomad nodes → ready, meta.dtp.* (labels, slots, slot size) (5 s cache)
   │    ledger           node → catalog pool (unassigned = no capacity), minus in-flight runs
   │    reconcile        one GET /v1/allocations; per in-flight run: timeout?
   │                     alloc failed/lost/complete-without-callback? → finish
   │    admit            queued runs → no rule at its limit → pick the least loaded node
   │                     with a free slot, matching labels and room under the
   │                     node/pool rules → dispatch
   │    rollup           per regression: totals, state; manifest.json on completion
   ▼
 dispatch: build RunSpec (that node's slot) → render Nomad batch job (one task,
   the node's slot as resources, meta constraints, pinned to the node) → register
   │
   ▼  Nomad starts the task on that node, reserving the slot's cores and memory
 dtp-runner in the task: POST started → ensure build payloads in the node
   cache (sha256-keyed, lock-coordinated) → run-suite.sh → parse surefire XML
   → upload results dir → POST finished
   │
   ▼
 finish: run terminal; retry queued as a new Run (attempt+1) when retryable
   and attempts remain; rollup → regression passed | failed | errored;
   manifest.json written
```

A suite is the unit throughout: one Run, one slot, one node, one artifact
prefix per attempt. There is no test-level sharding by design (an OSGi
runtime takes 15–45 s to boot; splitting below the suite would be dominated
by it).

## 3. Scheduling model

**Slots.** A slot is the resource envelope one suite gets: `cores` (dedicated,
a cpuset via Nomad's `cores` resource — the same on every machine whatever
its clock), `memory` (reserved, and the hard ceiling under docker),
`memory_max` (burst ceiling, Nomad memory oversubscription) and `disk`. Both
the count and the envelope are the **node's** declaration — `slots:` and
`slot:` in its `node.yaml`, published by the agent as `meta.dtp.slots` and
`meta.dtp.slot.*` — and every task placed on a node requests exactly one of
that node's slots. Nothing is inferred from hardware; the agent only warns
when the declaration does not fit.

**The ledger.** Each tick the scheduler builds a per-node ledger: every node
the backend reports, the pool the catalog assigns it to (none = no
capacity), its declared slots, and the in-flight runs charged to it (a run
carries the id of the node it was placed on). A pool's capacity is the sum
over its ready, assigned nodes — a virtual pool's over its members' nodes.
Admission is therefore exact: it never hands Nomad more than the nodes can
run, so the queue lives in the master (prioritized, inspectable,
cancellable) rather than as blocked evaluations in Nomad.

**Pools.** A pool is a row in the catalog: how suites run there (runtime,
driver, image, command, cache) and which nodes serve it (`nodes.pool_name`).
Nomad has no idea: every client stays in Nomad's default node pool. A
virtual pool spans concrete ones — its nodes are theirs, it inherits
runtime, driver and the rest from its first member, and members must share
runtime and driver (a job runs under one driver).

**Placement.** The master picks the node: among the pool's ready, assigned
nodes with a free slot whose labels satisfy the pool's `constraints` and the
suite's `requires`, the least loaded one (by free slots relative to
declared). The job is then pinned to it with a `${node.unique.id}`
constraint and sized from that node's slot; the `requires` also go on the
job as `${meta.dtp.<key>}` constraints, documenting why the node qualified.
Labels come from the node agent (detected: os, arch, cores, memory_mb,
display, java, docker, agent; configured: perf, anything you add).
`requires: {perf: "high"}` lands only where a node says so. The product
version is deliberately *not* a label: it belongs to the build payload, and
any node runs any version. Re-assigning a node to another pool takes effect
on the next tick; what it was running keeps running and stays charged to it.

**Admission order.** Priority (0–100, higher first) wins outright. Within a
priority level the next slot goes to the eligible run whose *user holds the
fewest slots*, oldest first on ties — fair share, so a 50-suite nightly and a
5-suite check interleave. Runs blocked by a full pool or by quota are
skipped, not barriers, and quota-blocked runs carry the reason.

**Quotas.** A rule table: subject (a user, a group, everyone) × scope (a
node, a pool, everywhere) → max slots, per user or `together`. The ledger
resolves, for the run's user and every scope the placement touches (anywhere,
the submitted pool, the node's concrete pool and the virtual pools spanning
it, the node itself), the governing per-user rule — own › groups', most
permissive › everyone — and every `together` rule covering the user, and
counts the in-flight runs each rule sees. A run may start only where none of
them is at its limit; node- and pool-scoped rules therefore take part in the
node choice, and a run that fits nowhere waits with the rule named. A 0 in
the governing per-user rule for the submitted pool (or anywhere) refuses the
submission. Groups themselves carry nothing; they are sets of users the rules
name.

**Retries and flakiness.** `retries: N` per suite (or `default_retries`). A
failed/errored/timed-out attempt queues a *new* Run with `attempt+1`, its own
token and artifact prefix. A suite's verdict is its newest attempt; a suite
that passes only on a retry is counted **flaky** in the regression totals.

**Timeouts.** Per suite (`timeout`, else `default_timeout`), enforced twice:
the runner kills the process group at the deadline (state `timeout`), and the
master stops the allocation 5 minutes past it in case the runner itself is
wedged.

## 4. What Nomad does here — and what it no longer does

Nomad is used as a **fleet execution engine**, not as a scheduler. Placement
is the master's: a run must be charged to one node's slot, nodes belong to
pools by a catalog row, and the quota rules can be scoped to a node, so the
master's ledger picks the node (§3) and the job is pinned to it. What Nomad
is still asked to do is what it does well and what the platform would
otherwise have to grow itself:

| Nomad does | Nomad no longer does |
|---|---|
| **Runs the task with isolation and limits** — the `docker` / `exec` / `raw_exec` drivers: a cgroup and task directory per attempt, `cores` as a cpuset, the memory reservation as the limit with `memory_max` as the burst ceiling, ephemeral disk, log capture, kill on stop. | **Choose the node.** Every job carries a `${node.unique.id} = <chosen node>` constraint; with one feasible node the bin-packer has nothing left to decide. |
| **Is the agent on every node** with one API the master and `dtp-node` talk to: fingerprinting (reservable cores, memory), liveness (a node goes `down` when its heartbeat lapses), dynamic node metadata (`meta.dtp.*` is stored and served by the client), start/stop/inspect of a process on any machine without SSH. | **Match labels.** The `${meta.dtp.*}` constraints from the pool's `constraints` and the suite's `requires` are still written on the job, but the master already checked them when it picked the node; on the job they document why that node qualified. |
| **Keeps allocations alive across a master restart** and reports how they ended: exit code, OOM detail, lost node; deterministic job ids let the master re-attach. | **Know about pools.** Every client sits in Nomad's default node pool; `high-perf-pool` and the others are rows in the master's catalog and moving a node between them is an UPDATE. |
| **Refuses what does not fit**: if a node's declared slots exceed what it fingerprinted, the pinned job's evaluation stays blocked and the run reports "waiting for placement" after 10 minutes — the one placement decision Nomad still makes, as a safety net under the operator's `slots:` number. | **Retry or reschedule.** Disabled on every job since the first version: a retry is a new attempt with its own artifacts, decided by the master. |
| Fetches the runner for process pools (`artifact` stanza), mounts host volumes, and shows allocations and their logs in its UI. | |

The job the master registers, one per attempt: id
`dtp-<regression>-<suite>-<attempt>` (deterministic, so a restarted master
re-attaches to live jobs), type `batch`, priority mapped from the submission,
`NodePool` `all`, the node pin plus the documentary meta constraints, one task
group with `Count: 1`, restarts and rescheduling **disabled**, ephemeral disk
= the node's slot disk, resources = one of the node's slots, `Meta` carrying
the regression/suite/run ids for the Nomad UI.

* container pools: `docker` driver, the worker image, `network_mode` when
  the client is containerized, the build cache bind-mounted (or a host
  volume), entrypoint `dtp-runner`.
* process pools: `exec` (or `raw_exec`) driver launching `dtp-runner` from
  the node (installed by the agent, or fetched via the `artifact` stanza from
  `runner_url`).

The master polls all allocations in one request per tick and maps them to
runs by job id; a run whose job vanished is `errored` ("job not found"), an
OOM-killed task is reported as such (the driver's `oom_killed` detail).

**Could Nomad go?** Yes, and the local backend shows the shape: the master
already runs suites as child processes against simulated nodes. Without Nomad
`dtp-node` would become the executor — launch a container or a cgroup'd
process with a cpuset and memory limit, enforce the timeout, capture logs,
report the exit, stay reachable from the master — and the master would need
a node RPC and liveness of its own. That is a few hundred lines of exactly
the process-supervision code Nomad has already debugged, in exchange for one
fewer cluster to run. The trade is worth making only if operating Nomad costs
more than writing that; the design keeps the option open by putting
everything Nomad-specific behind the `backend.Backend` interface.

## 5. The catalog: pools, nodes and quotas as data

Pools, node assignments, groups, users and quota rules are not process
configuration; they describe the lab, and they live only in the store:

* PostgreSQL: `pools` (one column per field) + `pool_members`, `nodes`
  (name → pool), `groups`, `users`, `group_members` (one row per membership;
  a user may be in several groups) and `quota_rules` (one row per rule). A fresh database gets the schema and, in
  the demo, the lab from `deploy/sql/*.sql` at first init; afterwards edit
  with the dashboard's ⚙ Config, the config API, `dtp config apply` /
  `dtp nodes assign` / `dtp quotas set`, or SQL followed by `dtp config reload`.
* file store: `catalog.json` in the state dir (`make demo` applies
  `deploy/local.catalog.json` on first start).

The master reads the store at start and on reload, and writes it only
through the config API — as upserts guarded by `IS DISTINCT FROM`, deleting
only rows the new catalog no longer has, so `created_at` survives and
`updated_at` moves only for rows that changed. Applying a catalog equal to
the live one is a no-op; every path is safe to repeat. The scheduler takes
one immutable, normalized snapshot per tick, so a change never lands halfway
through admission. Every change — a whole catalog or one row — is validated
as a whole (unknown members, mixed runtimes in a span, a node in an unknown
or virtual pool, a rule naming an unknown user, group, pool or node); a rejected one leaves the
previous catalog in force with the reason in the API response (or on `GET
/api/v1/config` as `store_error` when the store itself held a bad one).
What is stored is the catalog *as authored*; defaults and virtual-pool
inheritance are computed on load. The only thing the master writes on its
own is a `nodes` row for an agent it has never seen, in the pool its
`node.yaml` suggests (if that pool exists) or unassigned. Removing a pool,
node, group or user takes the rules that name it (and a group's
memberships) with it rather than leaving dangling names.

## 6. Nodes

A worker is a Nomad client plus the agent:

* The **Nomad client HCL** carries only what cannot change at runtime: data
  dir, servers, drivers, `reservable_cores` / `memory_total_mb` when a
  machine is shared. No `node_pool`: which pool a node serves is a catalog
  row on the master. `dtp-node nomad-config` renders the HCL from
  `node.yaml`.
* **`dtp-node`** (`node.yaml` beside it) detects the machine, merges
  configured labels, publishes the declared `slots` and `slot` (warning when
  `slots × slot` does not fit the node), and writes `meta.dtp.*` into the
  running client through Nomad's dynamic node metadata API — tracking which
  keys it owns so removed labels are removed. It heartbeats to the master
  (which registers a new node, and answers with the pool the catalog has it
  in and the runner checksum), installs `dtp-runner` into `runner_dir` when
  the master's copy changes, and prunes cache entries unused for
  `cache_keep`.
* The **assignment** is the master's: `nodes.pool_name`, edited in ⚙ Config
  → Nodes, with `dtp nodes assign`, or in SQL. A node in no pool is listed
  as unassigned and gets no new work.

**How a node's slot count is decided** — it is declared, never inferred:

1. The operator writes `slots: N` and the `slot:` block in the node's
   `node.yaml`; `dtp-node` publishes them as `meta.dtp.slots` and
   `meta.dtp.slot.*`. The agent checks them against what Nomad fingerprinted
   for the node and warns when `N × slot` does not fit
   ([`agent.CheckCapacity`](internal/agent/agent.go)) — a warning, not a
   correction.
2. The master's inventory takes them as is; a node that declares nothing has
   **no** slots and is shown as such. The ledger counts *ready, assigned*
   nodes per pool and admits only into free slots, choosing the least loaded
   node that satisfies the pool's constraints and the suite's `requires`.
3. Nomad enforces the physical side: every task reserves one slot's cores
   and memory on the pinned node, so an over-declared node simply leaves its
   extra slots unplaceable (and the agent will have warned).

Nothing in the master depends on the agent — a node labelled by hand in HCL
works — but the dashboard shows agent health per node, and `dtp nodes` shows
what each node is and where it serves.

## 7. Execution on the node

`dtp-runner` (one per attempt) does, in order: `started` callback → for each
build payload, `cache.Ensure` (download by URL or `s3://`, verify sha256,
unpack zip/tar into `<cache_dir>/<sha256>/payload`, marked ready by an atomic
rename; concurrent slots wanting the same payload wait on a lock file) →
export `DTP_BUILD_<NAME>` (+ `_VERSION`, `_ID` from the manifest), `DTP_SUITE`,
`DTP_RESULTS_DIR`, `DTP_WORKSPACE`, `DTP_SLOT_*`, the suite's `env` → run the
pool's command (`run-suite.sh`) in
its own process group with the timeout → parse every `*.xml` under the
results dir as surefire → classify → upload the results dir → `finished`.

Classification is what makes retries meaningful: parsed failures →
`failed` (a test problem); non-zero exit with **no** results → `errored` (a
harness problem); timeout → `timeout`.

`run-suite.sh` picks the harness from `DTP_HARNESS` (or infers it):

| Harness | Needs | Does |
|---|---|---|
| `tycho` | a reactor payload (`src/` + `m2/` + `mvn.args`, from `scripts/build-egit.sh` or your equivalent) | copies the tree into the workspace, starts Xvfb + metacity, `mvn -o verify -pl <suite>` against the shared repository, mirrors surefire XML as classes finish, collects the workspace log and every `screenshots/` directory |
| `eclipse` | an RCP product payload named `product` + `DTP_TEST_APPLICATION` | starts Xvfb + a WM and launches the Eclipse test framework application with `-testpluginname`/`-classname`, surefire XML via the formatter |
| `fixture` | nothing | the simulated suite |

Adding a harness is a `case` branch in that script; the platform sees only
the results directory.

## 8. Results

The master parses; the node uploads. `internal/junit` handles a
`<testsuites>` wrapper or a bare `<testsuite>`, nested suites, and keeps
failing/errored cases (message + truncated stack) in the store — the full
text is always in the artifact. Totals per regression fold the newest attempt
of every suite: tests, passed, failed (+errors), skipped, flaky, and the suite
counts. The regression is `errored` if any suite errored, else `failed` if
any failed, else `passed`; `manifest.json` (the regression, every attempt,
every artifact path) is written to `results/<regression>/` on completion.
Artifacts are proxied through the master (`/api/v1/runs/{id}/artifacts/…`),
so the browser needs no object-store credentials.

## 9. Failure handling

| What breaks | What happens |
|---|---|
| a test fails | the suite is `failed`, retried if `retries` allows; flaky if the retry passes |
| the harness dies (no XML) | `errored`, retried the same way |
| the task is OOM-killed | `errored` with "OOM killed … (exit 137)"; raise `memory`/`memory_max` |
| the runner cannot reach the master | it retries the callback with backoff; the master's poll sees the allocation complete and classifies from what it has |
| the allocation is lost (node died) | `errored`, "allocation lost", retried |
| the master restarts | the store reloads everything; runs mid-flight are re-queued and their old allocations reconciled away (deterministic job ids) |
| a node disappears | its slots leave the ledger at the next inventory refresh; running work on it is caught by reconciliation |
| an invalid catalog is stored | rejected, logged once, shown on `/api/v1/config`; the last good one stays live |
| the agent is silent | the node shows "agent silent" — scheduling continues on the last labels Nomad has |

## 10. API surface

| Endpoint | Purpose |
|---|---|
| `POST /api/v1/regressions` | submit; `GET` lists; `GET /{id}` detail with per-suite views |
| `POST /api/v1/regressions/{id}/cancel`, `…/suites/{suite}/cancel` | cancel everything, or one suite |
| `GET /api/v1/runs/{id}`, `…/artifacts`, `…/artifacts/{path}` | one attempt, its artifact list, an artifact streamed through the master |
| `POST /api/v1/runs/{id}/events` | the runner callback (bearer token per run) |
| `GET /api/v1/pools`, `/quotas`, `/nodes`, `/builds` | the ledger, quota usage, agents, the build repository |
| `GET /api/v1/overview?since=` | everything the dashboard shows; long-polls on the store revision |
| `GET /api/v1/catalog` | choices for the composer: pools, node label values, suites, users, templates, builds |
| `GET/PUT /api/v1/config`, `POST …/config/reload`, `PUT/DELETE …/pools/{name}`, `…/nodes/{name}`, `…/groups/{name}`, `…/users/{name}`, `…/quotas/{rule-key}` | the catalog: read, replace, re-read from the store, edit one row |
| `POST /api/v1/nodes/{name}/heartbeat`, `GET /api/v1/runner[/sha256]` | the node agent |

There is no authentication (internal system): `user` is trusted, runner
callbacks are the only token-protected calls.

## 11. Adapting it to your product

The platform is generic; the product-specific surface is small and lives in
config, scripts and payloads rather than Go:

1. **Build payload.** Produce one archive per build with everything a suite
   needs (for Tycho: the reactor + the Maven repository it resolved against,
   proven offline, plus `mvn.args`; for a product: the unpacked RCP product +
   test bundles), publish it to `s3://builds/…` **with its manifest**
   (`product`, `version`, `sha256`, `unpack`, `harness`, `suites`), and let
   submissions ask for `"id": "latest"` or a pinned id. `scripts/build-egit.sh`
   is the template — clone it.
2. **Harness.** If your suites run with Tycho, `DTP_HARNESS: tycho` already
   works (`dtp discover <reactor>` lists the modules). If they run through the
   Eclipse test framework, set `DTP_TEST_APPLICATION` and use `eclipse`. If
   they run any other way, add a `case` to `run-suite.sh`: start what you
   need, run, leave surefire XML in `$DTP_RESULTS_DIR`.
3. **Worker image / nodes.** Container pools: put your runtime deps in
   `deploy/images/rcp-runner.Dockerfile` (it already has JDK 21, Maven, Xvfb,
   metacity, GTK, WebKit). Process pools: provision the node, run `dtp-node`
   with a `node.yaml`, let it install the runner and label the node.
4. **Pools, nodes and slots.** Write your lab's pools as rows (copy
   `deploy/sql/010_seed_catalog.sql`) — names, runtime, image, command — and
   assign each node to one (`nodes` rows, or let the agents register with a
   `pool:` suggestion and adjust in ⚙ Config). Size the slot per node in its
   `node.yaml` for your heaviest suite (`memory_max_mb` for JVM bursts), and
   pick `slots` so that `slots × slot` fits the machine — the agent warns if
   it does not. Nothing to create in Nomad.
5. **Quotas.** One group per team, users listed once; then rules: an
   everyone-anywhere rule as the default cap, a per-group rule where a team
   gets more, `together` rules where a team or a pool must not be swamped,
   user rules for the exceptions (a CI identity that may run more, a
   contractor kept off a pool with a 0), a node rule where a machine must
   keep a slot free.
6. **Submissions.** Write one submission file per regression kind (nightly,
   per-branch, smoke) and keep them in `templates_dir` so the dashboard
   offers them; `${VAR}` placeholders take the build URL from CI. In CI:
   `dtp submit nightly.json -w -user ci` exits non-zero unless the regression
   passed.

## Design notes

The reasoning behind the less obvious choices.

### Why the master picks the node and Nomad still runs it

"Each node has N slots of this size" is a capacity statement; Nomad only
understands resources. The node's own declaration makes Nomad's bin-packer
and the master's ledger agree: every task placed on a node requests one of
that node's slots, and the node owns `slots ×` that. The ledger is
**admission control** — it decides *when* work is handed to Nomad so the
queue stays in the master where it can be prioritized, inspected and
cancelled — and, since the per-node ledger already knows which node has a
free slot with the right labels, it also decides *where*: the job is pinned
to that node. Nomad then does what it is good at — running the task with the
cores and memory reserved, restarting nothing, reporting the exit — and
knows nothing about pools. That is what lets a pool be a catalog row: moving
a node between pools is an UPDATE, not a client reconfiguration, and the
master never has to guess where a virtual-pool job went.

### Why the master parses and the runner uploads

The runner is a short-lived process on a machine that may vanish; the
master is the one place with durable state. So the runner does the minimum
that must happen on the node (fetch, run, upload) and reports what it found
in the XML; the master's parse of the same XML is what the rollup trusts.
Progress events reuse the parser on partial results.

### Why pools, nodes and quotas are data, not config

Pools, the node→pool assignment, groups and the quota rules describe the lab
and change without a deploy: a new machine, a team's cap, a box moved from
the dev pool to the fast one. They live only in the store — tables a DBA can read, seeded
by SQL at first init — and the API, the CLI and the dashboard write them
through one validated path; a psql edit is picked up on `dtp config reload`
rather than by polling, so the master never surprises anyone with a
half-typed row. Every write is idempotent (upserts that skip unchanged rows,
deletes of what is no longer listed), so a script can apply the same catalog
on every run. The config file is left describing the process alone. The
scheduler's per-tick snapshot keeps a change from landing halfway through
admission. Capacity deliberately does *not* live here: how many suites a
machine runs and what each gets is the machine's own statement, in its
`node.yaml`, because it is the operator of that box who knows.

### Why an agent on the node

Nomad's client config is static and per machine; labels, slot counts, slot
sizes and the runner binary are not. Dynamic node metadata lets one small
daemon keep a node's description current from one YAML file, so provisioning
a worker is "install Nomad client + dtp-node + node.yaml" and nothing in the
HCL needs editing again — not even which pool it serves, which the master
decides.

### Why the schema looks the way it does

States are enums so a typo cannot be stored; the invariants the scheduler
assumes (attempt ≥ 1, priority 0–100, burst ≥ reservation) are CHECK
constraints so no code path can violate them; relations are foreign keys
with cascades so deleting a group or a pool cannot leave dangling rows;
`updated_at` is a trigger so no writer can forget it; sets that the code
treats as sets (memberships) are join tables rather than JSON arrays, and
only the ordered lists (pools, groups, a virtual pool's members) carry a
position. A quota rule's polymorphic subject and scope are separate
nullable foreign-key columns (`user_name`/`group_name`,
`pool_name`/`node_name`) with CHECKs tying each to its kind, so a deleted
user, group, pool or node cascades to its rules and no rule can point at
nothing. The full document still rides along as JSONB on regressions and
runs, because the queryable columns are a projection and the model will grow.

### Why no third-party Go modules

`go.mod` has no `require` block: Nomad, S3/SigV4, PostgreSQL, JUnit, YAML and
the dashboard are hand-rolled against the standard library. The Docker builds
never fetch anything, the binaries are static, and a reader can follow every
wire format in the repository.

## What this PoC does not do yet

Deliberate omissions, roughly in the order they would matter:

* **No auth on the API.** Runner callbacks are token-authorized; operator
  endpoints are open and the `user` field is trusted. Fine for an internal
  system; front it with SSO if that changes, and move S3 credentials out of the
  RunSpec into Nomad Variables or Vault.
* **No baseline comparison.** The rollup says what failed, not whether it is
  *newly* failing. The manifest carries everything needed to diff against the
  previous green regression.
* **No test-level sharding.** Chosen deliberately — the suite is the unit, and
  an OSGi runtime takes 15–45s to boot, so one Nomad task per test method is
  the wrong shape. If suite-level parallelism ever runs out, the right next step
  is long-lived worker allocations that keep the runtime booted and pull test
  classes from a master-side queue, not smaller Nomad tasks.
* **No log streaming.** `dtp logs` reads the uploaded transcript after the
  fact; `progress` events cover the "is it moving" question. For centralized
  logs, run a shipper (Vector, Filebeat) as a Nomad system job over the
  allocation log directories and index into Elasticsearch/OpenSearch; the
  platform needs no change.
* **Build-cache eviction is age-based only.** `dtp-node` prunes entries unused
  for `cache_keep`; a size cap is the obvious add.
* **Single master.** State is in PostgreSQL, but admission runs in one
  process and is not leader-elected. A hot standby needs a lock; a second
  active master would claim queued rows with `FOR UPDATE SKIP LOCKED` instead
  of the in-memory ledger.
* **Nomad host volumes** are declared per pool but the compose demo uses plain
  bind mounts for the container pools' cache.
