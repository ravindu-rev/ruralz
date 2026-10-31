// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package benchhost

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/statestore/statestoretest/redisserver"
)

// The lab addresses the tests give the four hosts.
func labIPs() map[string]string {
	return map[string]string{"gateway": "10.0.0.1", "load": "10.0.0.2", "upstream": "10.0.0.3", "statestore": "10.0.0.4"}
}

// The harness's public key the tests authorize, and the remote host keys
// they trust.
const harnessKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIHARNESS ruralz-bench@rh-1"

// hostArgs returns host.sh's arguments for role of the committed inventory
// profile.
func hostArgs(t *testing.T, profile, role string, extra ...string) []string {
	args := []string{"--inventory", script(t, profile+".json"), "--role", role}
	for _, r := range []string{"gateway", "load", "upstream", "statestore"} {
		args = append(args, "--lab", r+"="+labIPs()[r])
	}
	return append(args, extra...)
}

// hostRig returns a rig of a fresh Debian host with the lab address of
// role.
func hostRig(t *testing.T, role string, f hostFacts) *rig {
	t.Helper()
	r := newRig(t, hostStandins())
	f.labIP = labIPs()[role]
	r.fakeHost(f)
	return r
}

// file writes a file outside the fake tree, for host.sh's inputs.
func file(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "input")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestHostGatewayRH1 provisions RH-1 from the committed rh-1 inventory and
// checks every setting of the baseline the gateway host takes.
func TestHostGatewayRH1(t *testing.T) {
	r := hostRig(t, "gateway", hostFacts{})
	known := "load.bench.example ssh-ed25519 AAAAL\nupstream-mock.bench.example ssh-ed25519 AAAAU\nstatestore.bench.example ssh-ed25519 AAAAS\n"
	r.mustRun("host.sh", nil, hostArgs(t, "rh-1", "gateway", "--known-hosts", file(t, known))...)
	calls := r.calls()

	t.Run("hosts", func(t *testing.T) {
		want := "127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost\n" +
			"# BEGIN ruralz-bench (deploy/benchhost)\n" +
			"10.0.0.1 gateway.bench.example\n" +
			"10.0.0.2 load.bench.example\n" +
			"10.0.0.3 idp.bench.example otel-sink.bench.example upstream-mock.bench.example\n" +
			"10.0.0.4 statestore.bench.example\n" +
			"# END ruralz-bench\n"
		if got := r.read("/etc/hosts"); got != want {
			t.Errorf("/etc/hosts =\n%s\nwant\n%s", got, want)
		}
	})
	t.Run("sysctls", func(t *testing.T) {
		got := r.read("/etc/sysctl.d/90-ruralz-bench.conf")
		for _, want := range []string{
			"net.ipv4.ip_local_port_range = 1024 65535\n", "net.core.somaxconn = 65535\n",
			"net.ipv4.tcp_max_syn_backlog = 65535\n", "net.core.netdev_max_backlog = 65536\n",
			"vm.swappiness = 0\n", "net.ipv4.tcp_tw_reuse = 1\n",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("sysctls lack %q:\n%s", want, got)
			}
		}
		if !called(calls, "sysctl", "-q", "-p", r.path("/etc/sysctl.d/90-ruralz-bench.conf")) {
			t.Error("the sysctls were not applied")
		}
	})
	t.Run("swap", func(t *testing.T) {
		if !called(calls, "swapoff", "-a") {
			t.Error("swap was not turned off")
		}
		if got := r.read("/etc/fstab"); !strings.Contains(got, "\n# ruralz-bench swap off: UUID=2 none swap sw 0 0\n") || !strings.HasPrefix(got, "UUID=1 / ext4") {
			t.Errorf("/etc/fstab =\n%s\nwant its swap entry commented out", got)
		}
	})
	t.Run("user", func(t *testing.T) {
		if !called(calls, "useradd", "--create-home", "--home-dir", "/home/ruralz-bench", "--shell", "/bin/bash", "--user-group", "ruralz-bench") {
			t.Error("the dedicated user was not created")
		}
		if !called(calls, "loginctl", "enable-linger", "ruralz-bench") {
			t.Error("the user manager does not linger")
		}
		dropIn := r.read("/etc/systemd/system/user@" + userUID + ".service.d/90-ruralz-bench.conf")
		if !strings.Contains(dropIn, "\nDelegate=cpu cpuset io memory pids\n") || !strings.Contains(dropIn, "\nLimitNOFILE=1048576\n") {
			t.Errorf("user manager drop-in =\n%s\nwant cpuset delegated and LimitNOFILE 1048576", dropIn)
		}
		if !called(calls, "systemctl", "restart", "user@"+userUID+".service") {
			t.Error("the user manager did not restart onto its delegation")
		}
		if got, want := r.read("/etc/security/limits.d/90-ruralz-bench.conf"), "ruralz-bench soft nofile 1048576\nruralz-bench hard nofile 1048576\n"; !strings.HasSuffix(got, want) {
			t.Errorf("limits =\n%s\nwant %q", got, want)
		}
	})
	t.Run("directories", func(t *testing.T) {
		dirs := []string{"/var/lib/ruralz-bench", "/var/lib/ruralz-bench/secrets", "/var/lib/ruralz-bench/secrets/tls"}
		for i, want := range []os.FileMode{0o750, 0o700, 0o700} {
			if got := r.mode(dirs[i]); got != want {
				t.Errorf("%s mode %v, want %v", dirs[i], got, want)
			}
		}
		if !called(calls, "chown", "ruralz-bench:ruralz-bench", r.path(dirs[0]), r.path(dirs[1]), r.path(dirs[2])) {
			t.Error("the working directory, secret root and TLS directory are not the user's")
		}
	})
	t.Run("ssh", func(t *testing.T) {
		if !called(calls, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "ruralz-bench@rh-1", "-f", r.path("/home/ruralz-bench/.ssh/id_ed25519")) {
			t.Error("the harness's key pair was not created")
		}
		if got := r.read("/home/ruralz-bench/.ssh/known_hosts"); got != known {
			t.Errorf("known_hosts = %q, want %q", got, known)
		}
		if got := r.mode("/home/ruralz-bench/.ssh/id_ed25519"); got != 0o600 {
			t.Errorf("private key mode %v", got)
		}
	})
	t.Run("packages and updates", func(t *testing.T) {
		for _, p := range []string{"jq", "ethtool", "dbus-user-session", "git", "make", "curl", "openssh-client", "openssl"} {
			if !slices.ContainsFunc(calls, func(c []string) bool {
				return len(c) > 1 && c[0] == "apt-get" && c[1] == "install" && slices.Contains(c, p)
			}) {
				t.Errorf("%s was not installed", p)
			}
		}
		if slices.ContainsFunc(calls, func(c []string) bool { return c[0] == "apt-get" && slices.Contains(c, "docker.io") }) {
			t.Error("the gateway host installed docker.io")
		}
		if !called(calls, "apt-get", "purge", "-y", "-q", "unattended-upgrades") {
			t.Error("unattended upgrades were kept")
		}
		for _, timer := range []string{"apt-daily.timer", "apt-daily-upgrade.timer", "irqbalance.service"} {
			if !called(calls, "systemctl", "mask", timer) {
				t.Errorf("%s is not masked", timer)
			}
		}
		if !called(calls, "apt-mark", "hold") {
			t.Error("the packages are not held")
		}
	})
	t.Run("tuning", func(t *testing.T) {
		if got := r.read("/etc/ruralz-bench/tune.conf"); !strings.Contains(got, "\nSTRICT=1\nLAB_IP=10.0.0.1\nMEASURED_CPUS=2-5\n") {
			t.Errorf("tune.conf =\n%s", got)
		}
		if got := r.read("/usr/local/lib/ruralz-bench/tune.sh"); got != readFile(t, script(t, "tune.sh")) || r.mode("/usr/local/lib/ruralz-bench/tune.sh") != 0o755 {
			t.Error("tune.sh is not installed as an executable copy")
		}
		unit := r.read("/etc/systemd/system/ruralz-bench-tune.service")
		if !strings.Contains(unit, "ExecStart=/usr/local/lib/ruralz-bench/tune.sh\n") || !strings.Contains(unit, "WantedBy=multi-user.target") {
			t.Errorf("ruralz-bench-tune.service =\n%s", unit)
		}
		if !called(calls, "systemctl", "enable", "ruralz-bench-tune.service") {
			t.Error("the tuning does not run at boot")
		}
		checkTuned(t, r, "0-1,6-15", calls)
	})
}

