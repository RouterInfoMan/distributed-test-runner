# syntax=docker/dockerfile:1
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -o /out/dtp-master ./cmd/dtp-master \
 && CGO_ENABLED=0 go build -trimpath -o /out/dtp        ./cmd/dtp

FROM alpine:3.20
RUN apk add --no-cache ca-certificates curl
COPY --from=build /out/dtp-master /usr/local/bin/dtp-master
COPY --from=build /out/dtp        /usr/local/bin/dtp
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/dtp-master"]
