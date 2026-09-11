# Distributed Test Platform (PoC)

A master node that distributes **Eclipse RCP test suites** across pools of worker
nodes using **HashiCorp Nomad**, then aggregates every result and artifact into
`results/<regression_id>/`.

```
                    submission.json
                          │
                    ┌─────▼─────────────────────────────────────┐
                    │  dtp-master                               │
                    │   • slot ledger (capacity per pool)       │
                    │   • admission: priority, then FIFO        │
                    │   • Nomad batch job per suite attempt     │
                    │   • JUnit rollup, retries, flaky marking  │
                    │   • REST API + dashboard                  │
                    └─────┬─────────────────────────┬───────────┘
                register  │                         │ results
                    ┌─────▼─────┐                   │
                    │   Nomad   │                   │
                    │  servers  │                   │
                    └─────┬─────┘                   │
           ┌──────────────┼──────────────┐          │
   pool: linux-container         pool: linux-process│
   ┌───────▼────────┐            ┌───────▼────────┐ │
   │ node (3 slots) │            │ node (2 slots) │ │
   │ ▣ ▣ ▢          │            │ ▣ ▢            │ │
   │ docker driver  │            │ exec driver    │ │
   │  └ dtp-runner  │            │  └ dtp-runner  │ │
   └───────┬────────┘            └───────┬────────┘ │
           │      build cache (sha256)   │          │
           └──────────────┬──────────────┘          │
                          │ artifacts               │
                    ┌─────▼───────────────────────┐ │
                    │  MinIO / S3                 │◄┘
                    │  results/<regression_id>/   │
                    │    <suite>/attempt-N/...    │
                    └─────────────────────────────┘
```

## What it does

| Concern | Decision |
|---|---|
| Unit of distribution | **One suite = one slot on one node.** No sharding. |
| Node capacity | `meta.dtp.slots` per node; the pool's slot size (CPU/MB) makes Nomad's bin-packer agree with the master's ledger. |
| Execution abstraction | **`process`** (task directory + process cgroup, Nomad `exec`/`raw_exec`) and **`container`** (Nomad `docker`). Same `dtp-runner` binary in both. |
| Placement | Submission names a **Nomad node pool** plus `requires` → `${meta.dtp.*}` constraints. Nomad schedules; the master only gates on free slots. |
| Code under test | Fetched per run from a URL/S3, verified by sha256, unpacked into a **content-addressed cache on the node** and reused across suites. |
| Results | Runner parses nothing — it uploads; the **master parses JUnit XML** into per-suite and per-regression rollups. |
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
make demo
./bin/dtp submit examples/regression.json -w
make smoke      # 20 end-to-end assertions: placement, retries, rollup, artifacts
```

### 2. Full Nomad stack

One Nomad server, three worker nodes across two pools, MinIO and the master —
all in compose.

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

## The submission document

```jsonc
{
  "name": "egit-nightly",
  "priority": 50,                        // higher is admitted first
  "labels": { "branch": "master" },

  "defaults": {                          // merged under every suite
    "pool": "linux-container",
    "timeout": "10m",
    "retries": 0,
    "requires": { "os": "linux", "display": "xvfb" },
    "build": [
      { "name": "product", "url": "s3://builds/rcp-4.30-linux.tar.gz",
        "sha256": "b1946ac9…", "unpack": "tar.gz" },
      { "name": "tests",   "url": "https://ci/tests-4.30.zip",
        "sha256": "4f2e1d…",   "unpack": "zip" }
    ]
  },

  "suites": [
    { "name": "org.eclipse.jgit.test" },

    { "name": "org.eclipse.egit.ui.test",
      "requires": { "rcp_version": "4.30" },   // → ${meta.dtp.rcp_version} = 4.30
      "timeout": "15m",
      "retries": 2,
      "env": { "SWTBOT_SCREENSHOT_DIR": "screenshots" } },

    { "name": "org.eclipse.egit.core.legacy.test",
      "pool": "linux-process",
      "requires": { "rcp_version": "4.26" } }
  ]
}
```

`requires` keys map onto node meta prefixed with `dtp.`, so a worker declares:

```hcl
client {
  node_pool = "linux-process"
  meta {
    "dtp.slots"       = "4"      # this node runs 4 suites in parallel
    "dtp.os"          = "linux"
    "dtp.display"     = "xvfb"
    "dtp.rcp_version" = "4.30"
  }
}
```

## CLI

```
dtp submit <file.json> [-w] [-id ID] [-priority N]   # -w follows and exits non-zero on failure
                                                     # flags go after the subcommand;
                                                     # -master URL or $DTP_MASTER selects the master
dtp status <regression-id> [-w]
dtp list
dtp pools                                            # slot ledger, live
dtp cancel <regression-id>
dtp artifacts <run-id> [-get PATH]
dtp logs <run-id>                                    # == artifacts -get dtp-runner.log
```

```
$ dtp pools
linux-container  [container/docker]  4/6 slots used
  rcp-container-1        ready    3/3  display=xvfb os=linux rcp_version=4.30
      ▸ org.eclipse.jgit.test
      ▸ org.eclipse.jgit.pgm.test
      ▸ org.eclipse.egit.core.test
  rcp-container-2        ready    1/3  display=xvfb os=linux rcp_version=4.30
      ▸ org.eclipse.egit.ui.test

