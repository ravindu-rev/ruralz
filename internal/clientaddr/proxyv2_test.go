// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package clientaddr

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net/netip"
	"os"
	"strings"
	"testing"
	"testing/iotest"
)

// v2 builds a PROXY v2 header with a raw version/command byte, family byte
// and variable part.
func v2(verCmd, fam byte, variable []byte) []byte {
	b := append([]byte(Signature), verCmd, fam, 0, 0)
	binary.BigEndian.PutUint16(b[14:16], uint16(len(variable))) //nolint:gosec // G115: test inputs stay below 64 KiB
	return append(b, variable...)
}

// v2Len builds a header whose length field is n regardless of the bytes
// that follow.
func v2Len(verCmd, fam byte, n uint16, rest []byte) []byte {
	b := append([]byte(Signature), verCmd, fam, 0, 0)
	binary.BigEndian.PutUint16(b[14:16], n)
	return append(b, rest...)
}

// tcp4Block is a TCP4 address block followed by tlvs.
func tcp4Block(src, dst netip.AddrPort, tlvs []byte) []byte {
	s, d := src.Addr().As4(), dst.Addr().As4()
	b := append(append([]byte{}, s[:]...), d[:]...)
	b = binary.BigEndian.AppendUint16(b, src.Port())
	b = binary.BigEndian.AppendUint16(b, dst.Port())
	return append(b, tlvs...)
}

// tcp6Block is a TCP6 address block followed by tlvs.
func tcp6Block(src, dst netip.AddrPort, tlvs []byte) []byte {
	s, d := src.Addr().As16(), dst.Addr().As16()
	b := append(append([]byte{}, s[:]...), d[:]...)
	b = binary.BigEndian.AppendUint16(b, src.Port())
	b = binary.BigEndian.AppendUint16(b, dst.Port())
	return append(b, tlvs...)
}

// proxyTCP4 is a complete PROXY TCP4 header.
func proxyTCP4(src, dst string, tlvs []byte) []byte {
	return v2(0x21, 0x11, tcp4Block(netip.MustParseAddrPort(src), netip.MustParseAddrPort(dst), tlvs))
}

// proxyTCP6 is a complete PROXY TCP6 header.
func proxyTCP6(src, dst string, tlvs []byte) []byte {
	return v2(0x21, 0x21, tcp6Block(netip.MustParseAddrPort(src), netip.MustParseAddrPort(dst), tlvs))
}

// local is a LOCAL AF_UNSPEC header with no address block.
func local() []byte { return v2(0x20, 0x00, nil) }

// tlv encodes one TLV.
func tlv(typ byte, v []byte) []byte {
	b := []byte{typ, 0, 0}
	binary.BigEndian.PutUint16(b[1:3], uint16(len(v))) //nolint:gosec // G115: test inputs stay below 64 KiB
	return append(b, v...)
}