// readFile returns a file outside the fake tree.
func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p) //nolint:gosec // G304: a repository file
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// checkTuned checks tune.sh's settings on the fake host: SMT and turbo
// off, the performance governor fixed at the base frequency, idle states
// deeper than C1 disabled, THP madvise, the NIC's rings at their maximum
// and coalescing off, and its interrupts on irqCPUs.
func checkTuned(t *testing.T, r *rig, irqCPUs string, calls [][]string) {
	t.Helper()
	cpu := "/sys/devices/system/cpu"
	for p, want := range map[string]string{cpu + "/smt/control": "off\n", cpu + "/intel_pstate/no_turbo": "1\n", "/sys/kernel/mm/transparent_hugepage/enabled": "madvise\n"} {
		if got := r.read(p); got != want {
			t.Errorf("%s = %q, want %q", p, got, want)
		}
	}
	for i := range cpuCount {
		p := cpu + "/cpufreq/policy" + strconv.Itoa(i)
		f := strconv.Itoa(baseFreqKHz) + "\n"
		if r.read(p+"/scaling_governor") != "performance\n" || r.read(p+"/scaling_min_freq") != f || r.read(p+"/scaling_max_freq") != f {
			t.Errorf("policy%d: governor %q, min %q, max %q; want performance fixed at %d kHz", i,
				r.read(p+"/scaling_governor"), r.read(p+"/scaling_min_freq"), r.read(p+"/scaling_max_freq"), baseFreqKHz)
		}
		for s, want := range []string{"0\n", "0\n", "1\n", "1\n"} {
			if got := r.read(cpu + "/cpu" + strconv.Itoa(i) + "/cpuidle/state" + strconv.Itoa(s) + "/disable"); got != want {
				t.Errorf("cpu%d state%d disable = %q, want %q (POLL and C1 kept)", i, s, got, want)
			}
		}
	}
	if !called(calls, "ethtool", "-G", nic, "rx", "4096", "tx", "4096") {
		t.Error("the NIC's rings are not at their maximum")
	}
	if !called(calls, "ethtool", "-C", nic, "adaptive-rx", "off", "adaptive-tx", "off", "rx-usecs", "0", "tx-usecs", "0") {
		t.Error("the NIC's interrupt coalescing is not off")
	}
	for _, irq := range nicIRQs() {
		if got := r.read("/proc/irq/" + irq + "/smp_affinity_list"); got != irqCPUs+"\n" {
			t.Errorf("interrupt %s on %q, want %s", irq, got, irqCPUs)
		}
	}
}

