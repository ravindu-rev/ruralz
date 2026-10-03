// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
)

// DNS wire constants (RFC 1035, RFC 2782, RFC 3596).
const (
	typeA    = 1
	typeAAAA = 28
	typeSRV  = 33
	classIN  = 1

	rcodeOK       = 0
	rcodeServFail = 2
	rcodeNXDomain = 3
)

// zoneRecord is what the test responder answers for one name.
type zoneRecord struct {
	a, aaaa []netip.Addr
	srv     []net.SRV
	rcode   int
}

// dnsResponder is a minimal stdlib UDP DNS server answering A, AAAA and SRV
// questions from a table; unknown names are NXDOMAIN (05 test 36).
type dnsResponder struct {
	conn    net.PacketConn
	mu      sync.Mutex
	zone    map[string]zoneRecord // lowercase FQDN
	queries int
	done    chan struct{}
}

func startResponder(t *testing.T) *dnsResponder {
	t.Helper()
	var lc net.ListenConfig
	pc, err := lc.ListenPacket(t.Context(), "udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := &dnsResponder{conn: pc, zone: map[string]zoneRecord{}, done: make(chan struct{})}
	go r.serve()
	t.Cleanup(func() {
		_ = pc.Close()
		<-r.done
	})
	return r
}

func (r *dnsResponder) set(name string, z zoneRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.zone[strings.ToLower(name)] = z
}

func (r *dnsResponder) remove(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.zone, strings.ToLower(name))
}

func (r *dnsResponder) serve() {
	defer close(r.done)
	buf := make([]byte, 1500)
	for {
		n, from, err := r.conn.ReadFrom(buf)
		if err != nil {
			return
		}
		if resp := r.answer(buf[:n]); resp != nil {
			_, _ = r.conn.WriteTo(resp, from)
		}
	}
}

// answer builds the response to one query, or nil for a malformed one.
func (r *dnsResponder) answer(q []byte) []byte {
	if len(q) < 12 || binary.BigEndian.Uint16(q[4:6]) != 1 {
		return nil
	}
	name, end, ok := readName(q, 12)
	if !ok || end+4 > len(q) {
		return nil
	}
	qtype := binary.BigEndian.Uint16(q[end : end+2])
	question := q[12 : end+4]

	r.mu.Lock()
	r.queries++
	z, found := r.zone[strings.ToLower(name)]
	r.mu.Unlock()

	rcode := rcodeOK
	var answers [][]byte
	switch {
	case !found:
		rcode = rcodeNXDomain
	case z.rcode != 0:
		rcode = z.rcode
	default:
		switch qtype {
		case typeA:
			for _, a := range z.a {
				b := a.As4()
				answers = append(answers, rr(typeA, b[:]))
			}
		case typeAAAA:
			for _, a := range z.aaaa {
				b := a.As16()
				answers = append(answers, rr(typeAAAA, b[:]))
			}
		case typeSRV:
			for _, s := range z.srv {
				data := make([]byte, 6, 6+len(s.Target)+2)
				binary.BigEndian.PutUint16(data[0:], s.Priority)
				binary.BigEndian.PutUint16(data[2:], s.Weight)
				binary.BigEndian.PutUint16(data[4:], s.Port)
				data = appendName(data, s.Target)
				answers = append(answers, rr(typeSRV, data))
			}
		}
	}
	resp := make([]byte, 12, 512)
	copy(resp[0:2], q[0:2])
	flags := uint16(0x8000|0x0400|0x0080) | binary.BigEndian.Uint16(q[2:4])&0x0100 | uint16(rcode)
	binary.BigEndian.PutUint16(resp[2:], flags)
	binary.BigEndian.PutUint16(resp[4:], 1)
	binary.BigEndian.PutUint16(resp[6:], uint16(len(answers))) //nolint:gosec // G115: a handful of records.
	resp = append(resp, question...)
	for _, a := range answers {
		resp = append(resp, a...)
	}
	return resp
}

// rr encodes one IN answer whose owner is the question name (pointer 12).
func rr(typ uint16, data []byte) []byte {
	b := make([]byte, 12, 12+len(data))
	binary.BigEndian.PutUint16(b[0:], 0xc00c)
	binary.BigEndian.PutUint16(b[2:], typ)
	binary.BigEndian.PutUint16(b[4:], classIN)
	binary.BigEndian.PutUint32(b[6:], 30)
	binary.BigEndian.PutUint16(b[10:], uint16(len(data))) //nolint:gosec // G115: small records.
	return append(b, data...)
}

// readName decodes an uncompressed query name starting at off.
func readName(b []byte, off int) (string, int, bool) {
	var labels []string
	for {
		if off >= len(b) {
			return "", 0, false
		}
		l := int(b[off])
		off++
		if l == 0 {
			return strings.Join(labels, ".") + ".", off, true
		}
		if l > 63 || off+l > len(b) {
			return "", 0, false
		}
		labels = append(labels, string(b[off:off+l]))
		off += l
	}
}

func appendName(b []byte, name string) []byte {
	for label := range strings.SplitSeq(strings.TrimSuffix(name, "."), ".") {
		b = append(b, byte(len(label))) //nolint:gosec // G115: test labels are at most 63 bytes.
		b = append(b, label...)
	}
	return append(b, 0)
}

