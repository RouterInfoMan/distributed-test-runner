# syntax=docker/dockerfile:1
#
# The master image also carries dtp-runner (served to node agents at
# GET /api/v1/runner) and dtp-node (the node agent, used by the compose
# sidecars; on a real node it runs as a service next to the Nomad client).
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -o /out/ ./cmd/...

FROM alpine:3.20
RUN apk add --no-cache ca-certificates curl
COPY --from=build /out/dtp-master /out/dtp /out/dtp-runner /out/dtp-node /usr/local/bin/
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/dtp-master"]
