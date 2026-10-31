// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package l4lb

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clientaddr"
)

// Tests for the PROXY protocol v2 option (Testing "Load generator and L4
// balancer", PROXY protocol): the header a Node's proxyProtocol listener
// reads (internal/clientaddr.ReadHeader is the oracle), its families, and
// its absence when the option is off.

// proxyBackend stands in for a Node listener. With header set it reads a
// PROXY v2 header first, as a proxyProtocol listener does; then it reads
// the client's first message and echoes it.
type proxyBackend struct {
	ln     net.Listener
	ready  *httptest.Server
	header bool

	mu      sync.Mutex
	headers []clientaddr.Header
	errs    []error
	first   [][]byte
	wg      sync.WaitGroup
}

func newProxyBackend(t *testing.T, header bool) *proxyBackend {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &proxyBackend{ln: ln, header: header}
	p.ready = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	p.wg.Add(1)
	go p.serve()
	t.Cleanup(func() {
		_ = p.ln.Close()
		p.wg.Wait()
		p.ready.Close()
	})
	return p
}

func (p *proxyBackend) serve() {
	defer p.wg.Done()
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			defer func() { _ = c.Close() }()
			_ = c.SetDeadline(time.Now().Add(10 * time.Second)) // a hang guard only
			if p.header {
				h, err := clientaddr.ReadHeader(c)
				p.mu.Lock()
				p.headers = append(p.headers, h)
				p.errs = append(p.errs, err)
				p.mu.Unlock()
				if err != nil {
					return
				}
			}
			msg := make([]byte, len("hello"))
			if _, err := io.ReadFull(c, msg); err != nil {
				return
			}
			p.mu.Lock()
			p.first = append(p.first, msg)
			p.mu.Unlock()
			_, _ = c.Write(msg)
		}()
	}
}

func (p *proxyBackend) backend() Backend {
	return Backend{Name: "p", Addr: p.ln.Addr().String(), ReadyURL: p.ready.URL + "/readyz"}
}

// hello sends "hello" through the balancer and reads its echo, returning
// the client's own address.
func hello(t *testing.T, lb *Balancer) netip.AddrPort {
	t.Helper()
	nc, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", lb.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = nc.Close() }()
	_ = nc.SetDeadline(time.Now().Add(10 * time.Second)) // a hang guard only
	if _, err := io.WriteString(nc, "hello"); err != nil {
		t.Fatal(err)
	}
	echo := make([]byte, len("hello"))
	if _, err := io.ReadFull(nc, echo); err != nil || string(echo) != "hello" {
		t.Fatalf("echo = %q, %v", echo, err)
	}
	return nc.LocalAddr().(*net.TCPAddr).AddrPort()
}

// With ProxyProtocol each backend connection starts with one PROXY v2
// header naming the client's address and the balancer address it reached,
// and the client's bytes follow unchanged.
func TestProxyProtocolCarriesClientAddress(t *testing.T) {
	pb := newProxyBackend(t, true)
	lb := startLB(t, Config{Backends: []Backend{pb.backend()}, Clock: fake(), ProxyProtocol: true})
	probed(t, lb, 1, "p")
	client := hello(t, lb)
	pb.mu.Lock()
	defer pb.mu.Unlock()
	if len(pb.headers) != 1 || pb.errs[0] != nil {
		t.Fatalf("headers %+v, errors %v; want one valid header", pb.headers, pb.errs)
	}
	h := pb.headers[0]
	lbAddr := netip.MustParseAddrPort(lb.Addr())
	if h.Command != clientaddr.CommandProxy || h.Family != clientaddr.FamilyTCP4 || h.Len != 16+proxyTCP4Len {
		t.Errorf("header %+v, want PROXY over TCP4, 28 bytes", h)
	}
	if h.Source != client || h.Destination != lbAddr {
		t.Errorf("header source %v destination %v, want the client %v and the balancer %v", h.Source, h.Destination, client, lbAddr)
	}
	if len(pb.first) != 1 || string(pb.first[0]) != "hello" {
		t.Errorf("bytes after the header = %q, want the client's", pb.first)
	}
}

