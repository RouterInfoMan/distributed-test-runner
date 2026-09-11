# syntax=docker/dockerfile:1
#
# A Nomad client for the "process" pool. The official nomad image is distroless,
# so the agent binary is layered onto a JDK base instead: a process-pool node is
# a provisioned machine that already carries the RCP runtime dependencies, and
# the task launches dtp-runner directly in its own task dir + cgroup.
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

COPY --from=hashicorp/nomad:1.10 /bin/nomad /usr/local/bin/nomad
COPY --from=build /out/dtp-runner /usr/local/bin/dtp-runner
COPY examples/fixtures/rcp-suite.sh /opt/dtp/rcp-suite.sh
COPY deploy/images/run-suite.sh     /opt/dtp/run-suite.sh
RUN chmod +x /opt/dtp/*.sh /usr/local/bin/dtp-runner /usr/local/bin/nomad \
 && mkdir -p /var/lib/dtp/cache

ENV DISPLAY=:99
ENTRYPOINT ["/usr/local/bin/nomad"]
