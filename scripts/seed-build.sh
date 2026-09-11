#!/usr/bin/env bash
# Publishes a stand-in "RCP product" build to the object store and prints the
# environment that examples/regression-with-build.json expects:
#
#   source <(./scripts/seed-build.sh)
#   ./bin/dtp submit examples/regression-with-build.json -w
#
# Every suite in that submission names the same sha256, so the first suite on a
# node downloads and unpacks it and the rest hit the node's cache.
set -euo pipefail
cd "$(dirname "$0")/.."
OUT=.dtp/build
mkdir -p "$OUT"

# A minimal tree shaped like an unpacked Eclipse product.
PROD="$OUT/rcp-4.30-linux"
rm -rf "$PROD"; mkdir -p "$PROD"/{plugins,features,configuration}
cat > "$PROD/eclipse" <<'LAUNCH'
#!/bin/sh
echo "eclipse launcher stub: $*"
LAUNCH
chmod +x "$PROD/eclipse"
echo "eclipse.buildId=4.30.0" > "$PROD/configuration/config.ini"
for p in org.eclipse.egit.core org.eclipse.egit.ui org.eclipse.jgit org.eclipse.swtbot.swt.finder; do
  echo "stub bundle $p" > "$PROD/plugins/$p.jar"
done
# Pad it out so a cache hit is visibly faster than a fetch.
head -c 8000000 /dev/urandom > "$PROD/plugins/org.eclipse.platform.resources.bin"

TAR=".dtp/build/rcp-4.30-linux.tar.gz"
tar czf "$TAR" -C "$OUT" "$(basename "$PROD")"
SHA=$(sha256sum "$TAR" | awk '{print $1}')

# Upload through mc, on the compose network when the stack is up.
NET_ARGS=(--network host)
ENDPOINT="http://127.0.0.1:9000"
if docker network inspect dtp_default >/dev/null 2>&1; then
  NET_ARGS=(--network dtp_default)
  ENDPOINT="http://minio:9000"
fi
docker run --rm "${NET_ARGS[@]}" -v "$PWD/.dtp/build:/w" --entrypoint sh minio/mc -c "
  mc alias set m $ENDPOINT dtpadmin dtpadmin123 >/dev/null &&
  mc mb -p m/builds >/dev/null 2>&1;
  mc cp /w/rcp-4.30-linux.tar.gz m/builds/ >/dev/null" 1>&2

echo "# seeded s3://builds/rcp-4.30-linux.tar.gz ($(du -h "$TAR" | cut -f1), sha256 ${SHA:0:12}…)" 1>&2
echo "export BUILD_URL=s3://builds/rcp-4.30-linux.tar.gz"
echo "export BUILD_SHA256=$SHA"
