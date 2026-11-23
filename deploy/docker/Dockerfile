# mysqlite: multi-stage build, Debian 12 runtime (docs/07-testing.md section 2).
#
# Stage "build" holds the Go toolchain and a C compiler: the compose services that run
# `go test -race`, the linters and the harness use it. The runtime image holds only
# the static binary.

FROM golang:1.27.4-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mysqlite ./cmd/mysqlite \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mysqlite-corpus ./cmd/mysqlite-corpus

FROM debian:12-slim AS runtime
# A system user owns the data and log directories (0750: the server refuses a
# world-writable data directory, docs/06-transactions.md section 7).
RUN useradd --system --uid 10001 --user-group --home-dir /var/lib/mysqlite --shell /usr/sbin/nologin mysqlite \
 && install -d -o mysqlite -g mysqlite -m 0750 /var/lib/mysqlite /var/lib/mysqlite/data /var/lib/mysqlite/logs
COPY --from=build /out/mysqlite /usr/local/bin/mysqlite
COPY deploy/docker/mysqlite.yaml /etc/mysqlite/mysqlite.yaml
COPY LICENSE NOTICE /usr/share/doc/mysqlite/
USER mysqlite
WORKDIR /var/lib/mysqlite
VOLUME ["/var/lib/mysqlite/data"]
EXPOSE 3307
ENTRYPOINT ["/usr/local/bin/mysqlite", "--config", "/etc/mysqlite/mysqlite.yaml"]
