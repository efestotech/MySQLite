#!/usr/bin/env bash
# Connector/J scenario in the docker client matrix (docs/07-testing.md section 2: the
# jar is fetched from Maven Central). Runs in eclipse-temurin:21.
#
#   run.sh HOST PORT
set -euo pipefail

host="$1" port="$2"
version=9.1.0
jar="mysql-connector-j-$version.jar"
work="$(mktemp -d)"
cp "$(dirname "$0")/Scenario.java" "$work/"
cd "$work"

curl -fsSL -o "$jar" "https://repo1.maven.org/maven2/com/mysql/mysql-connector-j/$version/$jar"
echo "Connector/J $version, $(java -version 2>&1 | head -1)"
javac Scenario.java

# The server container may still be starting.
for i in $(seq 1 30); do
	if (exec 3<>"/dev/tcp/$host/$port") 2>/dev/null; then break; fi
	sleep 1
done

MYSQLITE_HOST="$host" MYSQLITE_PORT="$port" MYSQLITE_USER=test MYSQLITE_PASSWORD=test \
	MYSQLITE_DB=clients_java java -cp ".:$jar" Scenario
