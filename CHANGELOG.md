# Changelog

All notable changes to mysqlite are listed here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and versions follow
[Semantic Versioning](https://semver.org/).

## [0.3.0] - 2026-11-26

WordPress 7.1.5 installs and runs on mysqlite (tested with WP-CLI 2.12.0).

### Added

- RPM package `mysqlite-<version>-1.<arch>.rpm` for `x86_64` and `aarch64`, published
  with every release, with the same files and systemd service as the `.deb`. CI installs
  the `.deb` on Debian 12 and the `.rpm` on Fedora 41 and Rocky Linux 9.
- Collations `utf8mb4_unicode_520_ci` and `utf8mb3_unicode_520_ci`.
- Non-unique keys with a prefix length (`KEY k (col(191))`), reported by `SHOW INDEX`
  (`Sub_part`) and `SHOW CREATE TABLE`. Prefixes on unique and primary keys still answer
  1235.
- `REGEXP` and `RLIKE` (Go RE2 syntax), `LIKE BINARY` and `REGEXP BINARY`.
- Multi-table `DELETE`, in both the `DELETE t1, t2 FROM ...` and the `USING` form.
- `SUBSTRING`, `SUBSTR`, `MID`, `LEFT`, `RIGHT`, `REPLACE`, `LTRIM`, `RTRIM`.
- `SQL_CALC_FOUND_ROWS` and `FOUND_ROWS()`.

### Known limits

- `SQL_CALC_FOUND_ROWS` inside a `UNION` answers 1234.
- `SUBSTRING` with a non-numeric position such as `'2x'` returns an empty string.

## [0.2.0] - 2026-11-26

### Added

- Debian package `mysqlite_<version>_<arch>.deb` for `amd64` and `arm64`, published with
  every release. It installs `/usr/bin/mysqlite`, a default configuration in
  `/etc/mysqlite/mysqlite.yaml` (listening on `127.0.0.1:3307`, user `test`) and a systemd
  service that runs as the `mysqlite` system user and starts at install time. Databases
  live in `/var/lib/mysqlite`, logs in `/var/log/mysqlite`. An upgrade keeps local
  changes to the configuration; `purge` removes configuration and logs and keeps the
  databases.
- The Linux archives also contain `mysqlite.service`.

### Fixed

- The CI license check accepts `modernc.org/mathutil`, whose BSD-3-Clause text the
  license classifier does not recognize.

## [0.1.0] - 2026-11-25

First release.

### Added

- MySQL 8.0 client protocol: `mysql_native_password` authentication (clients proposing
  another plugin are switched to it), text and binary (prepared statement) protocol,
  multi-statement queries, `COM_INIT_DB`, `COM_FIELD_LIST`, `COM_STATISTICS`.
- Each database is a SQLite file `<name>.db` in `data_dir`, with one writer and a pool of
  readers per database.
- Translation of the MySQL dialect to SQLite: DDL (including `ALTER TABLE` with a backup
  copy), DML with `INSERT IGNORE`, `ON DUPLICATE KEY UPDATE` and `REPLACE`, InnoDB-style
  `AUTO_INCREMENT`, transactions, `SHOW` statements and `information_schema`.
- MySQL result metadata (column types, lengths, flags) and error codes; constructions that
  cannot be translated faithfully answer error 1235.
- YAML configuration with command-line overrides, rotating logs and a JSONL query journal.
- Static binaries without CGO, and release archives for Linux, Windows and macOS on
  `amd64` and `arm64`, with `SHA256SUMS`.

[0.3.0]: https://github.com/efestotech/MySQLite/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/efestotech/MySQLite/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/efestotech/MySQLite/releases/tag/v0.1.0