// TestHostIdempotent: a second run of host.sh with the same arguments
// changes no file and makes no changing call.
func TestHostIdempotent(t *testing.T) {
	for _, role := range []string{"gateway", "statestore"} {
		t.Run(role, func(t *testing.T) {
			r := hostRig(t, role, hostFacts{})
			args := hostArgs(t, "rh-1", role, "--known-hosts", file(t, "load.bench.example ssh-ed25519 AAAAL\n"))
			if role == "statestore" {
				args = hostArgs(t, "rh-1", role, "--authorized-key", file(t, harnessKey+"\n"), "--valkey-image", redisserver.ImageValkey9)
			}
			r.mustRun("host.sh", nil, args...)
			first := r.snapshot()
			_ = r.calls()
			r.mustRun("host.sh", nil, args...)
			calls := r.calls()
			second := r.snapshot()
			if !maps.Equal(first, second) {
				for k, v := range second {
					if first[k] != v {
						t.Errorf("the second run changed %s", k)
					}
				}
			}
			for _, c := range [][]string{
				{"apt-get"},
				{"useradd"},
				{"ssh-keygen"},
				{"systemctl", "restart"},
				{"systemctl", "mask"},
				{"ethtool", "-G"},
				{"ethtool", "-C"},
				{"docker", "pull"},
				{"apt-mark", "hold"},
			} {
				if called(calls, c...) {
					t.Errorf("the second run called %q", c)
				}
			}
		})
	}
}

