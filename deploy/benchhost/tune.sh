#!/usr/bin/env bash
# Copyright 2026 Revington
# SPDX-License-Identifier: Apache-2.0
#
# tune.sh applies the settings baseline that does not survive a reboot
# (docs/architecture/12-performance-budgets-and-benchmarking.md,
# "Reference hardware"): ruralz-bench-tune.service runs it at every boot
# and host.sh once when it provisions the host.
#
#   CPU         SMT off; turbo off; the performance governor with the
#               minimum frequency at the fixed maximum, 3.0 GHz or more
#               (target); idle states deeper than C1 disabled
#   THP         madvise
#   NIC         the lab NIC's rings at their maximum; adaptive interrupt
#               coalescing off, rx-usecs and tx-usecs 0
#   Interrupts  the lab NIC's queue interrupts on the cores of its NUMA node
#               outside the host's measured cpusets
#
# It reads /etc/ruralz-bench/tune.conf, which host.sh writes: STRICT=1 makes
# every setting the host cannot apply a failure (rh-1), STRICT=0 skips it
# with a line (rh-3); LAB_IP selects the NIC; MEASURED_CPUS lists the
# cpusets of the host's measured processes. A second run changes nothing.
# BENCHHOST_ROOT prefixes every file path (the tests run it on a fake tree).
set -euo pipefail

# One collation for sort and comm.
export LC_ALL=C
R=${BENCHHOST_ROOT:-}
cpu=$R/sys/devices/system/cpu
min_khz=3000000

log() { printf 'tune.sh: %s\n' "$*"; }
die() {
  printf 'tune.sh: %s\n' "$*" >&2
  exit 1
}

STRICT=1 LAB_IP="" MEASURED_CPUS=""
while IFS='=' read -r k v; do
  case "$k" in
    STRICT) STRICT=$v ;;
    LAB_IP) LAB_IP=$v ;;
    MEASURED_CPUS) MEASURED_CPUS=$v ;;
  esac
done <"$R/etc/ruralz-bench/tune.conf"
[[ $STRICT == 0 || $STRICT == 1 ]] || die "tune.conf: STRICT is 0 or 1"
[[ -n $LAB_IP ]] || die "tune.conf: no LAB_IP"

# unmet reports a setting the host cannot apply: a failure when strict.
unmet() {
  if ((STRICT)); then
    die "$*"
  fi
  log "skipped: $*"
}

# set_sys FILE VALUE writes VALUE to FILE unless it already reads VALUE.
set_sys() {
  if [[ $(<"$1") != "$2" ]]; then
    printf '%s\n' "$2" >"$1"
  fi
}

# expand_cpus LIST prints the CPUs of a cpu list such as 0-3,8 one per line.
expand_cpus() {
  local part lo hi
  for part in ${1//,/ }; do
    lo=${part%-*} hi=${part#*-}
    seq "$lo" "$hi"
  done | sort -n -u
}

# join_cpus prints the sorted CPUs on stdin as a cpu list such as 0-3,8.
join_cpus() {
  awk 'NR == 1 { lo = hi = $1; next }
       $1 == hi + 1 { hi = $1; next }
       { printf "%s%s,", lo, (hi > lo ? "-" hi : ""); lo = hi = $1 }
       END { if (NR) printf "%s%s\n", lo, (hi > lo ? "-" hi : "") }'
}

# CPU: SMT, then turbo, then governor and frequency.
smt=$cpu/smt/control
if [[ -f $smt ]]; then
  case $(<"$smt") in
    on) printf 'off\n' >"$smt" ;;
    off | forceoff | notsupported | notimplemented) ;;
    *) unmet "SMT control reads $(<"$smt")" ;;
  esac
  if [[ -f $cpu/smt/active && $(<"$cpu/smt/active") != 0 ]]; then
    unmet "SMT is still active"
  fi
else
  unmet "no SMT control at /sys/devices/system/cpu/smt/control"
fi

turbo=0
if [[ -f $cpu/intel_pstate/no_turbo ]]; then
  set_sys "$cpu/intel_pstate/no_turbo" 1
  turbo=1
fi
if [[ -f $cpu/cpufreq/boost ]]; then
  set_sys "$cpu/cpufreq/boost" 0
  turbo=1
fi
for b in "$cpu"/cpufreq/policy*/boost; do
  if [[ -f $b ]]; then
    set_sys "$b" 0
    turbo=1
  fi
done
((turbo)) || unmet "no turbo control (intel_pstate/no_turbo or cpufreq boost)"

policies=0
for p in "$cpu"/cpufreq/policy*; do
  [[ -d $p ]] || continue
  policies=$((policies + 1))
  if [[ " $(<"$p/scaling_available_governors") " != *" performance "* ]]; then
    unmet "$(basename "$p") has no performance governor"
    continue
  fi
  set_sys "$p/scaling_governor" performance
  target=$(<"$p/scaling_max_freq")
  if [[ -f $p/base_frequency ]] && (($(<"$p/base_frequency") < target)); then
    target=$(<"$p/base_frequency")
  fi
  if (($(<"$p/scaling_min_freq") > target)); then
    set_sys "$p/scaling_min_freq" "$target"
  fi
  set_sys "$p/scaling_max_freq" "$target"
  set_sys "$p/scaling_min_freq" "$target"
  if (($(<"$p/scaling_min_freq") != $(<"$p/scaling_max_freq"))); then
    unmet "$(basename "$p") frequency is not fixed"
  elif ((target < min_khz)); then
    unmet "$(basename "$p") is fixed at $target kHz, below $min_khz"
  fi
