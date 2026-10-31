#!/usr/bin/env bash
# Copyright 2026 Revington
# SPDX-License-Identifier: Apache-2.0
#
# host.sh applies the benchmark settings baseline to one lab host of a
# profile inventory (docs/architecture/12-performance-budgets-and-benchmarking.md,
# "Reference hardware"). provision.sh copies it, tune.sh and the inventory
# to each host and runs it there as root; README.md is the runbook.
#
# It is idempotent: a second run with the same arguments changes nothing.
# On the rh-1 profile every setting must apply, or the run fails; on rh-3,
# whose cloud VMs are never gated, tune.sh skips what a VM does not expose.
# Steps:
#   1. Debian only; the role's packages; automatic updates off.
#   2. The dedicated user the harness runs as and reaches the hosts as (the
#      inventory's sshUser): lingering, and cpu, cpuset, io, memory and pids
#      delegated to its user manager for the harness's systemd-run scopes,
#      with LimitNOFILE 1,048,576 for its sessions and its manager.
#   3. The host's working directory, and inside it the secret root and the
#      TLS directory.
#   4. /etc/hosts: the inventory's bench.example names on the lab addresses.
#   5. ssh: on the gateway host (RH-1) the harness's key pair and the other
#      hosts' host keys (--known-hosts); elsewhere the harness's public key
#      (--authorized-key), accepted only from the gateway's lab address.
#   6. sysctls, swap off, irqbalance off.
#   7. tune.sh, now and at every boot through ruralz-bench-tune.service: SMT,
#      turbo, governor and fixed frequency, C-states, THP, NIC rings and
#      coalescing, NIC interrupts outside the measured cpusets.
#   8. On the State Store host (RH-1S): Docker and the pinned Valkey 9 image
#      (--valkey-image), unless the inventory names a server executable.
#   9. Every installed package held, so updates happen only in a recorded
#      maintenance window.
# Under a directory the user owns, its home and the working directory, the
# user can plant a link that root would follow, so every change there runs
# as the user.
# BENCHHOST_ROOT prefixes every file path (the tests run it on a fake tree).
set -euo pipefail

# One collation for sort and comm.
export LC_ALL=C
R=${BENCHHOST_ROOT:-}
self_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

log() { printf 'host.sh: %s\n' "$*"; }
die() {
  printf 'host.sh: %s\n' "$*" >&2
  exit 1
}
usage() {
  cat >&2 <<'EOF'
usage: host.sh --inventory FILE --role gateway|load|upstream|statestore
               --lab gateway=IPV4 --lab load=IPV4 --lab upstream=IPV4 --lab statestore=IPV4
               [--known-hosts FILE] [--authorized-key FILE] [--valkey-image REF]
EOF
  exit 2
}

inventory="" role="" known_hosts_in="" authorized_key_in="" valkey_image=""
declare -A lab=()
while (($#)); do
  case "$1" in
    --inventory) inventory=${2:-}; shift 2 || usage ;;
    --role) role=${2:-}; shift 2 || usage ;;
    --lab)
      [[ ${2:-} == *=* ]] || usage
      lab[${2%%=*}]=${2#*=}
      shift 2
      ;;
    --known-hosts) known_hosts_in=${2:-}; shift 2 || usage ;;
    --authorized-key) authorized_key_in=${2:-}; shift 2 || usage ;;
    --valkey-image) valkey_image=${2:-}; shift 2 || usage ;;
    *) usage ;;
  esac
done

roles=(gateway load upstream statestore)
ipv4='^([0-9]{1,3}\.){3}[0-9]{1,3}$'
[[ -n $inventory && -f $inventory ]] || die "--inventory names no file"
case "$role" in gateway | load | upstream | statestore) ;; *) usage ;; esac
for r in "${roles[@]}"; do
  [[ ${lab[$r]:-} =~ $ipv4 ]] || die "--lab $r=IPV4 is missing: every role's lab address is required"
done
if [[ -n $known_hosts_in ]]; then
  [[ $role == gateway && -f $known_hosts_in ]] || die "--known-hosts names a file, for the gateway host"