// TestReadHeader covers 04 req 16 and 06 req 61 (test plans 04 item 10 and
// 06 "PROXY v2": TCP4, TCP6, LOCAL, bad signature, v1 text, truncated,
// oversized TLVs).
func TestReadHeader(t *testing.T) {
	tlvs := append(tlv(0x01, []byte("h2")), tlv(0x04, bytes.Repeat([]byte{0}, 40))...) // ALPN, NOOP
	tests := []struct {
		name    string
		in      []byte
		want    Header
		wantErr error
	}{
		{
			name: "req16 TCP4 PROXY",
			in:   proxyTCP4("203.0.113.7:51000", "192.0.2.1:443", nil),
			want: Header{
				Command: CommandProxy, Family: FamilyTCP4, Len: 28,
				Source:      netip.MustParseAddrPort("203.0.113.7:51000"),
				Destination: netip.MustParseAddrPort("192.0.2.1:443"),
			},
		},
		{
			name: "req16 TCP6 PROXY",
			in:   proxyTCP6("[2001:db8::7]:4711", "[2001:db8::1]:8443", nil),
			want: Header{
				Command: CommandProxy, Family: FamilyTCP6, Len: 52,
				Source:      netip.MustParseAddrPort("[2001:db8::7]:4711"),
				Destination: netip.MustParseAddrPort("[2001:db8::1]:8443"),
			},
		},
		{
			name: "req16 TCP6 with an IPv4-mapped source kept as sent",
			in:   proxyTCP6("[::ffff:198.51.100.9]:1", "[::1]:2", nil),
			want: Header{
				Command: CommandProxy, Family: FamilyTCP6, Len: 52,
				Source:      netip.MustParseAddrPort("[::ffff:198.51.100.9]:1"),
				Destination: netip.MustParseAddrPort("[::1]:2"),
			},
		},
		{
			name: "req61 TLVs skipped",
			in:   proxyTCP4("203.0.113.7:1", "192.0.2.1:2", tlvs),
			want: Header{
				Command: CommandProxy, Family: FamilyTCP4, Len: 16 + 12 + len(tlvs),
				Source:      netip.MustParseAddrPort("203.0.113.7:1"),
				Destination: netip.MustParseAddrPort("192.0.2.1:2"),
			},
		},
		{
			name: "req16 variable part at the 4096 cap",
			in:   proxyTCP4("203.0.113.7:1", "192.0.2.1:2", tlv(0x04, make([]byte, MaxVariableLen-12-3))),
			want: Header{
				Command: CommandProxy, Family: FamilyTCP4, Len: MaxHeaderLen,
				Source:      netip.MustParseAddrPort("203.0.113.7:1"),
				Destination: netip.MustParseAddrPort("192.0.2.1:2"),
			},
		},
		{
			name: "req16 LOCAL with AF_UNSPEC",
			in:   local(),
			want: Header{Command: CommandLocal, Family: FamilyUnspec, Len: 16},
		},
		{
			name: "req16 LOCAL discards an address block",
			in:   v2(0x20, 0x11, tcp4Block(netip.MustParseAddrPort("1.2.3.4:5"), netip.MustParseAddrPort("5.6.7.8:9"), nil)),
			want: Header{Command: CommandLocal, Family: FamilyTCP4, Len: 28},
		},
		{
			name: "req16 LOCAL with AF_UNSPEC and TLVs",
			in:   v2(0x20, 0x00, tlvs),
			want: Header{Command: CommandLocal, Family: FamilyUnspec, Len: 16 + len(tlvs)},
		},
		{name: "req16 bad signature", in: append([]byte("\r\n\r\n\x00\r\nQUIX\n"), 0x21, 0x11, 0, 12), wantErr: ErrSignature},
		{name: "req16 v1 text header", in: []byte("PROXY TCP4 203.0.113.7 192.0.2.1 51000 443\r\n"), wantErr: ErrSignature},
		{name: "req16 plain HTTP request", in: []byte("GET / HTTP/1.1\r\nHost: a\r\n\r\n"), wantErr: ErrSignature},
		{name: "req16 TLS ClientHello", in: []byte{0x16, 0x03, 0x01, 0x02, 0x00}, wantErr: ErrSignature},
		{name: "req16 version 1 nibble", in: v2(0x11, 0x11, make([]byte, 12)), wantErr: ErrVersion},
		{name: "req16 version 3 nibble", in: v2(0x31, 0x11, make([]byte, 12)), wantErr: ErrVersion},
		{name: "req16 command 2", in: v2(0x22, 0x11, make([]byte, 12)), wantErr: ErrCommand},
		{name: "req16 command 15", in: v2(0x2F, 0x11, make([]byte, 12)), wantErr: ErrCommand},
		{name: "req16 UDP4 family", in: v2(0x21, 0x12, make([]byte, 12)), wantErr: ErrFamily},
		{name: "req16 UDP6 family", in: v2(0x21, 0x22, make([]byte, 36)), wantErr: ErrFamily},
		{name: "req16 UNIX stream family", in: v2(0x21, 0x31, make([]byte, 216)), wantErr: ErrFamily},
		{name: "req16 UNIX datagram family", in: v2(0x21, 0x32, make([]byte, 216)), wantErr: ErrFamily},
		// 04 req 16 refuses UDP and UNIX families whatever the command,
		// stricter than the PROXY protocol text for LOCAL (deliberate).
		{name: "req16 UDP family with LOCAL", in: v2(0x20, 0x12, nil), wantErr: ErrFamily},
		{name: "req16 UNIX family with LOCAL", in: v2(0x20, 0x31, make([]byte, 216)), wantErr: ErrFamily},
		{name: "req16 unknown family", in: v2(0x21, 0x41, make([]byte, 12)), wantErr: ErrFamily},
		{name: "req16 AF_INET with UNSPEC transport", in: v2(0x21, 0x10, make([]byte, 12)), wantErr: ErrFamily},
		{name: "req16 AF_UNSPEC only with LOCAL", in: v2(0x21, 0x00, nil), wantErr: ErrFamily},
		{name: "req16 length above the cap", in: v2Len(0x21, 0x11, MaxVariableLen+1, make([]byte, 12)), wantErr: ErrLength},
		{name: "req16 length 0xffff", in: v2Len(0x20, 0x00, 0xFFFF, nil), wantErr: ErrLength},
		{name: "req16 TCP4 block too short", in: v2(0x21, 0x11, make([]byte, 11)), wantErr: ErrLength},
		{name: "req16 TCP6 block too short", in: v2(0x21, 0x21, make([]byte, 35)), wantErr: ErrLength},
		{name: "req16 empty stream", in: nil, wantErr: io.EOF},
		{name: "req16 truncated TLVs", in: v2Len(0x21, 0x11, 100, make([]byte, 50)), wantErr: io.ErrUnexpectedEOF},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ReadHeader(bytes.NewReader(tt.in))
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) || !errors.Is(err, ErrMalformed) {
					t.Fatalf("ReadHeader = %+v, %v; want %v wrapping ErrMalformed", got, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ReadHeader: %v", err)
			}
			if got != tt.want {
				t.Errorf("ReadHeader = %+v, want %+v", got, tt.want)
			}
			if got.Len != len(tt.in) {
				t.Errorf("Len = %d, input is %d bytes", got.Len, len(tt.in))
			}
		})
	}
}

