// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package launch

import (
	"strings"
)

// Process settings the launcher sets or removes (docs/_meta/foundation-pack.md
// section 2, "Process settings").
//
//nolint:gosec // G101: environment variable names, not credentials.
const (
	envConfig            = "RURALZ_CONFIG"
	envDataDir           = "RURALZ_DATA_DIR"
	envAdminTokenFile    = "RURALZ_ADMIN_TOKEN_FILE"
	envBindLoopback      = "RURALZ_BIND_LOOPBACK"
	envSecretRoot        = "RURALZ_SECRET_ROOT"
	envFetchAllow        = "RURALZ_FETCH_ALLOW"
	envAdminTLSDir       = "RURALZ_ADMIN_TLS_DIR"
	envAdminMetricsToken = "RURALZ_ADMIN_METRICS_TOKEN_FILE"
)

// loopbackFetchAllow is the RURALZ_FETCH_ALLOW the child gets when the
// CLI's environment leaves it unset or empty, so a Bundle reaches local
// mocks.
const loopbackFetchAllow = "127.0.0.0/8,::1"

// Environ returns the child's environment
// (docs/reference/01-cli-and-api-surface.md "Local ruralzd launched by the
// CLI", Environment): the CLI's environ, in os.Environ form, with
// RURALZ_CONFIG, RURALZ_DATA_DIR, RURALZ_ADMIN_TOKEN_FILE and
// RURALZ_BIND_LOOPBACK=true set from ws, RURALZ_SECRET_ROOT set to ws's
// private secret root when secretRoot is set, RURALZ_FETCH_ALLOW set to
// 127.0.0.0/8,::1 when environ leaves it unset or empty, and
// RURALZ_ADMIN_TLS_DIR and RURALZ_ADMIN_METRICS_TOKEN_FILE removed.
// Everything else passes through, RURALZ_LOG_LEVEL, RURALZ_SECRET_*,
// RURALZ_STATE_STORE_URL and RURALZ_STATE_STORE_MAC_KEY_FILE included, and
// without secretRoot the CLI's own RURALZ_SECRET_ROOT applies. An entry the
// launcher sets replaces every entry of that name.
func Environ(environ []string, ws *Workspace, secretRoot bool) []string {
	set := []string{
		envConfig + "=" + ws.Config(),
		envDataDir + "=" + ws.DataDir(),
		envAdminTokenFile + "=" + ws.TokenFile(),
		envBindLoopback + "=true",
	}
	drop := map[string]bool{
		envConfig: true, envDataDir: true, envAdminTokenFile: true, envBindLoopback: true,
		envAdminTLSDir: true, envAdminMetricsToken: true,
	}
	if secretRoot {
		set = append(set, envSecretRoot+"="+ws.SecretRoot())
		drop[envSecretRoot] = true
	}
	if v, ok := lookup(environ, envFetchAllow); !ok || v == "" {
		set = append(set, envFetchAllow+"="+loopbackFetchAllow)
		drop[envFetchAllow] = true
	}
	out := make([]string, 0, len(environ)+len(set))
	for _, kv := range environ {
		if drop[name(kv)] {
			continue
		}
		out = append(out, kv)
	}
	return append(out, set...)
}

// name returns the name of an environ entry; a leading '=' belongs to the
// name, as in Windows "=C:" entries.
func name(kv string) string {
	if i := strings.IndexByte(kv[min(1, len(kv)):], '='); i >= 0 {
		return kv[:i+min(1, len(kv))]
	}
	return kv
}

// lookup returns the value of the first entry named key in environ, as
// the process environment reads it.
func lookup(environ []string, key string) (string, bool) {
	for _, kv := range environ {
		if name(kv) == key && len(kv) > len(key) {
			return kv[len(key)+1:], true
		}
	}
	return "", false
}
