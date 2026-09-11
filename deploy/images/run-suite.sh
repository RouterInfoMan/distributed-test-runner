#!/usr/bin/env bash
# run-suite.sh - launches one Eclipse RCP test suite inside a worker.
#
# dtp-runner has already populated the build cache and exported:
#   DTP_BUILD_PRODUCT   unpacked RCP product (when the submission declares it)
#   DTP_BUILD_TESTS     unpacked test bundles
#   DTP_RESULTS_DIR     everything written here is uploaded
#   DTP_SUITE           the suite to run
#
# With no product payload it falls back to the fixture generator, so the stack
# is demoable before you have a build to point it at.
set -uo pipefail

RESULTS="${DTP_RESULTS_DIR:?}"
SUITE="${DTP_SUITE:?}"

# The real Eclipse path is taken only when the submission names a test
# application, because that is the thing a build alone cannot imply. Everything
# else - including a submission that carries a build payload, which is how the
# node build cache gets exercised - falls through to the fixture.
if [ -z "${DTP_TEST_APPLICATION:-}" ] || [ -z "${DTP_BUILD_PRODUCT:-}" ]; then
  echo "DTP_TEST_APPLICATION unset - running the fixture suite"
  exec /opt/dtp/rcp-suite.sh
fi

# --- real Eclipse RCP invocation ------------------------------------------
# Start a virtual framebuffer for SWT/SWTBot; a window manager is required for
# anything that moves, resizes or focuses shells.
export DISPLAY="${DISPLAY:-:99}"
Xvfb "$DISPLAY" -screen 0 1920x1080x24 -nolisten tcp &
XVFB_PID=$!
trap 'kill $XVFB_PID 2>/dev/null' EXIT
for i in $(seq 1 30); do xdpyinfo -display "$DISPLAY" >/dev/null 2>&1 && break; sleep 0.5; done
# A window manager is required for anything that moves, resizes or focuses
# shells. Absent from the slim PoC image; install metacity for real UI suites.
command -v metacity >/dev/null && metacity --display="$DISPLAY" --sm-disable --replace >/dev/null 2>&1 &

ECLIPSE="$DTP_BUILD_PRODUCT/eclipse"
[ -x "$ECLIPSE" ] || ECLIPSE="$(find "$DTP_BUILD_PRODUCT" -maxdepth 3 -name eclipse -type f -perm -u+x | head -1)"

WORKSPACE="${DTP_WORKSPACE:-$PWD}/eclipse-workspace"
mkdir -p "$WORKSPACE"

# The Eclipse test application writes surefire XML straight into -Ddata.dir.
"$ECLIPSE" \
  -application "$DTP_TEST_APPLICATION" \
  -product "${DTP_PRODUCT_ID:-org.eclipse.platform.ide}" \
  -data "$WORKSPACE" \
  -testpluginname "$SUITE" \
  -classname "${DTP_TEST_CLASS:-$SUITE.AllTests}" \
  -consoleLog -nosplash \
  formatter=org.apache.tools.ant.taskdefs.optional.junit.XMLJUnitResultFormatter,"$RESULTS/TEST-$SUITE.xml" \
  -vmargs -Xmx2048m -Dorg.eclipse.swt.browser.DefaultType=webkit
STATUS=$?

# The workspace log is the first thing anyone looks at when a suite dies.
cp "$WORKSPACE/.metadata/.log" "$RESULTS/eclipse.log" 2>/dev/null || true
cp -r "$WORKSPACE/screenshots" "$RESULTS/screenshots" 2>/dev/null || true
exit $STATUS
