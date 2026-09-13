# syntax=docker/dockerfile:1
#
# A Nomad client for the "process" pools. The official nomad image is distroless,
# so the agent binary is layered onto the same JDK/Maven/GTK base as the
# container worker: a process-pool node is a provisioned machine that already
# carries the RCP runtime dependencies, and the task launches dtp-runner directly
# in its own task dir + cgroup.
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

COPY --from=hashicorp/nomad:1.10 /bin/nomad /usr/local/bin/nomad
COPY --from=build /out/dtp-runner /usr/local/bin/dtp-runner
COPY examples/fixtures/rcp-suite.sh /opt/dtp/rcp-suite.sh
COPY examples/fixtures/Screenshot.java /opt/dtp/Screenshot.java
COPY deploy/images/run-suite.sh     /opt/dtp/run-suite.sh
RUN chmod +x /opt/dtp/*.sh /usr/local/bin/dtp-runner /usr/local/bin/nomad \
 && mkdir -p /var/lib/dtp/cache

ENV DISPLAY=:99
ENTRYPOINT ["/usr/local/bin/nomad"]
