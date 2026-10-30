// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration || t5

package t5

import (
	"strings"
	"testing"
)

// TestCheckSystemUser checks the system user rule both suites apply: user
// and group ruralz, system IDs, the group as the user's primary group, home
// /var/lib/ruralz and no login shell.
func TestCheckSystemUser(t *testing.T) {
	const group = "root:x:0:\nruralz:x:999:\n"
	for _, tc := range []struct {
		name, passwd, group string
		want                string // a substring of the one problem; "" for none
	}{
		{"sysusers entry", "root:x:0:0::/root:/bin/bash\nruralz:x:999:999:Ruralz Gateway:/var/lib/ruralz:/usr/sbin/nologin\n", group, ""},
		{"false shell", "ruralz:x:999:999::/var/lib/ruralz:/bin/false\n", group, ""},
		{"no user", "root:x:0:0::/root:/bin/bash\n", group, "no passwd entry"},
		{"no group", "ruralz:x:999:999::/var/lib/ruralz:/usr/sbin/nologin\n", "root:x:0:\n", "no group entry"},
		{"regular UID", "ruralz:x:1000:999::/var/lib/ruralz:/usr/sbin/nologin\n", group, "UID \"1000\""},
		{"root UID", "ruralz:x:0:999::/var/lib/ruralz:/usr/sbin/nologin\n", group, "UID \"0\""},
		{"regular GID", "ruralz:x:999:1001::/var/lib/ruralz:/usr/sbin/nologin\n", "ruralz:x:1001:\n", "GID \"1001\""},
		{"other primary group", "ruralz:x:999:998::/var/lib/ruralz:/usr/sbin/nologin\n", group, "primary GID"},
		{"other home", "ruralz:x:999:999::/home/ruralz:/usr/sbin/nologin\n", group, "home"},
		{"login shell", "ruralz:x:999:999::/var/lib/ruralz:/bin/bash\n", group, "login shell"},
		{"commented user", "#ruralz:x:999:999::/var/lib/ruralz:/usr/sbin/nologin\n", group, "no passwd entry"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := checkSystemUser([]byte(tc.passwd), []byte(tc.group), 999, 999)
			switch {
			case tc.want == "" && len(got) > 0:
				t.Errorf("checkSystemUser() = %q, want no problem", got)
			case tc.want != "" && (len(got) != 1 || !strings.Contains(got[0], tc.want)):
				t.Errorf("checkSystemUser() = %q, want one problem with %q", got, tc.want)
			}
		})
	}
}

// TestSysIDMax checks that SYS_UID_MAX and SYS_GID_MAX come from
// login.defs, defaulting to 999.
func TestSysIDMax(t *testing.T) {
	for _, tc := range []struct {
		name, defs     string
		uidMax, gidMax int
	}{
		{"absent", "", 999, 999},
		{"set", "# comment\nSYS_UID_MAX   499\nSYS_GID_MAX\t599\n", 499, 599},
		{"commented", "#SYS_UID_MAX 499\n", 999, 999},
		{"not a number", "SYS_UID_MAX many\n", 999, 999},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if u, g := sysIDMax([]byte(tc.defs)); u != tc.uidMax || g != tc.gidMax {
				t.Errorf("sysIDMax(%q) = %d, %d; want %d, %d", tc.defs, u, g, tc.uidMax, tc.gidMax)
			}
		})
	}
}
