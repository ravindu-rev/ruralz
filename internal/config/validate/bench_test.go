// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package validate

import (
	"runtime"
	"testing"
)

// BenchmarkRun measures stage H on the example Bundle and on a Bundle of
// about 3,000 resources with about 4,000 findings, with every processor.
func BenchmarkRun(b *testing.B) {
	for _, bc := range []struct {
		name  string
		files func(testing.TB) map[string]string
	}{
		{"example", func(t testing.TB) map[string]string { return readDirs(t, "testdata/shop-bundle") }},
		{"findings-1000", func(t testing.TB) map[string]string { return scaleFiles(t, 1000) }},
	} {
		b.Run(bc.name, func(b *testing.B) {
			bundle, files := loadFiles(b, bc.files(b))
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
