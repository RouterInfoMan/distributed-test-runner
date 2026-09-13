#!/usr/bin/env bash
# rcp-suite.sh - SIMULATED suite: a stand-in for a headless Eclipse RCP /
# SWTBot suite that runs no real tests.
#
# It produces the same shape of output as the real thing - surefire JUnit XML,
# an Eclipse .log, a rendered "failure screenshot" per failing case (watermarked
# SIMULATED) - so the platform can be exercised end to end without a product
# build: examples/regression.json and scripts/smoke.sh use it. Real suites run
# through run-suite.sh (tycho / eclipse harnesses); see examples/egit.json.
#
# Contract with dtp-runner (all supplied via the RunSpec):
#   DTP_SUITE        suite name, used as the JUnit testsuite name
#   DTP_RESULTS_DIR  everything written here is uploaded to the object store
#   DTP_ATTEMPT      1-based attempt number (used to simulate flakiness)
#   DTP_BUILD_*      unpacked build payloads from the node's cache
#
# Fixture knobs (set per suite in the submission JSON's "env"):
#   FIXTURE_TESTS     number of test cases            (default 24)
#   FIXTURE_FAIL      number of cases that fail       (default 0)
#   FIXTURE_SKIP      number of cases skipped         (default 0)
#   FIXTURE_SECONDS   wall-clock duration of the run  (default 20)
#   FIXTURE_FLAKY     fail only on attempt 1          (default 0)
#   FIXTURE_CRASH     exit non-zero without results   (default 0)
set -uo pipefail
# awk's %f honours the locale; surefire XML wants a decimal point.
export LC_ALL=C

SUITE="${DTP_SUITE:-simulated.example.test}"
RESULTS="${DTP_RESULTS_DIR:-./results}"
ATTEMPT="${DTP_ATTEMPT:-1}"
TESTS="${FIXTURE_TESTS:-24}"
FAILS="${FIXTURE_FAIL:-0}"
SKIPS="${FIXTURE_SKIP:-0}"
SECONDS_TOTAL="${FIXTURE_SECONDS:-20}"
FLAKY="${FIXTURE_FLAKY:-0}"
CRASH="${FIXTURE_CRASH:-0}"

mkdir -p "$RESULTS" "$RESULTS/screenshots"
LOG="$RESULTS/eclipse.log"
SHOT_JAVA="${FIXTURE_SCREENSHOT_JAVA:-$(dirname "$0")/Screenshot.java}"

log() { printf '!ENTRY org.eclipse.dtp 1 0 %s\n!MESSAGE %s\n' "$(date -u +%FT%TZ)" "$*" >>"$LOG"; }

log "Eclipse RCP test harness starting"
log "suite=$SUITE attempt=$ATTEMPT node=${DTP_NODE_NAME:-unknown} pid=$$"
log "java=$(command -v java >/dev/null && java -version 2>&1 | head -1 || echo 'not present')"

# Report the build payloads the runner resolved from the node cache, which is
# how you verify the cache is actually being reused across suites.
for v in $(compgen -e | grep '^DTP_BUILD_' || true); do
  log "build payload $v -> ${!v}"
  [ -d "${!v}" ] && log "  contents: $(ls "${!v}" 2>/dev/null | head -5 | tr '\n' ' ')"
done

if [ "$CRASH" != "0" ]; then
  log "SIMULATED HARNESS CRASH: product failed to start (org.eclipse.swt.SWTError: No more handles)"
  echo "!SESSION CRASHED - no test results produced" >&2
  exit 42
fi

# A flaky suite fails its first attempt and passes on retry.
if [ "$FLAKY" != "0" ] && [ "$ATTEMPT" -gt 1 ]; then
  FAILS=0
  log "retry attempt $ATTEMPT: previously flaky cases are expected to pass"
fi

PER_TEST=$(awk -v t="$SECONDS_TOTAL" -v n="$TESTS" 'BEGIN{ printf "%.3f", (n>0? t/n : 0) }')

XML="$RESULTS/TEST-$SUITE.xml"
CASES_FILE="$(mktemp)"
failed=0; skipped=0; passed=0

# A tiny valid PNG, stamped out as the "screenshot on failure" an SWTBot
# listener would capture.
PNG_B64='iVBORw0KGgoAAAANSUhEUgAAAAgAAAAIAQMAAAD+wSzIAAAABlBMVEX///+/v7+jQ3Y5AAAADklEQVQI12P4AIX8EAgALgAD/aNpbtEAAAAASUVORK5CYII='

