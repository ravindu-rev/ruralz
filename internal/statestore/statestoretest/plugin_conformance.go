// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package statestoretest

import (
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/statestore/keys"
)

// pluginKey returns a Plugin guest key of the suite's namespace.
func (s *suite) pluginKey(guest string) string {
	return string(keys.AppendPlugin(nil, s.name("env"), "pol", guest))
}

// pluginGet reads a Plugin key.
func (s *suite) pluginGet(what, key string) *statestore.PluginState {
	c := &statestore.Call{Kind: statestore.OpPluginGet, Timeout: callTimeout, Plugin: statestore.PluginState{Key: key}}
	s.read(c)
	s.answered(what, c)
	return &c.Plugin
}

// pluginIncr increments a Plugin key before commit.
func (s *suite) pluginIncr(key string, delta int64, ttl time.Duration) *statestore.Call {
	c := &statestore.Call{Kind: statestore.OpPluginIncr, Timeout: callTimeout, Plugin: statestore.PluginState{Key: key, Delta: delta, TTL: ttl}}
	s.consume(c)
	return c
}

// pluginCases cover the Plugin State Store operations
// (docs/architecture/05-wasm-plugin-system.md "Host Function rules", State
// Store): reads of absent and present keys, increments returning the new
// value, the TTL a key takes when an increment creates it and keeps
// afterwards, the post-commit increment, and the error reply of an
// overflowing integer.
func pluginCases() []conformanceCase {
	return []conformanceCase{
		{name: "plugin/absent", run: func(s *suite) {
			if p := s.pluginGet("read", s.pluginKey("none")); p.Found {
				s.t.Fatalf("an absent key read %q", p.Value)
			}
		}},
		{name: "plugin/increment", run: func(s *suite) {
			k := s.pluginKey("count")
			for i, want := range []int64{3, 5, -2} {
				delta := []int64{3, 2, -7}[i]
				c := s.pluginIncr(k, delta, time.Hour*s.unit)
				s.answered("increment", c)
				if c.Plugin.N != want {
					s.t.Fatalf("increment %d = %d, want %d", i, c.Plugin.N, want)
				}
			}
			if p := s.pluginGet("read", k); !p.Found || string(p.Value) != "-2" {
				s.t.Fatalf("read = %v %q, want -2", p.Found, p.Value)
			}
		}},
		{name: "plugin/ttl-on-creation", run: func(s *suite) {
			// A key the increment creates expires its TTL later; a later
			// increment keeps that expiry.
			k := s.pluginKey("window")
			ttl := 2 * time.Second * s.unit
			s.answered("first", s.pluginIncr(k, 1, ttl))
			s.advance(ttl / 2)
			s.answered("second", s.pluginIncr(k, 1, time.Hour*s.unit))
			if p := s.pluginGet("before expiry", k); !p.Found || string(p.Value) != "2" {
				s.t.Fatalf("before expiry: %v %q, want 2", p.Found, p.Value)
			}
			s.advancePast(ttl/2 + time.Millisecond*s.unit)
			if p := s.pluginGet("after expiry", k); p.Found {
				s.t.Fatalf("the key outlived the TTL of the increment that created it: %q", p.Value)
			}
		}},
		{name: "plugin/post-commit", run: func(s *suite) {
			k := s.pluginKey("logged")
			w := &statestore.Write{Kind: statestore.OpPluginIncr, Timeout: callTimeout, Plugin: statestore.PluginState{Key: k, Delta: 4, TTL: time.Hour * s.unit}}
			s.write(w)
			if w.Err != nil || !w.Applied {
				s.t.Fatalf("post-commit increment: applied %v, err %v", w.Applied, w.Err)
			}
			if p := s.pluginGet("read", k); !p.Found || string(p.Value) != "4" {
				s.t.Fatalf("read = %v %q, want 4", p.Found, p.Value)
			}
		}},
		{name: "plugin/overflow", run: func(s *suite) {
			k := s.pluginKey("big")
			s.answered("max", s.pluginIncr(k, 1<<63-1, time.Hour*s.unit))
			if c := s.pluginIncr(k, 1, time.Hour*s.unit); c.Err == nil || c.Err.Code() != "RZ-STS-002" {
				s.t.Fatalf("an overflowing increment = %v, want RZ-STS-002", c.Err)
			}
		}},
	}
}
