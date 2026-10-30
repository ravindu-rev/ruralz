// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package launch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/adminapi"
	"github.com/ravindu-rev/ruralz/internal/cli/adminclient"
	"github.com/ravindu-rev/ruralz/internal/cli/command"
	"github.com/ravindu-rev/ruralz/internal/nodedir"
	"github.com/ravindu-rev/ruralz/internal/ulid"
)

// TestReasons checks the one-line form of a /readyz answer's reasons.
func TestReasons(t *testing.T) {
	r := adminclient.Readiness{Readyz: adminapi.Readyz{Status: "not_ready", Reasons: []adminapi.ReadyReason{
		{Reason: adminapi.ReasonNoRevision},
		{Reason: adminapi.ReasonSecretsUnresolved, Code: "RZ-CFG-026", Detail: "file:/run/secrets/tls.pem is unreadable"},
		{Reason: adminapi.ReasonListenersUnbound, Code: "RZ-CFG-039"},
	}}}
	want := "no_revision; secrets_unresolved RZ-CFG-026: file:/run/secrets/tls.pem is unreadable; listeners_unbound RZ-CFG-039"
	if got := Reasons(r); got != want {
		t.Errorf("Reasons = %q, want %q", got, want)
	}
	if got := Reasons(adminclient.Readiness{Readyz: adminapi.Readyz{Status: "not_ready"}}); got != "not_ready" {
		t.Errorf("Reasons without reasons = %q", got)
	}
	if got := Reasons(adminclient.Readiness{}); got != "no reason given" {
		t.Errorf("Reasons of an empty answer = %q", got)
	}
}

// TestCheckHolder: no holder.json yet is no holder; the CLI's own version
// passes; another version, and a record that does not decode, exit 2.
func TestCheckHolder(t *testing.T) {
	ws := &Workspace{root: t.TempDir()}
	if err := os.Mkdir(ws.DataDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	o := Options{Version: "0.4.0"}
	if held, err := CheckHolder(ws, "ruralzd", o); held || err != nil {
		t.Fatalf("CheckHolder without holder.json = %v, %v", held, err)
	}
	id, err := ulid.Parse("01ARYZ6S41TSV4RRFFQ69G5FAV")
	if err != nil {
		t.Fatal(err)
	}
	write := func(version string) {
		h, err := nodedir.NewHolder(id, version)
		if err != nil {
			t.Fatal(err)
		}
		data, err := h.Encode()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ws.DataDir(), nodedir.HolderFile), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("0.4.0")
	if held, err := CheckHolder(ws, "ruralzd", o); !held || err != nil {
		t.Fatalf("CheckHolder with the same version = %v, %v", held, err)
	}
	write("0.3.9")
	held, err := CheckHolder(ws, "/opt/ruralzd", o)
	want := "/opt/ruralzd is version 0.3.9, but this ruralz is 0.4.0; launch the ruralzd of the same release with --ruralzd PATH"
	if !held || command.Code(nil, err) != command.ExitNoResult || err.Error() != want {
		t.Fatalf("CheckHolder with another version = %v, %v; want exit 2 with %q", held, err, want)
	}
	if err := os.WriteFile(filepath.Join(ws.DataDir(), nodedir.HolderFile), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckHolder(ws, "ruralzd", o); command.Code(nil, err) != command.ExitNoResult || !strings.Contains(err.Error(), "read holder.json") {
		t.Fatalf("CheckHolder with a broken record = %v", err)
	}
}
