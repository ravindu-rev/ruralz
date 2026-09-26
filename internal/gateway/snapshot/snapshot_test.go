// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package snapshot

import (
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Tests for architecture section 2.14 (WP-01) and spec 04 group H
// (requirements 49 to 53, the types): striped pins with per-stripe
// intrusive lists (req 50, 53), Listener.Key and ErrNotReplayable (R-41).

func pinned(p *Pins) []*PinnedRequest {
	var out []*PinnedRequest
	p.Each(func(r *PinnedRequest) { out = append(out, r) })
	return out
}

func TestPinsRemoveHeadMiddleTail(t *testing.T) {
	// Each stripe is a doubly linked list with the newest request first.
	tests := []struct {
		name   string
		remove int // index in insertion order
		want   []int
	}{
		{"head", 2, []int{1, 0}},
		{"middle", 1, []int{2, 0}},
		{"tail", 0, []int{2, 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewPins(1)
			rs := []*PinnedRequest{{}, {}, {}}
			for _, r := range rs {
				p.Add(0, r)
			}
			p.Remove(0, rs[tt.remove])
			var want []*PinnedRequest
			for _, i := range tt.want {
				want = append(want, rs[i])
			}
			if got := pinned(p); !slices.Equal(got, want) || p.Count() != 2 {
				t.Fatalf("after removing the %s: %d pinned, count %d", tt.name, len(got), p.Count())
			}
			// The removed record is unlinked, ready for its pool.
			if r := rs[tt.remove]; r.prev != nil || r.next != nil {
				t.Fatal("the removed record is still linked")
			}
			// Removing the rest empties the stripe.
			for _, i := range tt.want {
				p.Remove(0, rs[i])
			}
			if p.Count() != 0 || len(pinned(p)) != 0 {
				t.Fatalf("count %d after removing everything", p.Count())
			}
		})
	}
}

func TestPinsReaddAfterRemove(t *testing.T) {
	p := NewPins(2)
	r := &PinnedRequest{}
	for range 3 {
		p.Add(1, r)
		p.Remove(1, r)
	}
	a, b := &PinnedRequest{}, &PinnedRequest{}
	p.Add(1, a)
	p.Add(1, r)
	p.Add(1, b)
	if got := pinned(p); !slices.Equal(got, []*PinnedRequest{b, r, a}) {
		t.Fatalf("a reused record is not linked correctly: %v", got)
	}
}

func TestPinsStripes(t *testing.T) {
	if n := len(NewPins(0).stripes); n != 1 {
		t.Fatalf("NewPins(0) has %d stripes, want 1", n)
	}
	if n := len(NewPins(-3).stripes); n != 1 {
		t.Fatalf("NewPins(-3) has %d stripes, want 1", n)
	}
	p := NewPins(8)
	rs := make([]*PinnedRequest, 20)
	for i := range rs {
		rs[i] = &PinnedRequest{}
		p.Add(i, rs[i]) // stripe index wraps modulo 8
	}
	if p.Count() != 20 {
		t.Fatalf("Count = %d", p.Count())
	}
	if n := p.stripes[1].n.Load(); n != 3 { // 1, 9, 17
		t.Fatalf("stripe 1 holds %d, want 3", n)
	}
	seen := map[*PinnedRequest]bool{}
	for _, r := range pinned(p) {
		seen[r] = true
	}
	if len(seen) != 20 {
		t.Fatalf("Each visited %d distinct records", len(seen))
	}
	for i, r := range rs {
		p.Remove(i+8, r) // same stripe, different index
	}
	if p.Count() != 0 {
		t.Fatalf("Count = %d after removing everything", p.Count())
	}
}

func TestPinsConcurrent(t *testing.T) {
	// 04 req 50: 8 stripes, concurrent requests pin and unpin while the
	// retirer counts and walks.
	p := NewPins(8)
	const workers, rounds = 16, 500
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rs := make([]PinnedRequest, 4)
			for i := range rounds {
				r := &rs[i%len(rs)]
				s := w + i
				p.Add(s, r)
				if p.Count() <= 0 {
					t.Error("Count is not positive while a request is pinned")
				}
				p.Remove(s, r)
			}
		}()
	}
	stop := make(chan struct{})
	walker := make(chan struct{})
	go func() {
		defer close(walker)
		for {
			select {
			case <-stop:
				return
			default:
				p.Each(func(r *PinnedRequest) {
					r.Mu.Lock()
					_ = r.Ended
					r.Mu.Unlock()
				})
			}
		}
	}()
	wg.Wait()
	close(stop)
	<-walker
	if p.Count() != 0 || len(pinned(p)) != 0 {
		t.Fatalf("Count = %d after every request unpinned", p.Count())
	}
}

