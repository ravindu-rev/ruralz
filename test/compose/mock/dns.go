// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package mock

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// DNS sink limits.
const (
	// MaxDNSQueries bounds the recorded questions; once full, newer ones
	// are counted in DNSLog.Dropped instead.
	MaxDNSQueries = 4096
	// dnsHeaderLen is the fixed header of a DNS message (RFC 1035
	// section 4.1.1).
	dnsHeaderLen = 12
	// dnsMaxMessage is the largest message the sink reads: the 16-bit
	// length of a TCP message bounds it, and EDNS(0) lets UDP queries
	// announce up to that size.
	dnsMaxMessage = 65535
	// dnsMaxName is the longest name in wire form, length octets and the
	// root label included (RFC 1035 section 2.3.4).
	dnsMaxName = 255
	// dnsMaxTCPConns bounds the TCP connections served at once; another
	// is closed at once.
	dnsMaxTCPConns = 64
	// dnsTCPIdle bounds the wait for each message on a TCP connection.
	dnsTCPIdle = 10 * time.Second
)

// Response codes the sink answers with (RFC 1035 section 4.1.1).
const (
	rcodeFormErr  = 1
	rcodeNXDomain = 3
	rcodeNotImp   = 4
)

// DNSQuery is one question the sink received.
type DNSQuery struct {
	// Name is the queried name in presentation form, letters lower-cased,
	// with the trailing dot of the root ("example.com."); a byte other
	// than a letter, digit, '-' or '_' is written \DDD (RFC 4343).
	Name string `json:"name"`
	// Type is the QTYPE mnemonic (A, AAAA, ...) or TYPE<n> (RFC 3597).
	Type string `json:"type"`
	// Transport is "udp" or "tcp".
	Transport string `json:"transport"`
	// Client is the sender's address.
	Client string `json:"client"`
}

// DNSLog is what the sink recorded: the questions in arrival order, the
// questions dropped once MaxDNSQueries were kept, and the messages that
// were no well-formed standard query, which name nothing the sink could
// record and so count as unknown lookups.
type DNSLog struct {
	Queries   []DNSQuery `json:"queries"`
	Dropped   int        `json:"dropped"`
	Malformed int        `json:"malformed"`
}

// dnsSink answers every standard query with NXDOMAIN and records its
// question: as the Node's only resolver it shows every name the Node
// tried to look up. It serves UDP and TCP on one address.
type dnsSink struct {
	udp net.PacketConn
	tcp net.Listener

	mu    sync.Mutex
	log   DNSLog
	conns map[net.Conn]struct{}
	// closed is set when Close closes the open TCP connections: a
	// connection accepted before Close but registered after it is closed
	// at once, else its reader would hold Close until it went idle.
	closed bool
	slots  chan struct{} // bounds the TCP connections served at once

	wg        sync.WaitGroup
	closeOnce sync.Once
}

// dnsPortAttempts bounds the ports tried for port 0, whose UDP port may be
// taken for TCP by another process.
const dnsPortAttempts = 16

// startDNSSink listens on addr for UDP and TCP and serves until Close.
// Port 0 picks one free port for both.
func startDNSSink(ctx context.Context, addr string) (*dnsSink, error) {
	return startDNSSinkWith(ctx, addr, listenBoth)
}

// listenFunc opens the sink's UDP and TCP sockets on one address.
type listenFunc func(ctx context.Context, addr string) (net.PacketConn, net.Listener, error)

// startDNSSinkWith is startDNSSink over listen. For port 0 the UDP port
// the kernel picks may be taken for TCP by another process, so a taken
// port is tried again on a new one, at most dnsPortAttempts times.
func startDNSSinkWith(ctx context.Context, addr string, listen listenFunc) (*dnsSink, error) {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("dns sink: %w", err)
	}
	attempts := 1
	if port == "0" {
		attempts = dnsPortAttempts
	}
	var udp net.PacketConn
	var tcp net.Listener
	for i := range attempts {
		udp, tcp, err = listen(ctx, addr)
		if err == nil || !errors.Is(err, syscall.EADDRINUSE) || i == attempts-1 {
			break
		}
	}
	if err != nil {
		return nil, fmt.Errorf("dns sink: %w", err)
	}
	s := &dnsSink{
		udp:   udp,
		tcp:   tcp,
		conns: map[net.Conn]struct{}{},
		slots: make(chan struct{}, dnsMaxTCPConns),
	}
	s.log.Queries = []DNSQuery{}
	s.wg.Add(2)
	go s.serveUDP()
	go s.serveTCP()
	return s, nil
}