done
((policies)) || unmet "no cpufreq policies"

for s in "$cpu"/cpu[0-9]*/cpuidle/state[0-9]*; do
  [[ -d $s ]] || continue
  case $(<"$s/name") in
    POLL | C1 | C1_ACPI) ;;
    *) set_sys "$s/disable" 1 ;;
  esac
done

thp=$R/sys/kernel/mm/transparent_hugepage/enabled
if [[ -f $thp ]]; then
  [[ $(<"$thp") == *"[madvise]"* ]] || printf 'madvise\n' >"$thp"
else
  unmet "no transparent huge page control"
fi

# NIC: the interface that carries the lab address.
nic=$(ip -o -4 addr show | awk -v ip="$LAB_IP" '{ split($4, a, "/"); if (a[1] == ip) { print $2; exit } }')
if [[ -z $nic ]]; then
  unmet "no interface carries the lab address $LAB_IP"
  log "settings baseline applied (no NIC)"
  exit 0
fi

rings=$(ethtool -g "$nic" | awk '
  /^Pre-set maximums:/ { sec = "max"; next }
  /^Current hardware settings:/ { sec = "cur"; next }
  sec != "" && ($1 == "RX:" || $1 == "TX:") { v[sec $1] = $2 }
  END { print v["maxRX:"], v["maxTX:"], v["curRX:"], v["curTX:"] }')
read -r max_rx max_tx cur_rx cur_tx <<<"$rings"
if [[ $max_rx =~ ^[0-9]+$ && $max_tx =~ ^[0-9]+$ ]]; then
  if [[ $cur_rx != "$max_rx" || $cur_tx != "$max_tx" ]]; then
    ethtool -G "$nic" rx "$max_rx" tx "$max_tx"
  fi
else
  unmet "$nic reports no ring maximums"
fi

coalesce=()
while read -r key value; do
  case "$key" in
    adaptive-rx | adaptive-tx) [[ $value == off || $value == n/a ]] || coalesce+=("$key" off) ;;
    rx-usecs | tx-usecs) [[ $value == 0 || $value == n/a ]] || coalesce+=("$key" 0) ;;
  esac
done < <(ethtool -c "$nic" | awk '
  $1 == "Adaptive" && $2 == "RX:" { print "adaptive-rx", $3; print "adaptive-tx", $5 }
  $1 == "rx-usecs:" { print "rx-usecs", $2 }
  $1 == "tx-usecs:" { print "tx-usecs", $2 }')
if ((${#coalesce[@]})); then
  ethtool -C "$nic" "${coalesce[@]}"
fi

# Interrupts: the NIC's NUMA node's online cores outside the measured cpusets.
online=$(expand_cpus "$(<"$cpu/online")")
node=-1
if [[ -f $R/sys/class/net/$nic/device/numa_node ]]; then
  node=$(<"$R/sys/class/net/$nic/device/numa_node")
fi
if ((node >= 0)); then
  node_cpus=$(comm -12 <(expand_cpus "$(<"$R/sys/devices/system/node/node$node/cpulist")" | sort) <(sort <<<"$online") | sort -n)
else
  node_cpus=$online
fi
measured=""
if [[ -n $MEASURED_CPUS ]]; then
  measured=$(expand_cpus "$MEASURED_CPUS")
  outside=$(comm -23 <(sort <<<"$measured") <(sort <<<"$node_cpus") | sort -n | join_cpus)
  [[ -z $outside ]] || unmet "measured CPUs $outside are not online cores of $nic's NUMA node"
fi
housekeeping=$(comm -23 <(sort <<<"$node_cpus") <(sort <<<"$measured" | sed '/^$/d') | sort -n | join_cpus)
irqs=0
if [[ -z $housekeeping ]]; then
  unmet "no core of $nic's NUMA node is left for its interrupts"
else
  for i in "$R/sys/class/net/$nic/device/msi_irqs"/*; do
    [[ -e $i ]] || continue
    irqs=$((irqs + 1))
    f=$R/proc/irq/$(basename "$i")/smp_affinity_list
    if [[ ! -f $f ]]; then
      unmet "interrupt $(basename "$i") of $nic has no affinity file"
    elif [[ $(expand_cpus "$(<"$f")" | join_cpus) != "$housekeeping" ]]; then
      printf '%s\n' "$housekeeping" >"$f" || unmet "interrupt $(basename "$i") of $nic refuses cores $housekeeping"
    fi
  done
  ((irqs)) || unmet "$nic has no MSI interrupts to pin"
fi

log "settings baseline applied: NIC $nic, interrupts on ${housekeeping:-no cores}, measured ${MEASURED_CPUS:-none}"
