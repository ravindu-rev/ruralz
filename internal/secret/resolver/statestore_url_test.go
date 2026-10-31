// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package resolver

import (
	"testing"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/secret"
)

// TestStateStoreURLCodes: a State Store URL that names a non-loopback host
// without credentials is RZ-CFG-042 (TB-4,
// docs/architecture/08-security-and-identity.md "Transport security"); a
// URL that does not parse, like every other failing use, stays
// RZ-CFG-026.
func TestStateStoreURLCodes(t *testing.T) {
	if c, ok := errcode.Lookup(CodeStateStoreURL); !ok || c.Status != 0 || c.Area != errcode.AreaCFG {
		t.Fatalf("%s: registered %v, %+v", CodeStateStoreURL, ok, c)
	}
	tests := []struct {
		name, value, want string
	}{
		{"no credentials", "rediss://cache.internal:6379", CodeStateStoreURL},
		{"sentinel seeds without credentials", "rediss://127.0.0.1:26379?addr=10.0.0.2:26379&master_set=main", CodeStateStoreURL},
		{"unparsable", "redis://[::1", CodeUnresolvable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, nil)
			h.setEnv(EnvStateStoreURL, tc.value)
			diags := h.resolveDiags(use(envRef(EnvStateStoreURL), secret.KindStateStoreURL))
			if len(diags) != 1 || diags[0].Code != tc.want {
				t.Fatalf("diagnostics:\n%s\nwant one %s", diagText(diags), tc.want)
			}
		})
	}
}
