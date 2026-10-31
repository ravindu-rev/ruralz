// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package gameday

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/discovery"
)

// DNS server bounds.
const (
	// DNSMaxMessage is the largest message the server reads: the 16-bit
	// length of a TCP message bounds it (RFC 1035 section 4.2.2), and
	// EDNS(0) lets a UDP query announce up to that size.
	DNSMaxMessage = 65535
	// DNSMaxTCPConns bounds the TCP connections served at once; another
	// is closed at once.
	DNSMaxTCPConns = 64
	// DNSTCPIdle bounds the wait for each message on a TCP connection.
	DNSTCPIdle = 10 * time.Second
	// DNSMaxNames bounds the names the server holds records or a mode
	// for; it counts the queries of those names alone.
	DNSMaxNames = 256
	// dnsPortAttempts bounds the ports tried for port 0, whose TCP port may
	// be taken for UDP by another process.
	dnsPortAttempts = 16
	// dnsMaxUDP is the largest response sent over UDP: a larger one goes
	// out truncated (TC) and the client asks again over TCP (RFC 1035
	// section 4.2.1).
	dnsMaxUDP = 512
	// dnsTTL is every record's TTL: nothing the drill asks may be cached.
	dnsTTL = 0
	// dnsHeaderLen is the fixed header of a DNS message (RFC 1035 section
	// 4.1.1).
	dnsHeaderLen = 12
	// dnsMaxName is the longest name in wire form, length octets and the
	// root label included (RFC 1035 section 2.3.4); dnsMaxLabel the longest
	// label.
	dnsMaxName  = 255
	dnsMaxLabel = 63
)

// DNS record types and the IN class (RFC 1035 section 3.2.2, RFC 3596,
// RFC 2782).
const (
	dnsTypeA    = 1
	dnsTypeAAAA = 28
	dnsTypeSRV  = 33
	dnsClassIN  = 1
)

// DNS response codes (RFC 1035 section 4.1.1).
const (
	dnsRcodeOK       byte = 0
	dnsRcodeFormErr  byte = 1
	dnsRcodeServFail byte = 2
	dnsRcodeNXDomain byte = 3
	dnsRcodeNotImp   byte = 4
)

// DNSMode is how the server answers a name.
type DNSMode uint8

// Answer modes of the FC-19 drill's sub-phases.
const (
	// DNSAnswer answers from the records the server holds: the question
	// type's records, none (NOERROR) for a name holding only records of
	// other types, NXDOMAIN for a name it holds no record of.
	DNSAnswer DNSMode = iota + 1
	// DNSFail answers SERVFAIL: the server fails.
	DNSFail
	// DNSNXDomain answers NXDOMAIN: the name does not exist.
	DNSNXDomain
	// DNSEmpty answers NOERROR without a record: the name exists without
	// records of the type, an empty answer.
	DNSEmpty
)

// String names the mode.
func (m DNSMode) String() string {
	switch m {
	case DNSAnswer:
		return "answer"
	case DNSFail:
		return "servfail"
	case DNSNXDomain:
		return "nxdomain"
	case DNSEmpty:
		return "empty"
	default:
		return fmt.Sprintf("DNSMode(%d)", uint8(m))
	}
}

// DNSSRV is one SRV record (RFC 2782).
type DNSSRV struct {
	Priority, Weight, Port uint16
	// Target is the host name the record points at.
	Target string
}

