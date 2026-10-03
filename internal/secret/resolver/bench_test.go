// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package resolver

import (
	"context"
	"strconv"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/secret"
)

// Benchmarks: Store.Get is on the request path (lock-free, no
// allocation; architecture 2.8), and a poll cycle over unchanged files is
// the steady-state cost of the 2 s poller (spec 01 requirement 46: stat
// only, no re-read).

func BenchmarkStoreGet(b *testing.B) {
	h := newHarness(b, nil)
	var uses []secret.Use
	for i := range 64 {
		name := "RURALZ_SECRET_" + strconv.Itoa(i)
		h.setEnv(name, "value-"+strconv.Itoa(i))
		uses = append(uses, use(envRef(name), secret.KindOpaque))
	}
	st, diags := h.r.Resolve(context.Background(), uses)
	if st == nil {
		b.Fatal(diagText(diags))
	}
	ref := envRef("RURALZ_SECRET_42")
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if v, ok := st.Get(ref); !ok || v.Len() == 0 {
				b.Fail()
			}
		}
	})
}

func BenchmarkPollUnchanged(b *testing.B) {
	for _, n := range []int{10, 100} {
		b.Run(strconv.Itoa(n)+"-files", func(b *testing.B) {
			h := newHarness(b, nil)
			var uses []secret.Use
			for i := range n {
				rel := "s" + strconv.Itoa(i)
				h.write(rel, "value-"+strconv.Itoa(i))
				uses = append(uses, use(fileRef(h.path(rel), ""), secret.KindOpaque))
			}
			st, diags := h.r.Resolve(context.Background(), uses)
			if st == nil {
				b.Fatal(diagText(diags))
			}
			h.r.Activate(st)
			ctx := context.Background()
			b.ReportAllocs()
			for b.Loop() {
				h.r.cycle(ctx)
			}
			if h.fileN.value() != 0 {
				b.Fatalf("failures during the benchmark: %d", h.fileN.value())
			}
		})
	}
}

func BenchmarkResolveFiles(b *testing.B) {
	h := newHarness(b, nil)
	p := newPEM(b)
	var uses []secret.Use
	for i := range 20 {
		rel := "tls" + strconv.Itoa(i) + ".crt"
		h.write(rel, p.cert)
		uses = append(uses, use(fileRef(h.path(rel), ""), secret.KindPEMCertificate))
	}
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		if st, diags := h.r.Resolve(ctx, uses); st == nil {
			b.Fatal(diagText(diags))
		}
	}
}
