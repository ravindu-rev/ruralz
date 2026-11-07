// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package signing

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/ravindu-rev/ruralz/internal/errcode"
)

// The signed annotations of an OCI Revision's signature
// (docs/architecture/08-security-and-identity.md "Signed Revisions and
// Plugin artifacts"): the Environment the Revision was rendered for and
// the sequence its push took, one more than the highest signed sequence of
// the repository's Revisions for that Environment.
const (
	AnnotationEnvironment = "ruralz.io/environment"
	AnnotationSequence    = "ruralz.io/sequence"
)

// maxSequenceDigits bounds a sequence's decimal form: uint64's 20 digits.
const maxSequenceDigits = 20

// Errors of the Revision binding, each RZ-CFG-033.
var (
	// ErrEnvironment reports a Revision no valid signature binds to the
	// Node's Environment: another Environment's Revision.
	ErrEnvironment = errors.New("signing: no valid signature binds the Revision to this Environment")
	// ErrSequence reports a Revision whose highest signed sequence is
	// below the Node's high-water mark: an older Revision replayed.
	ErrSequence = errors.New("signing: the Revision's signed sequence is below the high-water mark")
	// ErrSequenceSyntax reports a sequence annotation that is not a
	// canonical decimal within uint64.
	ErrSequenceSyntax = errors.New("signing: not a sequence")
)

// RevisionAnnotations returns the annotations an OCI Revision's signature
// signs: its Environment and sequence.
func RevisionAnnotations(environment string, sequence uint64) map[string]string {
	return map[string]string{AnnotationEnvironment: environment, AnnotationSequence: strconv.FormatUint(sequence, 10)}
}

// ParseSequence parses a sequence annotation: decimal digits without a
// sign or leading zero (0 itself is "0"), within uint64.
func ParseSequence(s string) (uint64, error) {
	if s == "" || len(s) > maxSequenceDigits || (len(s) > 1 && s[0] == '0') {
		return 0, fmt.Errorf("%w: %q", ErrSequenceSyntax, s)
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, fmt.Errorf("%w: %q", ErrSequenceSyntax, s)
		}
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q is out of range", ErrSequenceSyntax, s)
	}
	return n, nil
}

// Binding is what an OCI Revision's signature binds besides its manifest
// digest.
type Binding struct {
	// Environment is the Environment the Revision was signed for.
	Environment string
	// Sequence is its signed sequence.
	Sequence uint64
	// Signer names the signature that bound it (Signature.Signer).
	Signer string
}

// BindRevision returns the binding of the valid signature that names
// environment, the Node's Environment, with the highest sequence, since a
// manifest can carry several signatures. mark is the Node's high-water
// mark of environment, the highest sequence it accepted, 0 before any; a
// sequence equal to it is accepted, as a restart pulls the Revision that
// set it. A signature without both annotations in their forms names no
// Environment. The errors are RZ-CFG-033: ErrEnvironment when no valid
// signature names environment (an empty environment included), ErrSequence
// when the highest such sequence is below mark.
func BindRevision(valid []Signature, environment string, mark uint64) (Binding, error) {
	if environment == "" {
		return Binding{}, errcode.Wrap(CodeSignature, fmt.Errorf("%w: the trust policy names no Environment", ErrEnvironment))
	}
	var best Binding
	found := false
	var others []string
	for _, s := range valid {
		env, ok := s.Annotations[AnnotationEnvironment]
		raw, hasSeq := s.Annotations[AnnotationSequence]
		if !ok || !hasSeq || env == "" {
			others = append(others, "a signature without "+AnnotationEnvironment+" and "+AnnotationSequence)
			continue
		}
		seq, err := ParseSequence(raw)
		if err != nil {
			others = append(others, "a signature whose "+AnnotationSequence+" is not a sequence")
			continue
		}
		if env != environment {
			others = append(others, "Environment "+strconv.Quote(env))
			continue
		}
		if !found || seq > best.Sequence {
			best, found = Binding{Environment: env, Sequence: seq, Signer: s.Signer}, true
		}
	}
	if !found {
		detail := "no valid signature"
		if len(others) > 0 {
			detail = fmt.Sprintf("the valid signatures name %v", others)
		}
		return Binding{}, errcode.Wrap(CodeSignature, fmt.Errorf("%w %q: %s", ErrEnvironment, environment, detail))
	}
	if best.Sequence < mark {
		return Binding{}, errcode.Wrap(CodeSignature, fmt.Errorf("%w: sequence %d of Environment %q is below the high-water mark %d",
			ErrSequence, best.Sequence, environment, mark))
	}
	return best, nil
}