// TestReadHeaderReadsExactlyTheHeader: the next byte after the header is
// the request line or ClientHello (04 test plan item 10), both with full
// reads and one byte at a time.
func TestReadHeaderReadsExactlyTheHeader(t *testing.T) {
	const next = "GET / HTTP/1.1\r\n"
	for _, hdr := range [][]byte{
		proxyTCP4("203.0.113.7:1", "192.0.2.1:2", tlv(0x04, make([]byte, 700))),
		proxyTCP6("[2001:db8::7]:1", "[2001:db8::1]:2", nil),
		local(),
		v2(0x20, 0x00, make([]byte, 1500)),
	} {
		for name, wrap := range map[string]func(io.Reader) io.Reader{
			"whole":    func(r io.Reader) io.Reader { return r },
			"one byte": iotest.OneByteReader,
			"half":     iotest.HalfReader,
		} {
			r := bytes.NewReader(append(append([]byte{}, hdr...), next...))
			h, err := ReadHeader(wrap(r))
			if err != nil {
				t.Fatalf("%s: ReadHeader: %v", name, err)
			}
			rest, _ := io.ReadAll(r)
			if string(rest) != next || h.Len != len(hdr) {
				t.Errorf("%s: rest %q, Len %d; want %q, %d", name, rest, h.Len, next, len(hdr))
			}
		}
	}
}

// TestReadHeaderTruncatedAtEveryOffset: every strict prefix of a valid
// header is refused as truncated (04 test plan item 10).
func TestReadHeaderTruncatedAtEveryOffset(t *testing.T) {
	full := proxyTCP6("[2001:db8::7]:1", "[2001:db8::1]:2", tlv(0x02, []byte("authority.example")))
	for n := range len(full) {
		_, err := ReadHeader(bytes.NewReader(full[:n]))
		if !errors.Is(err, ErrTruncated) {
			t.Fatalf("prefix %d: err = %v, want ErrTruncated", n, err)
		}
		wantEOF := io.ErrUnexpectedEOF
		if n == 0 {
			wantEOF = io.EOF
		}
		if !errors.Is(err, wantEOF) {
			t.Errorf("prefix %d: err = %v, want it to wrap %v", n, err, wantEOF)
		}
	}
}

// chunkReader returns its chunks one per Read, then fails with after.
type chunkReader struct {
	chunks [][]byte
	after  error
	reads  int
}

