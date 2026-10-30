// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/gateway/setting"
)

// Tests of the files linux ruralzd archives and the container image ship:
// deploy/systemd/ (docs/operations/01-deployment-topologies.md, "T5 and
// T6") and deploy/container/ (docs/engineering/04-release-versioning-and-compatibility.md,
// "Release artifacts").

// repoRoot is the repository root seen from this package.
func repoRoot() string { return filepath.Join("..", "..", "..") }

func readRepo(t *testing.T, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(), filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// unitFile parses a systemd unit into section -> key -> values, in order.
func unitFile(t *testing.T, text string) map[string]map[string][]string {
	t.Helper()
	out := map[string]map[string][]string{}
	section := ""
	sc := bufio.NewScanner(strings.NewReader(text))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";"):
		case strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"):
			section = strings.Trim(line, "[]")
			out[section] = map[string][]string{}
		default:
			k, v, ok := strings.Cut(line, "=")
			if !ok || section == "" {
				t.Fatalf("unit line %d: %q is not Key=Value in a section", n, line)
			}
			out[section][k] = append(out[section][k], v)
		}
	}
	return out
}

// envValue returns the value an Environment= line of the unit gives name.
func envValue(service map[string][]string, name string) (string, bool) {
	for _, e := range service["Environment"] {
		if k, v, ok := strings.Cut(e, "="); ok && k == name {
			return v, true
		}
	}
	return "", false
}

