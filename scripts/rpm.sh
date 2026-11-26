#!/bin/sh
# Builds the RPM package of mysqlite from a built Linux binary:
#   scripts/rpm.sh <version> <arch> <binary> <outdir>
# version is the tag (v0.3.0 or v0.3.0-rc.1), arch is amd64 or arm64. The package
# installs /usr/bin/mysqlite, the default configuration /etc/mysqlite/mysqlite.yaml
# (%config(noreplace): an upgrade keeps local changes) and the systemd unit, which %post
# enables and starts. The spec is deploy/linux/rpm/mysqlite.spec; rpmbuild is required.
set -eu

if [ $# -ne 4 ]; then
	echo "usage: $0 <version> <arch> <binary> <outdir>" >&2
	exit 2
fi
tag=$1 arch=$2 bin=$3 out=$4
root=$(cd "$(dirname "$0")/.." && pwd)

# RPM versions cannot hold "-"; "~" sorts a pre-release before the release.
version=$(printf '%s' "${tag#v}" | tr '-' '~')
case "$version" in
[0-9]*) ;;
*) echo "rpm.sh: version $tag does not start with a digit" >&2; exit 2 ;;
esac
case "$arch" in
amd64) rpmarch=x86_64 ;;
arm64) rpmarch=aarch64 ;;
*) echo "rpm.sh: unknown architecture $arch" >&2; exit 2 ;;
esac

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
stage="$work/stage"

install -D -m 0755 "$bin" "$stage/usr/bin/mysqlite"
install -D -m 0640 "$root/deploy/linux/mysqlite.yaml" "$stage/etc/mysqlite/mysqlite.yaml"
install -D -m 0644 "$root/deploy/linux/mysqlite.service" "$stage/usr/lib/systemd/system/mysqlite.service"
install -D -m 0644 "$root/README.md" "$stage/usr/share/doc/mysqlite/README.md"
install -D -m 0644 "$root/LICENSE" "$stage/usr/share/doc/mysqlite/LICENSE"
install -D -m 0644 "$root/NOTICE" "$stage/usr/share/doc/mysqlite/NOTICE"
install -d -m 0750 "$stage/var/lib/mysqlite" "$stage/var/log/mysqlite"

rpmbuild -bb --quiet --target "$rpmarch" \
	--define "_topdir $work/build" \
	--define "pkg_version $version" \
	--define "stage $stage" \
	"$root/deploy/linux/rpm/mysqlite.spec" >/dev/null

mkdir -p "$out"
rpm="$out/mysqlite-${version}-1.${rpmarch}.rpm"
cp "$work/build/RPMS/$rpmarch/mysqlite-${version}-1.${rpmarch}.rpm" "$rpm"
echo "$rpm"
