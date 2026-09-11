# syntax=docker/dockerfile:1
#
# The container-pool worker image: a JDK plus the virtual framebuffer and
# GTK/SWT runtime an Eclipse RCP product needs, with dtp-runner as the
# entrypoint. Nomad's docker driver starts this image; dtp-runner resolves the
# build from the node cache, runs the suite, uploads the results and reports.
#
# For a real RCP product also install:
#   libwebkit2gtk-4.0-37    the SWT Browser widget / internal web browser
#   metacity (or another WM) anything that moves, resizes or focuses shells
#   libcanberra-gtk-module dbus-x11
# They are left out here only to keep the PoC image small. The apt line is kept
# byte-identical to deploy/images/nomad-client.Dockerfile so both worker images
# share one cached layer.
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -o /out/dtp-runner ./cmd/dtp-runner

FROM eclipse-temurin:17-jdk-jammy
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
      bash coreutils curl ca-certificates unzip \
      xvfb x11-utils xauth libgtk-3-0 fonts-dejavu-core \
 && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/dtp-runner /usr/local/bin/dtp-runner
COPY examples/fixtures/rcp-suite.sh /opt/dtp/rcp-suite.sh
COPY deploy/images/run-suite.sh     /opt/dtp/run-suite.sh
RUN chmod +x /opt/dtp/*.sh /usr/local/bin/dtp-runner && mkdir -p /var/lib/dtp/cache

ENV DISPLAY=:99
ENTRYPOINT ["/usr/local/bin/dtp-runner"]
