// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration

// Package t5 checks the systemd files of T5, Ruralz Gateway on a VM under
// systemd, as deploy/systemd/ ships them in every linux ruralzd archive
// (docs/operations/01-deployment-topologies.md "T5 and T6"): an
// integration test runs systemd-analyze verify on a copy of
// ruralzd.service whose /usr/local/bin/ paths name the ExecReload helper
// and a stand-in ruralzd, and systemd-sysusers with the sysusers.d entry
// against a temporary root, so it changes nothing on the host and needs
// neither root nor a running systemd.
package t5

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/testkit/binaries"
)

// The shipped T5 files the checks read, as deploy/systemd/ holds them and
// every linux ruralzd archive carries them at the same path
// (docs/engineering/04-release-versioning-and-compatibility.md
// "Packaging and audit").
const (
	systemdDir   = "deploy/systemd"
	unitFile     = "ruralzd.service"
	helperFile   = "ruralzd-handover"
	sysusersFile = "ruralzd.sysusers"
)

// What the sysusers.d entry creates and the unit runs as
// (docs/operations/01-deployment-topologies.md "T5 and T6").
const (
	// userName is the system user and group of ruralzd.service.
	userName = "ruralz"
	// dataDir is the unit's StateDirectory=, RURALZ_DATA_DIR, and the
	// user's home.
	dataDir = "/var/lib/ruralz"
	// installBin is where the binary and the helper install, the prefix
	// the unit's ExecStart= and ExecReload= name.
	installBin = "/usr/local/bin/"
)

// defaultSysIDMax bounds system IDs: login.defs(5) SYS_UID_MAX and
// SYS_GID_MAX default to 999, systemd's SYSTEM_UID_MAX and SYSTEM_GID_MAX
// as Debian and Ubuntu build it.
const defaultSysIDMax = 999

// repoRoot returns the repository root.
func repoRoot(t testing.TB) string {
	t.Helper()
	root, err := binaries.RepoRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// shipped returns the content of deploy/systemd/name in the repository at
// root.
func shipped(t testing.TB, root, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(systemdDir), name)) //nolint:gosec // G304: a fixed file of the repository
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// userEntry returns the colon-separated fields of the line naming ruralz
// in a passwd(5) or group(5) file, or nil when no line does.
func userEntry(data []byte) []string {
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") {
			continue
		}
		if f := strings.Split(line, ":"); len(f) > 1 && f[0] == userName {
			return f
		}
	}
	return nil
}

// loginDefsValue returns the decimal value of key in login.defs(5)
// content, or defaultSysIDMax when the key is absent or not a number.
func loginDefsValue(data []byte, key string) int {
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 || f[0] != key {
			continue
		}
		if v, err := strconv.Atoi(f[1]); err == nil {
			return v
		}
	}
	return defaultSysIDMax
}

// sysIDMax returns SYS_UID_MAX and SYS_GID_MAX of the login.defs(5)
// content, each defaulting to defaultSysIDMax.
func sysIDMax(defs []byte) (uidMax, gidMax int) {
	return loginDefsValue(defs, "SYS_UID_MAX"), loginDefsValue(defs, "SYS_GID_MAX")
}

// checkSystemUser returns what is wrong with the ruralz user and group in
// passwd(5) and group(5) content against the sysusers.d entry: a user and
// a group named ruralz, each with a system ID (1 to uidMax, 1 to gidMax),
// the group the user's primary group, home dataDir and no login shell.
func checkSystemUser(passwd, group []byte, uidMax, gidMax int) []string {
	var problems []string
	u, g := userEntry(passwd), userEntry(group)
	if len(u) != 7 {
		problems = append(problems, fmt.Sprintf("no passwd entry for user %s with 7 fields: %q", userName, u))
	}
	if len(g) < 3 {
		problems = append(problems, fmt.Sprintf("no group entry for group %s: %q", userName, g))
	}
	if len(problems) > 0 {
		return problems
	}
	uid, uerr := strconv.Atoi(u[2])
	gid, gerr := strconv.Atoi(u[3])
	groupID, ggerr := strconv.Atoi(g[2])
	if uerr != nil || uid < 1 || uid > uidMax {
		problems = append(problems, fmt.Sprintf("user %s has UID %q, want a system ID from 1 to %d", userName, u[2], uidMax))
	}
	switch {
	case ggerr != nil || groupID < 1 || groupID > gidMax:
		problems = append(problems, fmt.Sprintf("group %s has GID %q, want a system ID from 1 to %d", userName, g[2], gidMax))
	case gerr != nil || gid != groupID:
		problems = append(problems, fmt.Sprintf("user %s has primary GID %q, want group %s's %d", userName, u[3], userName, groupID))
	}
	if u[5] != dataDir {
		problems = append(problems, fmt.Sprintf("user %s has home %q, want %s", userName, u[5], dataDir))
	}
	if shell := path.Base(u[6]); shell != "nologin" && shell != "false" {
		problems = append(problems, fmt.Sprintf("user %s has login shell %q, want nologin or false", userName, u[6]))
	}
	return problems
}
