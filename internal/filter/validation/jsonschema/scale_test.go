// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonschema

import (
	"fmt"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/filter"
)

// TestCompileScale covers the compile bounds structurally: a schema at
// the 256 KiB size bound and the library's quadratic case at MaxSubschemas
// compile without asking the loader, one more position past the bound is
// one finding, and Check of the large schema names the one broken member.
// It reads no clock, so it does not depend on the machine.
func TestCompileScale(t *testing.T) {
	big := schema256KiB()
	if n := len(big); n < MaxSchemaBytes-1024 || n > MaxSchemaBytes {
		t.Fatalf("schema256KiB is %d bytes", n)
	}
	for _, raw := range []string{big, refsToRoot(MaxSubschemas - 1)} {
		spy := &spyLoader{}
		if s, _, issues := compileRaw([]byte(raw), spy); s == nil || len(issues) > 0 || len(spy.urls) > 0 {
			t.Errorf("compile(%.60q...) = %v, %+v, loads %q; want a schema without loads", raw, s, issues, spy.urls)
		}
	}
	if issues := compileIssues(t, refsToRoot(2*MaxSubschemas)); len(issues) != 1 || !strings.Contains(issues[0].Message, "subschemas") {
		t.Errorf("issues = %+v, want the subschema bound alone", issues)
	}
	bad := strings.Replace(big, `"maxLength":64`, `"pattern":"(?<=x)"`, 1)
	if got := runCheck(policyResource("big", bad)); len(got) != 1 || !strings.HasSuffix(got[0].Path.String(), ".pattern") {
		t.Errorf("Check = %+v", got)
	}
}

// TestValidateScale checks large bodies structurally: an object of 20,000
// members and an array of 50,000 items validate, and a refusal names the
// failing keyword. The validation stays linear in the body
// (TestRecursiveBodies counts allocations).
func TestValidateScale(t *testing.T) {
	f := buildFilter(t, `{"type":"object","patternProperties":{"^k":{"type":"integer","minimum":0}},`+
		`"additionalProperties":false,"properties":{"list":{"type":"array","items":{"type":"string","maxLength":8},"uniqueItems":true}}}`, 64<<20)
	var b strings.Builder
	b.WriteString(`{"list":[`)
	for i := range 50000 {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"%x"`, i)
	}
	b.WriteString(`]`)
	for i := range 20000 {
		fmt.Fprintf(&b, `,"k%d":%d`, i, i)
	}
	b.WriteString(`}`)
	body := b.String()
	if r := handle(t, f, body); r.Outcome != filter.Continue {
		t.Errorf("Handle = %+v", r)
	}
	bad := strings.Replace(body, `"k19999":19999`, `"k19999":-1`, 1)
	if got := detailOf(t, handle(t, f, bad)); got != DetailMismatch+" at /patternProperties/^k/minimum" {
		t.Errorf("detail = %q", got)
	}
}

// schema256KiB returns a schema of about 256 KiB in canonical form
// within MaxSubschemas: 340 documented object properties of four
// subschemas each, sharing one definition.
func schema256KiB() string {
	var b strings.Builder
	b.WriteString(`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object",` +
		`"$defs":{"money":{"type":"string","pattern":"^[0-9]+\\.[0-9]{2}$"}},"properties":{`)
	desc := strings.Repeat("Describes the field for API consumers and documentation. ", 10)
	for i := 0; b.Len() < MaxSchemaBytes-900; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"field_%04d":{"description":%q,"type":"object","required":["name"],"properties":{"name":{"type":"string","maxLength":64},"amount":{"$ref":"#/$defs/money"},"count":{"type":"integer","minimum":0}}}`, i, desc)
	}
	b.WriteString(`}}`)
	return b.String()
}

// refsToRoot returns a schema with n properties that each refer to the
// root.
func refsToRoot(n int) string {
	var b strings.Builder
	b.WriteString(`{"properties":{`)
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"%x":{"$ref":"#"}`, i)
	}
	b.WriteString(`}}`)
	return b.String()
}
