// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package precedence

import (
	"strings"
	"testing"
	"unicode"

	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/registry"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// TestRowsPrintableRemovedType is TestRowsPrintable's row for the Type of
// a removed Policy, which Rows reads from its Bundle argument: a type
// holding a tab and a newline, which no registry lists, is escaped too.
func TestRowsPrintableRemovedType(t *testing.T) {
	b := newBuilder()
	odd := b.policy("odd", v1alpha1.PolicyTypeRateLimit, nil)
	odd.Spec.Type = "x\ty\nz"
	c := &hub.Chain{Route: "r", Removed: []hub.Removed{{Policy: "odd", Slot: "odd", Reason: hub.Excluded}}}
	rows := mustNew(t, nil, Options{}).Rows(c, b.bundle())
	if len(rows) != 1 || rows[0].Type != `x\ty\nz` || rows[0].FilterClass != "" {
		t.Errorf("rows = %+v, want one row of Type %q and no class", rows, `x\ty\nz`)
	}
}

// TestRowsPrintable covers the WP-38 S2 round-4 minor that Rows copied
// authored text into the render --effective table unescaped, so a slot
// holding "\n" and "\t" forged a row for a Policy that does not exist,
// and its class: every Row field that carries authored text, live and
// removed. Each case puts a newline, a tab, an ESC byte, a DEL byte, a C1
// control, an invalid UTF-8 byte, a backslash or a line separator into
// one authored value; the 02 req 51 table then has exactly one line per
// row plus the header and no character a terminal or tabwriter would act
// on, the affected fields hold the strconv.Quote escape of the value, and
// two values that differ only in an escaped character stay distinct.
func TestRowsPrintable(t *testing.T) {
	const forged = "x\nonRequestHeaders\tclient\tauthz-strict\tGateway\town slot, not overridable"
	const forgedText = `x\nonRequestHeaders\tclient\tauthz-strict\tGateway\town slot, not overridable`
	for _, tc := range []struct {
		name string
		// build adds the Policies, Upstreams and Routes; the Route "r" is
		// rendered.
		build func(b *builder)
		// policy names the row to check, want the text of its fields.
		policy string
		want   map[string]string
	}{
		{"Route Policy slot: the forged row of the finding", func(b *builder) {
			b.policy("jwt-default", v1alpha1.PolicyTypeAuthJWT, nil, locked)
			b.policy("noop", v1alpha1.PolicyTypeHeaders, headersCfg(true, false), slot(forged))
			b.gateway("jwt-default")
			b.route("r", []string{"noop"}, nil, nil)
		}, "noop", map[string]string{"Reason": "slot " + forgedText}},
		{"Route Policy slot field", func(b *builder) {
			b.policy("noop", v1alpha1.PolicyTypeHeaders, headersCfg(true, false), slot(forged))
			b.route("r", []string{"noop"}, nil, nil)
		}, "noop", map[string]string{"Slot": forgedText}},
		{"Gateway Policy slot, inherited", func(b *builder) {
			b.policy("g", v1alpha1.PolicyTypeRateLimit, nil, slot("a\x1b[2Jb"))
			b.gateway("g")
			b.route("r", nil, nil, nil)
		}, "g", map[string]string{"Slot": `a\x1b[2Jb`, "Reason": `slot a\x1b[2Jb, inherited`}},
		{"Gateway Policy replaced: removed row", func(b *builder) {
			b.policy("g", v1alpha1.PolicyTypeRateLimit, nil, slot("s\tt"))
			b.policy("x", v1alpha1.PolicyTypeRateLimit, nil, slot("s\tt"))
			b.gateway("g")
			b.route("r", []string{"x"}, nil, nil)
		}, "g", map[string]string{"Slot": `s\tt`, "Reason": `replaced in slot s\tt by x`}},
		{"Route Policy replacing: live row", func(b *builder) {
			b.policy("g", v1alpha1.PolicyTypeRateLimit, nil, slot("s\u2028t"))
			b.policy("x", v1alpha1.PolicyTypeRateLimit, nil, slot("s\u2028t"))
			b.gateway("g")
			b.route("r", []string{"x"}, nil, nil)
		}, "x", map[string]string{"Slot": `s\u2028t`, "Reason": `slot s\u2028t, replaces g`}},
		{"Upstream name: Leg", func(b *builder) {
			b.policy("h", v1alpha1.PolicyTypeHeaders, headersCfg(true, false))
			b.upstream("up\nx", "h")
			b.route("r", nil, nil, []string{"up\nx"})
		}, "h", map[string]string{"Leg": `up\nx`, "From": `Upstream up\nx`, "Reason": `leg up\nx only`}},
		{"Upstream name: From and Reason", func(b *builder) {
			b.policy("h", v1alpha1.PolicyTypeHeaders, headersCfg(true, false))
			b.upstream("up\tx", "h")
			b.route("r", nil, nil, []string{"up\tx"})
		}, "h", map[string]string{"Leg": `up\tx`, "From": `Upstream up\tx`, "Reason": `leg up\tx only`}},
		{"Policy name", func(b *builder) {
			b.policy("p\u009bq", v1alpha1.PolicyTypeRateLimit, nil, slot("s"))
			b.route("r", []string{"p\u009bq"}, nil, nil)
		}, `p\u009bq`, map[string]string{"Policy": `p\u009bq`, "Reason": "slot s"}},
		{"DEL byte", func(b *builder) {
			b.policy("p", v1alpha1.PolicyTypeRateLimit, nil, slot("a\x7fb"))
			b.route("r", []string{"p"}, nil, nil)
		}, "p", map[string]string{"Slot": `a\x7fb`, "Reason": `slot a\x7fb`}},
		{"control after printable non-ASCII", func(b *builder) {
			b.policy("p", v1alpha1.PolicyTypeRateLimit, nil, slot("é\n"))
			b.route("r", []string{"p"}, nil, nil)
		}, "p", map[string]string{"Slot": `é\n`}},
		{"invalid UTF-8 byte", func(b *builder) {
			b.policy("p", v1alpha1.PolicyTypeRateLimit, nil, slot("a\xffb"))
			b.route("r", []string{"p"}, nil, nil)
		}, "p", map[string]string{"Slot": `a\xffb`, "Reason": `slot a\xffb`}},
		{"backslash: tells a written \\n from a newline", func(b *builder) {
			b.policy("p", v1alpha1.PolicyTypeRateLimit, nil, slot(`a\nb`))
			b.route("r", []string{"p"}, nil, nil)
		}, "p", map[string]string{"Slot": `a\\nb`, "Reason": `slot a\\nb`}},
		{"printable text unchanged", func(b *builder) {
			b.policy("p", v1alpha1.PolicyTypeRateLimit, nil, slot("café \"q\" ü"))
			b.route("r", []string{"p"}, nil, nil)
		}, "p", map[string]string{"Slot": "café \"q\" ü", "Reason": "slot café \"q\" ü"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBuilder()
			tc.build(b)
			bundle := b.bundle()
			r := mustNew(t, registry.New(), Options{Body: bodyOracle{}})
			chains, _, err := r.Run(t.Context(), bundle, nil, 1)
			if err != nil {
				t.Fatal(err)
			}
			rows := r.Rows(chains["r"], bundle)
			text := table(t, rows)
			if lines := strings.Count(text, "\n"); lines != len(rows)+1 {
				t.Errorf("table has %d lines for %d rows:\n%s", lines, len(rows), text)
			}
			for _, c := range strings.TrimSuffix(text, "\n") {
				if c != '\n' && (!unicode.IsPrint(c) || c == '\uFFFD') {
					t.Errorf("table holds %U:\n%s", c, text)
				}
			}
			row, ok := policyRow(rows, tc.policy)
			if !ok {
				t.Fatalf("no row for %q:\n%s", tc.policy, text)
			}
			fields := map[string]string{
				"Phase": row.Phase, "Leg": row.Leg, "Policy": row.Policy, "Type": row.Type, "FilterClass": row.FilterClass,
				"Slot": row.Slot, "From": row.From, "Reason": row.Reason,
			}
			for f, want := range tc.want {
				if fields[f] != want {
					t.Errorf("%s = %q, want %q", f, fields[f], want)
				}
			}
		})
	}
}
