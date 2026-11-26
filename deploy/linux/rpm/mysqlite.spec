# RPM spec of mysqlite, filled in and built by scripts/rpm.sh from a built static
# binary: the files are staged in %{stage}, so nothing is compiled here.
%global debug_package %{nil}
%global __os_install_post %{nil}

Name:           mysqlite
Version:        %{pkg_version}
Release:        1
Summary:        MySQL-protocol server backed by SQLite
License:        Apache-2.0
URL:            https://github.com/efestotech/MySQLite
Requires(pre):  shadow-utils

%description
mysqlite speaks the MySQL client protocol and stores every database in a
SQLite file. The package installs a systemd service that listens on
127.0.0.1:3307 with the configuration in /etc/mysqlite/mysqlite.yaml.

%install
cp -a %{stage}/. %{buildroot}/

# Creates the mysqlite system user before the files it owns are installed.
%pre
getent group mysqlite >/dev/null || groupadd -r mysqlite
getent passwd mysqlite >/dev/null ||
	useradd -r -g mysqlite -d /var/lib/mysqlite -s /sbin/nologin -c "mysqlite server" mysqlite
exit 0

# Enables and starts the service on install, restarts it on upgrade. A service that
# fails to start does not fail the installation.
%post
if [ -d /run/systemd/system ]; then
	systemctl daemon-reload >/dev/null 2>&1 || :
	if [ "$1" -eq 1 ]; then
		systemctl enable --now mysqlite.service >/dev/null 2>&1 ||
			echo "mysqlite: the service did not start, see: journalctl -u mysqlite" >&2
	else
		systemctl try-restart mysqlite.service >/dev/null 2>&1 ||
			echo "mysqlite: the service did not restart, see: journalctl -u mysqlite" >&2
	fi
fi
exit 0

# Stops and disables the service when the package is removed (not on upgrade).
%preun
if [ "$1" -eq 0 ] && [ -d /run/systemd/system ]; then
	systemctl disable --now mysqlite.service >/dev/null 2>&1 || :
fi
exit 0

%postun
if [ -d /run/systemd/system ]; then
	systemctl daemon-reload >/dev/null 2>&1 || :
fi
exit 0

# The databases in /var/lib/mysqlite, the logs and the mysqlite user stay after an
# erase: remove them by hand when they are no longer needed.
%files
%attr(0755,root,root) /usr/bin/mysqlite
%dir %attr(0755,root,root) /etc/mysqlite
%config(noreplace) %attr(0640,root,mysqlite) /etc/mysqlite/mysqlite.yaml
%attr(0644,root,root) /usr/lib/systemd/system/mysqlite.service
%dir %attr(0750,mysqlite,mysqlite) /var/lib/mysqlite
%dir %attr(0750,mysqlite,mysqlite) /var/log/mysqlite
%doc /usr/share/doc/mysqlite/README.md
%doc /usr/share/doc/mysqlite/NOTICE
%license /usr/share/doc/mysqlite/LICENSE