fi
if [[ -n $authorized_key_in ]]; then
  [[ $role != gateway && -f $authorized_key_in ]] || die "--authorized-key names a file, for a host other than the gateway"
  key=$(head -n 1 "$authorized_key_in")
  [[ $key =~ ^ssh-ed25519\ [A-Za-z0-9+/=]+(\ .*)?$ ]] || die "--authorized-key holds no ssh-ed25519 public key"
fi

# 1. Debian, packages and automatic updates.
os_id=$(sed -n 's/^ID=//p' "$R/etc/os-release" | tr -d '"')
[[ $os_id == debian ]] || die "the settings baseline wants Debian stable, found ${os_id:-no /etc/os-release ID}"

export DEBIAN_FRONTEND=noninteractive
installed() { [[ $(dpkg-query -W -f='${Status}' "$1" 2>/dev/null || true) == "install ok installed" ]]; }
apt_install() {
  local missing=() p
  for p in "$@"; do
    installed "$p" || missing+=("$p")
  done
  if ((${#missing[@]})); then
    log "installing ${missing[*]}"
    apt-get update -q
    apt-get install -y -q --no-install-recommends "${missing[@]}"
  fi
}
apt_install jq ethtool iproute2 procps dbus-user-session ca-certificates openssh-server

jqi() { jq -er "$@" "$inventory"; }
profile=$(jqi .profile)
case "$profile" in
  rh-1) strict=1 ;;
  rh-3) strict=0 ;;
  *) die "inventory profile $profile: only rh-1 and rh-3 hosts are provisioned" ;;
esac
user=$(jqi .sshUser)
ssh_key=$(jqi .sshKey)
known_hosts=$(jqi .knownHosts)
secret_root=$(jqi .secretRoot)
tls_dir=$(jqi .tlsDir)
host_dir=$(jqi --arg r "$role" '.hosts[$r].dir')
cpus=$(jqi --arg r "$role" '.hosts[$r].cpus // ""')
aux_cpus=$(jqi --arg r "$role" '.hosts[$r].auxCpus // ""')
store_server=$(jqi '.services.stateStoreServer // ""')
[[ $user =~ ^[a-z_][a-z0-9_-]*$ ]] || die "sshUser $user is not a user name"
home=$(dirname "$(dirname "$ssh_key")")
[[ $(dirname "$ssh_key") == "$home/.ssh" && $(dirname "$known_hosts") == "$home/.ssh" ]] ||
  die "sshKey and knownHosts must be in one .ssh directory of the user's home"
