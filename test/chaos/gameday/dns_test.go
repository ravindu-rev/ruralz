// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package gameday

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/discovery"
)

// startTestDNS starts a DNS server on a free loopback port for t.
func startTestDNS(t *testing.T) *DNSServer {
	t.Helper()
	s, err := StartDNSServer(t.Context(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// dnsResolver returns a pure-Go resolver asking only s, over network
// ("udp" lets the resolver fall back to TCP on a truncated answer, "tcp"
// asks over TCP alone).
func dnsResolver(s *DNSServer, network string) *net.Resolver {
	return &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, n, _ string) (net.Conn, error) {
		var d net.Dialer
		if network == "tcp" {
			n = "tcp"
		}
		return d.DialContext(ctx, n, s.Addr())
	}}
}

// lookupCtx bounds one lookup.
func lookupCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// addrs returns the addresses of a lookup answer as strings, sorted.
func addrs(as []net.IPAddr) []string {
	out := make([]string, len(as))
	for i, a := range as {
		out[i] = a.IP.String()
	}
	slices.Sort(out)
	return out
}

// TestDNSServerAnswers checks A, AAAA and SRV answers over UDP and over
// TCP, the SRV targets resolving through the server's own host records,
// and the per-name query counts.
func TestDNSServerAnswers(t *testing.T) {
	s := startTestDNS(t)
	if err := s.SetHost("host.example.test", netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("::1")); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"b1.example.test", "b2.example.test"} {
		if err := s.SetHost(target, netip.MustParseAddr("127.0.0.1")); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetSRV("_http._tcp.svc.example.test",
		DNSSRV{Priority: 10, Weight: 1, Port: 8081, Target: "b1.example.test"},
		DNSSRV{Priority: 10, Weight: 2, Port: 8082, Target: "b2.example.test"},
	); err != nil {
		t.Fatal(err)
	}
	for _, network := range []string{"udp", "tcp"} {
		t.Run(network, func(t *testing.T) {
			res := dnsResolver(s, network)
			got, err := res.LookupIPAddr(lookupCtx(t), "host.example.test.")
			if err != nil || !slices.Equal(addrs(got), []string{"127.0.0.1", "::1"}) {
				t.Fatalf("LookupIPAddr(host) = %v, %v; want 127.0.0.1 and ::1", got, err)
			}
			_, recs, err := res.LookupSRV(lookupCtx(t), "http", "tcp", "svc.example.test.")
			if err != nil || len(recs) != 2 {
				t.Fatalf("LookupSRV = %v, %v; want two records", recs, err)
			}
			var srv []string
			for _, r := range recs {
				srv = append(srv, r.Target+":"+strconv.Itoa(int(r.Port)))
				if _, err := res.LookupIPAddr(lookupCtx(t), r.Target); err != nil {
					t.Errorf("LookupIPAddr(%s) = %v", r.Target, err)
				}
			}
			slices.Sort(srv)
			if want := []string{"b1.example.test.:8081", "b2.example.test.:8082"}; !slices.Equal(srv, want) {
				t.Errorf("SRV targets = %v, want %v", srv, want)
			}
		})
	}
	if n := s.Queries("host.example.test"); n < 4 {
		t.Errorf("Queries(host) = %d, want an A and an AAAA question per transport", n)
	}
	if n := s.Queries("_http._tcp.svc.example.test."); n < 2 {
		t.Errorf("Queries(SRV name) = %d, want one per transport", n)
	}
	if n := s.OtherQueries(); n != 0 {
		t.Errorf("OtherQueries = %d, want 0", n)
	}
}

// TestDNSServerModes checks the answers the FC-19 drill switches between:
// SERVFAIL is a temporary failure, NXDOMAIN and an empty answer both read
// as not found (the empty answer authoritative, never a lame referral),
// per-name modes override the default, and an unknown name is NXDOMAIN.
func TestDNSServerModes(t *testing.T) {
	s := startTestDNS(t)
	for _, n := range []string{"a.example.test", "b.example.test"} {
		if err := s.SetHost(n, netip.MustParseAddr("127.0.0.1")); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetSRV("_http._tcp.srv.example.test", DNSSRV{Priority: 1, Weight: 1, Port: 80, Target: "a.example.test"}); err != nil {
		t.Fatal(err)
	}
	type outcome int
	const (
		answered outcome = iota
		temporary
		notFound
	)
	for _, tc := range []struct {
		name  string
		def   DNSMode
		names map[string]DNSMode
		srv   bool // look up the SRV name instead of a.example.test
		host  string
		want  outcome
	}{
		{"answer", DNSAnswer, nil, false, "a.example.test.", answered},
		{"unknown name", DNSAnswer, nil, false, "missing.example.test.", notFound},
		{"servfail", DNSFail, nil, false, "a.example.test.", temporary},
		{"servfail SRV", DNSFail, nil, true, "", temporary},
		{"nxdomain", DNSNXDomain, nil, false, "a.example.test.", notFound},
		{"empty", DNSEmpty, nil, false, "a.example.test.", notFound},
		{"empty SRV", DNSEmpty, nil, true, "", notFound},
		{"one name nxdomain", DNSAnswer, map[string]DNSMode{"a.example.test": DNSNXDomain}, false, "a.example.test.", notFound},
		{"another name answers", DNSAnswer, map[string]DNSMode{"a.example.test": DNSNXDomain}, false, "b.example.test.", answered},
		{"one SRV name empty", DNSAnswer, map[string]DNSMode{"_http._tcp.srv.example.test": DNSEmpty}, true, "", notFound},
		{"one name answers through a failing default", DNSFail, map[string]DNSMode{"b.example.test": DNSAnswer}, false, "b.example.test.", answered},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.SetModes(tc.def, tc.names); err != nil {
				t.Fatal(err)
			}
			res := dnsResolver(s, "udp")
			var err error
			if tc.srv {
				_, _, err = res.LookupSRV(lookupCtx(t), "http", "tcp", "srv.example.test.")
			} else {
				_, err = res.LookupIPAddr(lookupCtx(t), tc.host)
			}
			var de *net.DNSError
			got := answered
			switch {
			case err == nil:
			case errors.As(err, &de) && de.IsNotFound:
				got = notFound
			case errors.As(err, &de) && de.IsTemporary:
				got = temporary
			default:
				t.Fatalf("lookup error %v is neither not found nor temporary", err)
			}
			if got != tc.want {
				t.Errorf("lookup = %v (outcome %d), want outcome %d", err, got, tc.want)
			}
		})
	}
	if err := s.SetModes(DNSMode(0), nil); err == nil {
		t.Error("SetModes with an invalid default passed")
	}
	if err := s.SetModes(DNSAnswer, map[string]DNSMode{"a..test": DNSFail}); err == nil {
		t.Error("SetModes with a malformed name passed")
	}
}

