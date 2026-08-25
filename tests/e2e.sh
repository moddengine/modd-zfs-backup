#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
state="$root/.test-state"
current="$state/current"
runs="$state/runs"
vm=modd-zfs-backup-e2e
network=modd-zfs-backup-e2e-net
pool="${LIBVIRT_POOL:-default}"
uri="${LIBVIRT_URI:-qemu:///system}"
ip=192.168.253.10
mac=52:54:00:26:04:11
image=ubuntu-26.04-server-cloudimg-amd64.img
image_url=https://cloud-images.ubuntu.com/releases/server/server/releases/26.04/release
base_volume=modd-zfs-backup-ubuntu-26.04-base.qcow2
os_volume="$vm-os.qcow2"
source_volume="$vm-source.qcow2"
dest_volume="$vm-dest.qcow2"
seed_volume="$vm-seed.iso"
ssh_options=(-i "$state/id_ed25519" -o BatchMode=yes -o ConnectTimeout=5 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null)

for command in cloud-localds curl flock jq qemu-img scp sha256sum ssh ssh-keygen tar virsh virt-install; do
  command -v "$command" >/dev/null || { echo "Missing prerequisite: $command" >&2; exit 1; }
done

mkdir -p "$state/cache" "$runs"
exec 9>"$state/e2e.lock"
flock -n 9 || { echo "Another modd-zfs-backup VM test is running." >&2; exit 1; }

virsh_cmd() { virsh -c "$uri" "$@"; }

[[ $vm == modd-zfs-backup-e2e && $network == modd-zfs-backup-e2e-net ]] || exit 2

cleanup() {
  status=$?
  trap - EXIT INT TERM
  set +e
  stamp="$(date -u +%Y%m%dT%H%M%SZ)"
  artifacts="$runs/$stamp"
  mkdir -p "$artifacts"
  if ssh "${ssh_options[@]}" "ubuntu@$ip" true >/dev/null 2>&1; then
    ssh "${ssh_options[@]}" "ubuntu@$ip" 'sudo journalctl -b --no-pager; sudo zpool status; sudo zfs list -r -t all; sudo cat /run/modd-zfs-backup-test/commands.log; sudo cat /run/modd-zfs-backup-test/processes.log' >"$artifacts/guest.log" 2>&1
  fi
  [[ -f $current/test.log ]] && cp "$current/test.log" "$artifacts/test.log"
  [[ -f $current/coverage.out ]] && cp "$current/coverage.out" "$artifacts/coverage.out"
  if [[ ${KEEP_VM:-0} != 1 ]]; then
    virsh_cmd destroy "$vm" >/dev/null 2>&1 || true
    virsh_cmd undefine "$vm" --nvram >/dev/null 2>&1 || virsh_cmd undefine "$vm" >/dev/null 2>&1 || true
    for volume in "$os_volume" "$source_volume" "$dest_volume" "$seed_volume"; do
      virsh_cmd vol-delete --pool "$pool" "$volume" >/dev/null 2>&1 || true
    done
    virsh_cmd net-destroy "$network" >/dev/null 2>&1 || true
    virsh_cmd net-undefine "$network" >/dev/null 2>&1 || true
  else
    echo "KEEP_VM=1: retained $vm at $ip" >&2
  fi
  exit "$status"
}

destroy() {
  KEEP_VM=0 cleanup
}

if [[ ${1:-} == destroy ]]; then
  trap - EXIT
  destroy
fi

rm -rf "$current"
mkdir -p "$current"
trap cleanup EXIT INT TERM

# Remove only stale resources owned by this harness.
virsh_cmd destroy "$vm" >/dev/null 2>&1 || true
virsh_cmd undefine "$vm" --nvram >/dev/null 2>&1 || virsh_cmd undefine "$vm" >/dev/null 2>&1 || true
for volume in "$os_volume" "$source_volume" "$dest_volume" "$seed_volume"; do
  virsh_cmd vol-delete --pool "$pool" "$volume" >/dev/null 2>&1 || true
done
virsh_cmd net-destroy "$network" >/dev/null 2>&1 || true
virsh_cmd net-undefine "$network" >/dev/null 2>&1 || true

if [[ ! -f $state/id_ed25519 ]]; then
  ssh-keygen -q -t ed25519 -N '' -C "$vm" -f "$state/id_ed25519"
