// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package l4lb

import (
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"

	"github.com/ravindu-rev/ruralz/internal/clientaddr"
)

// Address block lengths of the PROXY protocol v2 TCP4 and TCP6 families.
const (
	proxyTCP4Len = 12
	proxyTCP6Len = 36
)

// proxyHeader returns the PROXY protocol v2 header of a connection from
// src to dst, the form internal/clientaddr.ReadHeader reads on a Node's
// proxyProtocol listener: version 2 and command PROXY, then TCP4 when both
// addresses are IPv4 (an IPv4-mapped IPv6 address counts as IPv4), else
// TCP6 with an IPv4 address in its IPv4-mapped form, since a header holds
// one family; no TLVs. Zones are dropped.
func proxyHeader(src, dst netip.AddrPort) []byte {
	sa, da := src.Addr().Unmap(), dst.Addr().Unmap()
	b := make([]byte, 0, len(clientaddr.Signature)+4+proxyTCP6Len)
	b = append(b, clientaddr.Signature...)
	b = append(b, 0x20|byte(clientaddr.CommandProxy))
	if sa.Is4() && da.Is4() {
		b = append(b, byte(clientaddr.FamilyTCP4))
		b = binary.BigEndian.AppendUint16(b, proxyTCP4Len)
		s, d := sa.As4(), da.As4()
		b = append(b, s[:]...)
		b = append(b, d[:]...)
	} else {
		b = append(b, byte(clientaddr.FamilyTCP6))
		b = binary.BigEndian.AppendUint16(b, proxyTCP6Len)
		s, d := sa.As16(), da.As16()
		b = append(b, s[:]...)
		b = append(b, d[:]...)
	}
	b = binary.BigEndian.AppendUint16(b, src.Port())
	return binary.BigEndian.AppendUint16(b, dst.Port())
}

// sendProxyHeader writes the PROXY protocol v2 header of client, the
// accepted connection, to up, the backend connection, as its first bytes.
// On failure it closes up.
func sendProxyHeader(up net.Conn, client *net.TCPConn) error {
	src, ok := client.RemoteAddr().(*net.TCPAddr)
	if !ok {
		_ = up.Close()
		return fmt.Errorf("l4lb: PROXY header: client address %v", client.RemoteAddr())
	}
	dst, ok := client.LocalAddr().(*net.TCPAddr)
	if !ok {
		_ = up.Close()
		return fmt.Errorf("l4lb: PROXY header: balancer address %v", client.LocalAddr())
	}
	// The header fits any socket send buffer, so the write never waits on
	// the backend reading.
	if _, err := up.Write(proxyHeader(src.AddrPort(), dst.AddrPort())); err != nil {
		_ = up.Close()
		return fmt.Errorf("l4lb: PROXY header: %w", err)
	}
	return nil
}
