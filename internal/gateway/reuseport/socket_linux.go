// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package reuseport

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	sharedPorts = true
	steering    = true
)

// somaxconnPath holds the kernel's listen backlog ceiling, which net.Listen
// also passes to listen(2).
const somaxconnPath = "/proc/sys/net/core/somaxconn"

// Socket is a TCP socket bound with SO_REUSEPORT that does not listen
// yet: it holds its port but is not in the port's reuseport group, so it
// receives no connection until Listen.
type Socket struct {
	mu   sync.Mutex
	fd   int // -1 once Listen or Close took it
	addr *net.TCPAddr
}

// Bind binds a TCP socket to addr with SO_REUSEADDR and SO_REUSEPORT
// without listening. An empty host binds every interface: an IPv6 socket
// that also accepts IPv4, or an IPv4 socket when the kernel has no IPv6,
// as net.Listen does, so the sockets of two processes join one group.
func Bind(ctx context.Context, addr string) (*Socket, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	a, err := parseAddr(addr)
	if err != nil {
		return nil, err
	}
	fd, err := bindFD(a)
	if err != nil {
		return nil, &net.OpError{Op: "listen", Net: "tcp", Addr: tcpAddrOf(a), Err: err}
	}
	sa, err := unix.Getsockname(fd)
	if err != nil {
		_ = unix.Close(fd)
		return nil, &net.OpError{Op: "listen", Net: "tcp", Addr: tcpAddrOf(a), Err: os.NewSyscallError("getsockname", err)}
	}
	return &Socket{fd: fd, addr: sockaddrTCP(sa)}, nil
}

// bindFD creates, configures and binds the socket of a.
func bindFD(a bindAddr) (int, error) {
	family := unix.AF_INET6
	if a.ip.Is4() {
		family = unix.AF_INET
	}
	fd, err := unix.Socket(family, unix.SOCK_STREAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, unix.IPPROTO_TCP)
	if err != nil && !a.ip.IsValid() && (errors.Is(err, unix.EAFNOSUPPORT) || errors.Is(err, unix.EPROTONOSUPPORT)) {
		family = unix.AF_INET
		fd, err = unix.Socket(family, unix.SOCK_STREAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, unix.IPPROTO_TCP)
	}
	if err != nil {
		return -1, os.NewSyscallError("socket", err)
	}
	if err := configure(fd, family); err != nil {
		_ = unix.Close(fd)
		return -1, err
	}
	if err := unix.Bind(fd, sockaddr(a, family)); err != nil {
		_ = unix.Close(fd)
		return -1, os.NewSyscallError("bind", err)
	}
	return fd, nil
}

// configure sets the options every ruralzd listening socket carries:
// SO_REUSEADDR (as net.Listen sets it), SO_REUSEPORT and, on IPv6, dual
// stack.
func configure(fd, family int) error {
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); err != nil {
		return os.NewSyscallError("setsockopt", err)
	}
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEPORT, 1); err != nil {
		return os.NewSyscallError("setsockopt", err)
	}
	if family == unix.AF_INET6 {
		if err := unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_V6ONLY, 0); err != nil {
			return os.NewSyscallError("setsockopt", err)
		}
	}
	return nil
}

// sockaddr returns the address a binds on a socket of family.
func sockaddr(a bindAddr, family int) unix.Sockaddr {
	if family == unix.AF_INET {
		sa := &unix.SockaddrInet4{Port: a.port}
		if a.ip.IsValid() {
			sa.Addr = a.ip.As4()
		}
		return sa
	}
	sa := &unix.SockaddrInet6{Port: a.port}
	if a.ip.IsValid() {
		sa.Addr = a.ip.As16()
	}
	return sa
}

// tcpAddrOf returns a as a *net.TCPAddr for error messages.
func tcpAddrOf(a bindAddr) *net.TCPAddr {
	ta := &net.TCPAddr{Port: a.port}
	if a.ip.IsValid() {
		ta.IP = a.ip.AsSlice()
	}
	return ta
}

