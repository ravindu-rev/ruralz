// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package mock

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// dnsMessage builds a DNS message: the header fields, then the raw
// question and additional bytes.
func dnsMessage(id uint16, flags uint16, qdcount, arcount uint16, body ...[]byte) []byte {
	msg := make([]byte, dnsHeaderLen)
	binary.BigEndian.PutUint16(msg[0:2], id)
	binary.BigEndian.PutUint16(msg[2:4], flags)
	binary.BigEndian.PutUint16(msg[4:6], qdcount)
	binary.BigEndian.PutUint16(msg[10:12], arcount)
	for _, b := range body {
		msg = append(msg, b...)
	}
	return msg
}

// wireName encodes labels, each at most 63 bytes, as an uncompressed name.
func wireName(labels ...string) []byte {
	var b []byte
	for _, l := range labels {
		n := len(l)
		if n > 63 {
			panic("wireName: a label longer than 63 bytes")
		}
		b = append(b, byte(n))
		b = append(b, l...)
	}
	return append(b, 0)
}

// typeClass is QTYPE t and QCLASS IN.
func typeClass(t uint16) []byte {
	return binary.BigEndian.AppendUint16(binary.BigEndian.AppendUint16(nil, t), 1)
}

// optRecord is an EDNS(0) OPT pseudo-record announcing a 4,096-byte
// payload (RFC 6891).
func optRecord() []byte {
	return []byte{0, 0, 41, 0x10, 0x00, 0, 0, 0, 0, 0, 0}
}

const rdFlag = 0x0100

func TestParseQuery(t *testing.T) {
	long := make([]string, 0, 5)
	for range 5 {
		long = append(long, strings.Repeat("a", 63))
	}
	tests := []struct {
		name      string
		msg       []byte
		wantRCode int
		wantName  string
		wantType  uint16
	}{
		{"a query, lower-cased", dnsMessage(7, rdFlag, 1, 0, wireName("Example", "COM"), typeClass(1)), rcodeNXDomain, "example.com.", 1},
		{"an EDNS query", dnsMessage(7, rdFlag, 1, 1, wireName("orders", "shop", "internal"), typeClass(28), optRecord()), rcodeNXDomain, "orders.shop.internal.", 28},
		{"the root name", dnsMessage(1, 0, 1, 0, wireName(), typeClass(2)), rcodeNXDomain, ".", 2},
		{"bytes outside a hostname escaped", dnsMessage(1, 0, 1, 0, wireName("a.b", "x\x00_-"), typeClass(16)), rcodeNXDomain, `a\046b.x\000_-.`, 16},
		{"a response gets no answer", dnsMessage(1, 0x8000, 1, 0, wireName("a"), typeClass(1)), -1, "", 0},
		{"a short message gets no answer", []byte{0, 1, 0, 0}, -1, "", 0},
		{"a non-standard opcode", dnsMessage(1, 2<<11, 1, 0, wireName("a"), typeClass(1)), rcodeNotImp, "", 0},
		{"no question", dnsMessage(1, 0, 0, 0), rcodeFormErr, "", 0},
		{"two questions", dnsMessage(1, 0, 2, 0, wireName("a"), typeClass(1), wireName("b"), typeClass(1)), rcodeFormErr, "", 0},
		{"a compression pointer", dnsMessage(1, 0, 1, 0, []byte{0xC0, 0x0C}, typeClass(1)), rcodeFormErr, "", 0},
		{"a reserved label type", dnsMessage(1, 0, 1, 0, []byte{0x40, 'a', 0}, typeClass(1)), rcodeFormErr, "", 0},
		{"a truncated label", dnsMessage(1, 0, 1, 0, []byte{5, 'a', 'b'}), rcodeFormErr, "", 0},
		{"a name without its end", dnsMessage(1, 0, 1, 0, []byte{1, 'a'}), rcodeFormErr, "", 0},
		{"a name of 256 wire bytes", dnsMessage(1, 0, 1, 0, wireName(long[0], long[1], long[2], long[3][:62]), typeClass(1)), rcodeFormErr, "", 0},
		{"a missing QTYPE and QCLASS", dnsMessage(1, 0, 1, 0, wireName("a"), []byte{0, 1}), rcodeFormErr, "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q, rcode := parseQuery(tt.msg)
			if rcode != tt.wantRCode {
				t.Fatalf("parseQuery rcode = %d, want %d", rcode, tt.wantRCode)
			}
			if q.name != tt.wantName || q.qtype != tt.wantType {
				t.Errorf("parseQuery = %q type %d, want %q type %d", q.name, q.qtype, tt.wantName, tt.wantType)
			}
		})
	}
}

