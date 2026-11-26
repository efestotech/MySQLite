#!/bin/sh
# Installs a mysqlite package in a throwaway container and checks it:
#   scripts/pkgtest.sh <file.deb|file.rpm>
# Run as root inside the container (the CI job runs it in Debian, Fedora and Rocky
# Linux images). Containers have no systemd, so the service is started by hand as the
# mysqlite user, the way the unit runs it. Removal keeps the databases.
set -eu

# apt and dnf read a local package only from a path, not a bare relative name.
pkg=$(cd "$(dirname "$1")" && pwd)/$(basename "$1")
fail() { echo "pkgtest: $*" >&2; exit 1; }

case "$pkg" in
*.deb)
	apt-get update -qq
	apt-get install -y -qq "$pkg" >/dev/null
	remove() { apt-get remove -y -qq mysqlite >/dev/null; }
	;;
*.rpm)
	# setpriv, used below, is in util-linux, not in the images' util-linux-core.
	dnf install -y -q util-linux "$pkg" >/dev/null
	remove() { dnf remove -y -q mysqlite >/dev/null; }
	;;
*) fail "unknown package type: $pkg" ;;
esac

id mysqlite >/dev/null || fail "no mysqlite user"
[ -x /usr/bin/mysqlite ] || fail "no /usr/bin/mysqlite"
[ "$(stat -c '%a %U %G' /etc/mysqlite/mysqlite.yaml)" = "640 root mysqlite" ] ||
	fail "/etc/mysqlite/mysqlite.yaml: $(stat -c '%a %U %G' /etc/mysqlite/mysqlite.yaml)"
for d in /var/lib/mysqlite /var/log/mysqlite; do
	[ "$(stat -c '%a %U' "$d")" = "750 mysqlite" ] || fail "$d: $(stat -c '%a %U' "$d")"
done
[ -f /lib/systemd/system/mysqlite.service ] || [ -f /usr/lib/systemd/system/mysqlite.service ] ||
	fail "no systemd unit"

# The server starts with the default configuration and listens on 127.0.0.1:3307.
setpriv --reuid=mysqlite --regid=mysqlite --init-groups \
	/usr/bin/mysqlite -config /etc/mysqlite/mysqlite.yaml &
server=$!
up=""
for _ in 1 2 3 4 5 6 7 8 9 10; do
	if bash -c 'exec 3<>/dev/tcp/127.0.0.1/3307' 2>/dev/null; then up=yes; break; fi
	sleep 1
done
kill "$server" 2>/dev/null || :
wait "$server" 2>/dev/null || :
[ -n "$up" ] || fail "the server did not listen on 127.0.0.1:3307"

# A database file stands for the user's data, which removal must keep.
setpriv --reuid=mysqlite --regid=mysqlite --init-groups touch /var/lib/mysqlite/keep.db
remove
[ ! -e /usr/bin/mysqlite ] || fail "/usr/bin/mysqlite left after removal"
[ -f /var/lib/mysqlite/keep.db ] || fail "removal deleted the databases"
echo "pkgtest: $pkg ok"
