// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package benchhost

import (
	"maps"
	"strconv"
	"strings"
)

// The fake host's facts: its lab address on interface nic, the uid the
// stand-in id gives every user, the fixed frequency after turbo is off and
// the interrupts of the NIC.
const (
	nic         = "enp1s0"
	userUID     = "1001"
	baseFreqKHz = 3200000
	cpuCount    = 16
)

// nicIRQs are the fake NIC's MSI interrupts.
func nicIRQs() []string { return []string{"40", "41", "42"} }

// hostStandins are the stand-ins of host.sh and tune.sh: package, unit,
// user, network and container tools that keep their state under
// $STUB_STATE.
func hostStandins() map[string]string {
	return map[string]string{
		"dpkg-query": `case "${2:-}" in
  '-f=${Status}') grep -qxF -- "$3" "$STUB_STATE/installed" || exit 1; printf 'install ok installed' ;;
  '-f=${binary:Package}\n') cat "$STUB_STATE/installed" ;;
  *) exit 2 ;;
esac
`,
		"apt-get": `case "$1" in
  install) shift; for a in "$@"; do [[ $a == -* ]] || echo "$a" >>"$STUB_STATE/installed"; done ;;
  purge) shift; for a in "$@"; do [[ $a == -* ]] || { grep -vxF -- "$a" "$STUB_STATE/installed" >"$STUB_STATE/installed.new" || true; mv "$STUB_STATE/installed.new" "$STUB_STATE/installed"; }; done ;;
esac
`,
		"apt-mark": `touch "$STUB_STATE/held"
case "$1" in
  showhold) cat "$STUB_STATE/held" ;;
  hold) shift; printf '%s\n' "$@" >>"$STUB_STATE/held" ;;
esac
`,
		"systemctl": `mkdir -p "$STUB_STATE/units"
unit=${*: -1}
case "$1" in
  is-enabled) cat "$STUB_STATE/units/$unit" 2>/dev/null || exit 1 ;;
  mask) echo masked >"$STUB_STATE/units/$unit" ;;
  disable) [[ $(cat "$STUB_STATE/units/$unit" 2>/dev/null) == masked ]] || echo disabled >"$STUB_STATE/units/$unit" ;;
  enable) echo enabled >"$STUB_STATE/units/$unit" ;;
esac
`,
		"getent": `touch "$STUB_STATE/users"
grep -qxF -- "$2" "$STUB_STATE/users" || exit 2
`,
		"useradd":  `echo "${*: -1}" >>"$STUB_STATE/users"` + "\n",
		"id":       `case "$1" in -u) echo ` + userUID + ` ;; -gn) echo "$2" ;; *) exit 1 ;; esac` + "\n",
		"usermod":  "",
		"loginctl": "",
		"chown":    "",
		"sysctl":   "",
		"swapoff":  "",
		"runuser":  "",
		"ssh-keygen": `while (($#)); do if [[ $1 == -f ]]; then f=$2; fi; shift; done
echo PRIVATE >"$f"
echo "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIHARNESS ruralz-bench@rh-1" >"$f.pub"
`,
		"docker": `touch "$STUB_STATE/images"
case "$1 ${2:-}" in
  "image inspect") grep -qxF -- "$3" "$STUB_STATE/images" ;;
  "pull -q") echo "$3" >>"$STUB_STATE/images" ;;
esac
`,
		"ip": `cat "$STUB_STATE/ip-addr"` + "\n",
		"ethtool": `rings=$STUB_STATE/rings coal=$STUB_STATE/coalesce
case "$1" in
  -g) read -r rx tx <"$rings"; printf 'Ring parameters for %s:\nPre-set maximums:\nRX:\t\t4096\nRX Mini:\tn/a\nRX Jumbo:\tn/a\nTX:\t\t4096\nCurrent hardware settings:\nRX:\t\t%s\nRX Mini:\tn/a\nRX Jumbo:\tn/a\nTX:\t\t%s\n' "$2" "$rx" "$tx" ;;
  -G) echo "$4 $6" >"$rings" ;;
  -c) read -r arx atx rus tus <"$coal"; printf 'Coalesce parameters for %s:\nAdaptive RX: %s  TX: %s\nstats-block-usecs: n/a\nrx-usecs: %s\nrx-frames: n/a\ntx-usecs: %s\ntx-frames: n/a\n' "$2" "$arx" "$atx" "$rus" "$tus" ;;
  -C) read -r arx atx rus tus <"$coal"; shift 2
      while (($#)); do case "$1" in adaptive-rx) arx=$2 ;; adaptive-tx) atx=$2 ;; rx-usecs) rus=$2 ;; tx-usecs) tus=$2 ;; esac; shift 2; done
      echo "$arx $atx $rus $tus" >"$coal" ;;
esac
`,
	}
}