// DNSServer is the harness DNS server of the FC-19 drill
// (docs/engineering/03-testing-and-quality-strategy.md "Drills and failure
// rows"): it answers A, AAAA and SRV questions for the names it holds,
// over UDP and TCP on one address, and fails, answers NXDOMAIN or answers
// empty, for every name or one name, as the drill switches it. Its
// goroutines, a UDP loop, a TCP accept loop and at most DNSMaxTCPConns
// connection handlers, are owned by the server: Close, or the end of the
// Start context, stops them and waits. Its methods are safe for
// concurrent use.
type DNSServer struct {
	udp net.PacketConn
	tcp net.Listener

	mu      sync.Mutex
	hosts   map[string][]netip.Addr // canonical name to addresses
	srvs    map[string][]DNSSRV     // canonical name to SRV records
	def     DNSMode
	names   map[string]DNSMode // per-name modes overriding def
	queries map[string]int64   // questions per held name
	other   int64              // questions for every other name
	conns   map[net.Conn]struct{}

	slots     chan struct{} // bounds the TCP connections served at once
	wg        sync.WaitGroup
	closeOnce sync.Once
	stop      func() bool
}

// StartDNSServer listens on addr for UDP and TCP, port 0 picking one free
// port for both, and serves in DNSAnswer mode, holding no record, until
// Close or the end of ctx.
func StartDNSServer(ctx context.Context, addr string) (*DNSServer, error) {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("gameday: dns: %w", err)
	}
	attempts := 1
	if port == "0" {
		attempts = dnsPortAttempts
	}
	var udp net.PacketConn
	var tcp net.Listener
	for range attempts {
		if udp, tcp, err = dnsListen(ctx, addr); err == nil {
			break
		}
	}
	if err != nil {
		return nil, fmt.Errorf("gameday: dns: %w", err)
	}
	s := &DNSServer{
		udp: udp, tcp: tcp,
		hosts: map[string][]netip.Addr{}, srvs: map[string][]DNSSRV{},
		def: DNSAnswer, names: map[string]DNSMode{}, queries: map[string]int64{},
		conns: map[net.Conn]struct{}{}, slots: make(chan struct{}, DNSMaxTCPConns),
	}
	s.wg.Add(2)
	go s.serveUDP()
	go s.serveTCP()
	// The callback may run at once (ctx already done); Close then waits
	// for s.mu, so stop is always set before Close reads it.
	s.mu.Lock()
	s.stop = context.AfterFunc(ctx, func() { _ = s.Close() })
	s.mu.Unlock()
	return s, nil
}

// dnsListen listens for TCP on addr and for UDP on the port TCP got, so a
// client finds both on one address. TCP picks first: a host may exclude
// TCP port ranges inside the range it picks UDP ports from, as Windows
// does, so a UDP port is often unusable for TCP, while the converse is
// rare.
func dnsListen(ctx context.Context, addr string) (net.PacketConn, net.Listener, error) {
	var lc net.ListenConfig
	tcp, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, nil, err
	}
	udp, err := lc.ListenPacket(ctx, "udp", tcp.Addr().String())
	if err != nil {
		_ = tcp.Close()
		return nil, nil, err
	}
	return udp, tcp, nil
}

// Addr returns the address both transports listen on.
func (s *DNSServer) Addr() string { return s.udp.LocalAddr().String() }

// SetHost makes name answer its A and AAAA questions with addrs, in
// order, replacing what it held; no address removes the name's host
// records.
func (s *DNSServer) SetHost(name string, addrs ...netip.Addr) error {
	c, err := dnsCanonical(name)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(addrs) == 0 {
		delete(s.hosts, c)
		return nil
	}
	if err := s.roomLocked(c); err != nil {
		return err
	}
	out := make([]netip.Addr, len(addrs))
	for i, a := range addrs {
		out[i] = a.Unmap()
	}
	s.hosts[c] = out
	return nil
}