[[ $secret_root == "$host_dir"/* && $tls_dir == "$host_dir"/* && /$secret_root/$tls_dir/ != */../* ]] ||
  die "secretRoot and tlsDir must be inside the host's working directory $host_dir"
if [[ $role == statestore && -z $store_server ]]; then
  [[ $valkey_image =~ ^valkey/valkey:9\.[^@]+@sha256:[0-9a-f]{64}$ ]] ||
    die "--valkey-image must be the pinned valkey/valkey:9.x@sha256 image, redisserver.ImageValkey9"
fi

case "$role" in
  gateway) apt_install git make curl tar gzip openssh-client openssl ;;
  statestore) [[ -n $store_server ]] || apt_install docker.io ;;
esac

if installed unattended-upgrades; then
  log "removing unattended-upgrades: updates only in a maintenance window"
  apt-get purge -y -q unattended-upgrades
fi
for timer in apt-daily.timer apt-daily-upgrade.timer; do
  if [[ $(systemctl is-enabled "$timer" 2>/dev/null || true) != masked ]]; then
    systemctl disable --now "$timer" >/dev/null 2>&1 || true
    systemctl mask "$timer"
  fi
done

# write_file PATH MODE writes stdin to PATH under R when the content
# differs and sets changed=1 then, else changed=0.
changed=0
write_file() {
  local path=$R$1 mode=$2
  cat >"$work/new"
  changed=0
  if [[ ! -f $path ]] || ! cmp -s "$work/new" "$path"; then
    mkdir -p "$(dirname "$path")"
    install -m "$mode" "$work/new" "$path"
    changed=1
  else
    chmod "$mode" "$path"
  fi
}

# as_user runs a command as the dedicated user, from /.
as_user() { (cd / && runuser -u "$user" -- "$@"); }

# user_file PATH MODE writes stdin to PATH under R as the user when the
# content differs, and sets MODE.
user_file() {
  # shellcheck disable=SC2016 # the user's bash expands it
  local write='umask 077 && cat >"$1.new" && if cmp -s "$1.new" "$1"; then rm -f "$1.new"; else mv -f "$1.new" "$1"; fi && chmod "$2" "$1"'
  as_user bash -c "$write" user_file "$R$1" "$2"
}

# 2. The dedicated user.
if ! getent passwd "$user" >/dev/null; then
  log "creating the user $user"
  useradd --create-home --home-dir "$home" --shell /bin/bash --user-group "$user"
fi
uid=$(id -u "$user")
group=$(id -gn "$user")
if [[ $role == statestore && -z $store_server ]]; then
  usermod -aG docker "$user"
fi
loginctl enable-linger "$user"
write_file "/etc/systemd/system/user@$uid.service.d/90-ruralz-bench.conf" 0644 <<'EOF'
# deploy/benchhost: the harness starts each benchmark process in a
# systemd-run scope of this manager, on its cpuset, with the settings
# baseline's open-file limit.
[Service]
Delegate=cpu cpuset io memory pids
LimitNOFILE=1048576
EOF
if ((changed)); then
  systemctl daemon-reload
  systemctl restart "user@$uid.service"
fi
write_file /etc/security/limits.d/90-ruralz-bench.conf 0644 <<EOF
# deploy/benchhost: LimitNOFILE 1,048,576 for every benchmark process.
$user soft nofile 1048576
$user hard nofile 1048576
EOF

# 3. Directories the harness writes: root makes the working directory, in
# a directory only root writes, and gives it to the user, who makes the
# secret root and the TLS directory inside it.
mkdir -p "$R$host_dir"
chown "$user:$group" "$R$host_dir"
chmod 0750 "$R$host_dir"
as_user mkdir -p "$R$secret_root" "$R$tls_dir"
as_user chmod 0700 "$R$secret_root" "$R$tls_dir"

# 4. /etc/hosts: each role's address and each service's host on the lab
# address of the role that serves it.
is_ip() { [[ $1 =~ $ipv4 || $1 == *:* ]]; }
declare -A names=()
add_name() {
  if [[ -n $2 ]] && ! is_ip "$2"; then
    names[$1]="${names[$1]:-} $2"
  fi
}
for r in "${roles[@]}"; do
  add_name "$r" "$(jqi --arg r "$r" '.hosts[$r].address')"
done
for svc in mock:upstream idp:upstream sink:upstream stateStore:statestore faultProxy:statestore; do
  addr=$(jqi --arg s "${svc%%:*}" '.services[$s]')
  add_name "${svc#*:}" "${addr%:*}"
done
{
  sed '/^# BEGIN ruralz-bench/,/^# END ruralz-bench/d' "$R/etc/hosts"
  echo "# BEGIN ruralz-bench (deploy/benchhost)"
  for r in "${roles[@]}"; do
    if [[ -n ${names[$r]:-} ]]; then
      # shellcheck disable=SC2086 # one name per word
      echo "${lab[$r]} $(printf '%s\n' ${names[$r]} | sort -u | paste -sd ' ')"
    fi
  done
  echo "# END ruralz-bench"
} >"$work/hosts"
write_file /etc/hosts 0644 <"$work/hosts"

# 5. ssh, in the user's home, as the user.
as_user mkdir -p "$R$home/.ssh"
as_user chmod 0700 "$R$home/.ssh"
if [[ $role == gateway ]]; then
  if ! as_user test -f "$R$ssh_key"; then
    log "creating the harness's key pair $ssh_key"
    as_user ssh-keygen -q -t ed25519 -N "" -C "$user@rh-1" -f "$R$ssh_key"
  fi
  as_user chmod 0600 "$R$ssh_key"
  if [[ -n $known_hosts_in ]]; then
    user_file "$known_hosts" 0644 <"$known_hosts_in"
  fi
elif [[ -n $authorized_key_in ]]; then
  printf 'restrict,from="%s" %s\n' "${lab[gateway]}" "$key" >"$work/authorized_keys"
  user_file "$home/.ssh/authorized_keys" 0600 <"$work/authorized_keys"
fi

# 6. sysctls, swap and irqbalance.
{
  echo "# deploy/benchhost: the settings baseline's sysctls."
  echo "net.ipv4.ip_local_port_range = 1024 65535"
  echo "net.core.somaxconn = 65535"
  echo "net.ipv4.tcp_max_syn_backlog = 65535"
  echo "net.core.netdev_max_backlog = 65536"
  echo "vm.swappiness = 0"
  # TIME_WAIT reuse on RH-1 and RH-1L, the hosts that open connections.
  if [[ $role == gateway || $role == load ]]; then
    echo "net.ipv4.tcp_tw_reuse = 1"
  fi
} >"$work/sysctl"
write_file /etc/sysctl.d/90-ruralz-bench.conf 0644 <"$work/sysctl"
sysctl -q -p "$R/etc/sysctl.d/90-ruralz-bench.conf"
swapoff -a
swap_entry='^[^#[:space:]]+[[:space:]]+[^[:space:]]+[[:space:]]+swap[[:space:]]'
if grep -Eq "$swap_entry" "$R/etc/fstab"; then
  log "commenting out the swap entries of /etc/fstab"
  sed -E "s/($swap_entry.*)\$/# ruralz-bench swap off: \\1/" "$R/etc/fstab" >"$work/fstab"
  write_file /etc/fstab 0644 <"$work/fstab"
  systemctl daemon-reload
fi
if [[ $(systemctl is-enabled irqbalance.service 2>/dev/null || true) =~ ^(enabled|disabled|static|alias|indirect|linked)$ ]]; then
  systemctl disable --now irqbalance.service >/dev/null 2>&1 || true
  systemctl mask irqbalance.service
fi

# 7. tune.sh, at every boot and now.
measured=$cpus
if [[ -n $aux_cpus ]]; then measured=${measured:+$measured,}$aux_cpus; fi
write_file /usr/local/lib/ruralz-bench/tune.sh 0755 <"$self_dir/tune.sh"
write_file /etc/ruralz-bench/tune.conf 0644 <<EOF
# deploy/benchhost: what tune.sh applies at every boot on this host.
STRICT=$strict
LAB_IP=${lab[$role]}
MEASURED_CPUS=$measured
EOF
write_file /etc/systemd/system/ruralz-bench-tune.service 0644 <<'EOF'
[Unit]
Description=Ruralz benchmark settings baseline: CPU, NIC and interrupts (deploy/benchhost)
Wants=network-online.target
After=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/local/lib/ruralz-bench/tune.sh

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable ruralz-bench-tune.service
BENCHHOST_ROOT=$R bash "$R/usr/local/lib/ruralz-bench/tune.sh"

# 8. The State Store's image.
if [[ $role == statestore && -z $store_server ]]; then
  systemctl enable --now docker.service
  if ! docker image inspect "$valkey_image" >/dev/null 2>&1; then
    log "pulling $valkey_image"
    docker pull -q "$valkey_image"
  fi
fi

# 9. Every installed package held.
dpkg-query -W -f='${binary:Package}\n' | sort -u >"$work/packages"
apt-mark showhold | sort -u >"$work/held"
comm -23 "$work/packages" "$work/held" >"$work/hold"
if [[ -s $work/hold ]]; then
  xargs apt-mark hold <"$work/hold" >/dev/null
fi

log "$role host of $profile: settings baseline applied"
