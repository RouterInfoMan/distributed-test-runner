#!/usr/bin/env bash
# End-to-end assertion against a running master (local or Nomad backend):
# submit, wait, then check placement, retries, flaky detection, JUnit rollup and
# the artifact tree. Exits non-zero on the first failed expectation.
#
#   ./scripts/smoke.sh [master-url]
set -uo pipefail
cd "$(dirname "$0")/.."
MASTER="${1:-${DTP_MASTER:-http://127.0.0.1:8080}}"
DTP="./bin/dtp -master $MASTER"
fails=0

check() { # check <description> <expected> <actual>
  if [ "$2" = "$3" ]; then printf '  ok    %s\n' "$1"
  else printf '  FAIL  %s: want %s, got %s\n' "$1" "$2" "$3"; fails=$((fails+1)); fi
}

jqp() { python3 -c "import sys,json;d=json.load(sys.stdin);print($1)"; }

echo "submitting to $MASTER"
REG=$($DTP submit examples/regression.json | head -1 | awk '{print $1}')
[ -n "$REG" ] || { echo "submission failed"; exit 1; }
echo "  $REG"

echo "waiting for completion..."
for _ in $(seq 1 200); do
  STATE=$(curl -sf "$MASTER/api/v1/regressions/$REG" | jqp "d['regression']['state']")
  case "$STATE" in passed|failed|errored|canceled) break;; esac
  sleep 3
done
echo "  final state: $STATE"

D=$(curl -sf "$MASTER/api/v1/regressions/$REG")
say() { printf '%s' "$D" | jqp "$1"; }

echo "rollup"
check "regression state"      "errored" "$STATE"
check "suites counted"        "9"       "$(say "d['regression']['totals']['suites']")"
check "suites passed"         "7"       "$(say "d['regression']['totals']['suites_passed']")"
check "suites failed"         "1"       "$(say "d['regression']['totals']['suites_failed']")"
check "suites errored"        "1"       "$(say "d['regression']['totals']['suites_errored']")"
check "failing test cases"    "2"       "$(say "d['regression']['totals']['failed']")"
check "flaky suites"          "1"       "$(say "d['regression']['totals']['flaky']")"
check "nothing left queued"   "0"       "$(say "d['regression']['totals']['suites_queued']")"

echo "retries"
check "flaky suite retried"   "2" \
  "$(say "[s['attempts'] for s in d['suites'] if s['suite']=='org.eclipse.egit.mylyn.ui.test'][0]")"
check "crashing suite gave up after its retry" "2" \
  "$(say "[s['attempts'] for s in d['suites'] if s['suite']=='org.eclipse.egit.target.test'][0]")"
check "crash classified as infrastructure error" "errored" \
  "$(say "[s['state'] for s in d['suites'] if s['suite']=='org.eclipse.egit.target.test'][0]")"
check "test failures classified as failed" "failed" \
  "$(say "[s['state'] for s in d['suites'] if s['suite']=='org.eclipse.egit.ui.test'][0]")"

echo "placement"
check "rcp_version=4.26 suite landed on the legacy node" "1" \
  "$(say "sum(1 for s in d['suites'] if s['suite']=='org.eclipse.egit.core.legacy.test' and 'legacy' in (s['node'] or ''))")"
check "every suite got a node" "9" \
  "$(say "sum(1 for s in d['suites'] if s['node'])")"

echo "artifacts"
RUN=$(say "[s['run_id'] for s in d['suites'] if s['suite']=='org.eclipse.egit.ui.test'][0]")
A=$(curl -sf "$MASTER/api/v1/runs/$RUN/artifacts")
has() { printf '%s' "$A" | python3 -c "import sys,json;print(int(any('$1' in a['path'] for a in json.load(sys.stdin)['artifacts'])))"; }
check "junit xml uploaded"    "1" "$(has 'TEST-')"
check "eclipse log uploaded"  "1" "$(has 'eclipse.log')"
check "runner log uploaded"   "1" "$(has 'dtp-runner.log')"
check "screenshots uploaded"  "1" "$(has 'screenshots/')"
check "artifact is fetchable through the master" "200" \
  "$(curl -s -o /dev/null -w '%{http_code}' "$MASTER/api/v1/runs/$RUN/artifacts/dtp-runner.log")"

echo
if [ "$fails" -eq 0 ]; then echo "all checks passed"; else echo "$fails check(s) failed"; fi
exit $((fails > 0))