// SetSRV makes name, such as "_http._tcp.service.example", answer its SRV
// questions with recs, in order, replacing what it held; no record
// removes them.
func (s *DNSServer) SetSRV(name string, recs ...DNSSRV) error {
	c, err := dnsCanonical(name)
	if err != nil {
		return err
	}
	out := make([]DNSSRV, len(recs))
	for i, r := range recs {
		t, err := dnsCanonical(r.Target)
		if err != nil {
			return err
		}
		r.Target = t
		out[i] = r
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(recs) == 0 {
		delete(s.srvs, c)
		return nil
	}
	if err := s.roomLocked(c); err != nil {
		return err
	}
	s.srvs[c] = out
	return nil
}

// SetModes answers every name in def mode, except the names of names,
// each in its own mode; the switch is one step, so no question meets a
// mix of the old and the new modes.
func (s *DNSServer) SetModes(def DNSMode, names map[string]DNSMode) error {
	if err := dnsValidMode(def); err != nil {
		return err
	}
	byName := make(map[string]DNSMode, len(names))
	for n, m := range names {
		c, err := dnsCanonical(n)
		if err != nil {
			return err
		}
		if err := dnsValidMode(m); err != nil {
			return err
		}
		byName[c] = m
	}
	if len(byName) > DNSMaxNames {
		return fmt.Errorf("gameday: dns: %d names, at most %d", len(byName), DNSMaxNames)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.def, s.names = def, byName
	return nil
}

// Queries returns how many questions named name the server received.
func (s *DNSServer) Queries(name string) int64 {
	c, err := dnsCanonical(name)
	if err != nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.queries[c]
}

// OtherQueries returns how many questions named a name the server held
// nothing for when they came.
func (s *DNSServer) OtherQueries() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.other
}

// roomLocked refuses a name past DNSMaxNames.
func (s *DNSServer) roomLocked(c string) error {
	_, h := s.hosts[c]
	_, v := s.srvs[c]
	if !h && !v && len(s.hosts)+len(s.srvs) >= DNSMaxNames {
		return fmt.Errorf("gameday: dns: more than %d names", DNSMaxNames)
	}
	return nil
}

// Close stops both transports, closes every open TCP connection and
// waits for every goroutine of the server.
func (s *DNSServer) Close() error {
	var err error
	s.closeOnce.Do(func() {
		s.mu.Lock()
		stop := s.stop
		s.mu.Unlock()
		if stop != nil {
			stop()
		}
		err = errors.Join(s.udp.Close(), s.tcp.Close())
		s.mu.Lock()
		for c := range s.conns {
			_ = c.Close()
		}
		s.mu.Unlock()
		s.wg.Wait()
	})
	return err
}

func (s *DNSServer) serveUDP() {
	defer s.wg.Done()
	buf := make([]byte, DNSMaxMessage)
	for {
		n, from, err := s.udp.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		if resp := s.handle(buf[:n], true); resp != nil {
			_, _ = s.udp.WriteTo(resp, from)
		}
	}
}

func (s *DNSServer) serveTCP() {
	defer s.wg.Done()
	for {
		c, err := s.tcp.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		select {
		case s.slots <- struct{}{}:
		default:
			_ = c.Close()
			continue
		}
		s.mu.Lock()
		s.conns[c] = struct{}{}
		s.mu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() { <-s.slots }()
			s.serveConn(c)
			s.mu.Lock()
			delete(s.conns, c)
			s.mu.Unlock()
			_ = c.Close()
		}()
	}
}

// serveConn answers the length-prefixed messages of one TCP connection
// (RFC 1035 section 4.2.2) until the client closes it or stays idle.
func (s *DNSServer) serveConn(c net.Conn) {
	var prefix [2]byte
	buf := make([]byte, DNSMaxMessage)
	for {
		if err := c.SetReadDeadline(time.Now().Add(DNSTCPIdle)); err != nil {
			return
		}
		if _, err := io.ReadFull(c, prefix[:]); err != nil {
			return
		}
		n := int(binary.BigEndian.Uint16(prefix[:]))
		if _, err := io.ReadFull(c, buf[:n]); err != nil {
			return
		}
		resp := s.handle(buf[:n], false)
		if resp == nil {
			continue
		}
		out := binary.BigEndian.AppendUint16(make([]byte, 0, 2+len(resp)), uint16(len(resp))) //nolint:gosec // G115: a response holds one question and at most a few records, far below 65,535 bytes
		if _, err := c.Write(append(out, resp...)); err != nil {
			return
		}
	}
}

