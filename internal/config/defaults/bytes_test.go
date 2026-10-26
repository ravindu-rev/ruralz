// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package defaults

import (
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/registry"
)

// byteShapes are resources whose size is in their texts, not their nodes:
// each takes the length l of its long texts and returns the resource and
// the bytes of those texts. Every shape passes stage G.
func byteShapes() map[string]func(l int) (src string, text int) {
	long := strings.Repeat
	return map[string]func(l int) (string, int){
		// Long string values in a plugin config: free content, decoded
		// into any.
		"long strings": func(l int) (string, int) {
			parts := make([]string, 16)
			for i := range parts {
				parts[i] = `"m` + strconv.Itoa(i) + `": "` + long("a", l) + `"`
			}
			return pluginDoc(`{` + strings.Join(parts, ", ") + `}`), 16 * l
		},
		// Long strings in the objects of a list in a plugin config: the
		// list and each object are split by bytes, not only by nodes.
		"long strings in elements": func(l int) (string, int) {
			elem := `{"a": "` + long("a", l) + `", "b": "` + long("b", l) + `", "c": "` + long("c", l) + `"}`
			return pluginDoc(`{"x": [` + elem + `, ` + elem + `]}`), 6 * l
		},
		// Long member names sharing a prefix, in reverse order, so the
		// member sort compares long common prefixes.
		"long member names": func(l int) (string, int) {
			parts := make([]string, 16)
			for i := range parts {
				parts[i] = `"` + long("k", l) + strconv.Itoa(100-i) + `": 1`
			}
			return pluginDoc(`{` + strings.Join(parts, ", ") + `}`), 16 * l
		},
		// Long number literals in a plugin config.
		"long numbers": func(l int) (string, int) {
			parts := make([]string, 16)
			for i := range parts {
				parts[i] = `"n` + strconv.Itoa(i) + `": 0.` + long("0", l) + `1`
			}
			return pluginDoc(`{` + strings.Join(parts, ", ") + `}`), 16 * l
		},
		// Long host names in a set, sharing a prefix: lower-cased, sorted
		// and compared for duplicates.
		"long hosts": func(l int) (string, int) {
			parts := make([]string, 16)
			for i := range parts {
				parts[i] = `"` + long("H", l) + strconv.Itoa(100-i) + `.example"`
			}
			return route(`{"match": {"hosts": [` + strings.Join(parts, ", ") + `]}}`), 16 * l
		},
		// Long keyed-list keys sharing a prefix: sorted by key.
		"long keys": func(l int) (string, int) {
			parts := make([]string, 16)
			for i := range parts {
				parts[i] = `{"name": "` + long("u", l) + strconv.Itoa(100-i) + `", "weight": 1}`
			}
			return route(`{"match": {}, "upstreams": [` + strings.Join(parts, ", ") + `]}`), 16 * l
		},
		// A long duration, which the scalar normalization reads whole.
		"long duration": func(l int) (string, int) {
			return route(`{"match": {}, "timeout": "` + long("1s", l/2) + `"}`), l
		},
	}
}

// TestScaleBytes covers the work units of byte-heavy resources: strings,
// member names, number literals and sort comparisons count by their
// length, so the units of a 64 MiB string are not those of a short one.
// For each shape the units are at least the bytes of its long texts, and
// grow about linearly with their length.
func TestScaleBytes(t *testing.T) {
	s := newStage(t)
	for name, build := range byteShapes() {
		t.Run(name, func(t *testing.T) {
			cost := func(l int) (int, int) {
				src, text := build(l)
				w := s.newWork()
				if h, ds := s.resource(envelope(mustParse(t, src)), nil, w); h == nil {
					t.Fatalf("Resource: %q", texts(ds))
				}
				return w.total, text
			}
			a, ta := cost(1 << 10)
			b, tb := cost(8 << 10)
			if a < ta || b < tb {
				t.Errorf("units %d for %d bytes and %d for %d bytes: want at least one per byte", a, ta, b, tb)
			}
			if ratio := float64(b) / float64(a); ratio < 4 || ratio > 16 {
				t.Errorf("units: %d for 1 KiB texts, %d for 8 KiB: ratio %.2f, want about 8", a, b, ratio)
			}
		})
	}
}

