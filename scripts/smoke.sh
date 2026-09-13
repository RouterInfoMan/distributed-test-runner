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
  "$(say "[s['attempts'] for s in d['suites'] if s['suite']=='simulated.flaky.ui.test'][0]")"
check "crashing suite gave up after its retry" "2" \
  "$(say "[s['attempts'] for s in d['suites'] if s['suite']=='simulated.crash.test'][0]")"
check "crash classified as infrastructure error" "errored" \
  "$(say "[s['state'] for s in d['suites'] if s['suite']=='simulated.crash.test'][0]")"
check "test failures classified as failed" "failed" \
  "$(say "[s['state'] for s in d['suites'] if s['suite']=='simulated.ui.test'][0]")"

echo "placement"
check "dev-pool suites all landed on dev nodes" "$(say "sum(1 for s in d['suites'] if s['pool']=='dev-pool')")" \
  "$(say "sum(1 for s in d['suites'] if s['pool']=='dev-pool' and '-dev' in (s['node'] or ''))")"
check "every suite got a node" "9" \
  "$(say "sum(1 for s in d['suites'] if s['node'])")"
check "perf=high suite landed on the high-perf node" "1" \
  "$(say "sum(1 for s in d['suites'] if s['suite']=='simulated.ui.test' and '-hp' in (s['node'] or ''))")"
check "global suites all landed on nodes of the pools it spans" "$(say "sum(1 for s in d['suites'] if s['pool']=='global')")" \
  "$(say "sum(1 for s in d['suites'] if s['pool']=='global' and ('-hp' in (s['node'] or '') or '-mid' in (s['node'] or '')))")"

echo "quotas"
check "regression records the submitting user" "${USER}" "$(say "d['regression']['user']")"
check "an unlisted user is in no group" "0" "$(say "len(d['regression'].get('groups') or [])")"
# The everyone rule caps slots per user: no more than that many attempts may
# ever have been in flight at once (the cap comes from the live rule table).
Q=$(curl -sf "$MASTER/api/v1/quotas")
rules() { printf '%s' "$Q" | jqp "$1"; }
CAP=$(rules "[q['max_slots'] for q in d['quotas'] if q['key']=='global@global'][0]")
check "never more than $CAP attempts in flight (everyone rule)" "1" \
  "$(say "max(sum(1 for o in d['runs'] if o.get('dispatched_at') and o.get('finished_at') and o['dispatched_at'] <= r['dispatched_at'] < o['finished_at']) for r in d['runs'] if r.get('dispatched_at')) <= $CAP and 1 or 0")"
check "the everyone rule lists the user with nothing in use" "0" \
  "$(rules "[u['used'] for q in d['quotas'] if q['key']=='global@global' for u in q['users'] if u['user']=='${USER}'][0]")"
check "a user's own rule governs them under the everyone rule" "2 user:carol@global" \
  "$(rules "' '.join(str(x) for x in [(u['cap'],u.get('via')) for q in d['quotas'] if q['key']=='global@global' for u in q['users'] if u['user']=='carol'][0])")"
NODERULE=$(rules "[q for q in d['quotas'] if q['scope']=='node' and q['together']][0]['target']")
NODECAP=$(rules "[q for q in d['quotas'] if q['scope']=='node' and q['together']][0]['max_slots']")
check "never more than $NODECAP attempts at once on $NODERULE (node rule)" "1" \
  "$(say "max([sum(1 for o in d['runs'] if o.get('node_name')=='$NODERULE' and o.get('dispatched_at') and o.get('finished_at') and o['dispatched_at'] <= r['dispatched_at'] < o['finished_at']) for r in d['runs'] if r.get('dispatched_at') and r.get('node_name')=='$NODERULE'] or [0]) <= $NODECAP and 1 or 0")"
# A 0 rule refuses the submission outright, naming the rule.
check "a forbidden pool is refused at submission" "1" \
  "$($DTP submit examples/regression.json -user jenkins 2>&1 | grep -c 'user:jenkins@pool:dev-pool')"
# Rule writes are idempotent and validated.
$DTP quotas set global/together@pool:dev-pool 2 "smoke" >/dev/null
check "setting a rule twice is a no-op" "$(curl -sf "$MASTER/api/v1/config" | jqp "len(d['quotas'])")" \
  "$($DTP quotas set global/together@pool:dev-pool 2 "smoke" >/dev/null; curl -sf "$MASTER/api/v1/config" | jqp "len(d['quotas'])")"
check "a rule naming an unknown group is refused" "1" "$($DTP quotas set group:nope@global 1 2>&1 | grep -c 'unknown group')"
$DTP quotas rm global/together@pool:dev-pool >/dev/null
check "removing a rule twice is a no-op" "0" "$($DTP quotas rm global/together@pool:dev-pool >/dev/null; curl -sf "$MASTER/api/v1/config" | jqp "sum(1 for q in d['quotas'] if q['subject']=='global' and q.get('together') and q.get('target')=='dev-pool')")"

echo "nodes"
N=$(curl -sf "$MASTER/api/v1/nodes")
nodes() { printf '%s' "$N" | jqp "$1"; }
check "no node is unassigned" "0" "$(nodes "len(d['unassigned'])")"
check "every node declares a slot size" "$(nodes "sum(len(p['nodes'] or []) for p in d['pools'])")" \
  "$(nodes "sum(1 for p in d['pools'] for n in (p['nodes'] or []) if n['slot'].get('cores') or n['slot'].get('cpu'))")"
check "every run reserved its node's slot" "$(say "len(d['runs'])")" \
  "$(say "sum(1 for r in d['runs'] if r.get('node_id'))")"
# Assignment is idempotent: re-assigning a node to the pool it is in is a
# no-op (200, same catalog), and moving it out and back restores capacity.
NODE=$(nodes "[n['name'] for p in d['pools'] if p['name']=='dev-pool' for n in (p['nodes'] or [])][0]")
BEFORE=$(curl -sf "$MASTER/api/v1/config" | jqp "sorted((n['name'],n['pool']) for n in d['nodes'])")
check "re-assigning a node to its own pool is a no-op" "$BEFORE" \
  "$(curl -sf -X PUT "$MASTER/api/v1/config/nodes/$NODE" -H 'Content-Type: application/json' -d '{"pool":"dev-pool"}' | jqp "sorted((n['name'],n['pool']) for n in d['nodes'])")"
$DTP nodes assign "$NODE" >/dev/null
check "an unassigned node shows up as such" "1" "$(curl -sf "$MASTER/api/v1/nodes" | jqp "sum(1 for n in d['unassigned'] if n['name']=='$NODE')")"
$DTP nodes assign "$NODE" dev-pool >/dev/null
check "and comes back when re-assigned" "$BEFORE" "$(curl -sf "$MASTER/api/v1/config" | jqp "sorted((n['name'],n['pool']) for n in d['nodes'])")"

echo "artifacts"
RUN=$(say "[s['run_id'] for s in d['suites'] if s['suite']=='simulated.ui.test'][0]")
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
