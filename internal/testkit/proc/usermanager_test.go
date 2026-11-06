// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package proc

import (
	"slices"
	"testing"
)

// TestUserManagerEnv: the user manager's variables that are set, and no
// other variable.
func TestUserManagerEnv(t *testing.T) {
	env := map[string]string{"XDG_RUNTIME_DIR": "/run/user/1000", "DBUS_SESSION_BUS_ADDRESS": "unix:path=/run/user/1000/bus", "HOME": "/home/u"}
	got := UserManagerEnv(func(k string) string { return env[k] })
	if want := []string{"XDG_RUNTIME_DIR=/run/user/1000", "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1000/bus"}; !slices.Equal(got, want) {
		t.Errorf("UserManagerEnv = %q, want %q", got, want)
	}
	delete(env, "DBUS_SESSION_BUS_ADDRESS")
	if got := UserManagerEnv(func(k string) string { return env[k] }); !slices.Equal(got, []string{"XDG_RUNTIME_DIR=/run/user/1000"}) {
		t.Errorf("UserManagerEnv without a bus address = %q", got)
	}
	if got := UserManagerEnv(func(string) string { return "" }); got != nil {
		t.Errorf("UserManagerEnv of an empty environment = %q", got)
	}
}
