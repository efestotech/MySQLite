#!/usr/bin/env bash
# Developer entrypoint (docs/09-conventions.md section 6): wraps `docker compose` with
# the same verbs as scripts/dev.ps1. Needs only Docker; no environment variables.
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
compose=(docker compose -f "$root/deploy/docker/compose.yaml")

usage() {
	cat <<'EOF'
usage: scripts/dev.sh VERB

  up       build and start mysqlite (port 3307) and mysql-ref (port 3306)
  down     stop and remove every container
  build    build the images
  test     go vet and go test -race in the toolchain container
  harness  run the differential harness (mysql-ref against mysqlite)
  clients  run the client matrix scenarios (mysql 8.0, 8.4, MariaDB 11, PHP PDO,
           Adminer, Connector/J)
  lint     golangci-lint
  shell    a shell in the toolchain container
  logs     follow the mysqlite logs
EOF
}

verb="${1:-}"
case "$verb" in
up) "${compose[@]}" up -d --build mysqlite mysql-ref ;;
down) "${compose[@]}" --profile tools --profile clients down ;;
build) "${compose[@]}" --profile tools --profile clients build ;;
test) "${compose[@]}" run --rm dev sh -c 'go vet ./... && go test -race -count=1 ./...' ;;
harness)
	"${compose[@]}" up -d --build mysqlite mysql-ref
	"${compose[@]}" run --rm runner
	;;
clients)
	"${compose[@]}" up -d --build mysqlite adminer
	for s in cli80 cli84 cli-maria php java adminer-smoke; do
		echo "== $s"
		"${compose[@]}" run --rm --build "$s"
	done
	;;
lint) "${compose[@]}" run --rm lint ;;
shell) "${compose[@]}" run --rm dev bash ;;
logs) "${compose[@]}" logs -f mysqlite ;;
*)
	usage >&2
	exit 2
	;;
esac
