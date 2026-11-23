#!/usr/bin/env bash
# Command-line client scenario (docs/07-testing.md section 6): connect, list databases,
# list tables, describe a table, SELECT with a WHERE on a string, INSERT, UPDATE,
# DELETE, disconnect. Works with the mysql CLI (8.0, 8.4, 9.x) and the MariaDB CLI.
#
#   run.sh CLIENT HOST PORT TAG     e.g. run.sh mysql mysqlite 3307 cli80
#
# Prints one line per step and PASS at the end; exits 1 on the first failure. The
# scenario uses the database clients_TAG, created if missing.
set -euo pipefail

client="$1" host="$2" port="$3" tag="$4"
db="clients_$tag"

# The server has no TLS: say so, in each client's own words.
case "$client" in
mariadb) tls=(--skip-ssl) ;;
*) tls=(--ssl-mode=DISABLED) ;;
esac

q() {
	"$client" --protocol=TCP -h "$host" -P "$port" -u test -ptest "${tls[@]}" \
		--batch --skip-column-names "$@" 2>/tmp/client-stderr || {
		cat /tmp/client-stderr >&2
		return 1
	}
}

check() { # step want got
	if [ "$2" != "$3" ]; then
		printf 'FAIL %s: want %q, got %q\n' "$1" "$2" "$3" >&2
		exit 1
	fi
	echo "ok $1"
}

echo "client: $("$client" --version)"

# The server container may still be starting.
for i in $(seq 1 30); do
	if q -e 'SELECT 1' >/dev/null 2>&1; then break; fi
	if [ "$i" = 30 ]; then
		q -e 'SELECT 1'
		exit 1
	fi
	sleep 1
done
check connect 1 "$(q -e 'SELECT 1')"

q -e "CREATE DATABASE IF NOT EXISTS $db"
check "list databases" "$db" "$(q -e 'SHOW DATABASES' | grep -x "$db")"

q "$db" -e 'DROP TABLE IF EXISTS people;
CREATE TABLE people (id INT AUTO_INCREMENT PRIMARY KEY, name VARCHAR(50) NOT NULL, city VARCHAR(50))'
check "list tables" people "$(q "$db" -e 'SHOW TABLES' | grep -x people)"
check "describe table" "id name city" "$(q "$db" -e 'DESCRIBE people' | cut -f1 | tr '\n' ' ' | sed 's/ $//')"

q "$db" -e "INSERT INTO people (name, city) VALUES ('Ada', 'Roma'), ('Bea', 'Milano'), ('Cesare', 'Roma')"
check insert 3 "$(q "$db" -e 'SELECT COUNT(*) FROM people')"
check "select where string" "Ada Cesare" "$(q "$db" -e "SELECT name FROM people WHERE city = 'Roma' ORDER BY id" | tr '\n' ' ' | sed 's/ $//')"

q "$db" -e "UPDATE people SET city = 'Torino' WHERE name = 'Bea'"
check update Torino "$(q "$db" -e "SELECT city FROM people WHERE name = 'Bea'")"

q "$db" -e "DELETE FROM people WHERE city = 'Roma'"
check delete 1 "$(q "$db" -e 'SELECT COUNT(*) FROM people')"

q "$db" -e 'DROP TABLE people'
echo "ok disconnect" # every q call above connected and disconnected
echo PASS