// dnsQuery builds a query for name and type: the header with id, the
// flags, the question count, then the question.
func dnsQuery(t *testing.T, id uint16, flags byte, qdcount uint16, name string, qtype, class uint16) []byte {
	t.Helper()
	b := binary.BigEndian.AppendUint16(nil, id)
	b = append(b, flags, 0)
	b = binary.BigEndian.AppendUint16(b, qdcount)
	b = append(b, 0, 0, 0, 0, 0, 0)
	c, err := dnsCanonical(name)
	if err != nil {
		t.Fatal(err)
	}
	b = append(b, dnsWireName(c)...)
	b = binary.BigEndian.AppendUint16(b, qtype)
	return binary.BigEndian.AppendUint16(b, class)
}

// TestDNSHandle checks the answers to messages a resolver should not send
// and to a large answer: no answer to a response or a message shorter
// than a header, FORMERR for two questions or a compressed question name,
// NOTIMP for another opcode or class, and a truncated (TC) UDP answer
// that a resolver asks again over TCP, getting every record.
func TestDNSHandle(t *testing.T) {
	s := startTestDNS(t)
	if err := s.SetHost("a.example.test", netip.MustParseAddr("127.0.0.1")); err != nil {
		t.Fatal(err)
	}
	compressed := dnsQuery(t, 7, 0x01, 1, "a.example.test", dnsTypeA, dnsClassIN)[:dnsHeaderLen]
	compressed = append(compressed, 0xC0, 0x0C, 0, 1, 0, 1)
	for _, tc := range []struct {
		name   string
		msg    []byte
		silent bool // no answer at all
		rcode  byte
	}{
		{"short", []byte{0, 1, 2}, true, 0},
		{"response", dnsQuery(t, 7, 0x80, 1, "a.example.test", dnsTypeA, dnsClassIN), true, 0},
		{"two questions", dnsQuery(t, 7, 0x01, 2, "a.example.test", dnsTypeA, dnsClassIN), false, dnsRcodeFormErr},
		{"compressed question name", compressed, false, dnsRcodeFormErr},
		{"status opcode", dnsQuery(t, 7, 0x10, 1, "a.example.test", dnsTypeA, dnsClassIN), false, dnsRcodeNotImp},
		{"chaos class", dnsQuery(t, 7, 0x01, 1, "a.example.test", dnsTypeA, 3), false, dnsRcodeNotImp},
		{"standard", dnsQuery(t, 7, 0x01, 1, "a.example.test", dnsTypeA, dnsClassIN), false, dnsRcodeOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := s.handle(tc.msg, true)
			switch {
			case tc.silent && resp != nil:
				t.Fatalf("handle = % x, want no answer", resp)
			case tc.silent:
				return
			case len(resp) < dnsHeaderLen:
				t.Fatalf("handle = % x, want a response", resp)
			}
			if id := binary.BigEndian.Uint16(resp[0:2]); id != 7 || resp[2]&0x80 == 0 || resp[3]&0x0F != tc.rcode {
				t.Errorf("response id %d, flags %#x %#x; want id 7, QR and rcode %d", id, resp[2], resp[3], tc.rcode)
			}
		})
	}
	var many []netip.Addr
	for i := range 40 {
		many = append(many, netip.MustParseAddr("2001:db8::"+strconv.FormatInt(int64(i+1), 16)))
	}
	if err := s.SetHost("many.example.test", many...); err != nil {
		t.Fatal(err)
	}
	resp := s.handle(dnsQuery(t, 9, 0x01, 1, "many.example.test", dnsTypeAAAA, dnsClassIN), true)
	if resp[2]&0x02 == 0 || binary.BigEndian.Uint16(resp[6:8]) != 0 {
		t.Errorf("a 40-record UDP answer: flags %#x, %d answers; want TC and none", resp[2], binary.BigEndian.Uint16(resp[6:8]))
	}
	got, err := dnsResolver(s, "udp").LookupIPAddr(lookupCtx(t), "many.example.test.")
	if err != nil || len(got) != len(many) {
		t.Errorf("LookupIPAddr(many) = %d addresses, %v; want %d over the TCP retry", len(got), err, len(many))
	}
}

