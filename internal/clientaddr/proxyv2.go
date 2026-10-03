// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package clientaddr

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"strconv"
)

// Signature is the 12-byte PROXY protocol v2 signature every header starts
// with (04 req 16, 06 req 61).
const Signature = "\r\n\r\n\x00\r\nQUIT\n"

// MaxVariableLen caps the variable part of a header (address block plus
// TLVs) at 4,096 bytes (04 req 16 and 06 req 61, proposed cap); a larger
// declared length closes the connection before the part is read.
const MaxVariableLen = 4096

// MaxHeaderLen is the longest header ReadHeader accepts: the 16-byte fixed
// part plus MaxVariableLen.
const MaxHeaderLen = fixedLen + MaxVariableLen

// fixedLen is the signature, version and command, family and length.
const fixedLen = 16

// chunkLen is the buffer the address block and discarded TLVs are read
// into; common TLV sets (ALPN, authority, cloud VPC IDs) fit one read.
const chunkLen = 256

// Address block lengths of the TCP4 and TCP6 families.
const (
	tcp4AddrLen = 12
	tcp6AddrLen = 36
)

// Command is the PROXY v2 command (the low nibble of byte 13).
type Command uint8

// Commands.
const (
	// CommandLocal marks a connection the proxy made on its own (health
	// checks): the TCP peer stays the connection peer.
	CommandLocal Command = 0x0
	// CommandProxy carries the original client's addresses.
	CommandProxy Command = 0x1
)

// String returns LOCAL or PROXY.
func (c Command) String() string {
	switch c {
	case CommandLocal:
		return "LOCAL"
	case CommandProxy:
		return "PROXY"
	}
	return "Command(" + strconv.Itoa(int(c)) + ")"
}

// Family is the address family and transport byte (byte 14).
type Family uint8

// Families accepted by ReadHeader with PROXY; UDP and UNIX families are
// refused with PROXY. LOCAL is accepted with any family byte (R-65).
const (
	// FamilyUnspec is AF_UNSPEC, accepted with LOCAL only.
	FamilyUnspec Family = 0x00
	// FamilyTCP4 is AF_INET over STREAM: 12 address bytes.
	FamilyTCP4 Family = 0x11
	// FamilyTCP6 is AF_INET6 over STREAM: 36 address bytes.
	FamilyTCP6 Family = 0x21
)

// String returns UNSPEC, TCP4, TCP6 or the byte in hex.
func (f Family) String() string {
	switch f {
	case FamilyUnspec:
		return "UNSPEC"
	case FamilyTCP4:
		return "TCP4"
	case FamilyTCP6:
		return "TCP6"
	}
	return fmt.Sprintf("Family(0x%02x)", uint8(f))
}

// Header is one parsed PROXY v2 header.
type Header struct {
	// Command is LOCAL or PROXY.
	Command Command
	// Family is the address family and transport byte.
	Family Family
	// Source is the original client address and port for PROXY, as sent
	// (an IPv4-mapped address is not unmapped here); zero for LOCAL.
	Source netip.AddrPort
	// Destination is the address and port the client connected to for
	// PROXY; zero for LOCAL.
	Destination netip.AddrPort
	// Len is the number of bytes the header occupied on the wire.
	Len int
}

// ErrMalformed is wrapped by every error ReadHeader returns for a header
// that is not an acceptable PROXY v2 header, including a truncated or late
// one.
var ErrMalformed = errors.New("clientaddr: malformed PROXY v2 header")

// Specific causes; each wraps ErrMalformed.
var (
	// ErrSignature reports bytes that are not the v2 signature (a v1 text
	// header, a plain HTTP request or a TLS ClientHello included).
	ErrSignature = fmt.Errorf("%w: no v2 signature", ErrMalformed)
	// ErrVersion reports a version nibble other than 2.
	ErrVersion = fmt.Errorf("%w: version is not 2", ErrMalformed)
	// ErrCommand reports a command other than LOCAL or PROXY.
	ErrCommand = fmt.Errorf("%w: command is neither LOCAL nor PROXY", ErrMalformed)
	// ErrFamily reports a PROXY header whose family is not TCP4 or TCP6
	// (UDP, UNIX, AF_UNSPEC or unknown); LOCAL never fails with it.
	ErrFamily = fmt.Errorf("%w: unsupported address family", ErrMalformed)
	// ErrLength reports a variable part above MaxVariableLen or shorter
	// than the family's address block.
	ErrLength = fmt.Errorf("%w: bad length", ErrMalformed)
	// ErrTruncated reports a stream that ended inside the header; it also
	// wraps io.EOF when nothing was received, else io.ErrUnexpectedEOF.
	ErrTruncated = fmt.Errorf("%w: truncated", ErrMalformed)
	// ErrHeaderTimeout reports a header not complete within the header
	// timeout; it also wraps the read error (os.ErrDeadlineExceeded).
	ErrHeaderTimeout = fmt.Errorf("%w: not received in time", ErrMalformed)
)

// maxEmptyReads bounds consecutive (0, nil) reads before ReadHeader gives
// up with io.ErrNoProgress, as bufio does.
const maxEmptyReads = 100

