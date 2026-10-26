// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package canonical

import (
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/hub"
)

// largeResource returns a Plugin whose labels hold n members and whose
// configSchema holds an object of n members and a list of n objects, the
// large-object case: a stage that scans an object once per member
// becomes quadratic on it.
func largeResource(n int) string {
	var b strings.Builder
	b.WriteString(`{"apiVersion": "ruralz/v1alpha1", "kind": "Plugin", "metadata": {"name": "big", "labels": {`)
	for i := range n {
		if i > 0 {
			b.WriteString(", ")
		}
		// Descending names, so the encoder sorts every object.
		b.WriteString(`"l` + strconv.Itoa(n-i) + `": "v"`)
	}
	b.WriteString(`}}, "spec": {"configSchema": {"properties": {`)
	for i := range n {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(`"p` + strconv.Itoa(n-i) + `": {"type": "string", "maxLength": ` + strconv.Itoa(i) + `}`)
	}
	b.WriteString(`}, "examples": [`)
	for i := range n {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(`{"name": "e` + strconv.Itoa(i) + `", "weight": 1.50}`)
	}
	b.WriteString(`]}}}`)
	return b.String()
}

// roundTrip encodes the large resource, decodes and checks it and
// encodes it again, on one goroutine, and returns the bytes allocated.
func roundTrip(t *testing.T, b *hub.Bundle) uint64 {
	t.Helper()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	content := mustEncodeBundle(t, b)
	sameBytes(t, "large resource", reencode(t, mustDecode(t, content)), content)
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// TestScaleLinear: encoding, decoding and checking one resource whose
// objects and list hold tens of thousands of members stay linear in its
// size. Every object is visited once and its members are found through
// the schema's property maps, never by a scan per member; what the test
// counts is the bytes allocated, not the time, so it does not depend on
// the machine: four times the members allocate about four times the
// bytes, where a stage that copied or indexed an object once per member
// would allocate sixteen times.
func TestScaleLinear(t *testing.T) {
	if testing.Short() {
		t.Skip("scale test")
	}
	const small = 10_000
	hubIndex(t) // loaded once, outside the counts
	bs := bundleOf(t, largeResource(small))
	bl := bundleOf(t, largeResource(4*small))
	as, al := roundTrip(t, bs), roundTrip(t, bl)
	ratio := float64(al) / float64(as)
	t.Logf("4x members allocated %.2fx the bytes (%d vs %d)", ratio, al, as)
	if ratio < 3 || ratio > 5.5 {
		t.Errorf("4x members allocated %.2fx the bytes (%d vs %d); want about 4x", ratio, al, as)
	}
}

// TestScaleNewerFindings: content with tens of thousands of unknown
// members in one keyed list entry reports each once with its key-aware
// path; the entry's key is read once, not once per finding.
func TestScaleNewerFindings(t *testing.T) {
	var b strings.Builder
	// The key member comes last, so finding it scans the whole entry.
	b.WriteString(`{"kind":"Upstream","metadata":{"name":"u"},"spec":{"endpoints":[{`)
	const n = 20_000
	for i := range n {
		b.WriteString(`"x` + strconv.Itoa(100000+i) + `":1,`)
	}
	b.WriteString(`"address":"a:1"}]}}`)
	err := decodeErr(t, doc(b.String()))
	ds := DiagnosticsOf(err)
	if codeOf(err) != CodeNewer || len(ds) != n || ds[0].Path.String() != "spec.endpoints[address=a:1].x100000" {
		t.Fatalf("%d diagnostics, first %v", len(ds), ds[0])
	}
}