func TestParseQueryLongestName(t *testing.T) {
	l := strings.Repeat("a", 63)
	// 3 × 64 + 62 wire bytes and the root's length octet: 255.
	msg := dnsMessage(1, 0, 1, 0, wireName(l, l, l, l[:61]), typeClass(1))
	if _, rcode := parseQuery(msg); rcode != rcodeNXDomain {
		t.Fatalf("parseQuery of a 255-byte name: rcode %d, want %d", rcode, rcodeNXDomain)
	}
}

func TestAnswer(t *testing.T) {
	t.Run("echoes the question with NXDOMAIN", func(t *testing.T) {
		msg := dnsMessage(0xBEEF, rdFlag, 1, 1, wireName("a", "b"), typeClass(1), optRecord())
		q, rcode := parseQuery(msg)
		got := answer(msg, q, rcode)
		want := dnsMessage(0xBEEF, 0x8000|rdFlag|rcodeNXDomain, 1, 0, wireName("a", "b"), typeClass(1))
		if !bytes.Equal(got, want) {
			t.Errorf("answer = %x, want %x", got, want)
		}
	})
	t.Run("FORMERR carries no question", func(t *testing.T) {
		msg := dnsMessage(9, 0, 2, 0, wireName("a"), typeClass(1), wireName("b"), typeClass(1))
		q, rcode := parseQuery(msg)
		got := answer(msg, q, rcode)
		if want := dnsMessage(9, 0x8000|rcodeFormErr, 0, 0); !bytes.Equal(got, want) {
			t.Errorf("answer = %x, want %x", got, want)
		}
	})
	t.Run("NOTIMP keeps the opcode", func(t *testing.T) {
		msg := dnsMessage(9, 2<<11, 1, 0, wireName("a"), typeClass(1))
		q, rcode := parseQuery(msg)
		got := answer(msg, q, rcode)
		if want := dnsMessage(9, 0x8000|2<<11|rcodeNotImp, 0, 0); !bytes.Equal(got, want) {
			t.Errorf("answer = %x, want %x", got, want)
		}
	})
}

func TestTypeName(t *testing.T) {
	tests := []struct {
		t    uint16
		want string
	}{{1, "A"}, {28, "AAAA"}, {65, "HTTPS"}, {33, "SRV"}, {99, "TYPE99"}}
	for _, tt := range tests {
		if got := typeName(tt.t); got != tt.want {
			t.Errorf("typeName(%d) = %q, want %q", tt.t, got, tt.want)
		}
	}
}

func TestDNSSinkRecords(t *testing.T) {
	s := newTestSink(t)
	t.Run("a lookup through the sink is not found and recorded", func(t *testing.T) {
		r := sinkResolver(s.Addr())
		_, err := r.LookupHost(t.Context(), "Phone-Home.example.")
		var de *net.DNSError
		if !errors.As(err, &de) || !de.IsNotFound {
			t.Fatalf("LookupHost through the sink = %v, want a not-found DNS error", err)
		}
		got := map[string]bool{}
		for _, q := range s.Log().Queries {
			if q.Name == "phone-home.example." && q.Transport == "udp" && q.Client != "" {
				got[q.Type] = true
			}
		}
		if !got["A"] || !got["AAAA"] {
			t.Errorf("sink log %+v, want A and AAAA questions for phone-home.example. over udp", s.Log().Queries)
		}
	})
	t.Run("a query over TCP is answered and recorded", func(t *testing.T) {
		c, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", s.Addr())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close() }()
		if err := c.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
			t.Fatal(err)
		}
		for i := range 2 { // two messages on one connection
			msg := dnsMessage(uint16(40+i), rdFlag, 1, 0, wireName("tcp", "example"), typeClass(1))
			if _, err := c.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(msg))), msg...)); err != nil { //nolint:gosec // G115: a query of one short question, far below 65,535 bytes
				t.Fatal(err)
			}
			var prefix [2]byte
			if _, err := io.ReadFull(c, prefix[:]); err != nil {
				t.Fatal(err)
			}
			resp := make([]byte, binary.BigEndian.Uint16(prefix[:]))
			if _, err := io.ReadFull(c, resp); err != nil {
				t.Fatal(err)
			}
			if resp[3]&0x0F != rcodeNXDomain || binary.BigEndian.Uint16(resp[0:2]) != uint16(40+i) {
				t.Errorf("TCP answer %d = %x, want ID %d and NXDOMAIN", i, resp, 40+i)
			}
		}
		n := 0
		for _, q := range s.Log().Queries {
			if q.Name == "tcp.example." && q.Transport == "tcp" {
				n++
			}
		}
		if n != 2 {
			t.Errorf("sink log holds %d TCP questions for tcp.example., want 2", n)
		}
	})
}

