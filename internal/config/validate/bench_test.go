// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package validate

import (
	"runtime"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// BenchmarkRun measures stage H on the example Bundle of the golden corpus
// and on a Bundle of about 3,000 resources with about 4,000 findings, with
// every processor.
func BenchmarkRun(b *testing.B) {
	for _, bc := range []struct {
		name string
		load func(testing.TB) (*hub.Bundle, *tree.FileTable)
	}{
		{"example", func(t testing.TB) (*hub.Bundle, *tree.FileTable) { return loadShop(t) }},
		{"findings-1000", func(t testing.TB) (*hub.Bundle, *tree.FileTable) { return loadFiles(t, scaleFiles(t, 1000)) }},
	} {
		b.Run(bc.name, func(b *testing.B) {
			bundle, files := bc.load(b)
			v := newValidator(b, nil)
			b.ReportAllocs()
			for b.Loop() {
				if _, _, err := v.Run(b.Context(), bundle, files, runtime.GOMAXPROCS(0)); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
