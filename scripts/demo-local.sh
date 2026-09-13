#!/usr/bin/env bash
# Runs the platform without a Nomad cluster: the master's local backend forks
# dtp-runner against simulated nodes. Everything else - slot scheduling,
# constraints, retries, JUnit rollup, S3 artifacts, the dashboard - is the real
# code path.
set -euo pipefail
cd "$(dirname "$0")/.."
export DTP_ROOT="$PWD"

mkdir -p .dtp
[ -x bin/dtp-master ] || { echo "building..."; go build -trimpath -o bin/ ./cmd/...; }

if ! curl -sf http://127.0.0.1:9000/minio/health/live >/dev/null 2>&1; then
  echo "starting MinIO..."
  docker rm -f dtp-minio >/dev/null 2>&1 || true
  docker run -d --name dtp-minio -p 9000:9000 -p 9001:9001 \
    -e MINIO_ROOT_USER=dtpadmin -e MINIO_ROOT_PASSWORD=dtpadmin123 \
    quay.io/minio/minio:latest server /data --console-address ":9001" >/dev/null
  for _ in $(seq 1 40); do
    curl -sf http://127.0.0.1:9000/minio/health/live >/dev/null && break; sleep 0.5
  done
fi

if [ -f .dtp/master.pid ] && kill -0 "$(cat .dtp/master.pid)" 2>/dev/null; then
  kill "$(cat .dtp/master.pid)"; sleep 1
fi

./bin/dtp-master -config deploy/local.config.json > .dtp/master.log 2>&1 &
echo $! > .dtp/master.pid
for _ in $(seq 1 40); do curl -sf http://127.0.0.1:8080/healthz >/dev/null && break; sleep 0.5; done
# Pools and quotas live in the store; on a fresh state dir there are none yet.
if [ "$(curl -sf http://127.0.0.1:8080/api/v1/config | grep -c '"name"')" = "0" ]; then
  ./bin/dtp config apply deploy/local.catalog.json
fi
# Let the scheduler complete one tick so the slot ledger is populated.
for _ in $(seq 1 20); do
  [ "$(curl -sf http://127.0.0.1:8080/api/v1/pools | grep -c '"ready": true')" != "0" ] && break
  sleep 0.5
done

cat <<TXT

  master up (local backend, pid $(cat .dtp/master.pid))

  dashboard   http://localhost:8080
  minio       http://localhost:9001   (dtpadmin / dtpadmin123)
  master log  .dtp/master.log

  ./bin/dtp pools
  ./bin/dtp submit examples/regression.json -w

TXT
./bin/dtp pools