for i in $(seq 1 "$TESTS"); do
  name=$(printf 'test%03d_%s' "$i" "$(shuf -n1 -e shouldOpenPerspective commitStagedChanges resolvesConflictMarkers \
      refreshesDecorations honoursPreferenceStore clonesRemoteRepository opensCompareEditor \
      fetchesUpstreamRefs rebasesOntoOrigin validatesInputDialog 2>/dev/null || echo case)")
  # simulated.ui.test -> simulated.ui.UiTest
  base="${SUITE%.test}"; leaf="${base##*.}"
  cls="$base.$(printf '%s' "${leaf^}")Test"
  sleep "$PER_TEST"

  if [ "$i" -le "$FAILS" ]; then
    failed=$((failed+1))
    shot="screenshots/${cls}.${name}.png"
    # A rendered mock dialog naming the test and its assertion (needs a JDK,
    # which every worker image has); a 1-pixel PNG otherwise.
    if command -v java >/dev/null && [ -f "$SHOT_JAVA" ]; then
      java -Djava.awt.headless=true "$SHOT_JAVA" "$RESULTS/$shot" "Eclipse - $SUITE" "$cls.$name" \
        "java.lang.AssertionError: expected:<committed> but was:<conflicting>" \
        "at $cls.$name(${cls##*.}.java:$((100+i)))" "node ${DTP_NODE_NAME:-unknown}  attempt $ATTEMPT" 2>/dev/null \
        || echo "$PNG_B64" | base64 -d > "$RESULTS/$shot" 2>/dev/null || : >"$RESULTS/$shot"
    else
      echo "$PNG_B64" | base64 -d > "$RESULTS/$shot" 2>/dev/null || : >"$RESULTS/$shot"
    fi
    log "FAILED $cls.$name - see $shot"
    {
      printf '    <testcase classname="%s" name="%s" time="%s">\n' "$cls" "$name" "$PER_TEST"
      printf '      <failure message="%s" type="java.lang.AssertionError">' \
        "expected:&lt;committed&gt; but was:&lt;conflicting&gt;"
      printf 'java.lang.AssertionError: expected:&lt;committed&gt; but was:&lt;conflicting&gt;\n'
      printf '\tat org.junit.Assert.fail(Assert.java:89)\n'
      printf '\tat %s.%s(%s.java:%d)\n' "$cls" "$name" "${cls##*.}" $((100+i))
      printf '\tat org.eclipse.swtbot.swt.finder.junit.SWTBotJunit4ClassRunner.run(SWTBotJunit4ClassRunner.java:62)\n'
      printf '      </failure>\n      <system-out>screenshot: %s</system-out>\n    </testcase>\n' "$shot"
    } >>"$CASES_FILE"
  elif [ "$i" -gt $((TESTS - SKIPS)) ]; then
    skipped=$((skipped+1))
    printf '    <testcase classname="%s" name="%s" time="0"><skipped message="requires display"/></testcase>\n' \
      "$cls" "$name" >>"$CASES_FILE"
  else
    passed=$((passed+1))
    printf '    <testcase classname="%s" name="%s" time="%s"/>\n' "$cls" "$name" "$PER_TEST" >>"$CASES_FILE"
  fi
done

{
  printf '<?xml version="1.0" encoding="UTF-8"?>\n'
  printf '<testsuites>\n  <testsuite name="%s" tests="%d" failures="%d" errors="0" skipped="%d" time="%s">\n' \
    "$SUITE" "$TESTS" "$failed" "$skipped" "$SECONDS_TOTAL"
  printf '    <properties>\n'
  printf '      <property name="dtp.attempt" value="%s"/>\n' "$ATTEMPT"
  printf '      <property name="dtp.node" value="%s"/>\n' "${DTP_NODE_NAME:-unknown}"
  printf '    </properties>\n'
  cat "$CASES_FILE"
  printf '  </testsuite>\n</testsuites>\n'
} > "$XML"
rm -f "$CASES_FILE"

log "suite finished: $passed passed, $failed failed, $skipped skipped"
echo "[$SUITE] $passed passed, $failed failed, $skipped skipped (attempt $ATTEMPT)"

[ "$failed" -gt 0 ] && exit 1
exit 0