// TestYieldGapBytes covers the time between two reads of the yield timer
// for byte-heavy resources, where a piece of at most 256 nodes can hold
// tens of MiB and the member sort reads long names: with a clock that
// counts its reads, the timer is read at least once per decodeBytes of the
// long texts, for every shape; and no encoding/json call is handed more
// than decodeBytes of text beyond one scalar or member name, so a plugin
// config of long strings is one call per string.
func TestYieldGapBytes(t *testing.T) {
	var reads atomic.Int64
	s := stageWith(t, Options{Now: func() time.Time { reads.Add(1); return time.Unix(0, 0) }, YieldAfter: time.Hour})
	const l = 2 * decodeBytes
	for name, build := range byteShapes() {
		t.Run(name, func(t *testing.T) {
			src, text := build(l)
			res := envelope(mustParse(t, src))
			w := s.newWork()
			reads.Store(0)
			if h, ds := s.resource(res, nil, w); h == nil {
				t.Fatalf("Resource: %q", texts(ds))
			}
			if got, want := reads.Load(), int64(text/decodeBytes); got < want {
				t.Errorf("%d timer reads for %d bytes of text, want at least %d", got, text, want)
			}
			if w.widestBytes > decodeBytes+l+64 {
				t.Errorf("one encoding/json call decoded %d bytes, want at most %d plus one text", w.widestBytes, decodeBytes)
			}
		})
	}
	src, _ := byteShapes()["long strings"](l)
	w := s.newWork()
	if h, ds := s.resource(envelope(mustParse(t, src)), nil, w); h == nil || w.decodes < 16 {
		t.Errorf("Resource = %v, %q, %d encoding/json calls; want one per long string", h != nil, texts(ds), w.decodes)
	}
}

// TestDecodePiecesBytes covers the byte budget of a decode piece where
// the value can be split: with a budget of 64 bytes, every call over a
// typed list and over a plugin config of short strings stays within the
// budget plus one element, and both are split into several calls;
// decoding gives what one call over the tree gives.
func TestDecodePiecesBytes(t *testing.T) {
	idx := hubIndex(t)
	ups := make([]string, 40)
	for i := range ups {
		ups[i] = `{"name": "upstream-` + strconv.Itoa(i) + `", "weight": 1}`
	}
	cfg := make([]string, 40)
	for i := range cfg {
		cfg[i] = `"member-` + strconv.Itoa(i) + `": "value-` + strconv.Itoa(i) + `"`
	}
	for name, src := range map[string]string{
		"typed list":    route(`{"match": {}, "upstreams": [` + strings.Join(ups, ", ") + `]}`),
		"plugin config": pluginDoc(`{` + strings.Join(cfg, ", ") + `}`),
	} {
		t.Run(name, func(t *testing.T) {
			res := envelope(mustParse(t, src))
			w := newWork()
			w.chunkBytes = 64
			reg := registry.New()
			if _, _, ds, failed := decode(idx, reg, res, nil, w, nil); failed {
				t.Fatalf("decode: %q", texts(ds))
			}
			if w.widestBytes > 64+48 || w.decodes < 10 {
				t.Errorf("%d calls, the longest %d bytes; want several of at most the budget and one element", w.decodes, w.widestBytes)
			}
			// The chunker's result equals one call over the tree.
			obj, cfg, failed := wholeDecode(t, idx, reg, res)
			got, gotCfg, _, gotFailed := decode(idx, reg, res, nil, w, nil)
			if failed != gotFailed || !reflect.DeepEqual(obj, got) || !reflect.DeepEqual(cfg, gotCfg) {
				t.Errorf("decode in pieces = %+v %+v, one call %+v %+v", got, gotCfg, obj, cfg)
			}
		})
	}
}