linux-process    [process/exec]      3/3 slots used  (1 queued)
  rcp-process-1          ready    2/2  display=xvfb os=linux rcp_version=4.30
  rcp-process-legacy     ready    1/1  display=xvfb os=linux rcp_version=4.26
```

```
$ dtp status reg-20260911-103645-egit-nightly-b6d648
reg-20260911-103645-egit-nightly-b6d648  errored  egit-nightly
suites 9/9 done · 0 running · 0 queued   tests 164 pass / 2 fail / 0 skip   1 flaky
s3://dtp/results/reg-20260911-103645-egit-nightly-b6d648/

  SUITE                                  STATE     TRY    TESTS        TIME    NODE
  org.eclipse.egit.core.test             passed    1/1    30/30        20.3s   rcp-container-1
  org.eclipse.egit.mylyn.ui.test         passed    2/3*   14/14        12.1s   rcp-process-1
  org.eclipse.egit.target.test           errored   2/2    —            0.1s    rcp-container-2
      suite exited 42 and produced no JUnit results
  org.eclipse.egit.ui.test               failed    1/1    24/26 (2✗)   30.3s   rcp-container-2
      2 of 26 tests failed
```

## The node build cache

`results` are per-run; **builds are not**. Seed a stand-in product and submit a
regression that names it:

```bash
source <(./scripts/seed-build.sh)              # 7.7MB "RCP product" -> s3://builds/
./bin/dtp submit examples/regression-with-build.json -w
```

Each runner reports what it did with the payload:

```
$ for r in $(...run ids...); do ./bin/dtp logs $r | grep cache; done
org.eclipse.jgit.test                rcp-container-1     cache miss product -> fetching s3://builds/rcp-4.30-linux.tar.gz
org.eclipse.egit.core.test           rcp-container-1     cache hit  product (after wait)
org.eclipse.egit.ui.test             rcp-container-1     cache hit  product (after wait)
org.eclipse.egit.gitflow.test        rcp-process-legacy  cache miss product -> fetching s3://builds/rcp-4.30-linux.tar.gz
org.eclipse.egit.ui.smartimport.test rcp-process-1       cache miss product -> fetching s3://builds/rcp-4.30-linux.tar.gz
```

One fetch per node, not per suite — and the two suites that started concurrently
on `rcp-container-1` blocked on the cache lock rather than downloading twice.
The unpacked path reaches the suite as `DTP_BUILD_PRODUCT`.

`${VAR}` in a submission is expanded from the environment, which is how a CI job
injects the build URL and its sha256.

## Result layout

```
results/<regression_id>/
  manifest.json                       # regression + every attempt, written on completion
  org.eclipse.egit.ui.test/
    attempt-1/
      TEST-org.eclipse.egit.ui.test.xml
      eclipse.log
      dtp-runner.log                  # the runner's own transcript
      screenshots/test001_….png
    attempt-2/…
```

Artifacts are also reachable through the master, so the browser needs no object
store credentials: `GET /api/v1/runs/{run_id}/artifacts/{path}`.

## Running real Eclipse RCP suites

The fixture in `examples/fixtures/rcp-suite.sh` produces exactly what a real
headless suite produces (surefire XML, `eclipse.log`, failure screenshots), so
everything downstream is already exercised. To run the real thing, point the
pool at [`deploy/images/run-suite.sh`](deploy/images/run-suite.sh) — already the
default command in the compose config — and give the submission a build:

```jsonc
"build": [
  { "name": "product", "url": "s3://builds/egit-4.30-linux.tar.gz",
    "sha256": "…", "unpack": "tar.gz" }
]
```

`run-suite.sh` then starts Xvfb + a window manager and launches

```
$DTP_BUILD_PRODUCT/eclipse -application org.eclipse.test.uitestapplication \
  -data <workspace> -testpluginname $DTP_SUITE -classname $DTP_SUITE.AllTests \
  formatter=…XMLJUnitResultFormatter,$DTP_RESULTS_DIR/TEST-$DTP_SUITE.xml
```

It takes that path only when the suite's `env` sets `DTP_TEST_APPLICATION` —
a build payload alone cannot imply which Eclipse test application to launch, and
gating on it keeps the build-cache demo above running the fixture. Suite names
throughout the example submissions are EGit/JGit's real ones.

## Layout

```
cmd/dtp-master     control plane: API, scheduler, aggregator, dashboard
cmd/dtp-runner     node-side agent: build cache → run suite → upload → report
cmd/dtp            operator CLI
internal/sched     slot ledger, admission, reconciliation, retries, rollup
internal/backend   Nomad job generation; local process backend
internal/nomad     minimal Nomad API client
internal/runner    build cache, suite execution, artifact upload
internal/junit     surefire/JUnit XML parser
internal/s3        dependency-free S3 client (SigV4)
internal/api/web   dashboard (single page, no build step)
deploy/            compose stack, Nomad configs, worker images
examples/          submission + fixture suite
```

No third-party Go modules — `go.mod` has no `require` block. Nomad, S3/SigV4,
JUnit and the dashboard are all hand-rolled against the standard library.

See [ARCHITECTURE.md](ARCHITECTURE.md) for the control loop, the state machine,
and what this PoC deliberately does not do yet.