// Without the option the backend's first bytes are the client's own:
// every connection's peer is the balancer.
func TestProxyProtocolOffSendsNoHeader(t *testing.T) {
	pb := newProxyBackend(t, false)
	lb := startLB(t, Config{Backends: []Backend{pb.backend()}, Clock: fake()})
	probed(t, lb, 1, "p")
	hello(t, lb)
	pb.mu.Lock()
	defer pb.mu.Unlock()
	if len(pb.first) != 1 || !bytes.Equal(pb.first[0], []byte("hello")) {
		t.Fatalf("backend's first bytes = %q, want the client's hello", pb.first)
	}
}

// A client reaching the balancer over IPv6 gets a TCP6 header.
func TestProxyProtocolOverIPv6(t *testing.T) {
	probe, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback: %v", err)
	}
	_ = probe.Close()
	pb := newProxyBackend(t, true)
	lb := startLB(t, Config{Listen: "[::1]:0", Backends: []Backend{pb.backend()}, Clock: fake(), ProxyProtocol: true})
	probed(t, lb, 1, "p")
	client := hello(t, lb)
	pb.mu.Lock()
	defer pb.mu.Unlock()
	if len(pb.headers) != 1 || pb.errs[0] != nil {
		t.Fatalf("headers %+v, errors %v", pb.headers, pb.errs)
	}
	h := pb.headers[0]
	if h.Family != clientaddr.FamilyTCP6 || h.Source != client || h.Destination != netip.MustParseAddrPort(lb.Addr()) {
		t.Errorf("header %+v, want TCP6 from %v to %v", h, client, lb.Addr())
	}
}

// proxyHeader picks one family for both addresses: TCP4 when both are
// IPv4 (IPv4-mapped IPv6 counts as IPv4), else TCP6 with an IPv4 address
// mapped.
func TestProxyHeaderFamilies(t *testing.T) {
	tests := []struct {
		name             string
		src, dst         string
		family           clientaddr.Family
		wantSrc, wantDst string
		wantLen          int
	}{
		{"IPv4", "127.0.0.7:40000", "127.0.0.1:8080", clientaddr.FamilyTCP4, "127.0.0.7:40000", "127.0.0.1:8080", 28},
		{"IPv6", "[::1]:40000", "[::1]:8443", clientaddr.FamilyTCP6, "[::1]:40000", "[::1]:8443", 52},
		{"IPv4-mapped source", "[::ffff:127.0.0.9]:1", "127.0.0.1:2", clientaddr.FamilyTCP4, "127.0.0.9:1", "127.0.0.1:2", 28},
		{"mixed families", "[2001:db8::1]:5", "10.0.0.1:6", clientaddr.FamilyTCP6, "[2001:db8::1]:5", "[::ffff:10.0.0.1]:6", 52},
		{"zone dropped", "[fe80::1%lo]:7", "[fe80::2%lo]:8", clientaddr.FamilyTCP6, "[fe80::1]:7", "[fe80::2]:8", 52},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := proxyHeader(netip.MustParseAddrPort(tt.src), netip.MustParseAddrPort(tt.dst))
			h, err := clientaddr.ReadHeader(bytes.NewReader(append(raw, "next"...)))
			if err != nil {
				t.Fatalf("ReadHeader(proxyHeader(%s, %s)) = %v", tt.src, tt.dst, err)
			}
			if h.Family != tt.family || h.Len != tt.wantLen || len(raw) != tt.wantLen || h.Command != clientaddr.CommandProxy {
				t.Errorf("header %+v of %d bytes, want %v in %d bytes", h, len(raw), tt.family, tt.wantLen)
			}
			if h.Source != netip.MustParseAddrPort(tt.wantSrc) || h.Destination != netip.MustParseAddrPort(tt.wantDst) {
				t.Errorf("source %v destination %v, want %s and %s", h.Source, h.Destination, tt.wantSrc, tt.wantDst)
			}
		})
	}
}
