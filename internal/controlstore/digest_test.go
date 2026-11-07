// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package controlstore

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestDigestText parses only sha256:<64 lowercase hex>.
func TestDigestText(t *testing.T) {
	d := Sum([]byte("content"))
	got, err := ParseDigest(d.String())
	if err != nil || got != d {
		t.Fatalf("ParseDigest(%s) = %v, %v", d, got, err)
	}
	b, _ := d.MarshalText()
	var back Digest
	if err := back.UnmarshalText(b); err != nil || back != d {
		t.Errorf("text round trip = %v, %v", back, err)
	}
	for _, bad := range []string{"", "sha256:", strings.ToUpper(d.String()), "md5:" + d.String()[7:], d.String()[:70], d.String() + "0", "sha256:" + strings.Repeat("g", 64)} {
		if _, err := ParseDigest(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("ParseDigest(%q) = %v", bad, err)
		}
	}
	if !(Digest{}).IsZero() || d.IsZero() {
		t.Error("IsZero")
	}
}

// TestKindText round-trips every kind.
func TestKindText(t *testing.T) {
	for k := KindRevision; k < numKinds; k++ {
		b, err := k.MarshalText()
		if err != nil {
			t.Fatal(err)
		}
		var back Kind
		if err := back.UnmarshalText(b); err != nil || back != k {
			t.Errorf("kind %d round trip = %v, %v", k, back, err)
		}
	}
	if _, err := Kind(0).MarshalText(); err == nil {
		t.Error("kind 0 marshaled")
	}
	var k Kind
	if err := k.UnmarshalText([]byte("tarball")); err == nil {
		t.Error("an unknown kind parsed")
	}
}

// TestObjectValidate checks kinds, format markers and the 4 MiB audit
// segment bound.
func TestObjectValidate(t *testing.T) {
	cases := []struct {
		o  Object
		ok bool
	}{
		{Object{Kind: KindRevision, Format: "ruralz.canonical.v1"}, true},
		{Object{Kind: KindDiff, Format: "ruralz.diff.v1"}, true},
		{Object{Kind: KindAuditSegment, Format: "f", Data: make([]byte, MaxAuditSegmentBytes)}, true},
		{Object{Kind: KindAuditSegment, Format: "f", Data: make([]byte, MaxAuditSegmentBytes+1)}, false},
		{Object{Kind: 0, Format: "f"}, false},
		{Object{Kind: KindPlan}, false},
		{Object{Kind: KindPlan, Format: "has space"}, false},
		{Object{Kind: KindPlan, Format: strings.Repeat("f", 65)}, false},
	}
	for i, tc := range cases {
		if err := tc.o.Validate(); (err == nil) != tc.ok {
			t.Errorf("case %d: Validate = %v, want ok %v", i, err, tc.ok)
		}
	}
	if (Object{Data: []byte("x")}).Digest() != Sum([]byte("x")) {
		t.Error("an object's digest is not the SHA-256 of its bytes")
	}
}

// TestErrorCodes: a detailed error matches its sentinel and carries its
// code; NotLeader names the leader to forward to.
func TestErrorCodes(t *testing.T) {
	cause := errors.New("cause")
	e := Errorf(ErrContentQuorum, "reached 1 of 3: %w", cause)
	if !errors.Is(e, ErrContentQuorum) || errors.Is(e, ErrNoLeader) || !errors.Is(e, cause) {
		t.Errorf("Errorf matching: %v", e)
	}
	if CodeOf(fmt.Errorf("wrapped: %w", e)) != CodeContentQuorum || CodeOf(cause) != "" {
		t.Error("CodeOf")
	}
	if e.Error() != "RZ-CP-013: reached 1 of 3: cause" {
		t.Errorf("Error() = %q", e.Error())
	}
	nl := NotLeader("10.0.0.1:8092")
	if !errors.Is(nl, ErrNoLeader) || nl.Leader() != "10.0.0.1:8092" || nl.Code() != CodeNoLeader {
		t.Errorf("NotLeader = %v", nl)
	}
	if NotLeader("").Error() != "RZ-CP-004: no leader" {
		t.Errorf("NotLeader(\"\") = %q", NotLeader("").Error())
	}
	for _, s := range []*Error{ErrNoLeader, ErrContentQuorum, ErrBackup, ErrRemoveRefused, ErrFinalize} {
		if !strings.HasPrefix(s.Error(), s.Code()+": ") {
			t.Errorf("%v lacks its code", s)
		}
	}
}
