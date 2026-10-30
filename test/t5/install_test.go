// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build t5 && linux

package t5

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// releaseDir is where `make release-dry-run` leaves the release archives,
// the Makefile's RELEASE_DIR default.
const releaseDir = "dist/release"

// maxArchiveMember bounds one archive member the suite reads.
const maxArchiveMember = 256 << 20

// Where the T5 install steps put each file (the ruralzd.service header;
// docs/operations/01-deployment-topologies.md "T5 and T6").
const (
	binPath      = installBin + "ruralzd"
	helperPath   = installBin + helperFile
	unitPath     = "/etc/systemd/system/" + unitName
	sysusersPath = "/etc/sysusers.d/ruralzd.conf"
	sysctlPath   = "/etc/sysctl.d/" + sysctlFile
	etcDir       = "/etc/ruralzd"
	envPath      = etcDir + "/ruralzd.env"
	// dropInDir holds the suite's drop-in; the unit ships none.
	dropInDir  = "/etc/systemd/system/" + unitName + ".d"
	dropInPath = dropInDir + "/t5-watchdog-off.conf"
	// migrateReq is the sysctl 99-ruralzd.conf sets.
	migrateReq = "/proc/sys/net/ipv4/tcp_migrate_req"
)

// The ports of the Bundle under test: the listener and the admin port the
// T5 unit serves, 8080 and 9901 (docs/operations/01-deployment-topologies.md
// "Network and ports").
const (
	webPort   = 8080
	adminPort = 9901
)

// archive is the linux ruralzd archive under test: its binary and its
// deploy/systemd/ files.
type archive struct {
	path   string
	binary []byte
	files  map[string][]byte
}

// archiveGlob returns the pattern of the host architecture's linux ruralzd
// archive in the release directory of the repository at root.
func archiveGlob(root string) string {
	return filepath.Join(root, filepath.FromSlash(releaseDir), "ruralzd_*_linux_"+runtime.GOARCH+".tar.gz")
}

// findArchive returns the one archive archiveGlob matches.
func findArchive(root string) (string, error) {
	m, err := filepath.Glob(archiveGlob(root))
	if err != nil {
		return "", err
	}
	if len(m) != 1 {
		return "", fmt.Errorf("want exactly one archive matching %s, found %q: run `make release-dry-run` first", archiveGlob(root), m)
	}
	return m[0], nil
}

// parseSums returns the SHA-256 of each file a SHA256SUMS lists, by name;
// a line is the hex digest, a space, and " " or "*" before the name.
func parseSums(data []byte) (map[string]string, error) {
	sums := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		sum, name, ok := strings.Cut(line, " ")
		if !ok || len(sum) != sha256.Size*2 || len(name) < 2 || (name[0] != ' ' && name[0] != '*') {
			return nil, fmt.Errorf("SHA256SUMS line %q", line)
		}
		sums[name[1:]] = strings.ToLower(sum)
	}
	return sums, sc.Err()
}

// verifySum checks the archive at p against the SHA256SUMS beside it.
func verifySum(p string, data []byte) error {
	b, err := os.ReadFile(filepath.Join(filepath.Dir(p), "SHA256SUMS")) //nolint:gosec // G304: the release directory under test
	if err != nil {
		return err
	}
	sums, err := parseSums(b)
	if err != nil {
		return err
	}
	want, ok := sums[filepath.Base(p)]
	if !ok {
		return fmt.Errorf("SHA256SUMS lists no %s", filepath.Base(p))
	}
	if got := sha256.Sum256(data); hex.EncodeToString(got[:]) != want {
		return fmt.Errorf("%s has SHA-256 %x, SHA256SUMS says %s", filepath.Base(p), got, want)
	}
	return nil
}

// readArchive reads a linux ruralzd archive: every member is a directory
// or a regular file with a local, unique path, and it holds the binary
// ruralzd and each shipped deploy/systemd/ file.
func readArchive(data []byte) (*archive, error) {
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(zr)
	a := &archive{files: map[string][]byte{}}
	seen := map[string]bool{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		name := strings.TrimSuffix(h.Name, "/")
		switch {
		case name == "" || !filepath.IsLocal(name) || path.Clean(name) != name:
			return nil, fmt.Errorf("archive member %q is not a local path", h.Name)
		case seen[name]:
			return nil, fmt.Errorf("archive member %q appears twice", h.Name)
		}
		seen[name] = true
		switch h.Typeflag {
		case tar.TypeDir:
			continue
		case tar.TypeReg:
		default:
			return nil, fmt.Errorf("archive member %q is neither a directory nor a regular file", h.Name)
		}
		b, err := io.ReadAll(io.LimitReader(tr, maxArchiveMember+1))
		if err != nil {
			return nil, err
		}
		if len(b) > maxArchiveMember {
			return nil, fmt.Errorf("archive member %q exceeds %d bytes", h.Name, maxArchiveMember)
		}
		switch dir, file := path.Split(name); {
		case name == "ruralzd":
			a.binary = b
		case dir == systemdDir+"/":
			a.files[file] = b
		}
	}
	if a.binary == nil {
		return nil, errors.New("the archive holds no ruralzd")
	}
	for _, f := range []string{unitFile, helperFile, sysusersFile, envFile, sysctlFile} {
		if _, ok := a.files[f]; !ok {
			return nil, fmt.Errorf("the archive holds no %s/%s", systemdDir, f)
		}
	}
	return a, nil
}