// helperConstant reads "readonly <name>=<value>" from the handover helper.
func helperConstant(t *testing.T, script, name string) string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^readonly ` + name + `=([0-9.]+)`).FindStringSubmatch(script)
	if m == nil {
		t.Fatalf("ruralzd-handover has no readonly %s", name)
	}
	return m[1]
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// drainExitBound is the 30 s exit bound after SIGTERM (target) of
// docs/operations/02-zero-downtime-upgrades-and-hot-reload.md, "Drain
// timeline defaults", which TimeoutStopSec must exceed.
const drainExitBound = 30

// TestSystemdUnit: Type=notify with NotifyAccess=all; the data directory
// /var/lib/ruralz; ExecReload runs the helper on the replaced binary;
// TimeoutStopSec=40 above the Drain exit bound; WatchdogSec=30s, which
// ruralzd answers with WATCHDOG=1; reloads, which share the start timeout,
// outlast the helper; both processes of a handover get the stop signal; a
// crash, SIGKILL or watchdog kill restarts the Node; LimitNOFILE=65536,
// above the connection ceilings (docs/architecture/03-data-plane.md
// "Bounded resources").
func TestSystemdUnit(t *testing.T) {
	u := unitFile(t, readRepo(t, "deploy/systemd/ruralzd.service"))
	s := u["Service"]
	want := map[string]string{
		"Type": "notify", "NotifyAccess": "all", "User": "ruralz", "Group": "ruralz",
		"StateDirectory": "ruralz", "StateDirectoryMode": "0700",
		"EnvironmentFile": "/etc/ruralzd/ruralzd.env",
		"ExecStart":       "/usr/local/bin/ruralzd",
		"ExecReload":      "/usr/local/bin/ruralzd-handover /usr/local/bin/ruralzd",
		"TimeoutStopSec":  "40", "KillMode": "control-group", "KillSignal": "SIGTERM", "Restart": "on-failure",
		"WatchdogSec": "30s", "LimitNOFILE": "65536",
	}
	for k, v := range want {
		if !slices.Equal(s[k], []string{v}) {
			t.Errorf("Service %s = %q, want %q", k, s[k], v)
		}
	}
	if dataDir, ok := envValue(s, setting.EnvDataDir); !ok || dataDir != setting.DefaultDataDir || dataDir != "/var/lib/"+s["StateDirectory"][0] {
		t.Errorf("RURALZ_DATA_DIR = %q, want the StateDirectory %s", dataDir, setting.DefaultDataDir)
	}
	if stop := atoi(t, s["TimeoutStopSec"][0]); stop <= drainExitBound {
		t.Errorf("TimeoutStopSec %d does not exceed the %d s Drain exit bound", stop, drainExitBound)
	}
	helper := readRepo(t, "deploy/systemd/ruralzd-handover")
	wait := atoi(t, helperConstant(t, helper, "default_timeout")) + atoi(t, helperConstant(t, helper, "stop_grace"))
	if start := atoi(t, s["TimeoutStartSec"][0]); start <= wait {
		t.Errorf("TimeoutStartSec %d does not exceed the helper's %d s wait and stop grace", start, wait)
	}
	if !slices.Equal(u["Install"]["WantedBy"], []string{"multi-user.target"}) {
		t.Errorf("WantedBy = %q", u["Install"]["WantedBy"])
	}
}

// TestSystemdAdminToken: the admin token is a file outside
// RURALZ_SECRET_ROOT, which a Node refuses to start with, and never a
// systemd credential: systemd hides credentials from ExecReload=
// processes, mounting an inaccessible directory over /run/credentials, so
// a handover successor could not read the token, would refuse to start
// and every reload would fail.
func TestSystemdAdminToken(t *testing.T) {
	s := unitFile(t, readRepo(t, "deploy/systemd/ruralzd.service"))["Service"]
	for k, v := range s {
		if strings.Contains(k, "Credential") {
			t.Errorf("Service %s = %q: an ExecReload= process cannot read credentials", k, v)
		}
	}
	tokenFile, ok := envValue(s, setting.EnvAdminTokenFile)
	if !ok || tokenFile != "/etc/ruralzd/admin-token" {
		t.Errorf("RURALZ_ADMIN_TOKEN_FILE = %q, want /etc/ruralzd/admin-token", tokenFile)
	}
	if !path.IsAbs(tokenFile) || tokenFile == setting.DefaultSecretRoot || strings.HasPrefix(tokenFile, setting.DefaultSecretRoot+"/") ||
		strings.HasPrefix(tokenFile, "/run/credentials/") {
		t.Errorf("%s is not an absolute path outside RURALZ_SECRET_ROOT %s and the credentials directory", tokenFile, setting.DefaultSecretRoot)
	}
}

// TestSystemdEnvFile: the example EnvironmentFile sets RURALZ_CONFIG and
// names only M1 process settings, never those the unit sets itself.
func TestSystemdEnvFile(t *testing.T) {
	known := []string{
		setting.EnvConfig, setting.EnvLogLevel, setting.EnvStateStoreURL, setting.EnvAdminMetricsTokenFile,
		setting.EnvAdminTLSDir, setting.EnvSecretRoot, setting.EnvFetchAllow, setting.EnvStateStoreMACKeyFile,
		setting.EnvZone,
	}
	line := regexp.MustCompile(`^(#?)(RURALZ_[A-Z0-9_]+)=(.*)$`)
	set := map[string]string{}
	for _, l := range strings.Split(readRepo(t, "deploy/systemd/ruralzd.env"), "\n") {
		m := line.FindStringSubmatch(l)
		if m == nil {
			if l != "" && !strings.HasPrefix(l, "#") {
				t.Errorf("line %q is neither NAME=value nor a comment", l)
			}
			continue
		}
		if !slices.Contains(known, m[2]) {
			t.Errorf("%s is not an M1 setting the env file may hold", m[2])
		}
		if m[1] == "" {
			set[m[2]] = m[3]
		}
	}
	if set[setting.EnvConfig] == "" || !path.IsAbs(set[setting.EnvConfig]) {
		t.Errorf("RURALZ_CONFIG = %q, want an absolute directory", set[setting.EnvConfig])
	}
}

