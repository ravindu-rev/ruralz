// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonschema

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/phase"
)

// orderSchema reads the corpus schema of the 1 KiB order body.
func orderSchema(t testing.TB) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "schemas", "order.json"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// order1KiB returns a valid order body of about 1 KiB.
func order1KiB() []byte {
	var b strings.Builder
	b.WriteString(`{"id":"ord_0123abcd","customer":{"email":"buyer@example.com","name":"A. Buyer"},"items":[`)
	for i := range 12 {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"sku":"SKU-%05d-BLUE","qty":%d,"price":%d.99}`, i, i+1, 10+i)
	}
	b.WriteString(`],"total":1234.56,"notes":"leave at the door, ring twice","status":"paid"}`)
	return []byte(b.String())
}

// TestHandleAddsNoAllocations checks that Handle allocates nothing beyond
// the strict decode and the library's validation of a valid body, the
// decision pass.
func TestHandleAddsNoAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("sync.Pool drops items at random under -race")
	}
	f := buildFilter(t, orderSchema(t), testLimit)
	body := order1KiB()
	x, _ := exchange("application/json", body)
	ctx := t.Context()
	handleAllocs := testing.AllocsPerRun(200, func() { f.Handle(ctx, phase.OnRequestBody, x) })
	baseAllocs := testing.AllocsPerRun(200, func() {
		v, _, _ := f.decode(body)
		f.schema.valid(v)
	})
	if handleAllocs > baseAllocs {
		t.Errorf("Handle allocates %.0f, decode and validate %.0f: the Filter adds allocations", handleAllocs, baseAllocs)
	}
}
