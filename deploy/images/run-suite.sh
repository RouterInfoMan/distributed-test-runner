#!/usr/bin/env bash
# run-suite.sh - launches one Eclipse RCP test suite inside a worker.
#
# dtp-runner has already populated the build cache and exported:
#   DTP_BUILD_<NAME>    unpacked build payloads, one per "build" entry
#   DTP_RESULTS_DIR     everything written here is uploaded
#   DTP_WORKSPACE       scratch directory private to this attempt
#   DTP_SUITE           the suite to run
#
# DTP_HARNESS (from the suite's env) picks how the suite is executed:
#   tycho     `mvn -o verify -pl <suite>` in a private copy of a Tycho reactor
#             payload - EGit, or any Eclipse project that builds with Tycho.
#             DTP_TYCHO_BUILD names the payload (default: the first one carrying
#             src/pom.xml); DTP_TYCHO_MODULE overrides the module (default: the
#             suite name); DTP_MAVEN_ARGS is appended to the Maven command line,
#             after whatever the payload's own mvn.args asks for.
#   eclipse   the Eclipse test framework inside an RCP product payload:
#             DTP_TEST_APPLICATION + DTP_BUILD_PRODUCT (+ DTP_TEST_CLASS).
#   fixture   the stand-in generator, so the stack is demoable with no build.
# Unset, it is inferred from what the payloads contain, in that order.
set -uo pipefail

RESULTS="${DTP_RESULTS_DIR:?}"
SUITE="${DTP_SUITE:?}"
WORK="${DTP_WORKSPACE:-$PWD}"

die() { echo "run-suite: $*" >&2; exit 3; }

# --- display ---------------------------------------------------------------
# One Xvfb per attempt. Slots of the process pool share a host, so the display
# number is probed rather than fixed; a container has the namespace to itself.
start_display() {
  local d
  for d in $(seq 99 149); do
    [ -e "/tmp/.X$d-lock" ] && continue
    Xvfb ":$d" -screen 0 1920x1080x24 -nolisten tcp >/dev/null 2>&1 &
    XVFB_PID=$!
    for _ in $(seq 1 20); do
      xdpyinfo -display ":$d" >/dev/null 2>&1 && break
      kill -0 "$XVFB_PID" 2>/dev/null || break
      sleep 0.25
    done
    if xdpyinfo -display ":$d" >/dev/null 2>&1; then
      export DISPLAY=":$d"
      trap 'kill $XVFB_PID $WM_PID ${MIRROR_PID:-} 2>/dev/null' EXIT
      # SWTBot needs a window manager for anything that moves, resizes or
      # focuses shells.
      WM_PID=
      if command -v metacity >/dev/null; then
        metacity --display="$DISPLAY" --sm-disable --replace >/dev/null 2>&1 &
        WM_PID=$!
      fi
      echo "display $DISPLAY (Xvfb pid $XVFB_PID${WM_PID:+, metacity pid $WM_PID})"
      return 0
    fi
    kill "$XVFB_PID" 2>/dev/null
  done
  die "could not start Xvfb on any display :99-:149"
}

# --- harness: tycho ----------------------------------------------------------
# A payload is a Tycho reactor when it carries src/pom.xml next to the Maven
# repository (m2/) it was built against - the layout scripts/build-egit.sh
# produces.
tycho_root() {
  local v name
  if [ -n "${DTP_TYCHO_BUILD:-}" ]; then
    name=$(printf '%s' "$DTP_TYCHO_BUILD" | tr -c 'A-Za-z0-9' '_' | tr 'a-z' 'A-Z')
    v="DTP_BUILD_$name"
    printf '%s' "${!v:-}"
    return
  fi
  for v in $(compgen -e | grep '^DTP_BUILD_' | sort); do
    if [ -f "${!v}/src/pom.xml" ]; then printf '%s' "${!v}"; return; fi
  done
}

