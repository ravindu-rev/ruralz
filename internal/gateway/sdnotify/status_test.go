// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package sdnotify

import (
	"testing"

	"github.com/ravindu-rev/ruralz/internal/adminapi"
)

// TestStatusOf checks the STATUS= values of "Handover under systemd":
// "ready rev-<12 hex>"; "not ready: <reason> <code> <detail>" with reasons
// joined by a semicolon and a space and empty fields left out; "draining"
// whenever a draining reason is present; always one line of valid UTF-8.
func TestStatusOf(t *testing.T) {
	unresolved := adminapi.ReadyReason{Reason: adminapi.ReasonSecretsUnresolved, Code: "RZ-CFG-026", Detail: "file:/etc/ruralz/tls/api.key"}
	unbound := adminapi.ReadyReason{Reason: adminapi.ReasonListenersUnbound, Code: "RZ-CFG-039", Detail: "web"}
	noRevision := adminapi.ReadyReason{Reason: adminapi.ReasonNoRevision}
	draining := adminapi.ReadyReason{Reason: adminapi.ReasonDraining}
	tests := []struct {
		name string
		v    adminapi.Readyz
		want string
	}{
		{name: "ready", v: readyAt(rev), want: "ready " + rev},
		{name: "ready without a revision", v: adminapi.Readyz{Status: "ready"}, want: "ready"},
		{name: "no revision", v: notReady(noRevision), want: "not ready: no_revision"},
		{
			name: "every reason",
			v:    notReady(noRevision, unresolved, unbound),
			want: "not ready: no_revision; secrets_unresolved RZ-CFG-026 file:/etc/ruralz/tls/api.key; listeners_unbound RZ-CFG-039 web",
		},
		{name: "code without detail", v: notReady(adminapi.ReadyReason{Reason: adminapi.ReasonListenersUnbound, Code: "RZ-CFG-039"}), want: "not ready: listeners_unbound RZ-CFG-039"},
		{name: "draining alone", v: notReady(draining), want: "draining"},
		{name: "draining among other reasons", v: notReady(noRevision, unbound, draining), want: "draining"},
		{
			name: "a detail cannot add an assignment",
			v:    notReady(adminapi.ReadyReason{Reason: adminapi.ReasonSecretsUnresolved, Detail: "file:/a\nSTOPPING=1\r\tx"}),
			want: "not ready: secrets_unresolved file:/a STOPPING=1  x",
		},
		{
			name: "invalid UTF-8 is replaced",
			v:    notReady(adminapi.ReadyReason{Reason: adminapi.ReasonListenersUnbound, Detail: "w\xffeb"}),
			want: "not ready: listeners_unbound w�eb",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := StatusOf(tt.v); got != tt.want {
				t.Errorf("StatusOf(%+v) = %q, want %q", tt.v, got, tt.want)
			}
		})
	}
}