func TestDNSSinkCounts(t *testing.T) {
	s := &dnsSink{}
	s.log.Queries = []DNSQuery{}
	query := dnsMessage(1, 0, 1, 0, wireName("a"), typeClass(1))
	for range MaxDNSQueries + 3 {
		if resp := s.handle(query, "udp", "client"); resp == nil {
			t.Fatal("handle answered nothing to a query")
		}
	}
	if resp := s.handle(dnsMessage(1, 0, 2, 0), "udp", "client"); resp == nil {
		t.Fatal("handle answered nothing to a malformed query")
	}
	if resp := s.handle(dnsMessage(1, 0x8000, 1, 0, wireName("a"), typeClass(1)), "udp", "client"); resp != nil {
		t.Fatalf("handle answered a response: %x", resp)
	}
	log := s.Log()
	if len(log.Queries) != MaxDNSQueries || log.Dropped != 3 || log.Malformed != 2 {
		t.Errorf("log holds %d queries, %d dropped, %d malformed; want %d, 3, 2", len(log.Queries), log.Dropped, log.Malformed, MaxDNSQueries)
	}
}

func TestDNSSinkClose(t *testing.T) {
	s, err := startDNSSink(t.Context(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	c, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", s.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	done := make(chan error, 1)
	go func() { done <- s.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Close = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return while a TCP connection stayed open")
	}
	if err := s.Close(); err != nil {
		t.Errorf("second Close = %v", err)
	}
}

// newTestSink starts a sink on loopback, closed with the test.
func newTestSink(t *testing.T) *dnsSink {
	t.Helper()
	s, err := startDNSSink(context.WithoutCancel(t.Context()), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// sinkResolver resolves through the DNS server at addr only.
func sinkResolver(addr string) *net.Resolver {
	return &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}}
}

func TestDNSSinkRetriesATakenPort(t *testing.T) {
	taken := &net.OpError{Op: "listen", Net: "tcp", Err: os.NewSyscallError("bind", syscall.EADDRINUSE)}
	tests := []struct {
		name     string
		addr     string
		failures int
		wantErr  bool
		wantTry  int
	}{
		{"port 0 tries a new port", "127.0.0.1:0", 3, false, 4},
		{"port 0 gives up after its attempts", "127.0.0.1:0", dnsPortAttempts, true, dnsPortAttempts},
		{"a fixed port is tried once", "127.0.0.1:53", 1, true, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tries := 0
			listen := func(ctx context.Context, _ string) (net.PacketConn, net.Listener, error) {
				tries++
				if tries <= tt.failures {
					return nil, nil, taken
				}
				return separatePorts(ctx, t)
			}
			s, err := startDNSSinkWith(t.Context(), tt.addr, listen)
			if s != nil {
				defer func() { _ = s.Close() }()
			}
			if (err != nil) != tt.wantErr || tries != tt.wantTry {
				t.Errorf("startDNSSinkWith(%s) after %d taken ports = %v, %d tries; want error %v after %d tries", tt.addr, tt.failures, err, tries, tt.wantErr, tt.wantTry)
			}
			if err != nil && !errors.Is(err, syscall.EADDRINUSE) {
				t.Errorf("error %v, want it to wrap EADDRINUSE", err)
			}
		})
	}
	t.Run("another error is not retried", func(t *testing.T) {
		tries := 0
		listen := func(context.Context, string) (net.PacketConn, net.Listener, error) {
			tries++
			return nil, nil, errors.New("no network")
		}
		if _, err := startDNSSinkWith(t.Context(), "127.0.0.1:0", listen); err == nil || tries != 1 {
			t.Errorf("startDNSSinkWith = %v after %d tries, want an error after 1", err, tries)
		}
	})
}

// separatePorts opens a UDP and a TCP socket on ports of their own, which
// never collide: the fake listener's success.
func separatePorts(ctx context.Context, t *testing.T) (net.PacketConn, net.Listener, error) {
	t.Helper()
	var lc net.ListenConfig
	udp, err := lc.ListenPacket(ctx, "udp", "127.0.0.1:0")
	if err != nil {
		return nil, nil, err
	}
	tcp, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		_ = udp.Close()
		return nil, nil, err
	}
	return udp, tcp, nil
}
