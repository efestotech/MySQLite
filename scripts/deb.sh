#!/bin/sh
# Builds the Debian package of mysqlite from a built Linux binary:
#   scripts/deb.sh <version> <arch> <binary> <outdir>
# version is the tag (v0.2.0 or v0.2.0-rc.1), arch is amd64 or arm64. The package
# installs /usr/bin/mysqlite, the default configuration /etc/mysqlite/mysqlite.yaml
# (a conffile: an upgrade keeps local changes) and the systemd unit, which postinst
# enables and starts.
set -eu

if [ $# -ne 4 ]; then
	echo "usage: $0 <version> <arch> <binary> <outdir>" >&2
	exit 2
fi
tag=$1 arch=$2 bin=$3 out=$4
root=$(cd "$(dirname "$0")/.." && pwd)

# Debian versions start with a digit and sort a pre-release before the release with "~".
version=$(printf '%s' "${tag#v}" | tr '-' '~')
case "$version" in
[0-9]*) ;;
*) echo "deb.sh: version $tag does not start with a digit" >&2; exit 2 ;;
esac

pkg=$(mktemp -d)
trap 'rm -rf "$pkg"' EXIT

install -D -m 0755 "$bin" "$pkg/usr/bin/mysqlite"
install -D -m 0644 "$root/deploy/linux/mysqlite.yaml" "$pkg/etc/mysqlite/mysqlite.yaml"
install -D -m 0644 "$root/deploy/linux/mysqlite.service" "$pkg/lib/systemd/system/mysqlite.service"
install -D -m 0644 "$root/README.md" "$pkg/usr/share/doc/mysqlite/README.md"
install -D -m 0644 "$root/LICENSE" "$pkg/usr/share/doc/mysqlite/copyright"
install -D -m 0644 "$root/NOTICE" "$pkg/usr/share/doc/mysqlite/NOTICE"

mkdir -p "$pkg/DEBIAN"
for s in postinst prerm postrm; do
	install -m 0755 "$root/deploy/linux/debian/$s" "$pkg/DEBIAN/$s"
done
echo /etc/mysqlite/mysqlite.yaml > "$pkg/DEBIAN/conffiles"
size=$(du -sk "$pkg/usr" "$pkg/etc" "$pkg/lib" | awk '{s += $1} END {print s}')
cat > "$pkg/DEBIAN/control" <<CONTROL
Package: mysqlite
Version: $version
Architecture: $arch
Maintainer: Efesto Tech
Installed-Size: $size
Depends: adduser
Section: database
Priority: optional
Homepage: https://github.com/efestotech/MySQLite
Description: MySQL-protocol server backed by SQLite
 mysqlite speaks the MySQL client protocol and stores every database in a
 SQLite file. The package installs a systemd service that listens on
 127.0.0.1:3307 with the configuration in /etc/mysqlite/mysqlite.yaml.
CONTROL

mkdir -p "$out"
deb="$out/mysqlite_${version}_${arch}.deb"
dpkg-deb --root-owner-group --build "$pkg" "$deb" >/dev/null
echo "$deb"