// listenBoth listens for UDP on addr and for TCP on the port UDP got, so a
// client finds both on one address.
func listenBoth(ctx context.Context, addr string) (net.PacketConn, net.Listener, error) {
	var lc net.ListenConfig
	udp, err := lc.ListenPacket(ctx, "udp", addr)
	if err != nil {
		return nil, nil, err
	}
	tcp, err := lc.Listen(ctx, "tcp", udp.LocalAddr().String())
	if err != nil {
		_ = udp.Close()
		return nil, nil, err
	}
	return udp, tcp, nil
}

// Addr returns the address both transports listen on.
func (s *dnsSink) Addr() string { return s.udp.LocalAddr().String() }

// Log returns a copy of what the sink recorded.
func (s *dnsSink) Log() DNSLog {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.log
	out.Queries = append([]DNSQuery{}, s.log.Queries...)
	return out
}

// Close stops both transports, closes every open TCP connection and
// waits for every goroutine of the sink.
func (s *dnsSink) Close() error {
	var err error
	s.closeOnce.Do(func() {
		err = errors.Join(s.udp.Close(), s.tcp.Close())
		s.mu.Lock()
		s.closed = true
		for c := range s.conns {
			_ = c.Close()
		}
		s.mu.Unlock()
		s.wg.Wait()
	})
	return err
}

func (s *dnsSink) serveUDP() {
	defer s.wg.Done()
	buf := make([]byte, dnsMaxMessage)
	for {
		n, from, err := s.udp.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		if resp := s.handle(buf[:n], "udp", from.String()); resp != nil {
			_, _ = s.udp.WriteTo(resp, from)
		}
	}
}

