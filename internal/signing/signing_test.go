// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package signing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"regexp"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/errcode"
)

// Tests for spec 06 section 2.16: requirement 97 (the full
// sha256:<64 lowercase hex> is the only accepted expected digest,
// RZ-CFG-027 for Revisions, RZ-CFG-028 for Plugin artifacts, always on)
// and the M1 call-site contract of requirement 98 (DigestOnly as the
// Verifier), plus R-10 (revision.Digest is the only digest type) and R-50
// (VerifyArtifact: streaming SHA-256 with a size limit).

const canonical = `{"apiVersion":"ruralz.io/v1alpha1","kind":"Route","metadata":{"name":"orders"}}`

func digestHex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestParseDigestReq97(t *testing.T) {
	good := "sha256:" + digestHex([]byte(canonical))
	tests := []struct {
		name    string
		in      string
		display bool
	}{
		{name: "display form", in: "rev-" + digestHex([]byte(canonical))[:12], display: true},
		{name: "display form full length", in: "rev-" + digestHex([]byte(canonical)), display: true},
		{name: "uppercase hex", in: "sha256:" + strings.ToUpper(digestHex([]byte(canonical)))},
		{name: "one uppercase digit", in: good[:len(good)-1] + "A"},
		{name: "63 hex", in: good[:len(good)-1]},
		{name: "65 hex", in: good + "0"},
		{name: "missing prefix", in: digestHex([]byte(canonical))},
		{name: "sha512 prefix", in: "sha512:" + digestHex([]byte(canonical))},
		{name: "uppercase prefix", in: "SHA256:" + digestHex([]byte(canonical))},
		{name: "empty", in: ""},
		{name: "prefix only", in: "sha256:"},
		{name: "non-hex", in: "sha256:" + strings.Repeat("g", 64)},
		{name: "leading space", in: " " + good},
		{name: "trailing newline", in: good + "\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := ParseDigest(tt.in)
			if err == nil {
				t.Fatalf("ParseDigest(%q) = %v, want an error", tt.in, d)
			}
			if !errors.Is(err, revision.ErrSyntax) {
				t.Errorf("error %v does not match revision.ErrSyntax", err)
			}
			if got := errors.Is(err, ErrDisplayForm); got != tt.display {
				t.Errorf("errors.Is(ErrDisplayForm) = %v, want %v", got, tt.display)
			}
			if !d.IsZero() {
				t.Errorf("digest %v returned with an error", d)
			}
			if tt.in != "" && len(tt.in) > 8 && strings.Contains(err.Error(), tt.in) {
				t.Errorf("error %q quotes the input", err)
			}
		})
	}

	d, err := ParseDigest(good)
	if err != nil {
		t.Fatalf("ParseDigest(%q): %v", good, err)
	}
	if d.String() != good || d != revision.Sum([]byte(canonical)) {
		t.Fatalf("ParseDigest = %v, want %s", d, good)
	}
}

func TestParseDigestReasons(t *testing.T) {
	good := "sha256:" + digestHex(nil)
	tests := map[string]string{
		"rev-0123456789ab":      "display form",
		"md5:" + digestHex(nil): "prefix",
		good[:20]:               "13 hex characters, want 64",
		"sha256:" + strings.ToUpper(digestHex(nil)): "uppercase",
		"sha256:" + strings.Repeat("z", 64):         "not lowercase hex",
	}
	for in, want := range tests {
		if _, err := ParseDigest(in); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ParseDigest(%q) = %v, want an error mentioning %q", in, err, want)
		}
	}
}

// flipBit returns a copy of b with bit i flipped.
func flipBit(b []byte, i int) []byte {
	c := bytes.Clone(b)
	c[i/8] ^= 1 << (i % 8)
	return c
}

func TestVerifyRevisionReq97(t *testing.T) {
	content := []byte(canonical)
	want := revision.Sum(content)
	if err := VerifyRevision(content, want); err != nil {
		t.Fatalf("VerifyRevision of matching content: %v", err)
	}
	// Every single-bit flip of the content is RZ-CFG-027.
	for i := range len(content) * 8 {
		err := VerifyRevision(flipBit(content, i), want)
		if code, _ := errcode.CodeOf(err); code != "RZ-CFG-027" || !errors.Is(err, ErrMismatch) {
			t.Fatalf("bit %d: VerifyRevision = %v, want RZ-CFG-027 ErrMismatch", i, err)
		}
	}
	// A flipped digest bit is a mismatch too.
	raw := want.Bytes()
	other, err := ParseDigest("sha256:" + hex.EncodeToString(flipBit(raw[:], 7)))
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyRevision(content, other); !errors.Is(err, ErrMismatch) {
		t.Fatalf("flipped digest: %v", err)
	}
	// No expected digest never verifies.
	err = VerifyRevision(content, revision.Digest{})
	if code, _ := errcode.CodeOf(err); code != "RZ-CFG-027" || !errors.Is(err, ErrNoDigest) {
		t.Fatalf("zero digest: %v", err)
	}
	// The error names both digests and never the content.
	err = VerifyRevision([]byte("tampered"), want)
	if !strings.Contains(err.Error(), want.String()) || strings.Contains(err.Error(), "tampered") {
		t.Fatalf("error text %q", err)
	}
}