// sockaddrTCP converts a bound socket's address.
func sockaddrTCP(sa unix.Sockaddr) *net.TCPAddr {
	switch v := sa.(type) {
	case *unix.SockaddrInet4:
		return &net.TCPAddr{IP: net.IP(v.Addr[:]).To16(), Port: v.Port}
	case *unix.SockaddrInet6:
		return &net.TCPAddr{IP: net.IP(v.Addr[:]), Port: v.Port}
	default:
		return &net.TCPAddr{}
	}
}

// Addr returns the bound address, with the port the kernel chose for
// port 0.
func (s *Socket) Addr() net.Addr { return s.addr }

// Listen starts listening and returns the listener; the socket joins its
// port's reuseport group at the end. The Socket is spent: Close then does
// nothing, and the listener owns the descriptor.
func (s *Socket) Listen() (*net.TCPListener, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fd < 0 {
		return nil, &net.OpError{Op: "listen", Net: "tcp", Addr: s.addr, Err: net.ErrClosed}
	}
	fd := s.fd
	if err := unix.Listen(fd, backlog()); err != nil {
		return nil, &net.OpError{Op: "listen", Net: "tcp", Addr: s.addr, Err: os.NewSyscallError("listen", err)}
	}
	s.fd = -1
	f := os.NewFile(uintptr(fd), "tcp:"+s.addr.String())
	ln, err := net.FileListener(f)
	cerr := f.Close() // FileListener holds a duplicate
	if err != nil {
		return nil, &net.OpError{Op: "listen", Net: "tcp", Addr: s.addr, Err: err}
	}
	if cerr != nil {
		_ = ln.Close()
		return nil, &net.OpError{Op: "listen", Net: "tcp", Addr: s.addr, Err: cerr}
	}
	tl, ok := ln.(*net.TCPListener)
	if !ok {
		_ = ln.Close()
		return nil, &net.OpError{Op: "listen", Net: "tcp", Addr: s.addr, Err: fmt.Errorf("reuseport: %T is not a TCP listener", ln)}
	}
	return tl, nil
}

// Close releases a socket that never listened.
func (s *Socket) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fd < 0 {
		return nil
	}
	err := unix.Close(s.fd)
	s.fd = -1
	if err != nil {
		return os.NewSyscallError("close", err)
	}
	return nil
}

// backlog returns the kernel's somaxconn, the backlog net.Listen uses.
func backlog() int {
	b, err := os.ReadFile(somaxconnPath)
	if err != nil {
		return unix.SOMAXCONN
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || n <= 0 {
		return unix.SOMAXCONN
	}
	return n
}

// Listen binds addr like Bind and starts listening at once.
func Listen(ctx context.Context, addr string) (*net.TCPListener, error) {
	s, err := Bind(ctx, addr)
	if err != nil {
		return nil, err
	}
	ln, err := s.Listen()
	if err != nil {
		_ = s.Close()
		return nil, err
	}
	return ln, nil
}

// Steer attaches to the reuseport group of ln, a listening socket, a
// classic BPF program that returns index (SO_ATTACH_REUSEPORT_CBPF), so
// every new connection of the group goes to the socket at that position;
// a position past the end falls back to the kernel's hash. The program
// replaces the group's previous one and outlives ln while another socket
// remains in the group.
func Steer(ln syscall.Conn, index int) error {
	if index < 0 || int64(index) > math.MaxUint32 {
		return fmt.Errorf("reuseport: steer: position %d out of range", index)
	}
	rc, err := ln.SyscallConn()
	if err != nil {
		return fmt.Errorf("reuseport: steer: %w", err)
	}
	// One instruction: return the constant position.
	prog := []unix.SockFilter{{Code: unix.BPF_RET | unix.BPF_K, K: uint32(index)}}
	fprog := &unix.SockFprog{Len: 1, Filter: &prog[0]}
	var serr error
	if err := rc.Control(func(fd uintptr) {
		serr = unix.SetsockoptSockFprog(int(fd), unix.SOL_SOCKET, unix.SO_ATTACH_REUSEPORT_CBPF, fprog)
	}); err != nil {
		return fmt.Errorf("reuseport: steer: %w", err)
	}
	if serr != nil {
		return fmt.Errorf("reuseport: steer: %w", os.NewSyscallError("setsockopt", serr))
	}
	return nil
}