// with returns the stand-ins of base with extra added or replaced.
func with(base, extra map[string]string) map[string]string {
	out := maps.Clone(base)
	maps.Copy(out, extra)
	return out
}

// hostFacts configure the fake host tree.
type hostFacts struct {
	// labIP is the address of the NIC.
	labIP string
	// maxKHz is each policy's scaling_max_freq before tuning.
	maxKHz int
	// noCPUFreq, noNIC and noSMT leave out cpufreq, the NIC and the SMT
	// control, as on a cloud VM.
	noCPUFreq, noNIC, noSMT bool
	// os is /etc/os-release's ID.
	os string
}

// fakeHost writes the fake host: os-release, hosts, an fstab with a swap
// entry, and the sysfs and procfs files tune.sh reads and writes for 16
// online cores on one NUMA node, the NIC's three interrupts, and the
// stand-ins' package, NIC and interface state.
func (r *rig) fakeHost(f hostFacts) {
	r.t.Helper()
	if f.os == "" {
		f.os = "debian"
	}
	if f.maxKHz == 0 {
		f.maxKHz = 3400000
	}
	r.write("/etc/os-release", "PRETTY_NAME=\"Debian GNU/Linux 13 (trixie)\"\nID="+f.os+"\n")
	r.write("/etc/hosts", "127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost\n")
	r.write("/etc/fstab", "UUID=1 / ext4 errors=remount-ro 0 1\nUUID=2 none swap sw 0 0\n")
	cpu := "/sys/devices/system/cpu"
	r.write(cpu+"/online", "0-15\n")
	if !f.noSMT {
		r.write(cpu+"/smt/control", "on\n")
		r.write(cpu+"/smt/active", "0\n")
	}
	if !f.noCPUFreq {
		r.write(cpu+"/intel_pstate/no_turbo", "0\n")
		for i := range cpuCount {
			p := cpu + "/cpufreq/policy" + strconv.Itoa(i)
			r.write(p+"/scaling_available_governors", "performance powersave\n")
			r.write(p+"/scaling_governor", "powersave\n")
			r.write(p+"/scaling_max_freq", strconv.Itoa(f.maxKHz)+"\n")
			r.write(p+"/scaling_min_freq", "800000\n")
			r.write(p+"/base_frequency", strconv.Itoa(min(baseFreqKHz, f.maxKHz))+"\n")
		}
	}
	for i := range cpuCount {
		for s, name := range []string{"POLL", "C1", "C1E", "C6"} {
			d := cpu + "/cpu" + strconv.Itoa(i) + "/cpuidle/state" + strconv.Itoa(s)
			r.write(d+"/name", name+"\n")
			r.write(d+"/disable", "0\n")
		}
	}
	r.write("/sys/kernel/mm/transparent_hugepage/enabled", "[always] madvise never\n")
	r.write("/sys/devices/system/node/node0/cpulist", "0-15\n")
	if !f.noNIC {
		r.write("/sys/class/net/"+nic+"/device/numa_node", "0\n")
		for _, irq := range nicIRQs() {
			r.write("/sys/class/net/"+nic+"/device/msi_irqs/"+irq, "msix\n")
			r.write("/proc/irq/"+irq+"/smp_affinity_list", "0-15\n")
		}
		r.writeState("ip-addr", "1: lo    inet 127.0.0.1/8 scope host lo\\       valid_lft forever preferred_lft forever\n"+
			"2: "+nic+"    inet "+f.labIP+"/24 brd 10.0.0.255 scope global "+nic+"\\       valid_lft forever preferred_lft forever\n")
	} else {
		r.writeState("ip-addr", "1: lo    inet 127.0.0.1/8 scope host lo\\       valid_lft forever preferred_lft forever\n")
	}
	r.writeState("rings", "512 512\n")
	r.writeState("coalesce", "on on 3 16\n")
	// A fresh Debian install: unattended upgrades and the apt timers on,
	// irqbalance running.
	r.writeState("installed", strings.Join([]string{"base-files", "systemd", "unattended-upgrades"}, "\n")+"\n")
	r.writeState("units/apt-daily.timer", "enabled\n")
	r.writeState("units/apt-daily-upgrade.timer", "enabled\n")
	r.writeState("units/irqbalance.service", "enabled\n")
}
