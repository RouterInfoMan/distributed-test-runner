#!/usr/bin/env bash
# Builds EGit from source with Tycho and publishes the result as a build
# payload, then prints the environment examples/egit.json expects:
#
#   ./scripts/build-egit.sh            # publishes the payload and its manifest to the build repository
#   ./bin/dtp submit examples/egit.json -w   # "id": "latest" picks it up; or pin BUILD_ID from the output
#
# The build runs inside the worker image, so the JDK and Maven that compiled the
# tree are the ones that later run its suites. The payload is the checked-out
# tree, the Maven repository it resolved against (target platform, Tycho, the
# surefire OSGi runtime), the JGit p2 repository, and an mvn.args file with the
# flags the reactor needs; on a node, run-suite.sh runs
# `mvn -o verify -pl <suite>` from a private copy of the tree, so nothing is
# downloaded at test time. Every suite names the same sha256, so a node fetches
# the payload once and reuses it.
#
#   EGIT_REF=<tag>         tag or branch to build (default: the newest release)
#   JGIT_SITE=<url>        JGit p2 repository the tree builds against: a p2 site
#                          URL, or the URL of a p2 repository archive (.zip),
#                          which is how JGit releases are published. Derived
#                          from a release tag; required for any other ref.
#   EGIT_REPO=<url>        git remote (default github.com/eclipse-egit/egit)
#   EGIT_FRESH=1           discard the previous checkout and Maven repository
#   EGIT_SKIP_UPLOAD=1     build only; BUILD_URL becomes a file:// path
set -euo pipefail
DEFAULT_REF=v7.8.0.202609011348-r

# ---- inside the worker container -------------------------------------------
if [ "${1:-}" = "--inside" ]; then
  cd /w
  REF="${EGIT_REF:?}"; REPO="${EGIT_REPO:?}"; JGIT_SITE="${JGIT_SITE:?}"
  if [ "${EGIT_FRESH:-0}" = 1 ] || [ ! -d src/.git ]; then
    rm -rf src
    git clone --depth 1 --branch "$REF" "$REPO" src
  else
    git -C src fetch --depth 1 origin "$REF"
    git -C src checkout -q --detach FETCH_HEAD
  fi
  COMMIT=$(git -C src rev-parse --short=10 HEAD)
  echo "== egit $REF @ $COMMIT"

  # Test modules are what the platform distributes; report them so the
  # submission can be kept in step with the tree.
  MODULES=$(grep -oP '<module>\K[^<]+' src/pom.xml | grep '\.test$' | tr '\n' ' ')
  echo "== test modules: $MODULES"

  # The pom expects a sibling JGit checkout; jgit-site redirects it to a
  # published p2 repository instead. JGit ships its repository as a zip on
  # repo.eclipse.org, so an archive URL is unpacked into a local p2 site.
  if [[ "$JGIT_SITE" == *.zip ]]; then
    if [ ! -f jgit-site/.source ] || [ "$(cat jgit-site/.source)" != "$JGIT_SITE" ]; then
      rm -rf jgit-site && mkdir jgit-site
      echo "== fetching JGit p2 repository $JGIT_SITE"
      curl -fsSL "$JGIT_SITE" -o jgit-site.zip && unzip -q jgit-site.zip -d jgit-site && rm jgit-site.zip
      echo "$JGIT_SITE" > jgit-site/.source
    fi
    JGIT_SITE="file:/w/jgit-site"
  fi
  export MAVEN_OPTS="${MAVEN_OPTS:--Xmx2g}"
  MVN=(mvn -B -ntp -Dmaven.repo.local=/w/m2 -f src/pom.xml "-Djgit-site=$JGIT_SITE")

  echo "== compiling (tests included, not executed)"
  "${MVN[@]}" -DskipTests install

  # Tycho fetches its surefire OSGi booter and the JUnit provider the first
  # time a test actually executes, so run one headless and one UI test class to
  # pull them into the repository; their verdicts do not matter here.
  first_test() { find "src/$1/src" -name '*Test.java' -not -name 'Abstract*' -printf '%f\n' | sort | sed -n '1s/\.java$//p'; }
  CORE_T=$(first_test org.eclipse.egit.core.test); UI_T=$(first_test org.eclipse.egit.ui.test)
  Xvfb :99 -screen 0 1280x1024x24 -nolisten tcp >/dev/null 2>&1 &
  export DISPLAY=:99
  sleep 1; command -v metacity >/dev/null && metacity --display=:99 --sm-disable --replace >/dev/null 2>&1 &
  echo "== warming the test runtime ($CORE_T, $UI_T)"
  "${MVN[@]}" -pl org.eclipse.egit.core.test verify -Dtest="$CORE_T" -DfailIfNoTests=false -Dmaven.test.failure.ignore=true
  "${MVN[@]}" -pl org.eclipse.egit.ui.test   verify -Dtest="$UI_T"   -DfailIfNoTests=false -Dmaven.test.failure.ignore=true

  # The nodes run offline; prove the repository is complete before shipping it.
  echo "== offline check"
  "${MVN[@]}" -o -pl org.eclipse.egit.core.test verify -Dtest="$CORE_T" -DfailIfNoTests=false \
      -Dmaven.test.failure.ignore=true -Ddash.skip=true

  echo "== packaging"
  find src -type d -name target -prune -exec rm -rf {} +
  find m2 -name '*.lastUpdated' -delete
  NAME="egit-$COMMIT"
  printf 'egit ref=%s commit=%s built=%s\ntest modules: %s\n' \
    "$REF" "$(git -C src rev-parse HEAD)" "$(date -u +%FT%TZ)" "$MODULES" > BUILD_INFO
  # Flags the reactor needs on a node; run-suite.sh appends them, expanding
  # {payload} to wherever the payload was unpacked.
  EXTRA=(src m2 BUILD_INFO mvn.args)
  if [[ "$JGIT_SITE" == file:/w/jgit-site ]]; then
    echo "-Djgit-site=file:{payload}/jgit-site" > mvn.args
    EXTRA+=(jgit-site)
  else
    echo "-Djgit-site=$JGIT_SITE" > mvn.args
  fi
  rm -f "$NAME.tar.gz"
  tar -I pigz -cf "$NAME.tar.gz" --transform "s,^,$NAME/," "${EXTRA[@]}"
  SHA=$(sha256sum "$NAME.tar.gz" | awk '{print $1}')
  # The manifest is what the master reads from the build repository: which
  # product and version this is, how to fetch and unpack it, what it can run.
  # "v7.8.0.202609011348-r" -> version 7.8.0; other refs get 0.0.0-<commit>.
  if [[ "$REF" =~ ^v([0-9]+\.[0-9]+\.[0-9]+) ]]; then VERSION="${BASH_REMATCH[1]}"; else VERSION="0.0.0-$COMMIT"; fi
  SUITES=$(printf '%s\n' $MODULES | sed 's/.*/"&"/' | paste -sd, -)
  cat > "$NAME.json" <<JSON
{
  "id": "$NAME",
  "product": "egit",
  "version": "$VERSION",
  "ref": "$REF",
  "commit": "$(git -C src rev-parse HEAD)",
  "built": "$(date -u +%FT%TZ)",
  "url": "s3://${BUILDS_BUCKET:-builds}/$NAME.tar.gz",
  "sha256": "$SHA",
  "size": $(stat -c %s "$NAME.tar.gz"),
  "unpack": "tar.gz",
  "harness": "tycho",
  "suites": [$SUITES]
}
JSON
  echo "$NAME" > LATEST
  echo "== $NAME.tar.gz ($(du -h "$NAME.tar.gz" | cut -f1)) version $VERSION, manifest $NAME.json"
  exit 0