// TestHostRemoteRoles: RH-1L, RH-1U and RH-1S accept the harness's key
// only from RH-1's lab address, reuse TIME_WAIT ports only on RH-1L, pin
// the NIC's interrupts outside their measured and auxiliary cpusets, and
// RH-1S runs Docker with the pinned Valkey image, its user in the docker
// group.
func TestHostRemoteRoles(t *testing.T) {
	cases := []struct {
		role, measured, irqs string
		twReuse              bool
	}{
		{"load", "2-9", "0-1,10-15", true},
		{"upstream", "2-9,10-13", "0-1,14-15", false},
		{"statestore", "2-5,6-7", "0-1,8-15", false},
	}
	for _, c := range cases {
		t.Run(c.role, func(t *testing.T) {
			r := hostRig(t, c.role, hostFacts{})
			args := hostArgs(t, "rh-1", c.role, "--authorized-key", file(t, harnessKey+"\n"))
			if c.role == "statestore" {
				args = append(args, "--valkey-image", redisserver.ImageValkey9)
			}
			r.mustRun("host.sh", nil, args...)
			calls := r.calls()
			if got, want := r.read("/home/ruralz-bench/.ssh/authorized_keys"), `restrict,from="10.0.0.1" `+harnessKey+"\n"; got != want {
				t.Errorf("authorized_keys = %q, want %q", got, want)
			}
			if r.mode("/home/ruralz-bench/.ssh/authorized_keys") != 0o600 {
				t.Error("authorized_keys is not mode 0600")
			}
			if got := strings.Contains(r.read("/etc/sysctl.d/90-ruralz-bench.conf"), "tcp_tw_reuse"); got != c.twReuse {
				t.Errorf("tcp_tw_reuse set: %t, want %t", got, c.twReuse)
			}
			if got := r.read("/etc/ruralz-bench/tune.conf"); !strings.Contains(got, "\nLAB_IP="+labIPs()[c.role]+"\nMEASURED_CPUS="+c.measured+"\n") {
				t.Errorf("tune.conf =\n%s\nwant measured cpusets %s", got, c.measured)
			}
			checkTuned(t, r, c.irqs, calls)
			docker := c.role == "statestore"
			if got := slices.ContainsFunc(calls, func(c []string) bool { return c[0] == "apt-get" && slices.Contains(c, "docker.io") }); got != docker {
				t.Errorf("docker.io installed: %t, want %t", got, docker)
			}
			if got := called(calls, "docker", "pull", "-q", redisserver.ImageValkey9); got != docker {
				t.Errorf("Valkey image pulled: %t, want %t", got, docker)
			}
			if got := called(calls, "usermod", "-aG", "docker", "ruralz-bench"); got != docker {
				t.Errorf("user in the docker group: %t, want %t", got, docker)
			}
			if called(calls, "ssh-keygen") {
				t.Error("a remote host made a key pair")
			}
		})
	}
}

