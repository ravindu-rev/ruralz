// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package signing

import (
	"errors"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/errcode"
)

// Tests for the binding of an OCI Revision's signature: the signed
// annotations ruralz.io/environment and ruralz.io/sequence, the Node's
// Environment, and the high-water mark below which a sequence is refused
// with RZ-CFG-033 (docs/architecture/08-security-and-identity.md "Signed
// Revisions and Plugin artifacts").

func TestRevisionAnnotations(t *testing.T) {
	got := RevisionAnnotations("prod", 42)
	if len(got) != 2 || got["ruralz.io/environment"] != "prod" || got["ruralz.io/sequence"] != "42" {
		t.Fatalf("RevisionAnnotations = %v", got)
	}
}

func TestParseSequence(t *testing.T) {
	for in, want := range map[string]uint64{
		"0": 0, "1": 1, "42": 42, "18446744073709551615": 1<<64 - 1,
	} {
		got, err := ParseSequence(in)
		if err != nil || got != want {
			t.Errorf("ParseSequence(%q) = %d, %v, want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "-1", "+1", "01", "00", " 1", "1 ", "1.0", "0x10", "18446744073709551616", "123456789012345678901", "١"} {
		if got, err := ParseSequence(in); !errors.Is(err, ErrSequenceSyntax) {
			t.Errorf("ParseSequence(%q) = %d, %v, want ErrSequenceSyntax", in, got, err)
		}
	}
}

// signed returns a valid signature with the given annotations.
func signed(signer string, ann map[string]string) Signature {
	return Signature{Signer: signer, Annotations: ann}
}

func TestBindRevision(t *testing.T) {
	prod := func(seq string) map[string]string {
		return map[string]string{AnnotationEnvironment: "prod", AnnotationSequence: seq}
	}
	tests := []struct {
		name    string
		valid   []Signature
		env     string
		mark    uint64
		want    Binding
		wantErr error
	}{
		{
			name: "one signature at the mark", valid: []Signature{signed("ci", prod("7"))}, env: "prod", mark: 7,
			want: Binding{Environment: "prod", Sequence: 7, Signer: "ci"},
		},
		{
			name: "above the mark", valid: []Signature{signed("ci", prod("9"))}, env: "prod", mark: 7,
			want: Binding{Environment: "prod", Sequence: 9, Signer: "ci"},
		},
		{
			name: "no mark yet", valid: []Signature{signed("ci", prod("1"))}, env: "prod",
			want: Binding{Environment: "prod", Sequence: 1, Signer: "ci"},
		},
		{
			name: "highest of several", env: "prod", mark: 5,
			valid: []Signature{signed("old", prod("3")), signed("new", prod("8")), signed("mid", prod("6"))},
			want:  Binding{Environment: "prod", Sequence: 8, Signer: "new"},
		},
		{
			name: "another Environment's signature beside its own", env: "prod",
			valid: []Signature{signed("staging", map[string]string{AnnotationEnvironment: "staging", AnnotationSequence: "99"}), signed("prod", prod("4"))},
			want:  Binding{Environment: "prod", Sequence: 4, Signer: "prod"},
		},
		{name: "below the mark", valid: []Signature{signed("ci", prod("6"))}, env: "prod", mark: 7, wantErr: ErrSequence},
		{name: "highest still below the mark", valid: []Signature{signed("a", prod("2")), signed("b", prod("6"))}, env: "prod", mark: 7, wantErr: ErrSequence},
		{
			name: "another Environment's Revision", env: "prod", wantErr: ErrEnvironment,
			valid: []Signature{signed("ci", map[string]string{AnnotationEnvironment: "staging", AnnotationSequence: "9"})},
		},
		{name: "environment differs in case", valid: []Signature{signed("ci", map[string]string{AnnotationEnvironment: "Prod", AnnotationSequence: "1"})}, env: "prod", wantErr: ErrEnvironment},
		{name: "no valid signature", env: "prod", wantErr: ErrEnvironment},
		{name: "message signature without annotations", valid: []Signature{signed("key", nil)}, env: "prod", wantErr: ErrEnvironment},
		{name: "sequence missing", valid: []Signature{signed("ci", map[string]string{AnnotationEnvironment: "prod"})}, env: "prod", wantErr: ErrEnvironment},
		{name: "environment missing", valid: []Signature{signed("ci", map[string]string{AnnotationSequence: "1"})}, env: "prod", wantErr: ErrEnvironment},
		{name: "malformed sequence", valid: []Signature{signed("ci", prod("07"))}, env: "prod", wantErr: ErrEnvironment},
		{name: "no Environment configured", valid: []Signature{signed("ci", prod("1"))}, env: "", wantErr: ErrEnvironment},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := BindRevision(tt.valid, tt.env, tt.mark)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("BindRevision = %+v, %v, want %v", got, err, tt.wantErr)
				}
				if code, _ := errcode.CodeOf(err); code != "RZ-CFG-033" {
					t.Fatalf("code %q, want RZ-CFG-033", code)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("BindRevision = %+v, %v, want %+v", got, err, tt.want)
			}
		})
	}
}

func TestBindRevisionMessages(t *testing.T) {
	_, err := BindRevision([]Signature{signed("ci", map[string]string{AnnotationEnvironment: "staging", AnnotationSequence: "9"})}, "prod", 0)
	if err == nil || !strings.Contains(err.Error(), `"staging"`) || !strings.Contains(err.Error(), `"prod"`) {
		t.Fatalf("wrong Environment message: %v", err)
	}
	_, err = BindRevision([]Signature{signed("ci", map[string]string{AnnotationEnvironment: "prod", AnnotationSequence: "3"})}, "prod", 5)
	if err == nil || !strings.Contains(err.Error(), "sequence 3") || !strings.Contains(err.Error(), "high-water mark 5") {
		t.Fatalf("older sequence message: %v", err)
	}
}
