#!/usr/bin/env bash
# Runs the PDO scenario twice, with native and with emulated prepares (docs/00-brief.md
# section 5: "PHP PDO (emulated + native prepares)"). Connection settings come from the
# MYSQLITE_* variables the compose service sets.
set -euo pipefail
here="$(dirname "$0")"
php --version | head -1

# The server container may still be starting.
for i in $(seq 1 30); do
	if php -r 'new PDO("mysql:host=".getenv("MYSQLITE_HOST").";port=".getenv("MYSQLITE_PORT"), getenv("MYSQLITE_USER"), getenv("MYSQLITE_PASSWORD"));' 2>/dev/null; then break; fi
	sleep 1
done
php -r '$p = new PDO("mysql:host=".getenv("MYSQLITE_HOST").";port=".getenv("MYSQLITE_PORT"), getenv("MYSQLITE_USER"), getenv("MYSQLITE_PASSWORD")); $p->exec("CREATE DATABASE IF NOT EXISTS ".getenv("MYSQLITE_DB"));'

for emulate in 0 1; do
	echo "== emulated prepares: $emulate"
	MYSQLITE_EMULATE=$emulate php "$here/scenario.php"
done
