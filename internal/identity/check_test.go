// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Table tests for the static checks of spec 06 requirement 20 (RZ-CFG-035
// duplicate basic username, certificate subject or uriSan across
// Consumers; RZ-CFG-036 iterations outside 600,000 to 1,000,000) and the
// RZ-CFG-005 rule of OQ-security-and-identity-24 (a) (oauthClients need a
// jwt issuer).

// bundleOf wraps Consumers as validated resources whose source start is
// line = position in the list + 1.
func bundleOf(cs ...*v1alpha1.Consumer) *hub.Bundle {
	rs := make([]*hub.Resource, 0, len(cs))
	for i, c := range cs {
		rs = append(rs, &hub.Resource{
			ID:     hub.ID{Kind: v1alpha1.KindConsumer, Name: c.Metadata.Name},
			Object: c,
			Source: hub.Source{File: "consumers.yaml", Start: diag.Location{File: "consumers.yaml", Line: i + 1, Column: 1}},
		})
	}
	return hub.NewBundle(rs)
}

// finding is the comparable part of a diagnostic.
type finding struct {
	code, resource, path string
	related              int // line of the first Related location, 0 for none
}

func findingsOf(l diag.List) []finding {
	out := make([]finding, 0, len(l))
	for _, d := range l {
		f := finding{code: d.Code, resource: d.Resource.Kind + "/" + d.Resource.Name, path: d.Path.String()}
		if len(d.Related) > 0 {
			f.related = d.Related[0].Line
		}
		out = append(out, f)
	}
	return out
}

func TestReq20StaticChecks(t *testing.T) {
	tests := []struct {
		name string
		cs   []*v1alpha1.Consumer
		want []finding
	}{
		{
			name: "clean",
			cs: []*v1alpha1.Consumer{
				consumer("a", withBasic("ua", cfgBasicHash, nil), withCert("c", "CN=a", ""), withSubject(iss, "a"), withClient("app")),
				consumer("b", withBasic("ub", cfgBasicHash, i32(1_000_000)), withCert("c", "", "spiffe://b")),
			},
		},
		{
			name: "basic username in two Consumers",
			cs: []*v1alpha1.Consumer{
				consumer("a", withBasic("shared", cfgBasicHash, nil)),
				consumer("b", withBasic("shared", cfgBasicHash, nil)),
			},
			want: []finding{{CodeDuplicate, "Consumer/b", "spec.credentials.basic[username=shared].username", 1}},
		},
		{
			name: "three declarations report the later two",
			cs: []*v1alpha1.Consumer{
				consumer("a", withBasic("shared", cfgBasicHash, nil)),
				consumer("b", withBasic("shared", cfgBasicHash, nil)),
				consumer("c", withBasic("shared", cfgBasicHash, nil)),
			},
			want: []finding{
				{CodeDuplicate, "Consumer/b", "spec.credentials.basic[username=shared].username", 1},
				{CodeDuplicate, "Consumer/c", "spec.credentials.basic[username=shared].username", 1},
			},
		},
		{
			name: "one Consumer repeating a value is not RZ-CFG-035",
			cs: []*v1alpha1.Consumer{
				consumer("a", withCert("one", "CN=a", ""), withCert("two", "CN=a", "")),
			},
		},
		{
			name: "certificate subject and uriSan in two Consumers",
			cs: []*v1alpha1.Consumer{
				consumer("a", withCert("x", "CN=dup", ""), withCert("y", "", "spiffe://dup")),
				consumer("b", withCert("s", "CN=dup", ""), withCert("u", "", "spiffe://dup")),
			},
			want: []finding{
				{CodeDuplicate, "Consumer/b", "spec.credentials.certificates[name=s].subject", 1},
				{CodeDuplicate, "Consumer/b", "spec.credentials.certificates[name=u].uriSan", 1},
			},
		},
		{
			name: "a subject equal to another Consumer's uriSan is not a duplicate",
			cs: []*v1alpha1.Consumer{
				consumer("a", withCert("x", "spiffe://same", "")),
				consumer("b", withCert("y", "", "spiffe://same")),
			},
		},
		{
			name: "iterations out of range",
			cs: []*v1alpha1.Consumer{
				consumer("a", withBasic("low", cfgBasicHash, i32(599_999)), withBasic("high", cfgBasicHash, i32(1_000_001)),
					withBasic("min", cfgBasicHash, i32(600_000))),
			},
			want: []finding{
				{CodeIterations, "Consumer/a", "spec.credentials.basic[username=low].iterations", 0},
				{CodeIterations, "Consumer/a", "spec.credentials.basic[username=high].iterations", 0},
			},
		},
		{
			name: "oauthClients without a jwt issuer",
			cs:   []*v1alpha1.Consumer{consumer("a", withClient("app"))},
			want: []finding{{CodeSchema, "Consumer/a", "spec.credentials.oauthClients", 0}},
		},
		{
			name: "jwt entries that bind nothing (req 16)",
			cs: []*v1alpha1.Consumer{
				consumer("a", withClaims("https://idp", map[string]string{}), withSubject("https://other", ""),
					withSubject("https://ok", "s"), withClaims("https://ok2", map[string]string{"env": "prod"})),
			},
			want: []finding{
				{CodeSchema, "Consumer/a", "spec.credentials.jwt[issuer=https://idp].claims", 0},
				{CodeSchema, "Consumer/a", "spec.credentials.jwt[issuer=https://other].subject", 0},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := Diagnostics(bundleOf(tt.cs...), nil)
			got := findingsOf(l)
			if len(got) != len(tt.want) {
				t.Fatalf("findings = %+v, want %+v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("finding %d = %+v, want %+v", i, got[i], tt.want[i])
				}
				if l[i].Severity != diag.SeverityError || l[i].Message == "" {
					t.Errorf("finding %d: severity %v, message %q", i, l[i].Severity, l[i].Message)
				}
			}
		})
	}
}

