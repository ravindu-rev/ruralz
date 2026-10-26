// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonschema

import (
	"fmt"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/filter"
)

// TestCompileScale covers the compile budget structurally: a schema at
// the 256 KiB size bound and the library's quadratic case at MaxSubschemas
// compile without asking the loader, one more position past the bound is
// one finding, and Check of the large schema names the one broken member.
// The time each takes is measured by BenchmarkCompileSchema256KiB and
// BenchmarkCompileSchemaWorstCase, not here, so the test does not depend on
// the machine.
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
// (TestRecursiveBodies counts allocations; BenchmarkValidate1KiB times
// it).
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