func TestVerifierReq98(t *testing.T) {
	ctx := t.Context()
	content := []byte(canonical)
	want := revision.Sum(content)
	for name, v := range map[string]Verifier{"DigestOnly": DigestOnly{}, "Default": Default(), "WithSignature(nil)": WithSignature(nil)} {
		if _, ok := v.(DigestOnly); !ok {
			t.Errorf("%s is %T, want DigestOnly", name, v)
		}
		if err := v.Verify(ctx, content, want); err != nil {
			t.Errorf("%s.Verify: %v", name, err)
		}
		if code, _ := errcode.CodeOf(v.Verify(ctx, flipBit(content, 3), want)); code != "RZ-CFG-027" {
			t.Errorf("%s accepted tampered content (code %q)", name, code)
		}
	}
}

// fakeSignature is an M2 signature verifier stand-in.
type fakeSignature struct {
	calls int
	err   error
}

func (f *fakeSignature) Verify(context.Context, []byte, revision.Digest) error {
	f.calls++
	return f.err
}

func TestWithSignatureSlot(t *testing.T) {
	ctx := t.Context()
	content := []byte(canonical)
	want := revision.Sum(content)

	sig := &fakeSignature{}
	v := WithSignature(sig)
	if err := v.Verify(ctx, content, want); err != nil || sig.calls != 1 {
		t.Fatalf("Verify = %v, signature calls %d", err, sig.calls)
	}
	// The digest check runs first and cannot be skipped.
	err := v.Verify(ctx, flipBit(content, 0), want)
	if code, _ := errcode.CodeOf(err); code != "RZ-CFG-027" || sig.calls != 1 {
		t.Fatalf("tampered content: %v, signature calls %d", err, sig.calls)
	}
	sigErr := errcode.Wrap("RZ-CFG-033", errors.New("no trusted signature"))
	sig.err = sigErr
	if err := v.Verify(ctx, content, want); !errors.Is(err, sigErr) {
		t.Fatalf("signature failure = %v", err)
	}
}

func TestVerifyArtifactR50(t *testing.T) {
	ctx := t.Context()
	artifact := bytes.Repeat([]byte("\x00asm-module-bytes-"), 5000) // 85,000 bytes, several chunks
	want := revision.Sum(artifact)
	n := int64(len(artifact))

	tests := []struct {
		name     string
		r        func() io.Reader
		want     revision.Digest
		limit    int64
		code     string
		sentinel error
	}{
		{name: "match", r: func() io.Reader { return bytes.NewReader(artifact) }, want: want, limit: n},
		{name: "match with a larger limit", r: func() io.Reader { return bytes.NewReader(artifact) }, want: want, limit: 1 << 30},
		{name: "match one byte at a time", r: func() io.Reader { return iotest.OneByteReader(bytes.NewReader(artifact)) }, want: want, limit: n},
		{name: "match with data and EOF together", r: func() io.Reader { return iotest.DataErrReader(bytes.NewReader(artifact)) }, want: want, limit: n},
		{name: "single-bit flip", r: func() io.Reader { return bytes.NewReader(flipBit(artifact, 8*40000+5)) }, want: want, limit: n, code: "RZ-CFG-028", sentinel: ErrMismatch},
		{name: "limit exceeded by one byte", r: func() io.Reader { return bytes.NewReader(artifact) }, want: want, limit: n - 1, code: "RZ-CFG-028", sentinel: ErrTooLarge},
		{name: "limit far exceeded", r: func() io.Reader { return bytes.NewReader(artifact) }, want: want, limit: 10, code: "RZ-CFG-028", sentinel: ErrTooLarge},
		{name: "truncated", r: func() io.Reader { return bytes.NewReader(artifact[:n-1]) }, want: want, limit: n, code: "RZ-CFG-028", sentinel: ErrMismatch},
		{name: "empty artifact", r: func() io.Reader { return bytes.NewReader(nil) }, want: revision.Sum(nil), limit: 1},
		{name: "read error", r: func() io.Reader { return iotest.TimeoutReader(bytes.NewReader(artifact)) }, want: want, limit: n, code: "RZ-CFG-028", sentinel: iotest.ErrTimeout},
		{name: "zero digest", r: func() io.Reader { return bytes.NewReader(artifact) }, limit: n, code: "RZ-CFG-028", sentinel: ErrNoDigest},
		{name: "zero limit", r: func() io.Reader { return bytes.NewReader(artifact) }, want: want, limit: 0, sentinel: ErrLimit},
		{name: "negative limit", r: func() io.Reader { return bytes.NewReader(artifact) }, want: want, limit: -1, sentinel: ErrLimit},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			check := func(fn string, err error) {
				t.Helper()
				if tt.sentinel == nil {
					if err != nil {
						t.Fatalf("%s: %v", fn, err)
					}
					return
				}
				if !errors.Is(err, tt.sentinel) {
					t.Fatalf("%s = %v, want %v", fn, err, tt.sentinel)
				}
				if code, _ := errcode.CodeOf(err); code != tt.code {
					t.Fatalf("%s code %q, want %q", fn, code, tt.code)
				}
			}
			check("VerifyArtifact", VerifyArtifact(ctx, tt.r(), tt.want, tt.limit))
			got, err := ReadArtifact(ctx, tt.r(), tt.want, tt.limit)
			check("ReadArtifact", err)
			if err != nil && got != nil {
				t.Fatal("ReadArtifact returned bytes with an error")
			}
			if err == nil {
				all, _ := io.ReadAll(tt.r())
				if !bytes.Equal(got, all) {
					t.Fatal("ReadArtifact returned other bytes")
				}
			}
		})
	}
}