fi
public_key="$(<"$state/id_ed25519.pub")"

expected="$(curl -fsSL "$image_url/SHA256SUMS" | awk -v image="*$image" '$2 == image || $2 == substr(image, 2) { print $1 }')"
[[ $expected =~ ^[[:xdigit:]]{64}$ ]] || { echo "Could not find $image in SHA256SUMS." >&2; exit 1; }
if [[ ! -f $state/cache/$image ]] || [[ $(sha256sum "$state/cache/$image" | awk '{print $1}') != "$expected" ]]; then
  curl -fL --progress-bar "$image_url/$image" -o "$state/cache/$image"
fi
[[ $(sha256sum "$state/cache/$image" | awk '{print $1}') == "$expected" ]]

cached_digest="$(cat "$state/cache/base.digest" 2>/dev/null || true)"
if [[ $cached_digest != "$expected" ]]; then
  virsh_cmd vol-delete --pool "$pool" "$base_volume" >/dev/null 2>&1 || true
fi
if ! virsh_cmd vol-info --pool "$pool" "$base_volume" >/dev/null 2>&1; then
  virtual_size="$(qemu-img info --output=json "$state/cache/$image" | jq -r '."virtual-size"')"
  cat >"$current/base.xml" <<EOF
<volume>
  <name>$base_volume</name>
  <capacity unit="bytes">$virtual_size</capacity>
  <target><format type="qcow2"/></target>
</volume>
EOF
  virsh_cmd vol-create --pool "$pool" "$current/base.xml"
  virsh_cmd vol-upload --pool "$pool" "$base_volume" "$state/cache/$image" --sparse
  printf '%s\n' "$expected" >"$state/cache/base.digest"
fi

virsh_cmd vol-create-as --pool "$pool" "$os_volume" 16G --format qcow2 --backing-vol "$base_volume" --backing-vol-format qcow2
virsh_cmd vol-create-as --pool "$pool" "$source_volume" 8G --format qcow2
virsh_cmd vol-create-as --pool "$pool" "$dest_volume" 8G --format qcow2

private_key_yaml="$(sed 's/^/      /' "$state/id_ed25519")"
cat >"$current/user-data" <<EOF
#cloud-config
package_update: true
packages: [zfsutils-linux, golang-go, openssh-server, pv, ca-certificates]
ssh_authorized_keys:
  - $public_key
users:
  - default
  - name: mzbsource
    shell: /bin/bash
    lock_passwd: true
    ssh_authorized_keys:
      - $public_key
write_files:
  - path: /root/.ssh/id_ed25519
    owner: root:root
    permissions: '0600'
    content: |
$private_key_yaml
runcmd:
  - [modprobe, zfs]
  - [mkdir, -p, /root/.ssh, /run/modd-zfs-backup-test]
  - [chmod, '0777', /run/modd-zfs-backup-test]
  - [bash, -c, 'ssh-keyscan -H 127.0.0.1 > /root/.ssh/known_hosts']
  - [zpool, create, -f, -o, cachefile=none, -O, mountpoint=none, mzbsource, /dev/disk/by-id/virtio-mzb-src-e2e]
  - [zpool, create, -f, -o, cachefile=none, -O, mountpoint=none, mzbbackup, /dev/disk/by-id/virtio-mzb-dst-e2e]
  - [zfs, allow, -u, mzbsource, 'create,destroy,snapshot,hold,release,send,mount,userprop', mzbsource]
EOF
printf 'instance-id: %s\nlocal-hostname: %s\n' "$vm" "$vm" >"$current/meta-data"
cloud-localds "$current/seed.iso" "$current/user-data" "$current/meta-data"
virsh_cmd vol-create-as --pool "$pool" "$seed_volume" "$(stat -c %s "$current/seed.iso")" --format raw
virsh_cmd vol-upload --pool "$pool" "$seed_volume" "$current/seed.iso"

cat >"$current/network.xml" <<EOF
<network>
  <name>$network</name>
  <forward mode="nat"/>
  <ip address="192.168.253.1" netmask="255.255.255.0">
    <dhcp><host mac="$mac" name="$vm" ip="$ip"/></dhcp>
  </ip>
</network>
EOF
virsh_cmd net-define "$current/network.xml"
virsh_cmd net-start "$network"