// TestHostRefuses: host.sh fails, naming the cause, on a host that is not
// Debian, without every role's lab address, for a profile it does not
// provision, without the pinned Valkey image on the State Store host, with
// a public key that is not ed25519 or for an unknown role.
func TestHostRefuses(t *testing.T) {
	cases := []struct {
		name, role, want string
		facts            hostFacts
		args             func(t *testing.T) []string
	}{
		{"not debian", "gateway", "wants Debian stable", hostFacts{os: "ubuntu"}, func(t *testing.T) []string { return hostArgs(t, "rh-1", "gateway") }},
		{"no lab address", "gateway", "--lab statestore=IPV4 is missing", hostFacts{}, func(t *testing.T) []string {
			return []string{"--inventory", script(t, "rh-1.json"), "--role", "gateway", "--lab", "gateway=10.0.0.1", "--lab", "load=10.0.0.2", "--lab", "upstream=10.0.0.3"}
		}},
		{"loopback", "gateway", "only rh-1 and rh-3 hosts are provisioned", hostFacts{}, func(t *testing.T) []string { return hostArgs(t, "loopback", "gateway") }},
		{"no valkey image", "statestore", "--valkey-image must be the pinned", hostFacts{}, func(t *testing.T) []string {
			return hostArgs(t, "rh-1", "statestore", "--valkey-image", "valkey/valkey:latest")
		}},
		{"rsa key", "load", "no ssh-ed25519 public key", hostFacts{}, func(t *testing.T) []string {
			return hostArgs(t, "rh-1", "load", "--authorized-key", file(t, "ssh-rsa AAAAB3Nza key\n"))
		}},
		{"unknown role", "gateway", "usage:", hostFacts{}, func(t *testing.T) []string { return hostArgs(t, "rh-1", "harness") }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := hostRig(t, c.role, c.facts)
			out, err := r.run("host.sh", nil, c.args(t)...)
			if err == nil || !strings.Contains(out, c.want) {
				t.Errorf("host.sh = %v:\n%s\nwant a failure naming %q", err, out, c.want)
			}
		})
	}
}

// TestTuneStrictness: on rh-1 a setting the host cannot apply fails the
// run, such as a fixed frequency below 3.0 GHz (target) or no NIC on the
// lab address; on rh-3, a cloud VM without cpufreq, SMT control or the
// lab NIC, tune.sh skips them, each with a line.
func TestTuneStrictness(t *testing.T) {
	t.Run("rh-1 slow cores", func(t *testing.T) {
		r := hostRig(t, "gateway", hostFacts{maxKHz: 2800000})
		out, err := r.run("host.sh", nil, hostArgs(t, "rh-1", "gateway")...)
		if err == nil || !strings.Contains(out, "is fixed at 2800000 kHz, below 3000000") {
			t.Errorf("host.sh = %v:\n%s\nwant the frequency floor to fail it", err, out)
		}
	})
	t.Run("rh-1 no lab NIC", func(t *testing.T) {
		r := hostRig(t, "gateway", hostFacts{noNIC: true})
		out, err := r.run("host.sh", nil, hostArgs(t, "rh-1", "gateway")...)
		if err == nil || !strings.Contains(out, "no interface carries the lab address 10.0.0.1") {
			t.Errorf("host.sh = %v:\n%s\nwant the missing NIC to fail it", err, out)
		}
	})
	t.Run("rh-3 VM", func(t *testing.T) {
		r := hostRig(t, "gateway", hostFacts{noCPUFreq: true, noNIC: true, noSMT: true})
		out := r.mustRun("host.sh", nil, hostArgs(t, "rh-3", "gateway")...)
		for _, want := range []string{"skipped: no SMT control", "skipped: no turbo control", "skipped: no cpufreq policies", "skipped: no interface carries the lab address"} {
			if !strings.Contains(out, want) {
				t.Errorf("host.sh output lacks %q:\n%s", want, out)
			}
		}
		if got := r.read("/etc/ruralz-bench/tune.conf"); !strings.Contains(got, "\nSTRICT=0\n") {
			t.Errorf("tune.conf =\n%s\nwant STRICT=0 on rh-3", got)
		}
	})
}
