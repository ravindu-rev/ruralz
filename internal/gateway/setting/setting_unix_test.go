// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package setting

import (
	"io/fs"
	"strings"
	"syscall"
	"testing"
)

// statInfo is a FileInfo whose Sys returns a chosen value.
type statInfo struct {
	fs.FileInfo
	sys any
}

func (s statInfo) Sys() any { return s.sys }

// TestCheckOwner covers the owner rule of the MAC key file (R-49): the
// effective user or root may own it; any other owner could rewrite it.
func TestCheckOwner(t *testing.T) {
	const euid = 1000
	cases := []struct {
		name string
		sys  any
		msg  string // "" when accepted
	}{
		{name: "effective user", sys: &syscall.Stat_t{Uid: euid}},
		{name: "root", sys: &syscall.Stat_t{Uid: 0}},
		{name: "another user", sys: &syscall.Stat_t{Uid: euid + 1}, msg: "owned by uid 1001, not by the effective user (uid 1000) or root"},
		{name: "nobody", sys: &syscall.Stat_t{Uid: 65534}, msg: "uid 65534"},
		{name: "no stat", sys: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkOwner(statInfo{sys: tc.sys}, euid)
			if tc.msg == "" {
				if err != nil {
					t.Fatalf("checkOwner = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.msg) {
				t.Fatalf("checkOwner = %v, want %q", err, tc.msg)
			}
		})
	}
	// The effective user as root: only root-owned files and its own.
	if err := checkOwner(statInfo{sys: &syscall.Stat_t{Uid: 7}}, 0); err == nil {
		t.Error("checkOwner accepted uid 7 for euid 0")
	}
}