virt-install --connect "$uri" --name "$vm" --memory 4096 --vcpus 4 --cpu host-passthrough \
  --import --osinfo ubuntu24.04 --boot uefi \
  --disk "vol=$pool/$os_volume,bus=virtio" \
  --disk "vol=$pool/$source_volume,bus=virtio,serial=mzb-src-e2e" \
  --disk "vol=$pool/$dest_volume,bus=virtio,serial=mzb-dst-e2e" \
  --disk "vol=$pool/$seed_volume,device=cdrom" \
  --network "network=$network,model=virtio,mac=$mac" --graphics none --noautoconsole

echo "Waiting for Ubuntu cloud-init at $ip..."
cloud_ready=0
for _ in {1..180}; do
  cloud_status="$(ssh "${ssh_options[@]}" "ubuntu@$ip" cloud-init status 2>/dev/null || true)"
  if [[ $cloud_status == *'status: done'* ]]; then
    cloud_ready=1
    break
  fi
  if [[ $cloud_status == *'status: error'* ]]; then
    echo "cloud-init failed: $cloud_status" >&2
    exit 1
  fi
  sleep 2
done
(( cloud_ready == 1 )) || { echo "Timed out waiting for cloud-init." >&2; exit 1; }

cat >"$current/zfs-wrapper" <<'EOF'
#!/usr/bin/env bash
set -o pipefail
real=/usr/sbin/zfs
origin=local
[[ -n ${SSH_CONNECTION:-} ]] && origin=remote
printf 'zfs|%s|%s\n' "$origin" "$*" >>/run/modd-zfs-backup-test/commands.log
printf '%s|%s|%s\n' "$$" "$origin" "$*" >>/run/modd-zfs-backup-test/processes.log
fault="$(cat /run/modd-zfs-backup-test/fault 2>/dev/null || true)"
state=/run/modd-zfs-backup-test/fault-state
if [[ $fault == source-exists-fail && $1 == list && $* == *' -o name mzbsource/'* ]] ||
   [[ $fault == dest-exists-fail && $1 == list && $* == *' -o name mzbbackup/'* ]]; then
  echo 'injected dataset lookup failure' >&2
  exit 6
fi
if [[ $fault == encryption-fail && $1 == get && $* == *' encryptionroot mzbsource/'* ]]; then
  echo 'injected encryption lookup failure' >&2
  exit 6
fi
if [[ $fault == source-snapshots-fail && $1 == list && $* == *'-t snapshot'* && $* == *' mzbsource/'* ]] ||
   [[ $fault == dest-snapshots-fail && $1 == list && $* == *'-t snapshot'* && $* == *' mzbbackup/'* ]]; then
  echo 'injected snapshot listing failure' >&2
  exit 6
fi
if [[ $fault == source-snapshot-future && $1 == list && $* == *'-t snapshot'* && $* == *' mzbsource/'* ]]; then
  "$real" "$@" | awk -v future="$(( $(date +%s) + 3600 ))" 'NF == 3 {$3 = future} {print}'
  exit ${PIPESTATUS[0]}
fi
if [[ $fault == destination-token-fail && $1 == get && $* == *' receive_resume_token mzbbackup/'* ]]; then
  echo 'injected receive token lookup failure' >&2
  exit 6
fi
if [[ ( $fault == dest-root-snapshots-fail || $fault == dest-root-snapshots-empty ) && $1 == list && $* == *'-t snapshot'* && $* == *' mzbbackup/'* ]]; then
  count="$(cat "$state" 2>/dev/null || echo 0)"
  printf '%s\n' "$((count + 1))" >"$state"
  if (( count > 0 )); then
    [[ $fault == dest-root-snapshots-fail ]] && { echo 'injected root snapshot listing failure' >&2; exit 6; }
    exit 0
  fi
fi
if [[ $fault == resume-token-wrong && $1 == send && $* == *'-nP -t'* ]]; then
  echo 'toname = mzbsource/wrong@mzb-wrong-1'
  echo 'size = 1'
  exit 0
fi
if [[ $fault == resume-size-zero && $1 == send && $* == *'-nP -t'* ]]; then
  count="$(cat "$state" 2>/dev/null || echo 0)"
  printf '%s\n' "$((count + 1))" >"$state"
  if (( count == 0 )); then
    "$real" "$@" | awk '$1 == "size" {$NF = 0} {print}'
    exit ${PIPESTATUS[0]}
  fi