// TestSystemdFiles: deploy/systemd/ holds exactly the files the T5 table
// lists and linux ruralzd archives ship, the sysusers.d entry included.
func TestSystemdFiles(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join(repoRoot(), "deploy", "systemd"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if !slices.Equal(names, systemdFiles()) {
		t.Errorf("deploy/systemd holds %q, want %q", names, systemdFiles())
	}
	if !strings.Contains(readRepo(t, "docs/operations/01-deployment-topologies.md"), "| `ruralzd.sysusers` |") {
		t.Error("the T5 table does not list ruralzd.sysusers")
	}
}

// sysusersFields splits a sysusers.d line into fields, a double-quoted
// field keeping its spaces.
func sysusersFields(line string) []string {
	var out []string
	for line = strings.TrimSpace(line); line != ""; line = strings.TrimSpace(line) {
		if strings.HasPrefix(line, `"`) {
			end := strings.Index(line[1:], `"`)
			if end < 0 {
				return append(out, line)
			}
			out = append(out, line[1:end+1])
			line = line[end+2:]
			continue
		}
		f, rest, _ := strings.Cut(line, " ")
		out = append(out, f)
		line = rest
	}
	return out
}

// TestSystemdSysusers: the sysusers.d entry creates the system user (and
// its group) the unit runs as, with an automatic system ID, the data
// directory as home and the default no-login shell.
func TestSystemdSysusers(t *testing.T) {
	var entries [][]string
	for _, l := range strings.Split(readRepo(t, "deploy/systemd/ruralzd.sysusers"), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			entries = append(entries, sysusersFields(l))
		}
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %q, want one user", entries)
	}
	unit := unitFile(t, readRepo(t, "deploy/systemd/ruralzd.service"))["Service"]
	f := entries[0]
	if len(f) != 5 || f[0] != "u" || f[2] != "-" || f[3] == "" || f[4] != setting.DefaultDataDir {
		t.Errorf("entry %q, want u <name> - \"<description>\" %s", f, setting.DefaultDataDir)
	}
	if len(f) > 1 && (!slices.Equal(unit["User"], []string{f[1]}) || !slices.Equal(unit["Group"], []string{f[1]})) {
		t.Errorf("entry creates %q, but the unit runs as %q:%q", f[1], unit["User"], unit["Group"])
	}
}

// TestSysctlConf: 99-ruralzd.conf sets net.ipv4.tcp_migrate_req = 1 and
// nothing else.
func TestSysctlConf(t *testing.T) {
	var settings []string
	for _, l := range strings.Split(readRepo(t, "deploy/systemd/99-ruralzd.conf"), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			settings = append(settings, l)
		}
	}
	if !slices.Equal(settings, []string{"net.ipv4.tcp_migrate_req = 1"}) {
		t.Errorf("sysctl settings = %q", settings)
	}
}

// TestHandoverHelperDefaults: the helper waits 75 s (target) by default,
// in polls whose count matches their interval, and checks one argument
// or two.
func TestHandoverHelperDefaults(t *testing.T) {
	h := readRepo(t, "deploy/systemd/ruralzd-handover")
	if !strings.HasPrefix(h, "#!/usr/bin/env bash\n") || !strings.Contains(h, "set -euo pipefail\n") {
		t.Error("the helper is not a strict bash script")
	}
	if got := helperConstant(t, h, "default_timeout"); got != "75" {
		t.Errorf("default_timeout = %s, want 75", got)
	}
	interval, err := strconv.ParseFloat(helperConstant(t, h, "poll_interval"), 64)
	if err != nil || interval <= 0 {
		t.Fatalf("poll_interval: %v", err)
	}
	if per := atoi(t, helperConstant(t, h, "polls_per_second")); float64(per)*interval != 1 {
		t.Errorf("polls_per_second %d times poll_interval %v is not one second", per, interval)
	}
	if !strings.Contains(h, `holder=${RURALZ_DATA_DIR:-`+setting.DefaultDataDir+`}/holder.json`) {
		t.Error("the helper does not read holder.json in RURALZ_DATA_DIR or its default")
	}
}

// dockerInstruction is one Dockerfile instruction with continuations
// joined.
type dockerInstruction struct {
	Cmd, Args string
}