func (c *chunkReader) Read(b []byte) (int, error) {
	c.reads++
	if len(c.chunks) == 0 {
		return 0, c.after
	}
	n := copy(b, c.chunks[0])
	c.chunks[0] = c.chunks[0][n:]
	if len(c.chunks[0]) == 0 {
		c.chunks = c.chunks[1:]
	}
	return n, nil
}

// TestReadHeaderRejectsEarly: the first byte off the signature is refused
// without waiting for the rest of the fixed part (a client speaking HTTP or
// TLS gets closed at once instead of after the header timeout).
func TestReadHeaderRejectsEarly(t *testing.T) {
	for _, first := range []string{"G", "P", "\x16", "\r\n\r\n\x00\r\nX"} {
		r := &chunkReader{chunks: [][]byte{[]byte(first)}, after: errors.New("read after rejection")}
		_, err := ReadHeader(r)
		if !errors.Is(err, ErrSignature) {
			t.Errorf("%q: err = %v, want ErrSignature", first, err)
		}
		if r.reads != 1 {
			t.Errorf("%q: %d reads, want 1", first, r.reads)
		}
	}
}

// timeoutErr mimics os.ErrDeadlineExceeded behind a net.Error.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

// TestReadHeaderReadErrors classifies deadline expiry, other read errors
// and readers that make no progress.
func TestReadHeaderReadErrors(t *testing.T) {
	boom := errors.New("connection reset")
	full := proxyTCP4("203.0.113.7:1", "192.0.2.1:2", tlv(0x04, make([]byte, 10)))
	tests := []struct {
		name string
		r    io.Reader
		want []error
	}{
		{"deadline in the fixed part", &chunkReader{chunks: [][]byte{full[:5]}, after: os.ErrDeadlineExceeded}, []error{ErrHeaderTimeout, os.ErrDeadlineExceeded}},
		{"deadline before any byte", &chunkReader{after: timeoutErr{}}, []error{ErrHeaderTimeout}},
		{"deadline in the address block", &chunkReader{chunks: [][]byte{full[:20]}, after: os.ErrDeadlineExceeded}, []error{ErrHeaderTimeout}},
		{"deadline in the TLVs", &chunkReader{chunks: [][]byte{full[:30]}, after: os.ErrDeadlineExceeded}, []error{ErrHeaderTimeout}},
		{"reset in the fixed part", &chunkReader{chunks: [][]byte{full[:3]}, after: boom}, []error{ErrTruncated, boom}},
		{"reset in the TLVs", &chunkReader{chunks: [][]byte{full[:30]}, after: boom}, []error{ErrTruncated, boom}},
		{"no progress in the fixed part", &chunkReader{after: nil}, []error{ErrTruncated, io.ErrNoProgress}},
		{"no progress in the address block", &chunkReader{chunks: [][]byte{full[:16]}, after: nil}, []error{ErrTruncated, io.ErrNoProgress}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ReadHeader(tt.r)
			for _, w := range append(tt.want, ErrMalformed) {
				if !errors.Is(err, w) {
					t.Errorf("err = %v, want it to wrap %v", err, w)
				}
			}
		})
	}
}

// TestReadHeaderDataWithError: a Read returning the last bytes together
// with io.EOF completes the header.
func TestReadHeaderDataWithError(t *testing.T) {
	full := proxyTCP4("203.0.113.7:1", "192.0.2.1:2", nil)
	h, err := ReadHeader(iotest.DataErrReader(bytes.NewReader(full)))
	if err != nil || h.Source != netip.MustParseAddrPort("203.0.113.7:1") {
		t.Fatalf("ReadHeader = %+v, %v", h, err)
	}
}

func TestCommandAndFamilyStrings(t *testing.T) {
	for v, want := range map[string]string{
		CommandLocal.String(): "LOCAL", CommandProxy.String(): "PROXY", Command(7).String(): "Command(7)",
		FamilyUnspec.String(): "UNSPEC", FamilyTCP4.String(): "TCP4", FamilyTCP6.String(): "TCP6",
		Family(0x31).String(): "Family(0x31)",
	} {
		if v != want {
			t.Errorf("String() = %q, want %q", v, want)
		}
	}
	if !strings.HasPrefix(ErrSignature.Error(), ErrMalformed.Error()) {
		t.Errorf("ErrSignature = %q", ErrSignature)
	}
}