// handle answers msg, nil for a message that gets no answer: a response,
// or one too short to hold a header.
func (s *DNSServer) handle(msg []byte, udp bool) []byte {
	q, rcode, answer := dnsParseQuery(msg)
	switch {
	case !answer:
		return nil
	case rcode != dnsRcodeOK:
		return dnsResponse(msg, q, rcode, nil)
	}
	s.mu.Lock()
	mode, named := s.names[q.name]
	if !named {
		mode = s.def
	}
	_, isHost := s.hosts[q.name]
	_, isSRV := s.srvs[q.name]
	if isHost || isSRV || named {
		s.queries[q.name]++
	} else {
		s.other++
	}
	var answers [][]byte
	switch mode {
	case DNSFail:
		rcode = dnsRcodeServFail
	case DNSNXDomain:
		rcode = dnsRcodeNXDomain
	case DNSEmpty:
		rcode = dnsRcodeOK
	default:
		if !isHost && !isSRV {
			rcode = dnsRcodeNXDomain
			break
		}
		answers = s.recordsLocked(q)
	}
	s.mu.Unlock()
	resp := dnsResponse(msg, q, rcode, answers)
	if udp && len(resp) > dnsMaxUDP {
		resp = dnsResponse(msg, q, rcode, nil)
		resp[2] |= 0x02 // TC: ask again over TCP
	}
	return resp
}

// recordsLocked returns the answer records of q's name and type, each
// naming the question's name by a pointer to it.
func (s *DNSServer) recordsLocked(q dnsQuestion) [][]byte {
	var out [][]byte
	switch q.qtype {
	case dnsTypeA, dnsTypeAAAA:
		for _, a := range s.hosts[q.name] {
			switch {
			case q.qtype == dnsTypeA && a.Is4():
				b := a.As4()
				out = append(out, dnsRecord(dnsTypeA, b[:]))
			case q.qtype == dnsTypeAAAA && a.Is6():
				b := a.As16()
				out = append(out, dnsRecord(dnsTypeAAAA, b[:]))
			}
		}
	case dnsTypeSRV:
		for _, r := range s.srvs[q.name] {
			rdata := binary.BigEndian.AppendUint16(nil, r.Priority)
			rdata = binary.BigEndian.AppendUint16(rdata, r.Weight)
			rdata = binary.BigEndian.AppendUint16(rdata, r.Port)
			out = append(out, dnsRecord(dnsTypeSRV, append(rdata, dnsWireName(r.Target)...)))
		}
	}
	return out
}

// dnsRecord returns one answer record of type typ: the question's name by
// the pointer 0xC00C, class IN, dnsTTL and rdata.
func dnsRecord(typ uint16, rdata []byte) []byte {
	b := []byte{0xC0, dnsHeaderLen}
	b = binary.BigEndian.AppendUint16(b, typ)
	b = binary.BigEndian.AppendUint16(b, dnsClassIN)
	b = binary.BigEndian.AppendUint32(b, dnsTTL)
	b = binary.BigEndian.AppendUint16(b, uint16(len(rdata))) //nolint:gosec // G115: rdata is an address or one SRV record, below 300 bytes
	return append(b, rdata...)
}

// dnsQuestion is the one question of a query: its canonical name, type
// and the offset just past its class.
type dnsQuestion struct {
	name  string
	qtype uint16
	end   int
}