// resolverFor returns the pure-Go resolver wired to the responder.
func resolverFor(r *dnsResponder) *net.Resolver {
	addr := r.conn.LocalAddr().String()
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "udp", addr)
		},
	}
}

// TestRealDNS exercises 05 reqs 6 and 7 end to end through the pure-Go
// net.Resolver against a stdlib UDP responder (05 test 36): A and AAAA
// answers, SRV with priorities, weights and target resolution, NXDOMAIN
// counted until 3 repeats, SERVFAIL keeping the last set, an SRV record
// with a malformed target dropped while the others stay.
func TestRealDNS(t *testing.T) {
	if testing.Short() {
		t.Skip("uses loopback UDP")
	}
	resp := startResponder(t)
	res := resolverFor(resp)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	// A and AAAA.
	resp.set("orders.test.", zoneRecord{
		a:    []netip.Addr{netip.MustParseAddr("192.0.2.10"), netip.MustParseAddr("192.0.2.11")},
		aaaa: []netip.Addr{netip.MustParseAddr("2001:db8::10")},
	})
	src, err := New(Spec{DNS: &DNSSpec{Service: "orders.test.", Port: 8080}}, Options{
		Upstream: "orders", Clock: clocktest.New(epoch()), Resolver: res, Rand: newRand(3),
	})
	if err != nil {
		t.Fatal(err)
	}
	set, _ := src.Refresh(ctx)
	wantSet(t, set,
		"192.0.2.10:8080=192.0.2.10:8080/1 host=orders.test",
		"192.0.2.11:8080=192.0.2.11:8080/1 host=orders.test",
		"[2001:db8::10]:8080=[2001:db8::10]:8080/1 host=orders.test")

	// SERVFAIL keeps the set and is not NXDOMAIN.
	resp.set("orders.test.", zoneRecord{rcode: rcodeServFail})
	for range NotFoundLimit + 1 {
		set, _ = src.Refresh(ctx)
	}
	if set.Len() != 3 || !set.Status.Stale || notFound(set.Status.Err) {
		t.Fatalf("SERVFAIL: %q %+v", describe(set), set.Status)
	}
	// NXDOMAIN empties it on the third consecutive refresh.
	resp.remove("orders.test.")
	for n := 1; n <= NotFoundLimit; n++ {
		set, _ = src.Refresh(ctx)
		if n < NotFoundLimit && (set.Len() != 3 || !notFound(set.Status.Err)) {
			t.Fatalf("NXDOMAIN %d: %q %+v", n, describe(set), set.Status)
		}
	}
	if set.Len() != 0 || set.Status.Stale {
		t.Fatalf("after %d NXDOMAIN: %q %+v", NotFoundLimit, describe(set), set.Status)
	}

	// SRV: the lowest-priority group, SRV weights, targets resolved.
	resp.set("_http._tcp.shop.test.", zoneRecord{srv: []net.SRV{
		{Target: "a.shop.test.", Port: 8081, Priority: 10, Weight: 20},
		{Target: "b.shop.test.", Port: 8082, Priority: 10, Weight: 60},
		{Target: "c.shop.test.", Port: 8083, Priority: 30, Weight: 90},
	}})
	resp.set("a.shop.test.", zoneRecord{a: []netip.Addr{netip.MustParseAddr("192.0.2.21")}})
	resp.set("b.shop.test.", zoneRecord{aaaa: []netip.Addr{netip.MustParseAddr("2001:db8::22")}})
	srvSrc, err := New(Spec{DNS: &DNSSpec{Service: "shop.test.", PortName: "http"}}, Options{
		Upstream: "shop", Clock: clocktest.New(epoch()), Resolver: res, Rand: newRand(4),
	})
	if err != nil {
		t.Fatal(err)
	}
	set, _ = srvSrc.Refresh(ctx)
	wantSet(t, set,
		"a.shop.test:8081=192.0.2.21:8081/20 host=a.shop.test",
		"b.shop.test:8082=[2001:db8::22]:8082/60 host=b.shop.test")
	if set.Status.Stale {
		t.Fatalf("status %+v", set.Status)
	}
	// A record with a malformed target: the pure-Go resolver drops it and
	// returns the others with a *net.DNSError, a good answer here.
	resp.set("_http._tcp.shop.test.", zoneRecord{srv: []net.SRV{
		{Target: "a.shop.test.", Port: 8081, Priority: 10, Weight: 20},
		{Target: "bad!name.shop.test.", Port: 8084, Priority: 10, Weight: 60},
	}})
	set, _ = srvSrc.Refresh(ctx)
	wantSet(t, set, "a.shop.test:8081=192.0.2.21:8081/20 host=a.shop.test")
	if set.Status.Stale || set.Status.Err != nil {
		t.Fatalf("malformed record: status %+v", set.Status)
	}
	var de *net.DNSError
	resp.remove("_http._tcp.shop.test.")
	set, _ = srvSrc.Refresh(ctx)
	if !errors.As(set.Status.Err, &de) || !de.IsNotFound || set.Len() != 1 {
		t.Fatalf("SRV NXDOMAIN: %v %q", set.Status.Err, describe(set))
	}
	resp.mu.Lock()
	queries := resp.queries
	resp.mu.Unlock()
	if queries == 0 {
		t.Fatal("responder saw no query")
	}
}
