// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package reuseport

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
)

// ErrAddress reports an address that is not "host:port" with an IP
// literal or empty host and a port from 0 to 65535.
var ErrAddress = errors.New("reuseport: address is not an IP literal or empty host and a port")

// SharedPorts reports whether this platform's sockets set SO_REUSEPORT, so
// a second socket of the same user binds a port a listening socket holds.
func SharedPorts() bool { return sharedPorts }

// Steering reports whether Bind and Steer work on this platform (Linux).
func Steering() bool { return steering }

// bindAddr is a parsed bind address.
type bindAddr struct {
	// ip is the address to bind; invalid means every interface.
	ip   netip.Addr
	port int
}

// parseAddr parses "host:port" where host is empty or an IP literal
// without zone; IPv4-mapped IPv6 addresses bind as IPv4.
func parseAddr(addr string) (bindAddr, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return bindAddr{}, fmt.Errorf("%w: %q: %w", ErrAddress, addr, err)
	}
	p, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return bindAddr{}, fmt.Errorf("%w: %q: port %q", ErrAddress, addr, port)
	}
	a := bindAddr{port: int(p)}
	if host == "" {
		return a, nil
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || ip.Zone() != "" {
		return bindAddr{}, fmt.Errorf("%w: %q: host %q", ErrAddress, addr, host)
	}
	a.ip = ip.Unmap()
	return a, nil
}