// dnsParseQuery reads the header and the question of msg. It returns
// dnsRcodeOK for a standard IN query with one question, or the code to
// answer a malformed (FORMERR) or unsupported (NOTIMP) query with; answer
// is false for a message that is a response or too short to answer, which
// gets no answer at all.
func dnsParseQuery(msg []byte) (q dnsQuestion, rcode byte, answer bool) {
	if len(msg) < dnsHeaderLen || msg[2]&0x80 != 0 {
		return dnsQuestion{}, 0, false
	}
	if opcode := (msg[2] >> 3) & 0x0F; opcode != 0 {
		return dnsQuestion{}, dnsRcodeNotImp, true
	}
	if binary.BigEndian.Uint16(msg[4:6]) != 1 {
		return dnsQuestion{}, dnsRcodeFormErr, true
	}
	name, off, err := dnsReadName(msg, dnsHeaderLen)
	if err != nil || off+4 > len(msg) {
		return dnsQuestion{}, dnsRcodeFormErr, true
	}
	q = dnsQuestion{name: name, qtype: binary.BigEndian.Uint16(msg[off : off+2]), end: off + 4}
	if binary.BigEndian.Uint16(msg[off+2:off+4]) != dnsClassIN {
		return q, dnsRcodeNotImp, true
	}
	return q, dnsRcodeOK, true
}

// errDNSName reports a name the server cannot read or hold.
var errDNSName = errors.New("gameday: dns: malformed name")

// dnsReadName reads the uncompressed name at off, lower-cased with its
// trailing dot, and the offset after it. A compression pointer or a
// reserved label type is refused: a question's name is the first one in
// the message, so it has nothing earlier to point at.
func dnsReadName(msg []byte, off int) (string, int, error) {
	var b strings.Builder
	wire := 1 // the root label's length octet
	for {
		if off >= len(msg) {
			return "", 0, errDNSName
		}
		n := int(msg[off])
		off++
		if n == 0 {
			if b.Len() == 0 {
				return ".", off, nil
			}
			return b.String(), off, nil
		}
		if n&0xC0 != 0 {
			return "", 0, errDNSName
		}
		wire += 1 + n
		if wire > dnsMaxName || off+n > len(msg) {
			return "", 0, errDNSName
		}
		b.WriteString(strings.ToLower(string(msg[off : off+n])))
		b.WriteByte('.')
		off += n
	}
}

// dnsCanonical returns name lower-cased with its trailing dot, refusing
// an empty label, a label past 63 bytes, a name past 255 bytes in wire
// form, or a byte that is not a letter, digit, '-' or '_'.
func dnsCanonical(name string) (string, error) {
	name = strings.TrimSuffix(strings.ToLower(name), ".")
	if name == "" {
		return "", fmt.Errorf("%w: %q", errDNSName, name)
	}
	wire := 1
	for _, l := range strings.Split(name, ".") {
		if l == "" || len(l) > dnsMaxLabel {
			return "", fmt.Errorf("%w: %q", errDNSName, name)
		}
		for _, c := range []byte(l) {
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '_' {
				return "", fmt.Errorf("%w: %q", errDNSName, name)
			}
		}
		wire += 1 + len(l)
	}
	if wire > dnsMaxName {
		return "", fmt.Errorf("%w: %q", errDNSName, name)
	}
	return name + ".", nil
}

// dnsWireName returns a canonical name in wire form, uncompressed.
func dnsWireName(canonical string) []byte {
	var b []byte
	for _, l := range strings.Split(strings.TrimSuffix(canonical, "."), ".") {
		b = append(b, byte(len(l))) //nolint:gosec // G115: a canonical label holds at most 63 bytes
		b = append(b, l...)
	}
	return append(b, 0)
}

