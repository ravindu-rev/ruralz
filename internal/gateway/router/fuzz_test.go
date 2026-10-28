// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package router

import "testing"

// FuzzMatchesReference is the Router match and header handling target of
// docs/engineering/03-testing-and-quality-strategy.md "Fuzzing": on every
// generated listener and request, Match equals the reference matcher.
func FuzzMatchesReference(f *testing.F) {
	progs := compileWhens(f)
	for _, seed := range [][]byte{
		nil,
		[]byte("routes"),
		{3, 7, 1, 2, 3, 4, 1, 0, 2, 1, 0, 9, 8, 7, 6, 5, 4, 3, 2, 1, 0},
		{2, 7, 0, 1, 1, 2, 2, 1, 3, 1, 0, 0, 1, 2, 4, 2, 2, 1, 3, 5, 6, 1, 1, 2, 3, 2, 1, 4, 4, 4},
		{1, 5, 2, 2, 4, 1, 1, 3, 0, 1, 3, 2, 3, 2, 1, 1, 2, 3, 0, 2, 2, 1, 4, 5, 1, 0, 1, 3, 2, 2, 1},
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		checkReference(t, progs, data)
	})
}
