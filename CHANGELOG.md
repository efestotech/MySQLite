# Changelog

All notable changes to mysqlite are listed here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and versions follow
[Semantic Versioning](https://semver.org/).

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

[0.2.0]: https://github.com/efestotech/MySQLite/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/efestotech/MySQLite/releases/tag/v0.1.0
