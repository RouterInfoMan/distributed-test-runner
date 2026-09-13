# syntax=docker/dockerfile:1
#
# The container-pool worker image: a JDK, Maven, the virtual framebuffer plus a
# window manager, and the GTK/WebKit runtime an Eclipse RCP product needs, with
# dtp-runner as the entrypoint. Nomad's docker driver starts this image;
# dtp-runner resolves the build from the node cache, runs the suite, uploads the
# results and reports.
#
# The same image also builds Tycho payloads (scripts/build-egit.sh), so the
# toolchain that compiled a tree is the one that later runs its suites. The apt
# and Maven lines are kept byte-identical to deploy/images/nomad-client.Dockerfile
# so both worker images share the cached layers.
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -o /out/dtp-runner ./cmd/dtp-runner

FROM eclipse-temurin:21-jdk-jammy
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
      bash coreutils curl ca-certificates unzip git pigz \
      xvfb x11-utils xauth metacity dbus-x11 \
      libgtk-3-0 libwebkit2gtk-4.0-37 libcanberra-gtk3-module fonts-dejavu-core \
 && rm -rf /var/lib/apt/lists/*
ARG MAVEN_VERSION=3.9.9
RUN curl -fsSL "https://archive.apache.org/dist/maven/maven-3/${MAVEN_VERSION}/binaries/apache-maven-${MAVEN_VERSION}-bin.tar.gz" \
      | tar xz -C /opt && ln -s "/opt/apache-maven-${MAVEN_VERSION}" /opt/maven
ENV PATH=/opt/maven/bin:$PATH

COPY --from=build /out/dtp-runner /usr/local/bin/dtp-runner
COPY examples/fixtures/rcp-suite.sh /opt/dtp/rcp-suite.sh
COPY examples/fixtures/Screenshot.java /opt/dtp/Screenshot.java
COPY deploy/images/run-suite.sh     /opt/dtp/run-suite.sh
RUN chmod +x /opt/dtp/*.sh /usr/local/bin/dtp-runner && mkdir -p /var/lib/dtp/cache

ENV DISPLAY=:99
ENTRYPOINT ["/usr/local/bin/dtp-runner"]