fi

# ---- on the host -------------------------------------------------------------
cd "$(dirname "$0")/.."
REF="${EGIT_REF:-$DEFAULT_REF}"
REPO="${EGIT_REPO:-https://github.com/eclipse-egit/egit.git}"
if [ -z "${JGIT_SITE:-}" ]; then
  # JGit and EGit releases share a version string, and JGit publishes its p2
  # repository (test-support bundles included) as a Maven artifact.
  if [[ "$REF" =~ ^v([0-9]+\.[0-9]+\.[0-9]+\.[0-9]+-[a-z0-9]+)$ ]]; then
    V="${BASH_REMATCH[1]}"
    JGIT_SITE="https://repo.eclipse.org/content/repositories/jgit-releases/org/eclipse/jgit/org.eclipse.jgit.repository/$V/org.eclipse.jgit.repository-$V.zip"
  else
    echo "build-egit: $REF is not a release tag; set JGIT_SITE to the JGit p2 repository it builds against" >&2
    exit 2
  fi
fi
IMAGE="${DTP_RUNNER_IMAGE:-dtp/rcp-runner:dev}"
OUT="$PWD/.dtp/egit"
mkdir -p "$OUT"

if ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
  echo "# building $IMAGE" >&2
  docker build -q -f deploy/images/rcp-runner.Dockerfile -t "$IMAGE" . >&2
fi

docker run --rm -u "$(id -u):$(id -g)" -e HOME=/w \
  -e EGIT_REF="$REF" -e EGIT_REPO="$REPO" -e EGIT_FRESH="${EGIT_FRESH:-0}" -e JGIT_SITE="$JGIT_SITE" \
  -e MAVEN_OPTS="${MAVEN_OPTS:-}" -e BUILDS_BUCKET="${BUILDS_BUCKET:-builds}" \
  -v "$OUT:/w" -v "$PWD/scripts/build-egit.sh:/w/build.sh:ro" \
  --entrypoint bash "$IMAGE" /w/build.sh --inside >&2

NAME=$(cat "$OUT/LATEST")
TAR="$OUT/$NAME.tar.gz"
SHA=$(grep -o '"sha256": "[0-9a-f]*"' "$OUT/$NAME.json" | cut -d'"' -f4)
BUCKET="${BUILDS_BUCKET:-builds}"

if [ "${EGIT_SKIP_UPLOAD:-0}" = 1 ]; then
  echo "export BUILD_URL=file://$TAR"
else
  # Upload payload + manifest through mc, on the compose network when the
  # stack is up. The manifest is what makes the build visible to the master:
  # dtp builds, the dashboard, and "id": "latest" in a submission.
  NET_ARGS=(--network host); ENDPOINT="http://127.0.0.1:9000"
  if docker network inspect dtp_default >/dev/null 2>&1; then
    NET_ARGS=(--network dtp_default); ENDPOINT="http://minio:9000"
  fi
  docker run --rm "${NET_ARGS[@]}" -v "$OUT:/w" --entrypoint sh quay.io/minio/mc -c "
    mc alias set m $ENDPOINT dtpadmin dtpadmin123 >/dev/null &&
    mc mb -p m/$BUCKET >/dev/null 2>&1;
    mc cp /w/$NAME.tar.gz m/$BUCKET/ >/dev/null && mc cp /w/$NAME.json m/$BUCKET/ >/dev/null" >&2
  echo "# published s3://$BUCKET/$NAME.tar.gz + $NAME.json ($(du -h "$TAR" | cut -f1), sha256 ${SHA:0:12}…)" >&2
  echo "export BUILD_URL=s3://$BUCKET/$NAME.tar.gz"
fi
echo "export BUILD_SHA256=$SHA"
echo "export BUILD_ID=$NAME"