// dnsResponse builds the response to msg: its ID, opcode and RD bit, QR,
// AA and RA set, rcode, the question echoed when it parsed, and answers.
// Authority matters: a resolver takes a non-authoritative NOERROR without
// records and without recursion for a lame referral, a failure, where the
// drill means an empty answer.
func dnsResponse(msg []byte, q dnsQuestion, rcode byte, answers [][]byte) []byte {
	out := make([]byte, dnsHeaderLen, 512)
	copy(out[0:2], msg[0:2])
	out[2] = 0x80 | msg[2]&0x78 | 0x04 | msg[2]&0x01 // QR, the opcode, AA, RD
	out[3] = 0x80 | rcode&0x0F                       // RA, rcode
	if q.end > 0 {
		binary.BigEndian.PutUint16(out[4:6], 1)
		out = append(out, msg[dnsHeaderLen:q.end]...)
	}
	binary.BigEndian.PutUint16(out[6:8], uint16(len(answers))) //nolint:gosec // G115: a few records
	for _, a := range answers {
		out = append(out, a...)
	}
	return out
}

// dnsValidMode refuses a mode outside the four.
func dnsValidMode(m DNSMode) error {
	switch m {
	case DNSAnswer, DNSFail, DNSNXDomain, DNSEmpty:
		return nil
	default:
		return fmt.Errorf("gameday: dns: mode %v", m)
	}
}

// DNSObservation is one /debug/upstreams reading of a discovery Upstream
// on one Node: when it arrived, its Source's failure streak and staleness,
// and its Endpoint identities, sorted.
type DNSObservation struct {
	At        time.Time
	Failures  int
	Stale     bool
	Endpoints []string
}

// DNSKept reports whether every observation in [from, to) shows the
// Endpoint set want, with the highest failure streak among them; ok is
// false when no observation falls in the interval.
func DNSKept(obs []DNSObservation, want []string, from, to time.Time) (kept bool, streak int, ok bool) {
	kept = true
	for _, o := range obs {
		if o.At.Before(from) || !o.At.Before(to) {
			continue
		}
		ok = true
		streak = max(streak, o.Failures)
		if !slices.Equal(o.Endpoints, want) {
			kept = false
		}
	}
	return kept && ok, streak, ok
}

// DNSEmptied reads the observations of a Source told NXDOMAIN, or an
// empty answer, from switched on: kept counts the refreshes that failed
// and kept its set, its failure streak's growth past the last observation
// before switched; lastKept is the last observation still showing a set,
// and emptiedAt the first showing none. A set kept to the last
// observation leaves emptied false.
func DNSEmptied(obs []DNSObservation, switched time.Time) (kept int, lastKept, emptiedAt time.Time, emptied bool) {
	base := 0
	for _, o := range obs {
		if o.At.Before(switched) {
			base = o.Failures
		}
	}
	for _, o := range obs {
		if o.At.Before(switched) {
			continue
		}
		if len(o.Endpoints) == 0 {
			return kept, lastKept, o.At, true
		}
		kept = max(kept, o.Failures-base)
		lastKept = o.At
	}
	return kept, lastKept, time.Time{}, false
}

// DNSBack returns the first observation at or after from that shows the
// Endpoint set want and a fresh Source.
func DNSBack(obs []DNSObservation, want []string, from time.Time) (time.Time, bool) {
	for _, o := range obs {
		if !o.At.Before(from) && !o.Stale && slices.Equal(o.Endpoints, want) {
			return o.At, true
		}
	}
	return time.Time{}, false
}

// DNSRefreshesWithin returns the longest a discovery Source takes, on its
// Node's clock, to make n refreshes in a row that fail: first, the most
// its next refresh may be due in, then after each failure the ceiling of
// its full-jitter backoff, min(BackoffMax, BackoffMin × 2^k) for the k-th
// failure in a row, streak failures having come before the first
// (docs/architecture/09-traffic-management-and-resilience.md "Service
// discovery": failures retry with full-jitter backoff, 1 s to 60 s).
func DNSRefreshesWithin(first time.Duration, streak, n int) time.Duration {
	total := first
	for k := streak + 1; k < streak+n; k++ {
		ceiling := discovery.BackoffMax
		if k < 6 {
			ceiling = min(discovery.BackoffMax, discovery.BackoffMin<<max(k, 1))
		}
		total += ceiling
	}
	return total
}
