// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package subst

import "strings"

// The functions of this file run outside stage E's units, so they call
// no Yield: Environ once per process on the binary's environment, and
// Escape on rendered values (TestLookupsMetered checks the other files).

// Environ returns the variables of an os.Environ snapshot taken by the
// binary's entry point (this package never reads the process
// environment). Entries without "=" or with an empty name (the Windows
// "=C:" drive entries) are skipped; the first of two equal names wins, as
// os.Getenv reads it.
func Environ(environ []string) Map {
	m := make(Map, len(environ))
	// bounded: runs once per process, outside stage E units.
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			continue
		}
		if _, dup := m[k]; !dup {
			m[k] = v
		}
	}
	return m
}

// Escape writes every literal ${ in s as $${, the form substitution reads
// back as ${: substituting Escape(s) yields s.
func Escape(s string) string { return strings.ReplaceAll(s, "${", "$${") }