// openArchive finds, verifies and reads the archive under test, whose
// deploy/systemd/ files must be the checkout's.
func openArchive(t *testing.T, root string) *archive {
	t.Helper()
	p, err := findArchive(root)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(p) //nolint:gosec // G304: the release directory under test
	if err != nil {
		t.Fatal(err)
	}
	if err := verifySum(p, data); err != nil {
		t.Fatal(err)
	}
	a, err := readArchive(data)
	if err != nil {
		t.Fatalf("%s: %v", p, err)
	}
	a.path = p
	for name, b := range a.files {
		if !bytes.Equal(b, shipped(t, root, name)) {
			t.Fatalf("%s holds a %s/%s other than the checkout's: rebuild it with `make release-dry-run`", p, systemdDir, name)
		}
	}
	t.Logf("testing %s", p)
	return a
}

// envValue returns the value of name in an EnvironmentFile, the last
// assignment winning as systemd reads it.
func envValue(data []byte, name string) string {
	var v string
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if k, val, ok := strings.Cut(line, "="); ok && !strings.HasPrefix(line, "#") && k == name {
			v = val
		}
	}
	return v
}

// adminTokenFile returns the RURALZ_ADMIN_TOKEN_FILE an Environment= line
// of the unit sets, or "".
func adminTokenFile(unit []byte) string {
	sc := bufio.NewScanner(bytes.NewReader(unit))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "Environment=RURALZ_ADMIN_TOKEN_FILE="); ok {
			return v
		}
	}
	return ""
}

// installation is the T5 deployment the live suite installed on the host,
// which its cleanup removes again.
type installation struct {
	// t is the test that installed it, whose cleanup uninstalls it.
	t        *testing.T
	arc      *archive
	upstream string
	// bundleDir is RURALZ_CONFIG, as the shipped EnvironmentFile sets it.
	bundleDir string
	// tokenPath is the admin token file, RURALZ_ADMIN_TOKEN_FILE as the
	// unit sets it.
	tokenPath string
	// userExisted reports a ruralz user present before the install.
	userExisted bool
	// migrateBefore is tcp_migrate_req before sysctl --system.
	migrateBefore string
}

// installFile writes data to path, atomically, with mode.
func installFile(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // G301: system directories such as /etc/sysusers.d are world-readable
		return err
	}
	return writeAtomic(path, data, mode)
}

// install installs the archive by the T5 install steps: the binary and the
// helper under /usr/local/bin, the unit, the sysusers.d entry, the
// EnvironmentFile, the sysctl file and an admin token, then
// systemd-sysusers, the token's group, sysctl --system and a daemon
// reload. The EnvironmentFile is the shipped example plus
// RURALZ_FETCH_ALLOW for the loopback Upstream, and the Bundle directory
// starts with an invalid Bundle. It refuses to replace an existing
// installation; t's end removes everything it installed.
func install(ctx context.Context, t *testing.T, arc *archive, upstream string) *installation {
	t.Helper()
	in := &installation{
		t: t, arc: arc, upstream: upstream,
		bundleDir: envValue(arc.files[envFile], "RURALZ_CONFIG"),
		tokenPath: adminTokenFile(arc.files[unitFile]),
	}
	if in.bundleDir == "" || in.tokenPath == "" {
		t.Fatalf("the shipped %s sets no RURALZ_CONFIG (%q) or %s no RURALZ_ADMIN_TOKEN_FILE (%q)", envFile, in.bundleDir, unitFile, in.tokenPath)
	}
	var present []string
	for _, p := range []string{binPath, helperPath, unitPath, dropInDir, sysusersPath, sysctlPath, etcDir, in.bundleDir, in.tokenPath, dataDir} {
		if _, err := os.Lstat(p); err == nil {
			present = append(present, p)
		}
	}
	if len(present) > 0 {
		t.Fatalf("an installation exists: %q; the live T5 suite installs a fresh one and replaces nothing, so remove these first", present)
	}
	for _, port := range []int{webPort, adminPort} {
		var lc net.ListenConfig
		ln, err := lc.Listen(ctx, "tcp", fmt.Sprintf(":%d", port))
		if err != nil {
			t.Fatalf("port %d, which the Bundle under test serves, is busy: %v", port, err)
		}
		_ = ln.Close()
	}
	_, err := run(ctx, "getent", "passwd", userName)
	in.userExisted = err == nil
	if b, err := os.ReadFile(migrateReq); err == nil {
		in.migrateBefore = strings.TrimSpace(string(b))
	}
	t.Cleanup(in.uninstall)

	env := append(append([]byte(nil), arc.files[envFile]...), "\n# The live T5 suite's loopback Upstream.\nRURALZ_FETCH_ALLOW=127.0.0.1\n"...)
	for _, f := range []struct {
		path string
		data []byte
		mode os.FileMode
	}{
		{binPath, arc.binary, 0o755},
		{helperPath, arc.files[helperFile], 0o755},
		{unitPath, arc.files[unitFile], 0o644},
		{sysusersPath, arc.files[sysusersFile], 0o644},
		{sysctlPath, arc.files[sysctlFile], 0o644},
		{envPath, env, 0o600},
		{in.tokenPath, []byte(adminToken + "\n"), 0o640},
		// The Node reads its Bundle as the ruralz user.
		{filepath.Join(in.bundleDir, "ruralz.yaml"), []byte(invalidBundle), 0o644},
	} {
		if err := installFile(f.path, f.data, f.mode); err != nil {
			t.Fatal(err)
		}
	}
	for _, cmd := range [][]string{
		{"systemd-sysusers", sysusersPath}, {"chgrp", userName, in.tokenPath}, {"sysctl", "--system"}, {"systemctl", "daemon-reload"},
	} {
		if _, err := run(ctx, cmd[0], cmd[1:]...); err != nil {
			t.Fatal(err)
		}
	}
	return in
}