// ReadHeader reads exactly one PROXY v2 header from r and nothing more
// (04 req 16, 06 req 61). It checks the signature byte by byte as bytes
// arrive, so a client that is not speaking PROXY v2 is refused on its first
// bytes, and checks the declared length against MaxVariableLen before
// reading the variable part. TLVs are read and discarded, not parsed.
// Every failure wraps ErrMalformed.
func ReadHeader(r io.Reader) (Header, error) {
	// One buffer (one allocation, since it escapes through r.Read): the
	// fixed part, then the address block, then TLVs in chunks.
	var buf [fixedLen + chunkLen]byte
	fixed, rest := buf[:fixedLen], buf[fixedLen:]
	if err := readSigned(r, fixed); err != nil {
		return Header{}, err
	}
	h := Header{Len: fixedLen}
	if v := fixed[12] >> 4; v != 2 {
		return Header{}, fmt.Errorf("%w (version %d)", ErrVersion, v)
	}
	h.Command = Command(fixed[12] & 0x0F)
	if h.Command != CommandLocal && h.Command != CommandProxy {
		return Header{}, fmt.Errorf("%w (command 0x%x)", ErrCommand, uint8(h.Command))
	}
	h.Family = Family(fixed[13])
	n := int(binary.BigEndian.Uint16(fixed[14:16]))
	if n > MaxVariableLen {
		return Header{}, fmt.Errorf("%w: %d bytes above the %d-byte cap", ErrLength, n, MaxVariableLen)
	}
	need, err := addrLen(h.Command, h.Family)
	if err != nil {
		return Header{}, err
	}
	if n < need {
		return Header{}, fmt.Errorf("%w: %d bytes, %s needs %d", ErrLength, n, h.Family, need)
	}
	addr := rest[:need]
	if err := readFull(r, addr, true); err != nil {
		return Header{}, err
	}
	switch need {
	case tcp4AddrLen:
		h.Source = netip.AddrPortFrom(netip.AddrFrom4([4]byte(addr[0:4])), binary.BigEndian.Uint16(addr[8:10]))
		h.Destination = netip.AddrPortFrom(netip.AddrFrom4([4]byte(addr[4:8])), binary.BigEndian.Uint16(addr[10:12]))
	case tcp6AddrLen:
		h.Source = netip.AddrPortFrom(netip.AddrFrom16([16]byte(addr[0:16])), binary.BigEndian.Uint16(addr[32:34]))
		h.Destination = netip.AddrPortFrom(netip.AddrFrom16([16]byte(addr[16:32])), binary.BigEndian.Uint16(addr[34:36]))
	}
	for skip := n - need; skip > 0; {
		k := min(skip, len(rest))
		if err := readFull(r, rest[:k], true); err != nil {
			return Header{}, err
		}
		skip -= k
	}
	h.Len += n
	return h, nil
}

// addrLen returns the length of the address block ReadHeader parses. LOCAL
// discards its block whatever the family byte, as the PROXY v2 text has
// receivers do (R-65, overriding the literal 04 req 16): the block is
// skipped within MaxVariableLen and the TCP peer stays the peer. PROXY
// needs TCP4 or TCP6; UDP, UNIX, AF_UNSPEC and unknown families close the
// connection.
func addrLen(c Command, f Family) (int, error) {
	if c == CommandLocal {
		return 0, nil
	}
	switch f {
	case FamilyTCP4:
		return tcp4AddrLen, nil
	case FamilyTCP6:
		return tcp6AddrLen, nil
	default:
		return 0, fmt.Errorf("%w (%s with %s)", ErrFamily, f, c)
	}
}

// readSigned fills the fixed part, rejecting the first byte that departs
// from the signature without waiting for the rest.
func readSigned(r io.Reader, b []byte) error {
	got, empty := 0, 0
	for {
		n, err := r.Read(b[got:])
		for i := got; i < got+n && i < len(Signature); i++ {
			if b[i] != Signature[i] {
				return fmt.Errorf("%w (byte %d is 0x%02x)", ErrSignature, i, b[i])
			}
		}
		got += n
		if got == len(b) {
			return nil
		}
		if err != nil {
			return readErr(err, got > 0)
		}
		if empty = nextEmpty(empty, n); empty >= maxEmptyReads {
			return fmt.Errorf("%w: %w", ErrTruncated, io.ErrNoProgress)
		}
	}
}

// readFull fills b; started reports whether header bytes were already read.
func readFull(r io.Reader, b []byte, started bool) error {
	if len(b) == 0 {
		return nil
	}
	got, empty := 0, 0
	for {
		n, err := r.Read(b[got:])
		got += n
		if got == len(b) {
			return nil
		}
		if err != nil {
			return readErr(err, started || got > 0)
		}
		if empty = nextEmpty(empty, n); empty >= maxEmptyReads {
			return fmt.Errorf("%w: %w", ErrTruncated, io.ErrNoProgress)
		}
	}
}

// nextEmpty counts consecutive empty reads.
func nextEmpty(empty, n int) int {
	if n == 0 {
		return empty + 1
	}
	return 0
}

// timeoutError is the net.Error shape of deadline expiry.
type timeoutError interface{ Timeout() bool }

// readErr classifies a read failure inside the header.
func readErr(err error, started bool) error {
	var te timeoutError
	if errors.As(err, &te) && te.Timeout() {
		return fmt.Errorf("%w: %w", ErrHeaderTimeout, err)
	}
	if errors.Is(err, io.EOF) {
		if started {
			return fmt.Errorf("%w: %w", ErrTruncated, io.ErrUnexpectedEOF)
		}
		return fmt.Errorf("%w: %w", ErrTruncated, io.EOF)
	}
	return fmt.Errorf("%w: %w", ErrTruncated, err)
}