// countingReader counts bytes delivered.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func TestVerifyArtifactReadsAtMostLimitPlusOne(t *testing.T) {
	// A huge (or endless) stream stops right after the limit.
	for _, limit := range []int64{1, 100, chunkSize - 1, chunkSize, chunkSize + 1, 3 * chunkSize} {
		cr := &countingReader{r: iotest.OneByteReader(zeroReader{})}
		if limit > 1000 {
			cr.r = zeroReader{}
		}
		err := VerifyArtifact(t.Context(), cr, revision.Sum(nil), limit)
		if !errors.Is(err, ErrTooLarge) {
			t.Fatalf("limit %d: %v", limit, err)
		}
		if cr.n != limit+1 {
			t.Fatalf("limit %d: read %d bytes, want %d", limit, cr.n, limit+1)
		}
	}
}

// zeroReader is an endless stream of zero bytes.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func TestVerifyArtifactEdgeReaders(t *testing.T) {
	ctx := t.Context()
	// The largest limit does not overflow.
	content := []byte("plugin")
	if err := VerifyArtifact(ctx, bytes.NewReader(content), revision.Sum(content), math.MaxInt64); err != nil {
		t.Fatalf("MaxInt64 limit: %v", err)
	}
	// A reader that makes no progress ends the stream.
	stuck := readerFunc(func([]byte) (int, error) { return 0, nil })
	err := VerifyArtifact(ctx, stuck, revision.Sum(content), 10)
	if code, _ := errcode.CodeOf(err); code != "RZ-CFG-028" || !errors.Is(err, io.ErrNoProgress) {
		t.Fatalf("stuck reader: %v", err)
	}
	// A few empty reads in between are fine.
	calls := 0
	r := bytes.NewReader(content)
	flaky := readerFunc(func(p []byte) (int, error) {
		calls++
		if calls%2 == 1 {
			return 0, nil
		}
		return r.Read(p)
	})
	if err := VerifyArtifact(ctx, flaky, revision.Sum(content), 10); err != nil {
		t.Fatalf("flaky reader: %v", err)
	}
}

func TestVerifyArtifactContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := VerifyArtifact(ctx, bytes.NewReader([]byte("x")), revision.Sum([]byte("x")), 10)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled context: %v", err)
	}
	// Cancellation between chunks stops the stream.
	ctx, cancel = context.WithCancel(t.Context())
	r := readerFunc(func(p []byte) (int, error) {
		cancel()
		clear(p)
		return len(p), nil
	})
	if err := VerifyArtifact(ctx, r, revision.Sum(nil), 1<<40); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled mid-stream: %v", err)
	}
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

var wireForm = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// FuzzParseDigest (spec 06 section 6.4): never panics; accepts exactly the
// wire form and round-trips it; every rejection matches
// revision.ErrSyntax.
func FuzzParseDigest(f *testing.F) {
	for _, s := range []string{
		"sha256:" + digestHex(nil),
		"sha256:" + strings.ToUpper(digestHex(nil)),
		"rev-0123456789ab",
		"sha256:",
		"sha512:" + digestHex(nil),
		"",
		"sha256:" + digestHex(nil)[:63] + "\x00",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		d, err := ParseDigest(s)
		if ok := wireForm.MatchString(s); ok != (err == nil) {
			t.Fatalf("ParseDigest(%q) error %v, wire form %v", s, err, ok)
		}
		if err != nil {
			if !errors.Is(err, revision.ErrSyntax) {
				t.Fatalf("error %v does not match revision.ErrSyntax", err)
			}
			return
		}
		if d.String() != s {
			t.Fatalf("round trip %q -> %q", s, d.String())
		}
	})
}

func BenchmarkVerifyArtifact(b *testing.B) {
	artifact := bytes.Repeat([]byte{0xa5}, 4<<20)
	want := revision.Sum(artifact)
	ctx := b.Context()
	b.SetBytes(int64(len(artifact)))
	b.ReportAllocs()
	for b.Loop() {
		if err := VerifyArtifact(ctx, bytes.NewReader(artifact), want, int64(len(artifact))); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkVerifyRevision(b *testing.B) {
	content := bytes.Repeat([]byte(canonical), 1000)
	want := revision.Sum(content)
	b.SetBytes(int64(len(content)))
	b.ReportAllocs()
	for b.Loop() {
		if err := VerifyRevision(content, want); err != nil {
			b.Fatal(err)
		}
	}
}
