# modd-zfs-backup

`modd-zfs-backup` receives a local or SSH-hosted ZFS dataset into a local,
dedicated backup dataset. The destination is always managed by the machine
running the command.

## Examples

Remote production dataset to a local backup pool:

```sh
sudo modd-zfs-backup \
  --name server-a \
  --source mzb-send@server-a:tank/data \
  --dest backup/server-a/data
```

Local pool-to-pool backup without SSH:

```sh
sudo modd-zfs-backup \
  --name local-data \
  --source tank/data \
  --dest backup/local-data
```

Mirror a complete dataset tree. Recursive mode uses ZFS replication streams
and may remove destination-only descendants:

```sh
sudo modd-zfs-backup \
  --name server-a \
  --source mzb-send@server-a:tank \
  --dest backup/server-a \
  --recursive
```

Show live progress in an interactive terminal and report real attempts to
Healthchecks.io:

```sh
sudo modd-zfs-backup \
  --name server-a \
  --source mzb-send@server-a:tank/data \
  --dest backup/server-a/data \
  --progress \
  --healthcheck-url https://hc-ping.com/your-check-uuid
```

If an owned destination has no common snapshot, the default is to stop without
changing it. `--full` explicitly permits replacing that destination:

```sh
sudo modd-zfs-backup \
  --name server-a \
  --source mzb-send@server-a:tank/data \
  --dest backup/server-a/data \
  --full
```

`--full` is destructive only when no common snapshot exists. Ownership is
verified first, application holds are released, and the destination dataset is
destroyed recursively before the full receive. An unrelated hold causes the
replacement to fail rather than bypassing that protection.

## ZFS permissions

### Source/send role

For an SSH source, create a dedicated account on the source host and delegate
only the operations used by the backup state machine:

```sh
sudo useradd --create-home --shell /bin/bash mzb-send
sudo zfs allow -u mzb-send \
  mount,snapshot,send,hold,release,destroy \
  tank/data
sudo zfs allow tank/data
```

Install the backup server's public key in
`/home/mzb-send/.ssh/authorized_keys`. Grants apply to descendants unless
restricted with `zfs allow -l`, so the same command supports `--recursive`.
The `send` permission is required for both ordinary and raw encrypted streams.
The account does not need receive permission or access to the backup pool.

When running the receiver with `sudo` and using a key already loaded into your
SSH agent, preserve the agent socket:

```sh
sudo --preserve-env=SSH_AUTH_SOCK modd-zfs-backup \
  --name server-a \
  --source mzb-send@server-a:tank/data \
  --dest backup/server-a/data
```

For a local source, grant the same permissions to the local account running
`modd-zfs-backup`, or run the command as root.

### Destination/receive role

The receive side is always local. On Linux, running `modd-zfs-backup` as root
is the recommended configuration because the program receives with `-u` and
enforces `readonly`, `canmount`, and `mountpoint` properties:

```sh
sudo modd-zfs-backup --name server-a \
  --source mzb-send@server-a:tank/data \
  --dest backup/server-a/data
```

On systems that support delegating all required properties, grant permissions
on the destination parent because the first receive creates the child dataset:

```sh
sudo zfs allow -u mzb-recv \
  create,destroy,receive,rollback,mount,hold,release,readonly,canmount,mountpoint,userprop \
  backup
sudo zfs allow backup
```

OpenZFS on Linux cannot delegate every mount-related operation, so this grant
may still require root or a tightly controlled sudo policy. Test delegation
with the exact installed OpenZFS version before scheduling unattended backups.
See the [OpenZFS delegated administration documentation](https://openzfs.github.io/openzfs-docs/Basic%20Concepts/Operations/Delegated%20Administration.html).

## Behaviour

The destination must not exist for its initial full receive. Later runs verify
destination ownership and a common snapshot GUID before sending an incremental
stream. Backups are received read-only and unmounted, interrupted receives are
resumable, and encrypted sources are sent raw.

Application snapshots are named `mzb-<name>-<timestamp>`. The timestamp is the
Unix time in seconds divided by 256 and encoded as lowercase base36, giving a
short, transcribable key that changes about every 4 minutes 16 seconds. A
numeric suffix prevents collisions if more than one snapshot is needed in the
same interval. Older decimal snapshot names remain valid replication bases.

Logs go to stderr in a journald-friendly format. `--healthcheck-url` enables
`/start`, success, and `/fail` lifecycle pings. Minimum-interval and concurrent
run skips make no request.

## Tests

Unit tests:

```sh
nix-shell --run 'go test -race ./...'
```

The full suite creates an isolated Ubuntu 26.04 KVM guest with separate source
and destination ZFS disks, then runs both local and SSH-source scenarios:

```sh
nix-shell --run tests/e2e.sh
```

The runner needs access to `qemu:///system` and the default libvirt storage
pool. The verified Ubuntu image and test SSH key are cached in `.test-state/`;
per-run VM resources are removed automatically. Set `KEEP_VM=1` to retain a
failed VM, or run `tests/e2e.sh destroy` to remove exact harness resources.
`tests/ci.sh` is the entrypoint for a KVM-capable self-hosted CI runner.