// TestDNSNames checks the names the server holds: lower-cased with a
// trailing dot, at most 63 bytes a label and 255 in wire form, letters,
// digits, '-' and '_' only, and at most DNSMaxNames of them.
func TestDNSNames(t *testing.T) {
	for in, want := range map[string]string{
		"A.Example.TEST":              "a.example.test.",
		"_http._tcp.srv.example.test": "_http._tcp.srv.example.test.",
		"x.":                          "x.",
	} {
		if got, err := dnsCanonical(in); err != nil || got != want {
			t.Errorf("dnsCanonical(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	long := string(slices.Repeat([]byte("a"), 64))
	for _, bad := range []string{"", ".", "a..b", long + ".test", "a b.test", "a/b.test"} {
		if _, err := dnsCanonical(bad); err == nil {
			t.Errorf("dnsCanonical(%q) passed", bad)
		}
	}
	s := startTestDNS(t)
	for i := range DNSMaxNames {
		if err := s.SetHost("n"+strconv.Itoa(i)+".example.test", netip.MustParseAddr("127.0.0.1")); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetHost("one-more.example.test", netip.MustParseAddr("127.0.0.1")); err == nil {
		t.Errorf("a name past DNSMaxNames was taken")
	}
	if err := s.SetHost("n0.example.test", netip.MustParseAddr("127.0.0.2")); err != nil {
		t.Errorf("replacing a held name past the bound = %v", err)
	}
	if err := s.SetHost("n0.example.test"); err != nil {
		t.Errorf("removing a name = %v", err)
	}
}

// TestDNSServerClose checks that Close stops both transports, twice
// without error, and that the end of the Start context closes the
// server too.
func TestDNSServerClose(t *testing.T) {
	s := startTestDNS(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("a second Close = %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	s2, err := StartDNSServer(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := (&net.Dialer{Timeout: time.Second}).DialContext(t.Context(), "tcp", s2.Addr())
			if err != nil {
				return
			}
			_ = c.Close()
			select {
			case <-t.Context().Done():
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the server still accepts TCP connections after its context ended")
	}
	_ = s2.Close()
}

var dnsT0 = time.Unix(1_000_000, 0)

func dnsAt(ms int) time.Time { return dnsT0.Add(time.Duration(ms) * time.Millisecond) }

// TestDNSObservations checks the readings of the FC-19 drill: a set kept
// through failed refreshes, the refreshes NXDOMAIN keeps a set for before
// it empties, and the set coming back.
func TestDNSObservations(t *testing.T) {
	set := []string{"127.0.0.1:9001"}
	obs := []DNSObservation{
		{At: dnsAt(0), Endpoints: set},
		{At: dnsAt(100), Failures: 1, Stale: true, Endpoints: set},
		{At: dnsAt(200), Failures: 3, Stale: true, Endpoints: set},
		// NXDOMAIN from 250: two refreshes kept the set, the third emptied it.
		{At: dnsAt(300), Failures: 4, Stale: true, Endpoints: set},
		{At: dnsAt(400), Failures: 5, Stale: true, Endpoints: set},
		{At: dnsAt(500), Endpoints: nil},
		// Answers again from 550.
		{At: dnsAt(600), Endpoints: nil},
		{At: dnsAt(700), Endpoints: set},
	}
	if kept, streak, ok := DNSKept(obs, set, dnsAt(100), dnsAt(250)); !kept || streak != 3 || !ok {
		t.Errorf("DNSKept while failing = %v, %d, %v; want kept with a streak of 3", kept, streak, ok)
	}
	if kept, _, _ := DNSKept(obs, set, dnsAt(100), dnsAt(600)); kept {
		t.Error("DNSKept across the emptied set = true")
	}
	if _, _, ok := DNSKept(obs, set, dnsAt(1000), dnsAt(2000)); ok {
		t.Error("DNSKept without observations = ok")
	}
	kept, lastKept, emptiedAt, emptied := DNSEmptied(obs, dnsAt(250))
	if kept != discovery.NotFoundLimit-1 || !lastKept.Equal(dnsAt(400)) || !emptiedAt.Equal(dnsAt(500)) || !emptied {
		t.Errorf("DNSEmptied = %d, %v, %v, %v; want 2 kept, last at 400, emptied at 500", kept, lastKept.Sub(dnsT0), emptiedAt.Sub(dnsT0), emptied)
	}
	// A set emptied at the first NXDOMAIN refresh kept it for none.
	early := []DNSObservation{{At: dnsAt(0), Failures: 2, Stale: true, Endpoints: set}, {At: dnsAt(100), Endpoints: nil}}
	if kept, _, _, emptied := DNSEmptied(early, dnsAt(50)); kept != 0 || !emptied {
		t.Errorf("DNSEmptied of an early empty set = %d, %v; want 0, emptied", kept, emptied)
	}
	if _, _, _, emptied := DNSEmptied(obs[:5], dnsAt(250)); emptied {
		t.Error("DNSEmptied of a set kept to the end = emptied")
	}
	if at, ok := DNSBack(obs, set, dnsAt(550)); !ok || !at.Equal(dnsAt(700)) {
		t.Errorf("DNSBack = %v, %v; want 700", at.Sub(dnsT0), ok)
	}
	if _, ok := DNSBack(obs[:6], set, dnsAt(550)); ok {
		t.Error("DNSBack without the set back = ok")
	}
}

// TestDNSRefreshesWithin checks the bounds the drill walks the Nodes'
// clock for: three failed refreshes from the 30 s schedule within 33 s
// plus the 2 s and 4 s backoff ceilings, from a long streak within three
// 60 s ceilings, and after four failures the 32 s ceiling of the fifth
// and the 60 s cap of the sixth.
func TestDNSRefreshesWithin(t *testing.T) {
	first := discovery.RefreshInterval + discovery.RefreshJitter
	for _, tc := range []struct {
		name      string
		first     time.Duration
		streak, n int
		want      time.Duration
	}{
		{"from the 30 s schedule", first, 0, discovery.NotFoundLimit, first + 6*time.Second},
		{"after a long streak", discovery.BackoffMax, 6, discovery.NotFoundLimit, 3 * discovery.BackoffMax},
		{"after four failures", time.Second, 4, 3, time.Second + 32*time.Second + discovery.BackoffMax},
		{"one refresh", first, 0, 1, first},
	} {
		if got := DNSRefreshesWithin(tc.first, tc.streak, tc.n); got != tc.want {
			t.Errorf("%s: DNSRefreshesWithin(%v, %d, %d) = %v, want %v", tc.name, tc.first, tc.streak, tc.n, got, tc.want)
		}
	}
}