run_tycho() {
  local root module tree status
  root="$(tycho_root)"
  [ -n "$root" ] && [ -f "$root/src/pom.xml" ] \
    || die "tycho harness: no reactor payload (expected DTP_BUILD_<name>/src/pom.xml)"
  [ -d "$root/m2" ] || die "tycho harness: $root has no m2/ repository"
  module="${DTP_TYCHO_MODULE:-$SUITE}"
  [ -f "$root/src/$module/pom.xml" ] || die "tycho harness: module $module is not in the reactor at $root/src"

  # The cache is shared by every slot on the node, so each attempt builds in a
  # private copy of the tree. The Maven repository stays shared: offline mode
  # only reads it.
  tree="$WORK/reactor"
  mkdir -p "$tree"
  cp -a "$root/src/." "$tree/"
  cat "$root/BUILD_INFO" 2>/dev/null || true

  start_display
  # Maven itself is small; the forked Eclipse runtime takes its heap from the
  # module's argLine. Under the docker driver the slot is a hard ceiling.
  export MAVEN_OPTS="${MAVEN_OPTS:--Xmx512m}"
  echo "slot: ${DTP_SLOT_CPU:-?}MHz ${DTP_SLOT_MEMORY_MB:-?}MB"

  # A payload may carry the flags its reactor needs (p2 sites it bundles, for
  # instance); {payload} expands to the unpacked root.
  local payload_args=""
  if [ -f "$root/mvn.args" ]; then
    payload_args=$(sed "s|{payload}|$root|g" "$root/mvn.args" | tr '\n' ' ')
    echo "payload args: $payload_args"
  fi

  # Surefire writes one XML per finished test class; mirroring them into the
  # results dir as they appear lets dtp-runner report progress mid-run.
  reports="$tree/$module/target/surefire-reports"
  ( while sleep 10; do cp "$reports"/TEST-*.xml "$RESULTS"/ 2>/dev/null; done ) &
  MIRROR_PID=$!

  # shellcheck disable=SC2086
  # dash.skip: the Eclipse license check phones home during verify; the
  # build already ran it, and a node has no network for it.
  mvn -B -ntp -o -f "$tree/pom.xml" -Dmaven.repo.local="$root/m2" \
      -pl "$module" verify \
      -Dmaven.test.failure.ignore=true -Dtycho.localArtifacts=default \
      -Dtycho.disableP2Mirrors=true -Ddash.skip=true $payload_args ${DTP_MAVEN_ARGS:-} 2>&1 | tee "$RESULTS/mvn.log"
  status=${PIPESTATUS[0]}
  kill "$MIRROR_PID" 2>/dev/null

  # Surefire XML is what the master rolls up; the workspace log and SWTBot's
  # screenshots are what a person reads when a suite goes red. SWTBot writes
  # screenshots relative to the test JVM's working directory, which under
  # Tycho is target/work, so every PNG under the module's build dir is taken.
  cp "$reports"/TEST-*.xml "$RESULTS"/ 2>/dev/null || true
  cp "$tree/$module"/target/work/data/.metadata/.log "$RESULTS/eclipse.log" 2>/dev/null || true
  # (SWTBot's default format is JPEG; PNG only when the suite asks for it.)
  find "$tree/$module" -type d -name screenshots -not -path '*/work/data/*' 2>/dev/null | while read -r dir; do
    find "$dir" -type f \( -name '*.png' -o -name '*.jpeg' -o -name '*.jpg' \) | while read -r shot; do
      mkdir -p "$RESULTS/screenshots"
      cp "$shot" "$RESULTS/screenshots/$(basename "$shot")"
    done
  done
  return "$status"
}

# --- harness: eclipse test framework ---------------------------------------
run_eclipse() {
  local eclipse workspace status
  [ -n "${DTP_TEST_APPLICATION:-}" ] && [ -n "${DTP_BUILD_PRODUCT:-}" ] \
    || die "eclipse harness needs DTP_TEST_APPLICATION and a build payload named product"
  eclipse="$DTP_BUILD_PRODUCT/eclipse"
  [ -x "$eclipse" ] || eclipse="$(find "$DTP_BUILD_PRODUCT" -maxdepth 3 -name eclipse -type f -perm -u+x | head -1)"
  [ -n "$eclipse" ] || die "no eclipse launcher under $DTP_BUILD_PRODUCT"

  start_display
  workspace="$WORK/eclipse-workspace"
  mkdir -p "$workspace"

  # The Eclipse test application writes surefire XML straight into the formatter target.
  "$eclipse" \
    -application "$DTP_TEST_APPLICATION" \
    -product "${DTP_PRODUCT_ID:-org.eclipse.platform.ide}" \
    -data "$workspace" \
    -testpluginname "$SUITE" \
    -classname "${DTP_TEST_CLASS:-$SUITE.AllTests}" \
    -consoleLog -nosplash \
    formatter=org.apache.tools.ant.taskdefs.optional.junit.XMLJUnitResultFormatter,"$RESULTS/TEST-$SUITE.xml" \
    -vmargs -Xmx2048m -Dorg.eclipse.swt.browser.DefaultType=webkit
  status=$?

  cp "$workspace/.metadata/.log" "$RESULTS/eclipse.log" 2>/dev/null || true
  cp -r "$workspace/screenshots" "$RESULTS/screenshots" 2>/dev/null || true
  return "$status"
}

# --- dispatch ---------------------------------------------------------------
harness="${DTP_HARNESS:-}"
if [ -z "$harness" ]; then
  if [ -n "$(tycho_root)" ]; then harness=tycho
  elif [ -n "${DTP_TEST_APPLICATION:-}" ] && [ -n "${DTP_BUILD_PRODUCT:-}" ]; then harness=eclipse
  else harness=fixture
  fi
fi
echo "harness: $harness  suite: $SUITE"
# Which build this is, as resolved by the master from the build repository.
for v in $(compgen -e | grep -E '^DTP_BUILD_.*_VERSION$' | sort); do
  id="${v%_VERSION}_ID"; echo "build: ${v#DTP_BUILD_} = ${!v} (${!id:-no id})"
done
case "$harness" in
  tycho)   run_tycho ;;
  eclipse) run_eclipse ;;
  fixture) exec /opt/dtp/rcp-suite.sh ;;
  *)       die "unknown DTP_HARNESS $harness (tycho|eclipse|fixture)" ;;
esac