func TestReq20CheckLocations(t *testing.T) {
	b := bundleOf(
		consumer("a", withBasic("shared", cfgBasicHash, nil)),
		consumer("b", withBasic("shared", cfgBasicHash, nil)),
	)
	loc := func(r *hub.Resource, p diag.Path) diag.Location {
		return diag.Location{File: r.Name + ".yaml", Line: len(p), Column: 3}
	}
	l := Diagnostics(b, loc)
	if len(l) != 1 {
		t.Fatalf("findings = %v", l)
	}
	d := l[0]
	if d.Location != (diag.Location{File: "b.yaml", Line: 5, Column: 3}) {
		t.Errorf("location = %+v", d.Location)
	}
	if len(d.Related) != 1 || d.Related[0].Location != (diag.Location{File: "a.yaml", Line: 5, Column: 3}) ||
		d.Related[0].Message != "declared in" {
		t.Errorf("related = %+v", d.Related)
	}
	if !strings.Contains(d.Message, `"a"`) {
		t.Errorf("message %q does not name the first Consumer", d.Message)
	}
	var text strings.Builder
	if err := diag.WriteText(&text, l); err != nil {
		t.Fatal(err)
	}
	want := `b.yaml:5:3 error RZ-CFG-035 Consumer/b spec.credentials.basic[username=shared].username: basic username "shared" is already declared by Consumer "a" (declared in a.yaml:5:3)` + "\n"
	if text.String() != want {
		t.Errorf("text form:\n%s\nwant:\n%s", text.String(), want)
	}
}

func TestReq20CheckIgnoresOtherResources(t *testing.T) {
	Check(nil, nil, func(diag.Diagnostic) { t.Fatal("nil Bundle produced a finding") })
	b := hub.NewBundle([]*hub.Resource{
		{ID: hub.ID{Kind: v1alpha1.KindRoute, Name: "r"}, Object: &v1alpha1.Route{}},
		// A Consumer resource whose typed view is missing is skipped.
		{ID: hub.ID{Kind: v1alpha1.KindConsumer, Name: "broken"}, Object: &v1alpha1.Route{}},
	})
	if l := Diagnostics(b, nil); len(l) != 0 {
		t.Fatalf("findings = %v", l)
	}
}