fi
if [[ $fault == send-estimate-fail && $1 == send && $* == *'-nP'* ]]; then
  echo 'injected send estimate failure' >&2
  exit 6
fi
if [[ $fault == destination-set-fail && $1 == set && $* == *' mzbbackup/'* ]]; then
  echo 'injected destination property failure' >&2
  exit 6
fi
if [[ $fault == holds-source-fail && $1 == holds && $* == *' mzbsource/'* ]] ||
   [[ $fault == holds-dest-fail && $1 == holds && $* == *' mzbbackup/'* ]]; then
  echo 'injected holds lookup failure' >&2
  exit 6
fi
if [[ $fault == cleanup-slow && $1 == release && $* == *' mzbsource/'* ]]; then
  sleep 5
fi
if [[ $fault == snapshot-fail && $1 == snapshot ]]; then
  echo 'injected snapshot failure' >&2
  exit 6
fi
if [[ $fault == guid-fail && $1 == get && $* == *' guid '* ]]; then
  echo 'injected GUID lookup failure' >&2
  exit 6
fi
if [[ $fault == received-guid-mismatch && $1 == get && $* == *' guid mzbbackup/'* ]]; then
  echo 1
  exit 0
fi
if [[ $fault == hold-source-fail && $1 == hold && $* == *' mzbsource/'* ]] ||
   [[ $fault == hold-dest-fail && $1 == hold && $* == *' mzbbackup/'* ]]; then
  echo 'injected hold failure' >&2
  exit 6
fi
if [[ $fault == release-source-fail && $1 == release && $* == *' mzbsource/'* ]] ||
   [[ $fault == release-dest-fail && $1 == release && $* == *' mzbbackup/'* ]]; then
  echo 'injected release failure' >&2
  exit 6
fi
if [[ $fault == destroy-source-fail && $1 == destroy && $* == *'mzbsource/'*'@'* ]] ||
   [[ $fault == destroy-dest-fail && $1 == destroy && $* == *'mzbbackup/'*'@'* ]]; then
  echo 'injected destroy failure' >&2
  exit 6
fi
if [[ $fault == send-fail && $1 == send && $* != *'-nP'* ]]; then
  echo 'injected send failure' >&2
  exit 7
fi
if [[ $fault == receive-fail && $1 == receive ]]; then
  head -c 65536 | "$real" "$@"
  status=$?
  (( status != 0 )) && exit "$status"
  exit 8
fi
if [[ $fault == receive-slow && $1 == receive ]]; then
  pv -qL 256k | "$real" "$@"
  exit $?
fi
exec "$real" "$@"
EOF
cat >"$current/ssh-wrapper" <<'EOF'
#!/usr/bin/env bash
printf 'ssh|%s\n' "$*" >>/run/modd-zfs-backup-test/commands.log
exec /usr/bin/ssh "$@"
EOF
scp "${ssh_options[@]}" "$current/zfs-wrapper" "$current/ssh-wrapper" "ubuntu@$ip:/tmp/"
ssh "${ssh_options[@]}" "ubuntu@$ip" 'sudo install -m 0755 /tmp/zfs-wrapper /usr/local/bin/zfs; sudo install -m 0755 /tmp/ssh-wrapper /usr/local/bin/ssh; sudo touch /run/modd-zfs-backup-test/commands.log /run/modd-zfs-backup-test/processes.log; sudo chmod 0666 /run/modd-zfs-backup-test/commands.log /run/modd-zfs-backup-test/processes.log'

tar --exclude=.git --exclude=.test-state -C "$root" -cf - . | ssh "${ssh_options[@]}" "ubuntu@$ip" 'sudo mkdir -p /opt/modd-zfs-backup; sudo tar -xf - -C /opt/modd-zfs-backup'

ssh "${ssh_options[@]}" "ubuntu@$ip" 'cd /opt/modd-zfs-backup && sudo go build -o /tmp/modd-zfs-backup . && sudo go vet ./... && sudo env MZB_INTEGRATION=1 MZB_BINARY=/tmp/modd-zfs-backup go test -race -count=1 -tags=integration -coverprofile=/tmp/coverage.out ./... && sudo go tool cover -func=/tmp/coverage.out' | tee "$current/test.log"
scp "${ssh_options[@]}" "ubuntu@$ip:/tmp/coverage.out" "$current/coverage.out"

echo "VM integration suite passed. Artifacts will be stored under $runs."
