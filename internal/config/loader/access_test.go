// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package loader

import (
	"slices"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// TestEnvironmentsFileOrder covers Environments.FileOrder: the names of
// the Environments that passed every check, in the order the file
// declares them, unlike the sorted Names; a failing Environment and a
// duplicate's second declaration are left out.
func TestEnvironmentsFileOrder(t *testing.T) {
	e := readEnvs(t, newLoader(t, Options{}), docs(
		environment("staging", " {}"),
		environment("prod", " {}"),
		environment("broken", "\n  overlay: 7"),
		environment("prod", "\n  overlay: other"),
		environment("canary", " {}"),
	))
	if got, want := e.FileOrder(), []string{"staging", "prod", "canary"}; !slices.Equal(got, want) {
		t.Errorf("FileOrder() = %q, want %q", got, want)
	}
	if got, want := e.Names(), []string{"canary", "prod", "staging"}; !slices.Equal(got, want) {
		t.Errorf("Names() = %q, want %q", got, want)
	}
	order := e.FileOrder()
	order[0] = "changed"
	if e.FileOrder()[0] != "staging" {
		t.Error("FileOrder returns the Environments' own slice")
	}
}

// TestBundleStopped covers Bundle.Stopped: false for a Bundle whose stages
// A to C ran to the end, findings or not, and true for one a limit or the
// diagnostics cap stopped, which every Render then reports as stopped.
func TestBundleStopped(t *testing.T) {
	tests := []struct {
		name   string
		limits Limits
		files  map[string]string
		want   bool
	}{
		{"clean", Limits{}, minimal(), false},
		{"finding", Limits{}, map[string]string{RootFile: gateway, "a.yaml": docs(upstream("a"), upstream("a"))}, false},
		{"source limit", Limits{MaxSourceBytes: 16}, minimal(), true},
		{"no root file", Limits{}, map[string]string{"a.yaml": upstream("a")}, true},
		{"diagnostics cap", Limits{MaxDiagnostics: 1}, map[string]string{
			RootFile: gateway, "a.yaml": docs(upstream("a"), upstream("a"), upstream("a")),
		}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l := newLoader(t, Options{Limits: tc.limits})
			b, err := l.Parse(t.Context(), Source{FS: mapFS(tc.files), Path: "."})
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got := b.Stopped(); got != tc.want {
				t.Errorf("Stopped() = %v, want %v; diagnostics %q", got, tc.want, texts(b.Diagnostics()))
			}
			r, err := b.Render(t.Context(), nil, nil)
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			if tc.want && !r.Stopped {
				t.Error("Render of a stopped Bundle did not stop")
			}
		})
	}
}

// TestResultText covers Result.Text: the bytes of every base and overlay
// file exactly as read, CR LF line breaks included, by the FileID the
// resources' positions carry; an unknown FileID has none, and neither has
// a Result that read no file.
func TestResultText(t *testing.T) {
	m := mapFS(map[string]string{
		RootFile:                   gateway,
		"routes/a.yaml":            "apiVersion: ruralz/v1alpha1\r\nkind: Upstream\r\nmetadata:\r\n  name: a\r\nspec:\r\n  protocol: http\r\n  endpoints: [{address: \"a:80\"}]\r\n",
		"overlays/prod/patch.yaml": upstream("b"),
	})
	r := loadEnv(t, newLoader(t, Options{}), m, environment("prod", " {}"), "prod")
	wantClean(t, "load", r.Diagnostics)
	for _, id := range []string{"Gateway/edge", "Upstream/a", "Upstream/b"} {
		res := find(t, r.Resources, id)
		data, ok := r.Text(res.Start.File)
		path := r.Files.File(res.Start.File).Path
		if !ok || string(data) != string(m[path].Data) {
			t.Errorf("Text(%s's file %s) = %q, %v; want the file's bytes", id, path, data, ok)
		}
	}
	if data, ok := r.Text(tree.FileID(1 << 20)); ok || data != nil {
		t.Errorf("Text(unknown) = %q, %v; want none", data, ok)
	}
	var none Result
	if _, ok := none.Text(0); ok {
		t.Error("Text of a Result without a load reported a file")
	}
}
