# modd-zfs-backup

`modd-zfs-backup` receives local or SSH-hosted ZFS datasets into a local,
dedicated backup dataset.

```sh
modd-zfs-backup --name server-a \
  --source backup@server-a:tank/data \
  --dest backup/server-a/data
```

Local sources use the same interface without SSH:

```sh
modd-zfs-backup --name local-data --source tank/data --dest backup/data
```

The destination must not already exist for its initial full receive. Later
runs verify destination ownership and a common snapshot GUID before sending an
incremental stream. `--recursive` is a mirror operation: after ownership is
verified, destination-only snapshots and descendants may be removed by ZFS.
Backups are received read-only, unmounted, and with automatic resumable receive
state. Encrypted sources are sent raw.

The process needs local ZFS receive/hold/destroy privileges and, for remote
sources, SSH access with source-side list/get/snapshot/send/hold/release/destroy
permissions. Run it as root or configure equivalent ZFS delegation. Logs go to
stderr in a journald-friendly format. `--healthcheck-url` enables `/start`,
success, and `/fail` lifecycle pings. Skipped runs make no request.

## Tests

Unit tests:

```sh
nix-shell --run 'go test -race ./...'
```

The full suite creates an isolated Ubuntu 26.04 KVM guest with separate source
and destination ZFS disks, then runs both local and SSH-source scenarios inside
it:

```sh
nix-shell --run tests/e2e.sh
```

The runner must have access to `qemu:///system` and the default libvirt storage
pool. The verified Ubuntu image and test SSH key are cached in `.test-state/`;
all per-run VM resources are removed automatically. Set `KEEP_VM=1` to retain a
failed VM, or run `tests/e2e.sh destroy` to remove exact harness resources.
`tests/ci.sh` is the generic entrypoint for a KVM-capable self-hosted CI runner.
