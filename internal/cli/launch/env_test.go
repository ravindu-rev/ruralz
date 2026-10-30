// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package launch

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestEnviron checks the child's environment: the settings the launcher
// sets replace every entry of their name, RURALZ_ADMIN_TLS_DIR and
// RURALZ_ADMIN_METRICS_TOKEN_FILE are removed, RURALZ_FETCH_ALLOW is set
// only when unset or empty, RURALZ_SECRET_ROOT only with overrides, and
// everything else passes through in order.
func TestEnviron(t *testing.T) {
	ws := &Workspace{root: "/private"}
	base := []string{
		"PATH=/bin",
		"RURALZ_CONFIG=/elsewhere",
		"RURALZ_DATA_DIR=/var/lib/ruralz",
		"RURALZ_ADMIN_TOKEN_FILE=/etc/token",
		"RURALZ_BIND_LOOPBACK=false",
		"RURALZ_ADMIN_TLS_DIR=/etc/tls",
		"RURALZ_ADMIN_METRICS_TOKEN_FILE=/etc/metrics",
		"RURALZ_LOG_LEVEL=debug",
		"RURALZ_SECRET_DB=x",
		"RURALZ_STATE_STORE_URL=redis://h",
		"RURALZ_STATE_STORE_MAC_KEY_FILE=/etc/mac",
		"RURALZ_SECRET_ROOT=/etc/ruralz",
		"RURALZ_CONFIG=/twice",
	}
	set := []string{
		"RURALZ_CONFIG=/private/bundle.yaml",
		"RURALZ_DATA_DIR=/private/data",
		"RURALZ_ADMIN_TOKEN_FILE=/private/admin-token",
		"RURALZ_BIND_LOOPBACK=true",
	}
	passed := []string{
		"PATH=/bin",
		"RURALZ_LOG_LEVEL=debug",
		"RURALZ_SECRET_DB=x",
		"RURALZ_STATE_STORE_URL=redis://h",
		"RURALZ_STATE_STORE_MAC_KEY_FILE=/etc/mac",
	}
	for _, c := range []struct {
		name       string
		environ    []string
		secretRoot bool
		want       []string
	}{
		{
			"without overrides", base, false,
			slices.Concat(passed, []string{"RURALZ_SECRET_ROOT=/etc/ruralz"}, set, []string{"RURALZ_FETCH_ALLOW=127.0.0.0/8,::1"}),
		},
		{
			"with overrides", base, true,
			slices.Concat(passed, set, []string{"RURALZ_SECRET_ROOT=/private/secrets", "RURALZ_FETCH_ALLOW=127.0.0.0/8,::1"}),
		},
		{
			"fetch allow set", []string{"RURALZ_FETCH_ALLOW=10.0.0.0/8"}, false,
			slices.Concat([]string{"RURALZ_FETCH_ALLOW=10.0.0.0/8"}, set),
		},
		{
			"fetch allow empty", []string{"RURALZ_FETCH_ALLOW="}, false,
			slices.Concat(set, []string{"RURALZ_FETCH_ALLOW=127.0.0.0/8,::1"}),
		},
		{
			"entry without a value", []string{"RURALZ_FETCH_ALLOW", "NOVALUE"}, false,
			slices.Concat([]string{"NOVALUE"}, set, []string{"RURALZ_FETCH_ALLOW=127.0.0.0/8,::1"}),
		},
	} {
		got := Environ(c.environ, ws, c.secretRoot)
		if !slices.Equal(got, nativePaths(c.want)) {
			t.Errorf("%s: Environ =\n%q\nwant\n%q", c.name, got, nativePaths(c.want))
		}
	}
}

// nativePaths writes the private directory paths of env's values with the
// operating system's separator, as the Workspace joins them.
func nativePaths(env []string) []string {
	out := slices.Clone(env)
	for i, kv := range out {
		if k, v, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(v, "/private/") {
			out[i] = k + "=" + filepath.Join("/private", strings.TrimPrefix(v, "/private/"))
		}
	}
	return out
}

// TestEnvironName checks entry names, Windows "=C:" entries included.
func TestEnvironName(t *testing.T) {
	for kv, want := range map[string]string{"A=1": "A", "A==1": "A", "=C:=C:\\": "=C:", "NOVALUE": "NOVALUE", "": ""} {
		if got := name(kv); got != want {
			t.Errorf("name(%q) = %q, want %q", kv, got, want)
		}
	}
}
