# Architecture

## Components

| Component | Runs on | Responsibility |
|---|---|---|
| `dtp-master` | one node (or as a Nomad service job) | submission API, slot ledger, dispatch, reconciliation, retries, JUnit rollup, dashboard |
| `dtp-runner` | every worker, once per suite attempt | resolve build from cache → run suite → upload artifacts → report result |
| `dtp` | operator/CI machine | submit, follow, inspect pools, pull artifacts |
| Nomad | cluster | placement, isolation, resource enforcement, node inventory |
| MinIO / S3 | cluster | artifact storage, optionally the build source |

The master holds no test logic and the runner holds no scheduling logic. They
meet at two contracts: the **RunSpec** (master → node) and the **RunEvent**
(node → master).

## Why a slot ledger *and* Nomad scheduling

"Each node has N slots" is a capacity statement; Nomad only understands
resources. The platform reconciles the two:

* A pool declares a **slot size** — `{cpu: 500, memory: 512, disk: 512}`.
* Every generated task requests exactly one slot's worth.
* A node declares `meta.dtp.slots = 3` and is sized at 3× the slot.

Nomad's bin-packer then lands exactly three suites on that node on its own. The
master's ledger (`capacity` − `in-flight`, per pool) is **admission control**,
not placement: it decides *when* to hand work to Nomad so the queue lives in the
master where it can be prioritized, inspected and cancelled, rather than as a
pile of blocked evaluations. Nomad still decides *which* node.

If a node's meta omits `dtp.slots`, the master derives the count from the node's
CPU divided by the pool slot size.

## Control loop

`internal/sched`, once a second:

```
refreshInventory   GET /v1/nodes → per-node meta, slots, readiness (5s cache)
reconcile          for every dispatched/running run:
                     timeout?            → Stop() + mark timeout
                     alloc complete/failed with no callback → mark errored
                     alloc lost          → mark errored
admit              queued runs, priority desc then FIFO:
                     free slot in pool?  → build RunSpec, register Nomad job
rollup             recompute each regression's totals/state;
                   on completion, write manifest.json to the object store
```

The runner's HTTP callback is authoritative for test results; the Nomad poll is
the safety net that catches runs which never reported.

## Run state machine

```
                     ┌──────────► canceled
queued ─► dispatched ─► running ─┬─► passed
   ▲                             ├─► failed    (tests failed)
   │                             ├─► errored   (harness/infra failure)
   └──── retry (attempt+1) ◄─────┴─► timeout
```

A retry is a **new run** with `attempt+1`, its own token, and its own artifact
prefix, so `attempt-1/` and `attempt-2/` sit side by side under the suite. The
suite's verdict is its newest attempt; a suite whose newest attempt passed with
`attempt > 1` is reported flaky.

Regression state is the fold over the newest attempt per suite: any errored →
`errored`, else any failed → `failed`, else `passed`.

## The two execution abstractions

Both run the same `dtp-runner` binary; only the Nomad driver and how the runner
arrives differ.

| | `process` | `container` |
|---|---|---|
| Nomad driver | `exec` (`raw_exec` in the compose demo, since the client is itself containerized) | `docker` |
| Isolation | task directory + process cgroup | OCI container |
| Runner delivery | pre-installed on the node, or fetched via the `artifact` stanza (`runner_url`) | baked into the image as the entrypoint |
| Build cache | node path (`cache_dir`) | host path bind-mounted into every suite container |
| Fits | provisioned VMs that already carry the RCP runtime, Windows nodes, real display sessions | reproducible Linux/GTK suites under Xvfb |

Pools pin the driver explicitly (`task_driver`), so `exec` vs `raw_exec` and
`docker` vs `podman` are configuration, not code.

## Contracts

### RunSpec — master → node

Handed over as base64 JSON in `DTP_RUN_SPEC` (or a Nomad dispatch payload). It
is self-contained: a runner needs no other configuration and no cluster lookup.

```jsonc
{
  "run_id": "run-…", "regression_id": "reg-…", "suite": "…", "attempt": 1,
  "build":   [{ "name": "product", "url": "s3://…", "sha256": "…", "unpack": "tar.gz" }],
  "command": ["/opt/dtp/run-suite.sh"],
  "env":     { "…": "…" },
  "timeout": "15m",
  "cache_dir": "/var/lib/dtp/cache",
  "s3":      { "endpoint": "…", "bucket": "dtp", "prefix": "results/<reg>/<suite>/attempt-1/", … },
  "master_url": "http://master:8080",
  "token": "<per-run bearer token>"
}
```

### RunEvent — node → master

`POST /api/v1/runs/{id}/events`, authorized by the per-run token minted at queue
time, so a node can only report on the attempt it was given. Two phases:
`started` (carries node identity) and `finished` (state, exit code, JUnit
summary, failing cases, artifact list). Posted with bounded retries — losing a
callback would cost a whole suite run.

## Build cache

`results` are per-run; **builds are not**. The runner keys each payload by
sha256 (or by URL hash when no sha is given) and unpacks it once into
`<cache_dir>/<key>/payload`, marked complete by a `.ready` file written after an
atomic directory rename. Concurrent slots on the same node coordinate through a
lock file: the loser waits for `.ready` instead of downloading again.

The unpacked path is exported as `DTP_BUILD_<NAME>` (e.g. `DTP_BUILD_PRODUCT`),
with single-directory archives flattened so the variable points at the product
root. A 400MB RCP product therefore crosses the network once per node per build,
not once per suite.

Archive extraction guards against path traversal (`zip`/`tar` entries that
escape the destination are rejected).

## Result aggregation

The runner uploads; the **master** parses. `internal/junit` accepts both a
`<testsuites>` wrapper and a bare `<testsuite>` root, recurses into nested
suites, and keeps failing/errored cases (message + truncated stack) in the
store — the full text is always in the uploaded artifact.

Classification distinguishes the two failures people confuse:

* non-zero exit **with** parsed failures → `failed` (a test problem)
* non-zero exit **with no results at all** → `errored` (a harness problem)

That distinction is what makes retry policy meaningful.

## Storage

The master keeps regressions in memory and mirrors each one to
`<state_dir>/regressions/<id>.json` on every mutation (atomic write + rename).
On restart, runs that were mid-flight are re-queued; the store interface is
narrow enough to swap for Postgres without touching the scheduler.

A mutation counter drives the dashboard's long-poll (`/api/v1/overview?since=`),
so the UI updates the moment anything changes rather than on a timer.

## What this PoC does not do yet

Deliberate omissions, roughly in the order they would matter:

* **No auth on the API.** Runner callbacks are token-authorized; operator
  endpoints are open. Put the master behind your SSO, and move S3 credentials
  out of the RunSpec into Nomad Variables or Vault.
* **State is JSON files.** Fine for hundreds of regressions, not for years of
  history. Swap `internal/store` for Postgres.
* **No baseline comparison.** The rollup says what failed, not whether it is
  *newly* failing. The manifest carries everything needed to diff against the
  previous green regression.
* **No test-level sharding.** Chosen deliberately — the suite is the unit. The
  `Run` model already carries `attempt`; adding `shard_index`/`shard_count`
  alongside it is a scheduler change, not a redesign.
* **No log streaming.** `dtp logs` reads the uploaded transcript after the fact;
  the Nomad client has `AllocLogs` wired for live tailing, unused by the UI.
* **Single master.** The control loop is not leader-elected. Nomad holds the
  real state, so a hot standby mostly needs a shared store and a lock.
* **Nomad host volumes** are declared per pool but the compose demo uses plain
  bind mounts for the container pool's cache.
