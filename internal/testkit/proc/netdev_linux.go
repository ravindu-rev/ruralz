// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package proc

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"syscall"

	"golang.org/x/sys/unix"
)

// PeerSuffix names the other end of the veth pair AddDevice creates where
// the kernel has no dummy driver: the device name followed by PeerSuffix.
const PeerSuffix = "-peer"

// vethInfoPeer is VETH_INFO_PEER (linux/veth.h).
const vethInfoPeer = 1

// AddDevice gives the caller's network namespace the IPv4 address of
// prefix, with its prefix length, on an up device named name: a dummy
// device, or, where the kernel has no dummy driver, one end of a veth pair
// whose other end, name+PeerSuffix, is up too. It is meant for an InNetNS
// child, so a test gets a local address that is not loopback, such as a
// peer the admin port must not trust, without touching the host's
// interfaces. It needs CAP_NET_ADMIN over the namespace: root, or the user
// namespace of NetNSOptions.User. name is at most 10 bytes, so the veth
// peer's name fits IFNAMSIZ.
func AddDevice(name string, prefix netip.Prefix) error {
	return addDevice(name, prefix, "dummy", "veth")
}

// addDevice is AddDevice trying the device kinds in order, moving to the
// next only while the kernel has no driver for one (EOPNOTSUPP).
func addDevice(name string, prefix netip.Prefix, kinds ...string) error {
	if err := checkDevice(name, prefix); err != nil {
		return err
	}
	err := fmt.Errorf("no device kind: %w", unix.EOPNOTSUPP)
	for _, kind := range kinds {
		if err = newDevice(name, kind); !errors.Is(err, unix.EOPNOTSUPP) {
			break
		}
	}
	if err == nil {
		err = linkUp(name)
	}
	if err == nil {
		err = addAddress(name, prefix)
	}
	if err != nil {
		return fmt.Errorf("proc: AddDevice %s: %w", name, err)
	}
	return nil
}

// checkDevice refuses what AddDevice cannot configure, before any netlink
// request: a prefix that is not IPv4, and a device name that is empty or
// leaves no room for PeerSuffix within IFNAMSIZ.
func checkDevice(name string, prefix netip.Prefix) error {
	if !prefix.Addr().Is4() {
		return fmt.Errorf("proc: AddDevice %s: an IPv4 prefix is required", prefix)
	}
	if name == "" || len(name)+len(PeerSuffix) >= unix.IFNAMSIZ {
		return fmt.Errorf("proc: AddDevice: device name %q is empty or longer than %d bytes", name, unix.IFNAMSIZ-1-len(PeerSuffix))
	}
	return nil
}

// newDevice creates the link name of kind; for a veth pair it creates the
// peer too and sets the peer up.
func newDevice(name, kind string) error {
	if kind != "veth" {
		return newLink(name, kind, nil)
	}
	peer := name + PeerSuffix
	info := append(ifInfo(0, 0, 0), rtAttr(unix.IFLA_IFNAME, append([]byte(peer), 0))...)
	if err := newLink(name, kind, rtAttr(unix.IFLA_INFO_DATA, rtAttr(vethInfoPeer, info))); err != nil {
		return err
	}
	return linkUp(peer)
}

// rtAttr encodes one route attribute, padded to RTA_ALIGNTO.
func rtAttr(typ uint16, data []byte) []byte {
	l := unix.SizeofRtAttr + len(data)
	b := make([]byte, (l+unix.RTA_ALIGNTO-1)&^(unix.RTA_ALIGNTO-1))
	binary.NativeEndian.PutUint16(b[0:2], uint16(l)) //nolint:gosec // G115: an attribute of a few bytes.
	binary.NativeEndian.PutUint16(b[2:4], typ)
	copy(b[unix.SizeofRtAttr:], data)
	return b
}