func (s *dnsSink) serveTCP() {
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
		if s.closed {
			s.mu.Unlock()
			<-s.slots
			_ = c.Close()
			continue
		}
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
func (s *dnsSink) serveConn(c net.Conn) {
	var prefix [2]byte
	buf := make([]byte, dnsMaxMessage)
	for {
		if err := c.SetReadDeadline(time.Now().Add(dnsTCPIdle)); err != nil {
			return
		}
		if _, err := io.ReadFull(c, prefix[:]); err != nil {
			return
		}
		n := int(binary.BigEndian.Uint16(prefix[:]))
		if _, err := io.ReadFull(c, buf[:n]); err != nil {
			return
		}
		resp := s.handle(buf[:n], "tcp", c.RemoteAddr().String())
		if resp == nil {
			continue
		}
		out := binary.BigEndian.AppendUint16(make([]byte, 0, 2+len(resp)), uint16(len(resp))) //nolint:gosec // G115: a response is a header and one question, far below 65,535 bytes
		if _, err := c.Write(append(out, resp...)); err != nil {
			return
		}
	}
}

// handle records the question of msg and returns the answer to send, or
// nil for a message that is itself a response.
func (s *dnsSink) handle(msg []byte, transport, client string) []byte {
	q, rcode := parseQuery(msg)
	switch {
	case rcode < 0:
		s.count(func(l *DNSLog) { l.Malformed++ })
		return nil
	case rcode != rcodeNXDomain:
		s.count(func(l *DNSLog) { l.Malformed++ })
	default:
		rec := DNSQuery{Name: q.name, Type: typeName(q.qtype), Transport: transport, Client: client}
		s.count(func(l *DNSLog) {
			if len(l.Queries) >= MaxDNSQueries {
				l.Dropped++
				return
			}
			l.Queries = append(l.Queries, rec)
		})
	}
	return answer(msg, q, rcode)
}

func (s *dnsSink) count(f func(*DNSLog)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(&s.log)
}

// question is the one question of a query: its name in presentation form,
// its QTYPE and the offset just past its QCLASS.
type question struct {
	name  string
	qtype uint16
	end   int
}

// parseQuery reads the header and the question of msg. It returns
// rcodeNXDomain for a well-formed standard query, the code to answer a
// malformed (rcodeFormErr) or non-standard (rcodeNotImp) query with, or
// -1 for a message that is a response or too short to answer, which gets
// no answer at all.
func parseQuery(msg []byte) (question, int) {
	if len(msg) < dnsHeaderLen {
		return question{}, -1
	}
	if msg[2]&0x80 != 0 { // QR: a response, never answered
		return question{}, -1
	}
	if opcode := (msg[2] >> 3) & 0x0F; opcode != 0 {
		return question{}, rcodeNotImp
	}
	if binary.BigEndian.Uint16(msg[4:6]) != 1 { // QDCOUNT
		return question{}, rcodeFormErr
	}
	name, off, err := readName(msg, dnsHeaderLen)
	if err != nil || off+4 > len(msg) {
		return question{}, rcodeFormErr
	}
	return question{name: name, qtype: binary.BigEndian.Uint16(msg[off : off+2]), end: off + 4}, rcodeNXDomain
}

// errName reports a question name the sink cannot read.
var errName = errors.New("malformed question name")

// readName reads the uncompressed name at off and returns it in
// presentation form with the offset after it. A compression pointer or a
// reserved label type is refused: a question's name is the first one in
// the message, so it has nothing earlier to point at.
func readName(msg []byte, off int) (string, int, error) {
	var b strings.Builder
	wire := 1 // the root label's length octet
	for {
		if off >= len(msg) {
			return "", 0, errName
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
			return "", 0, errName
		}
		wire += 1 + n
		if wire > dnsMaxName || off+n > len(msg) {
			return "", 0, errName
		}
		for _, c := range msg[off : off+n] {
			writeLabelByte(&b, c)
		}
		b.WriteByte('.')
		off += n
	}
}

// writeLabelByte writes one byte of a label in presentation form: letters
// lower-cased, digits, '-' and '_' as they are, any other byte as \DDD.
func writeLabelByte(b *strings.Builder, c byte) {
	switch {
	case c >= 'A' && c <= 'Z':
		b.WriteByte(c + 'a' - 'A')
	case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_':
		b.WriteByte(c)
	default:
		_, _ = fmt.Fprintf(b, `\%03d`, c)
	}
}

// answer builds the response to msg: its ID, opcode and RD bit, QR set,
// no recursion available, rcode, and the question echoed when it parsed.
func answer(msg []byte, q question, rcode int) []byte {
	out := make([]byte, dnsHeaderLen, dnsHeaderLen+max(q.end-dnsHeaderLen, 0))
	copy(out[0:2], msg[0:2])
	out[2] = 0x80 | msg[2]&0x78 | msg[2]&0x01 // QR, the opcode, RD
	out[3] = byte(rcode & 0x0f)               // RA and Z clear, the 4-bit RCODE
	if q.end > 0 {
		binary.BigEndian.PutUint16(out[4:6], 1)
		out = append(out, msg[dnsHeaderLen:q.end]...)
	}
	return out
}

// typeName returns the mnemonic of the common QTYPEs, else TYPE<n>.
func typeName(t uint16) string {
	switch t {
	case 1:
		return "A"
	case 2:
		return "NS"
	case 5:
		return "CNAME"
	case 6:
		return "SOA"
	case 12:
		return "PTR"
	case 15:
		return "MX"
	case 16:
		return "TXT"
	case 28:
		return "AAAA"
	case 33:
		return "SRV"
	case 65:
		return "HTTPS"
	case 255:
		return "ANY"
	default:
		return "TYPE" + strconv.Itoa(int(t))
	}
}
