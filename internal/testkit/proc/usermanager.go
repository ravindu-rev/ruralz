// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package proc

// UserManagerEnv returns the variables, as getenv reads them, through which
// systemd-run --user reaches the user manager: XDG_RUNTIME_DIR and
// DBUS_SESSION_BUS_ADDRESS, as "KEY=value" entries of those set. A process
// started with an environment of its own needs them to start in a scope
// of that manager, such as a Node on its cpuset.
func UserManagerEnv(getenv func(string) string) []string {
	var out []string
	for _, k := range []string{"XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"} {
		if v := getenv(k); v != "" {
			out = append(out, k+"="+v)
		}
	}
	return out
}