// uninstall stops the unit and removes what install installed: the files,
// the data directory, the user when it was new, and tcp_migrate_req's
// earlier value.
func (in *installation) uninstall() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*systemctlBound)
	defer cancel()
	report := func(err error) {
		if err != nil {
			in.t.Logf("uninstall: %v", err)
		}
	}
	_, err := systemctl(ctx, "stop", unitName)
	report(err)
	_, _ = systemctl(ctx, "reset-failed", unitName)
	for _, p := range []string{dropInDir, unitPath, binPath, helperPath, sysusersPath, sysctlPath, etcDir, in.bundleDir, in.tokenPath, dataDir} {
		report(os.RemoveAll(p))
	}
	_, err = systemctl(ctx, "daemon-reload")
	report(err)
	if !in.userExisted {
		_, err = run(ctx, "userdel", userName)
		report(err)
		if _, err := run(ctx, "getent", "group", userName); err == nil {
			_, err = run(ctx, "groupdel", userName)
			report(err)
		}
	}
	if in.migrateBefore != "" {
		report(os.WriteFile(migrateReq, []byte(in.migrateBefore+"\n"), 0o600))
	}
}

// adminURL is the admin port's base URL.
func (in *installation) adminURL() string { return fmt.Sprintf("http://127.0.0.1:%d", adminPort) }

// webURL is the listener's base URL.
func (in *installation) webURL() string { return fmt.Sprintf("http://127.0.0.1:%d", webPort) }

// setBundle replaces the Bundle under test, atomically: the valid one, or
// one that does not parse.
func (in *installation) setBundle(t *testing.T, valid bool) {
	t.Helper()
	data := []byte(invalidBundle)
	if valid {
		data = bundle(in.upstream, webPort, adminPort)
	}
	if err := installFile(filepath.Join(in.bundleDir, "ruralz.yaml"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// setBinary replaces /usr/local/bin/ruralzd, atomically, as an upgrade
// does before `systemctl reload`; running processes keep their image.
func (in *installation) setBinary(t *testing.T, data []byte) {
	t.Helper()
	if err := installFile(binPath, data, 0o755); err != nil {
		t.Fatal(err)
	}
}

// setWatchdogOff adds, with off, or removes the suite's drop-in setting
// WatchdogSec=0, then reloads the unit files.
func (in *installation) setWatchdogOff(ctx context.Context, t *testing.T, off bool) {
	t.Helper()
	var err error
	if off {
		err = installFile(dropInPath, []byte("# The live T5 suite: no watchdog while a handover successor has no\n# watchdog interval (OQ-zero-downtime-upgrades-and-hot-reload-12).\n[Service]\nWatchdogSec=0\n"), 0o644)
	} else {
		err = os.RemoveAll(dropInDir)
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err := systemctl(ctx, "daemon-reload"); err != nil {
		t.Fatal(err)
	}
}

// restartReady restarts the unit on the valid Bundle and waits until it is
// ready.
func (in *installation) restartReady(ctx context.Context, t *testing.T) {
	t.Helper()
	in.setBundle(t, true)
	if _, err := systemctl(ctx, "restart", unitName); err != nil {
		t.Fatal(err)
	}
	if _, err := waitReady(ctx, in.adminURL()); err != nil {
		t.Fatal(err)
	}
}

// evidence logs, when t failed, the unit's status and its journal since
// since: CI keeps the test log.
func evidence(ctx context.Context, t *testing.T, since time.Time) {
	t.Helper()
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), systemctlBound)
		defer cancel()
		status, _ := systemctl(ctx, "status", "--no-pager", "--full", unitName)
		t.Logf("systemctl status %s:\n%s\njournal since %s:\n%s", unitName, status, since.Format(time.RFC3339), journal(ctx, since))
	})
}