func TestPinsAllocateNothing(t *testing.T) {
	// 04 req 50: pinning allocates nothing.
	p := NewPins(8)
	r := &PinnedRequest{}
	if n := testing.AllocsPerRun(1000, func() {
		p.Add(3, r)
		p.Remove(3, r)
	}); n != 0 {
		t.Fatalf("Add and Remove allocate %v times", n)
	}
}

func TestPinStripeIsCacheLinePadded(t *testing.T) {
	if size := reflect.TypeFor[pinStripe]().Size(); size != 64 {
		t.Fatalf("pinStripe is %d bytes, want one 64-byte cache line", size)
	}
}

func TestListenerKey(t *testing.T) {
	base := &Listener{Name: "public", Protocol: v1alpha1.ListenerProtocolHTTPS, Port: 8443}
	changed := *base
	changed.Hostnames = []string{"api.example"}
	changed.TLS = &ListenerTLS{MinVersion: 0x0303, Certificates: []Certificate{{Name: "api", Certificate: secret.Ref{Name: "c"}}}}
	if base.Key() != changed.Key() {
		t.Fatal("TLS or hostname changes altered the listener identity")
	}
	for name, mutate := range map[string]func(*Listener){
		"name":           func(l *Listener) { l.Name = "internal" },
		"port":           func(l *Listener) { l.Port = 9443 },
		"protocol":       func(l *Listener) { l.Protocol = v1alpha1.ListenerProtocolHTTP },
		"proxy protocol": func(l *Listener) { l.ProxyProtocol = true },
	} {
		l := *base
		mutate(&l)
		if l.Key() == base.Key() {
			t.Errorf("a %s change kept the listener identity", name)
		}
	}
	want := ListenerKey{Name: "public", Port: 8443, Protocol: v1alpha1.ListenerProtocolHTTPS}
	if base.Key() != want {
		t.Fatalf("Key = %+v", base.Key())
	}
}

func TestErrNotReplayable(t *testing.T) {
	for _, other := range []error{filter.ErrBudget, filter.ErrTooLarge, filter.ErrNotAvailable, filter.ErrSecondBinding, io.EOF} {
		if errors.Is(ErrNotReplayable, other) || errors.Is(other, ErrNotReplayable) {
			t.Errorf("ErrNotReplayable matches %v", other)
		}
	}
	if !errors.Is(fmt.Errorf("attempt 2: %w", ErrNotReplayable), ErrNotReplayable) {
		t.Fatal("ErrNotReplayable is lost through wrapping")
	}
}

func TestEndReasons(t *testing.T) {
	// EndNone is the zero value, so a fresh record is not ended.
	var r PinnedRequest
	if r.Ended != EndNone || EndGrace == EndDrain || EndGrace == EndNone {
		t.Fatal("end reasons are not distinct or EndNone is not zero")
	}
}

// BenchmarkPinsAddRemove measures one pin and unpin per request across
// parallel connections, each on its own stripe (04 req 50).
func BenchmarkPinsAddRemove(b *testing.B) {
	p := NewPins(8)
	var next sync.Mutex
	stripe := 0
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		next.Lock()
		s := stripe
		stripe++
		next.Unlock()
		r := &PinnedRequest{}
		for pb.Next() {
			p.Add(s, r)
			p.Remove(s, r)
		}
	})
}
