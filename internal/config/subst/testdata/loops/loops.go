// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package loops is a fixture of TestUncountedLoopsFound: every loop
// marked "uncounted" counts no iteration, and every other loop counts each
// one, ranges over a literal or a constant, or says why it is bounded.
package loops

type meter struct{ units int }

func (m *meter) tick() { m.units++ }

// helper counts through tick, so a loop calling it counts.
func (m *meter) helper() { m.tick() }

const three = 3

func loops(m *meter, s []int, read func(int)) {
	for range s { // uncounted
	}
	for _, v := range s {
		m.tick()
		_ = v
	}
	for range s {
		m.helper()
	}
	for i := range s {
		read(i)
	}
	for _, v := range s { // uncounted
		if v > 0 {
			m.tick()
		}
	}
	for _, v := range s {
		if m.check(v) {
			continue
		}
	}
	for i := 0; i < len(s); i++ { // uncounted
		for range s {
			m.tick()
		}
	}
	for range [...]int{1, 2} {
	}
	for range three {
	}
	// bounded: s holds at most three elements here.
	for range s {
	}
	for range s { // bounded: as above.
	}
	// counted: every 64 iterations.
	for i := range s {
		if i%64 == 0 {
			m.tick()
		}
	}
	// counted: a comment that names no counting call does not exempt.
	for range s { // uncounted
	}
	f := func() {
		for range s { // uncounted
		}
	}
	f()
}

func (m *meter) check(v int) bool {
	m.tick()
	return v > 0
}
