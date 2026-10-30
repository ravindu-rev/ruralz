// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package loadgen

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Tests for loopback source addresses (Testing "Load generator and L4
// balancer", HTTP): requests leave from distinct 127.0.0.x addresses when
// a scenario needs per-client source.ip.

func TestLoopbackAddrs(t *testing.T) {
	got, err := LoopbackAddrs(3)
	if err != nil {
		t.Fatal(err)
	}
	want := []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("127.0.0.2"), netip.MustParseAddr("127.0.0.3")}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("LoopbackAddrs(3) = %v, want %v", got, want)
	}
	all, err := LoopbackAddrs(MaxLoopbackAddrs)
	if err != nil || len(all) != MaxLoopbackAddrs || all[MaxLoopbackAddrs-1] != netip.MustParseAddr("127.0.0.254") {
		t.Errorf("LoopbackAddrs(%d) ends at %v, err %v; want 127.0.0.254", MaxLoopbackAddrs, all[len(all)-1], err)
	}
	for _, n := range []int{0, -1, MaxLoopbackAddrs + 1} {
		if _, err := LoopbackAddrs(n); err == nil {
			t.Errorf("LoopbackAddrs(%d) succeeded", n)
		}
	}
}

func TestSourceAddrsRejected(t *testing.T) {
	two, _ := LoopbackAddrs(2)
	dial := func(context.Context, string, string) (net.Conn, error) { return nil, net.ErrClosed }
	for _, c := range []struct {
		name string
		cfg  HTTPConfig
	}{
		{"not loopback", HTTPConfig{URL: "http://127.0.0.1/", SourceAddrs: []netip.Addr{netip.MustParseAddr("10.0.0.1")}}},
		{"invalid", HTTPConfig{URL: "http://127.0.0.1/", SourceAddrs: []netip.Addr{{}}}},
		{"repeated", HTTPConfig{URL: "http://127.0.0.1/", SourceAddrs: []netip.Addr{two[0], netip.MustParseAddr("::ffff:127.0.0.1")}}},
		{"more than connections", HTTPConfig{URL: "http://127.0.0.1/", Connections: 1, SourceAddrs: two}},
		{"with Dial", HTTPConfig{URL: "http://127.0.0.1/", SourceAddrs: two, Dial: dial}},
	} {
		if _, err := NewHTTP(c.cfg); err == nil {
			t.Errorf("%s: NewHTTP succeeded", c.name)
		}
	}
	// The connections split evenly over the sources, the first ones one
	// more: 5 over 2 is 3 and 2.
	tg, err := NewHTTP(HTTPConfig{URL: "http://127.0.0.1/", Connections: 5, SourceAddrs: two})
	if err != nil {
		t.Fatal(err)
	}
	if len(tg.groups) != 2 || tg.groups[0][0].tr.MaxConnsPerHost != 3 || tg.groups[1][0].tr.MaxConnsPerHost != 2 {
		t.Errorf("HTTP/1.1 split %d groups, %d and %d connections; want 3 and 2", len(tg.groups),
			tg.groups[0][0].tr.MaxConnsPerHost, tg.groups[1][0].tr.MaxConnsPerHost)
	}
	tg, err = NewHTTP(HTTPConfig{URL: "http://127.0.0.1/", Protocol: H2C, Connections: 5, SourceAddrs: two})
	if err != nil || len(tg.groups) != 2 || len(tg.groups[0]) != 3 || len(tg.groups[1]) != 2 {
		t.Errorf("h2c split: %v; want 3 and 2 connections", err)
	}
	if ipv6, err := NewHTTP(HTTPConfig{URL: "http://[::1]/", SourceAddrs: []netip.Addr{netip.IPv6Loopback()}}); err != nil || len(ipv6.groups) != 1 {
		t.Errorf("::1 as the source: %v", err)
	}
}

// Request seq leaves from source seq % n, and Response.Conn.Local names
// it, on HTTP/1.1 and h2c: the server sees one client address per source.
func TestSourceAddrsSpreadRequests(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("only Linux answers every 127.0.0.0/8 address without aliases")
	}
	var mu sync.Mutex
	seen := map[uint64]string{} // seq -> client IP the server saw
	srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seq, err := strconv.ParseUint(r.Header.Get("X-Seq"), 10, 64)
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		if err == nil {
			mu.Lock()
			seen[seq] = host
			mu.Unlock()
		}
	}))
	sources, err := LoopbackAddrs(3)
	if err != nil {
		t.Fatal(err)
	}
	for _, proto := range []Protocol{HTTP1, H2C} {
		t.Run(proto.String(), func(t *testing.T) {
			mu.Lock()
			clear(seen)
			mu.Unlock()
			tg := newTarget(t, HTTPConfig{
				URL: srv.url, Protocol: proto, Connections: 4, SourceAddrs: sources,
				Prepare: func(r *http.Request, seq uint64) { r.Header.Set("X-Seq", strconv.FormatUint(seq, 10)) },
			})
			for seq := range uint64(9) {
				resp := tg.Do(t.Context(), Request{Seq: seq})
				want := sources[seq%3].String()
				if resp.Outcome != Success {
					t.Fatalf("request %d: %v %v", seq, resp.Outcome, resp.Err)
				}
				if host, _, _ := strings.Cut(resp.Conn.Local, ":"); host != want {
					t.Errorf("request %d left from %s, want %s", seq, resp.Conn.Local, want)
				}
				mu.Lock()
				got := seen[seq]
				mu.Unlock()
				if got != want {
					t.Errorf("server saw request %d from %s, want %s", seq, got, want)
				}
			}
		})
	}
}
