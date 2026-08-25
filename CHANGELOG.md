# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Added `--ssh-key` for unattended remote-source authentication.
- Added systemd service and hourly timer templates with activation-relative
  scheduling.

## [0.2.0] - 2026-08-25

### Changed

- Replaced nanosecond decimal snapshot suffixes with compact lowercase base36
  Unix-second timestamps divided by 256.
- Documented using an existing SSH agent when the receiver runs under `sudo`.

### Fixed

- Avoided snapshot-name collisions within the same timestamp interval,
  including names retained only on the destination by external holds.

## [0.1.1] - 2026-08-25

### Added

- Added `--full` to safely replace an owned destination when no common
  snapshot exists.
- Added CLI and real-ZFS integration coverage for full replacement and
  external-hold protection.
- Expanded usage and delegated ZFS send/receive permission documentation.

### Fixed

- Fixed release publication when the GitHub Actions release job has no
  repository checkout.

## [0.1.0] - 2026-08-25

### Added

- Added local and SSH ZFS sources with local-only destinations.
- Added full, incremental, recursive, resumable, and raw encrypted replication.
- Added snapshot holds, cleanup, ownership validation, locking, interruption
  handling, Healthchecks.io lifecycle notifications, structured logging, and
  interactive transfer progress.
- Added unit tests and an Ubuntu/libvirt integration suite using real ZFS pools.
- Added Nix development tooling and GitHub Actions builds for static Linux
  amd64 and arm64 binaries.

[Unreleased]: https://github.com/moddengine/modd-zfs-backup/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/moddengine/modd-zfs-backup/compare/v0.1.1...v0.2.0
[0.1.1]: https://github.com/moddengine/modd-zfs-backup/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/moddengine/modd-zfs-backup/releases/tag/v0.1.0
