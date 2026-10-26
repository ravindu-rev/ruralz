// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cel

import (
	"sync"

	"github.com/ravindu-rev/ruralz/internal/expr"
)

// programKey identifies a program: the site's environment and checks, and
// the source.
type programKey struct {
	site siteKey
	src  string
}

// ProgramSet is the expr.ProgramSet of one snapshot: the programs its
// Builder compiled or reused. It is immutable.
type ProgramSet struct {
	progs map[programKey]*Program
}

// Compile-time check of the contract.
var _ expr.ProgramSet = (*ProgramSet)(nil)

// Len returns the number of programs.
func (s *ProgramSet) Len() int {
	if s == nil {
		return 0
	}
	return len(s.progs)
}

// Builder is the expr.Builder of one snapshot. Compile is safe for
// concurrent use and compiles each distinct site and source once; Build is
// called once, after every Compile has returned.
type Builder struct {
	c *Compiler
	// prev holds the previous snapshot's programs; read-only.
	prev map[programKey]*Program

	mu      sync.Mutex
	entries map[programKey]*buildEntry
}

// buildEntry is one distinct site and source of a Builder.
type buildEntry struct {
	once sync.Once
	prg  *Program
	iss  []expr.Issue
}

// Compile-time check of the contract.
var _ expr.Builder = (*Builder)(nil)

// Compile returns the program of src at site, reusing the previous
// snapshot's program for an identical site and source, or nil and at least
// one error Issue. An empty src is absent: nil and no Issue.
func (b *Builder) Compile(site expr.Site, src string) (expr.Program, []expr.Issue) {
	if src == "" {
		return nil, nil
	}
	se, ok := resolve(site)
	if !ok {
		// An unknown place is not cached; compile reports it.
		_, iss := b.c.compile(site, src)
		return nil, iss
	}
	key := programKey{site: se.key(), src: src}
	b.mu.Lock()
	e, found := b.entries[key]
	if !found {
		e = &buildEntry{}
		b.entries[key] = e
	}
	b.mu.Unlock()
	e.once.Do(func() {
		if prg, ok := b.prev[key]; ok {
			e.prg = prg
			return
		}
		e.prg, e.iss = b.c.program(site, src)
	})
	if e.prg == nil {
		return nil, e.iss
	}
	return e.prg, nil
}

// Build returns the set of the programs compiled so far; the Builder is
// spent, and programs it compiles afterwards are not in the set.
func (b *Builder) Build() expr.ProgramSet {
	b.mu.Lock()
	defer b.mu.Unlock()
	set := &ProgramSet{progs: make(map[programKey]*Program, len(b.entries))}
	for k, e := range b.entries {
		if e.prg != nil {
			set.progs[k] = e.prg
		}
	}
	b.entries = map[programKey]*buildEntry{}
	return set
}