// ifInfo encodes a struct ifinfomsg.
func ifInfo(index int32, flags, change uint32) []byte {
	b := make([]byte, unix.SizeofIfInfomsg)
	b[0] = unix.AF_UNSPEC
	binary.NativeEndian.PutUint32(b[4:8], uint32(index)) //nolint:gosec // G115: an interface index.
	binary.NativeEndian.PutUint32(b[8:12], flags)
	binary.NativeEndian.PutUint32(b[12:16], change)
	return b
}

// newLink creates the link name of kind with the extra IFLA_INFO_* data.
func newLink(name, kind string, data []byte) error {
	info := append(rtAttr(unix.IFLA_INFO_KIND, []byte(kind)), data...)
	msg := append(ifInfo(0, 0, 0), rtAttr(unix.IFLA_IFNAME, append([]byte(name), 0))...)
	msg = append(msg, rtAttr(unix.IFLA_LINKINFO, info)...)
	return netlinkDo(unix.RTM_NEWLINK, unix.NLM_F_CREATE|unix.NLM_F_EXCL, msg)
}

// linkUp sets IFF_UP on the link name.
func linkUp(name string) error {
	ifc, err := net.InterfaceByName(name)
	if err != nil {
		return err
	}
	return netlinkDo(unix.RTM_NEWLINK, 0, ifInfo(int32(ifc.Index), unix.IFF_UP, unix.IFF_UP)) //nolint:gosec // G115: an interface index.
}

// addAddress adds the IPv4 address of p, with its prefix length, to the
// link name.
func addAddress(name string, p netip.Prefix) error {
	ifc, err := net.InterfaceByName(name)
	if err != nil {
		return err
	}
	ip := p.Addr().As4()
	msg := make([]byte, unix.SizeofIfAddrmsg)
	msg[0] = unix.AF_INET
	msg[1] = byte(p.Bits())                                    //nolint:gosec // G115: an IPv4 prefix length is at most 32.
	binary.NativeEndian.PutUint32(msg[4:8], uint32(ifc.Index)) //nolint:gosec // G115: an interface index.
	msg = append(msg, rtAttr(unix.IFA_LOCAL, ip[:])...)
	msg = append(msg, rtAttr(unix.IFA_ADDRESS, ip[:])...)
	return netlinkDo(unix.RTM_NEWADDR, unix.NLM_F_CREATE|unix.NLM_F_EXCL, msg)
}

// netlinkDo sends one rtnetlink request and returns its acknowledgment's
// error.
func netlinkDo(typ, flags uint16, payload []byte) error {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_ROUTE)
	if err != nil {
		return fmt.Errorf("netlink socket: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return fmt.Errorf("netlink bind: %w", err)
	}
	msg := make([]byte, unix.NLMSG_HDRLEN, unix.NLMSG_HDRLEN+len(payload))
	msg = append(msg, payload...)
	binary.NativeEndian.PutUint32(msg[0:4], uint32(len(msg))) //nolint:gosec // G115: a message of a few hundred bytes.
	binary.NativeEndian.PutUint16(msg[4:6], typ)
	binary.NativeEndian.PutUint16(msg[6:8], flags|unix.NLM_F_REQUEST|unix.NLM_F_ACK)
	binary.NativeEndian.PutUint32(msg[8:12], 1)
	if err := unix.Sendto(fd, msg, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return fmt.Errorf("netlink send: %w", err)
	}
	buf := make([]byte, 1<<16)
	n, _, err := unix.Recvfrom(fd, buf, 0)
	if err != nil {
		return fmt.Errorf("netlink receive: %w", err)
	}
	msgs, err := syscall.ParseNetlinkMessage(buf[:n])
	if err != nil {
		return fmt.Errorf("netlink reply: %w", err)
	}
	for _, m := range msgs {
		if m.Header.Type == unix.NLMSG_ERROR && len(m.Data) >= 4 {
			// The acknowledgment carries the negated errno as a signed 32-bit
			// integer, 0 for success; unsigned negation recovers the errno.
			if code := binary.NativeEndian.Uint32(m.Data[0:4]); code != 0 {
				return syscall.Errno(-code)
			}
			return nil
		}
	}
	return errors.New("netlink: no acknowledgment")
}