// dockerfile parses the instructions of a Dockerfile.
func dockerfile(t *testing.T, text string) []dockerInstruction {
	t.Helper()
	var out []dockerInstruction
	var cur strings.Builder
	for _, l := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(l)
		if cur.Len() == 0 && (trimmed == "" || strings.HasPrefix(trimmed, "#")) {
			continue
		}
		if strings.HasSuffix(trimmed, `\`) {
			cur.WriteString(strings.TrimSuffix(trimmed, `\`) + " ")
			continue
		}
		cur.WriteString(trimmed)
		cmd, args, _ := strings.Cut(cur.String(), " ")
		out = append(out, dockerInstruction{Cmd: cmd, Args: strings.TrimSpace(args)})
		cur.Reset()
	}
	return out
}

// dockerCopy is one COPY instruction.
type dockerCopy struct {
	From, Chmod, Chown, Src, Dst string
}

func parseCopy(t *testing.T, args string) dockerCopy {
	t.Helper()
	var c dockerCopy
	f := strings.Fields(args)
	for len(f) > 0 && strings.HasPrefix(f[0], "--") {
		k, v, _ := strings.Cut(strings.TrimPrefix(f[0], "--"), "=")
		switch k {
		case "from":
			c.From = v
		case "chmod":
			c.Chmod = v
		case "chown":
			c.Chown = v
		default:
			t.Fatalf("COPY flag --%s", k)
		}
		f = f[1:]
	}
	if len(f) != 2 {
		t.Fatalf("COPY %q has not one source and one destination", args)
	}
	c.Src, c.Dst = f[0], f[1]
	return c
}

// TestDockerfile: an empty final base running the released binary as user
// 65532; from the digest-pinned golang:1.27.1-bookworm stage of the tech
// stack's CI tooling row, named on a literal FROM line that Dependabot's
// docker updates move, only the CA bundle, the composed
// THIRD_PARTY_LICENSES and the empty data directory enter the image; the
// legal files sit under /usr/share/doc/ruralzd/.
func TestDockerfile(t *testing.T) {
	ins := dockerfile(t, readRepo(t, "deploy/container/Dockerfile"))
	row := regexp.MustCompile("\\| `(golang:[^`]+)` image \\| Digest pinned in `deploy/container/Dockerfile`").
		FindStringSubmatch(readRepo(t, "docs/engineering/01-tech-stack-and-libraries.md"))
	if row == nil {
		t.Fatal("the tech stack CI tooling table has no golang image row")
	}
	certs := regexp.MustCompile(`^--platform=\$BUILDPLATFORM ` + regexp.QuoteMeta(row[1]) + `@sha256:[0-9a-f]{64} AS certs$`)
	if ins[0].Cmd != "FROM" || !certs.MatchString(ins[0].Args) {
		t.Errorf("first instruction %+v, want FROM --platform=$BUILDPLATFORM %s@sha256:<digest> AS certs", ins[0], row[1])
	}
	// The instructions after the second FROM form the final stage.
	var froms []string
	var final []dockerInstruction
	for _, in := range ins {
		switch {
		case in.Cmd == "FROM":
			froms = append(froms, in.Args)
		case len(froms) == 2:
			final = append(final, in)
		}
	}
	if len(froms) != 2 || !certs.MatchString(froms[0]) || froms[1] != "scratch AS "+images("0.1.0")[0].Target {
		t.Fatalf("FROM lines %q, want the certs stage and scratch AS %s", froms, images("0.1.0")[0].Target)
	}
	copies := map[string]dockerCopy{}
	var user, entrypoint string
	var exposed []string
	for _, in := range final {
		switch in.Cmd {
		case "RUN", "ADD", "CMD", "HEALTHCHECK":
			t.Errorf("the final stage has %s: nothing runs in the image build", in.Cmd)
		case "COPY":
			c := parseCopy(t, in.Args)
			copies[c.Dst] = c
		case "USER":
			user = in.Args
		case "ENTRYPOINT":
			entrypoint = in.Args
		case "EXPOSE":
			exposed = strings.Fields(in.Args)
		}
	}
	wantCopies := map[string]dockerCopy{
		"/etc/ssl/certs/ca-certificates.crt":          {From: "certs", Chmod: "0644", Src: "/work/ca-certificates.crt"},
		"/usr/local/bin/ruralzd":                      {Chmod: "0755", Src: "dist/linux_${TARGETARCH}/ruralzd"},
		"/usr/share/doc/ruralzd/LICENSE":              {Chmod: "0644", Src: "LICENSE"},
		"/usr/share/doc/ruralzd/NOTICE":               {Chmod: "0644", Src: "dist/notices/ruralzd_linux_${TARGETARCH}/NOTICE"},
		"/usr/share/doc/ruralzd/THIRD_PARTY_LICENSES": {From: "certs", Chmod: "0644", Src: "/work/THIRD_PARTY_LICENSES"},
		"/var/lib/": {From: "certs", Chown: "65532:65532", Src: "/work/varlib/"},
	}
	for dst, w := range wantCopies {
		w.Dst = dst
		if copies[dst] != w {
			t.Errorf("COPY to %s = %+v, want %+v", dst, copies[dst], w)
		}
	}
	if len(copies) != len(wantCopies) {
		t.Errorf("the final stage copies %d paths, want %d", len(copies), len(wantCopies))
	}
	if user != "65532:65532" || entrypoint != `["/usr/local/bin/ruralzd"]` {
		t.Errorf("USER %q ENTRYPOINT %q", user, entrypoint)
	}
	if !slices.Equal(exposed, []string{"8080/tcp", "8443/tcp", "9901/tcp"}) {
		t.Errorf("EXPOSE %q", exposed)
	}
}

// TestDockerfileNotices: the certs stage appends the CA bundle's notice,
// with its MPL-2.0 source address, to the binary's THIRD_PARTY_LICENSES
// under the same section rule, copies the CA bundle anew, so its time is
// the build's and SOURCE_DATE_EPOCH clamps it, and makes the data
// directory owner-only.
func TestDockerfileNotices(t *testing.T) {
	text := readRepo(t, "deploy/container/Dockerfile")
	for _, want := range []string{
		"COPY dist/notices/ruralzd_linux_${TARGETARCH}/THIRD_PARTY_LICENSES /work/THIRD_PARTY_LICENSES.binary",
		"'" + sectionRule + "'",
		"License: MPL-2.0 (certificate data)",
		"https://sources.debian.org/src/ca-certificates/%s/",
		"cat /usr/share/doc/ca-certificates/copyright",
		"cp /etc/ssl/certs/ca-certificates.crt /work/ca-certificates.crt",
		"install -d -m 0700 /work/varlib/ruralz",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("Dockerfile lacks %q", want)
		}
	}
}

// ignored applies dockerignore patterns to a context path: the last
// pattern matching the path or one of its parent directories decides.
func ignored(patterns []string, p string) bool {
	excluded := false
	for _, pat := range patterns {
		include := strings.HasPrefix(pat, "!")
		pat = strings.TrimPrefix(pat, "!")
		matched := false
		for prefix := p; prefix != "."; prefix = path.Dir(prefix) {
			if ok, _ := path.Match(pat, prefix); ok {
				matched = true
				break
			}
		}
		if matched {
			excluded = !include
		}
	}
	return excluded
}

// TestDockerignore: the build context holds the COPY sources of both
// architectures and nothing else.
func TestDockerignore(t *testing.T) {
	var patterns []string
	for _, l := range strings.Split(readRepo(t, "deploy/container/Dockerfile.dockerignore"), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			patterns = append(patterns, l)
		}
	}
	var sources []string
	for _, in := range dockerfile(t, readRepo(t, "deploy/container/Dockerfile")) {
		if in.Cmd == "COPY" {
			if c := parseCopy(t, in.Args); c.From == "" {
				sources = append(sources, c.Src)
			}
		}
	}
	for _, arch := range []string{"amd64", "arm64"} {
		for _, src := range sources {
			if p := strings.ReplaceAll(src, "${TARGETARCH}", arch); ignored(patterns, p) {
				t.Errorf("COPY source %s is excluded from the build context", p)
			}
		}
	}
	for _, p := range []string{"go.mod", "bin/golangci-lint", "dist/linux_amd64/ruralz-control", "dist/notices/ruralz_linux_amd64/NOTICE", ".git/config", "dist/release/SHA256SUMS"} {
		if !ignored(patterns, p) {
			t.Errorf("%s enters the build context", p)
		}
	}
}
