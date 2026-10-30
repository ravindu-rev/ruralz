// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package lkg

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/gateway/handover"
	"github.com/ravindu-rev/ruralz/internal/nodedir"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
)

// Tests for the Last-Known-Good files of docs/architecture/03-data-plane.md
// "Last-Known-Good files": the layout and pointers, content before
// pointer, the cleanup of unnamed files, lkg_write_failed, absent
// pointers and the reads a boot and a handover successor make.

func epoch() time.Time { return time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC) }

// rev returns a Revision of content.
func rev(content string) revision.Revision {
	return revision.Revision{Digest: revision.Sum([]byte(content)), Content: []byte(content)}
}

// fakeStatus records degraded reasons.
type fakeStatus struct {
	mu  sync.Mutex
	set []string
}

func (f *fakeStatus) SetDegraded(r catalog.Reason, source string, on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	state := "off"
	if on {
		state = "on"
	}
	f.set = append(f.set, r.String()+"/"+source+"="+state)
}

func (f *fakeStatus) SetCleartextHops(catalog.Hop, int) {}

func (f *fakeStatus) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.set)
}

// rig is a Store over a fresh data directory, with the lock held.
type rig struct {
	t      *testing.T
	dir    *nodedir.Dir
	lock   *nodedir.Lock
	clk    *clocktest.Fake
	status *fakeStatus
	s      *Store
}

func newRig(t *testing.T, edit ...func(*Config)) *rig {
	t.Helper()
	dir, err := nodedir.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	lock, err := dir.TryLock()
	if errors.Is(err, errors.ErrUnsupported) {
		t.Skip("no flock on this platform")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Release() })
	r := &rig{t: t, dir: dir, lock: lock, clk: clocktest.New(epoch()), status: &fakeStatus{}}
	cfg := Config{Dir: dir, Version: "0.1.0-test", Clock: r.clk, Status: r.status}
	for _, e := range edit {
		e(&cfg)
	}
	r.s, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// files returns the names in lkg/.
func (r *rig) files() []string {
	r.t.Helper()
	entries, err := os.ReadDir(r.dir.LKGPath())
	if err != nil {
		r.t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// pointerOf decodes the pointer file name.
func (r *rig) pointerOf(name string) Pointer {
	r.t.Helper()
	data, err := os.ReadFile(r.s.Path(name)) //nolint:gosec // G304: a test file under the test's data directory.
	if err != nil {
		r.t.Fatal(err)
	}
	if !bytes.HasSuffix(data, []byte("\n")) {
		r.t.Errorf("%s does not end with a newline: %q", name, data)
	}
	var p Pointer
	if err := json.Unmarshal(data, &p); err != nil {
		r.t.Fatal(err)
	}
	return p
}

// write writes name in lkg/ directly, as another process or a crash
// would leave it.
func (r *rig) write(name, data string) {
	r.t.Helper()
	if err := os.WriteFile(r.s.Path(name), []byte(data), 0o600); err != nil {
		r.t.Fatal(err)
	}
}

func TestWriteLayout(t *testing.T) {
	r := newRig(t)
	a := rev(`{"format":"ruralz.canonical.v1","resources":[]}`)
	if err := r.s.Write(r.lock, a, true); err != nil {
		t.Fatalf("Write = %v", err)
	}
	want := []string{CandidateFile, LKGFile, ContentName(a.Digest)}
	slices.Sort(want)
	if got := r.files(); !slices.Equal(got, want) {
		t.Fatalf("lkg/ holds %v, want %v", got, want)
	}
	content, err := os.ReadFile(r.s.Path(ContentName(a.Digest)))
	if err != nil || !bytes.Equal(content, a.Content) {
		t.Fatalf("content file = %q, %v; want the exact canonical bytes", content, err)
	}
	wantPtr := Pointer{Format: "ruralz.lkg.v1", Digest: a.Digest.String(), WrittenAt: "2026-09-26T12:00:00Z", Version: "0.1.0-test"}
	for _, n := range []string{CandidateFile, LKGFile} {
		if got := r.pointerOf(n); got != wantPtr {
			t.Errorf("%s = %+v, want %+v", n, got, wantPtr)
		}
	}
	// Directories are 0700 and files 0600 (docs/architecture/03-data-plane.md
	// "Durable and shared state").
	for _, n := range want {
		fi, err := os.Stat(r.s.Path(n))
		if err != nil {
			t.Fatal(err)
		}
		if perm := fi.Mode().Perm(); perm != nodedir.FilePerm && filepath.Separator == '/' {
			t.Errorf("%s has mode %v, want %v", n, perm, nodedir.FilePerm)
		}
	}
	if got := r.status.calls(); len(got) != 0 {
		t.Errorf("degraded calls %v after a successful first write, want none", got)
	}
}

func TestPointerMemberOrder(t *testing.T) {
	// candidate.json and lkg.json are {"format","digest","writtenAt","version"}.
	r := newRig(t)
	a := rev(`{"format":"ruralz.canonical.v1","resources":[]}`)
	if err := r.s.Write(r.lock, a, true); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(r.s.Path(CandidateFile))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"format":"ruralz.lkg.v1","digest":"` + a.Digest.String() + `","writtenAt":"2026-09-26T12:00:00Z","version":"0.1.0-test"}` + "\n"
	if string(data) != want {
		t.Fatalf("candidate.json = %s, want %s", data, want)
	}
}

func TestWriteWithoutPromoteKeepsLastKnownGood(t *testing.T) {
	// A candidate that is not promoted (Control mode, M2) leaves lkg.json
	// and the content it names in place.
	r := newRig(t)
	a, b := rev("A"), rev("B")
	if err := r.s.Write(r.lock, a, true); err != nil {
		t.Fatal(err)
	}
	if err := r.s.Write(r.lock, b, false); err != nil {
		t.Fatal(err)
	}
	cand, last, err := r.s.Pointers()
	if err != nil || cand != b.Digest || last != a.Digest {
		t.Fatalf("Pointers = %s, %s, %v; want candidate B and Last-Known-Good A", cand.Short(), last.Short(), err)
	}
	want := []string{CandidateFile, LKGFile, ContentName(a.Digest), ContentName(b.Digest)}
	slices.Sort(want)
	if got := r.files(); !slices.Equal(got, want) {
		t.Fatalf("lkg/ holds %v, want %v", got, want)
	}
}

func TestWriteDeletesUnnamedFiles(t *testing.T) {
	r := newRig(t)
	a, b := rev("A"), rev("B")
	if err := r.s.Write(r.lock, a, true); err != nil {
		t.Fatal(err)
	}
	// What a crash or an older release left: a temporary file, content no
	// pointer names, and a stray file; a directory is left alone.
	r.write(".tmp-candidate.json-123", "partial")
	r.write(ContentName(rev("C").Digest), "C")
	r.write("notes.txt", "x")
	if err := os.Mkdir(r.s.Path("keep.d"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := r.s.Write(r.lock, b, true); err != nil {
		t.Fatal(err)
	}
	want := []string{CandidateFile, "keep.d", LKGFile, ContentName(b.Digest)}
	slices.Sort(want)
	if got := r.files(); !slices.Equal(got, want) {
		t.Fatalf("lkg/ holds %v, want %v", got, want)
	}
}

func TestWriteNeedsTheLock(t *testing.T) {
	r := newRig(t)
	if err := r.s.Write(nil, rev("A"), true); !errors.Is(err, ErrNotHolder) {
		t.Fatalf("Write(nil lock) = %v, want ErrNotHolder", err)
	}
	if got := r.files(); len(got) != 0 {
		t.Fatalf("lkg/ holds %v after a write without the lock", got)
	}
	if err := r.lock.Release(); err != nil {
		t.Fatal(err)
	}
	if err := r.s.Write(r.lock, rev("A"), true); !errors.Is(err, nodedir.ErrReleased) {
		t.Fatalf("Write(released lock) = %v, want nodedir.ErrReleased", err)
	}
	if got := r.status.calls(); len(got) != 0 {
		t.Errorf("degraded calls %v: a write without the lock is no failed write", got)
	}
	if r.s.Failing() {
		t.Error("Failing after a write without the lock")
	}
}

func TestFailedWriteRaisesDegradedUntilSuccess(t *testing.T) {
	r := newRig(t)
	a, b := rev("A"), rev("B")
	if err := r.s.Write(r.lock, a, true); err != nil {
		t.Fatal(err)
	}
	// A directory where the pointer goes makes its rename fail, even for
	// root, as a full disk would.
	if err := os.Remove(r.s.Path(CandidateFile)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(r.s.Path(CandidateFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := r.s.Write(r.lock, b, true); err == nil {
		t.Fatal("Write succeeded over a directory")
	}
	if !r.s.Failing() {
		t.Error("Failing = false after a failed write")
	}
	// Content before pointer: lkg.json still names A, whose content is
	// still there, so a crash now boots A.
	e, err := r.s.LastKnownGood()
	if err != nil || e.Digest != a.Digest || string(e.Content) != "A" {
		t.Fatalf("LastKnownGood = %s %q, %v; want A", e.Digest.Short(), e.Content, err)
	}
	if err := os.Remove(r.s.Path(CandidateFile)); err != nil {
		t.Fatal(err)
	}
	if err := r.s.Write(r.lock, b, true); err != nil {
		t.Fatalf("Write after the fault = %v", err)
	}
	if r.s.Failing() {
		t.Error("Failing after a successful write")
	}
	want := []string{"lkg_write_failed/lkg=on", "lkg_write_failed/lkg=off"}
	if got := r.status.calls(); !slices.Equal(got, want) {
		t.Errorf("degraded calls %v, want %v", got, want)
	}
	// The retry's cleanup deleted A's content, now unnamed.
	if _, err := os.Stat(r.s.Path(ContentName(a.Digest))); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("A's content survived the cleanup: %v", err)
	}
}

func TestReadsRoundTrip(t *testing.T) {
	r := newRig(t)
	a := rev(`{"format":"ruralz.canonical.v1","resources":[]}`)
	if err := r.s.Write(r.lock, a, true); err != nil {
		t.Fatal(err)
	}
	for name, read := range map[string]func() (Entry, error){
		"LastKnownGood": r.s.LastKnownGood,
		"Candidate":     r.s.Candidate,
		"Revision":      func() (Entry, error) { return r.s.Revision(a.Digest) },
	} {
		e, err := read()
		if err != nil || e.Digest != a.Digest || !bytes.Equal(e.Content, a.Content) || e.Path != r.s.Path(ContentName(a.Digest)) {
			t.Errorf("%s = %+v, %v; want A at its content file", name, e, err)
		}
	}
}

func TestAbsentPointers(t *testing.T) {
	d := rev("A").Digest.String()
	tests := map[string]string{
		"missing":          "",
		"unknown format":   `{"format":"ruralz.lkg.v2","digest":"` + d + `"}`,
		"no format":        `{"digest":"` + d + `"}`,
		"malformed JSON":   `{"format":`,
		"malformed digest": `{"format":"ruralz.lkg.v1","digest":"sha256:00"}`,
		"too large":        `{"format":"ruralz.lkg.v1","digest":"` + d + `","pad":"` + string(bytes.Repeat([]byte("x"), maxPointerBytes)) + `"}`,
	}
	for name, data := range tests {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			if data != "" {
				r.write(LKGFile, data)
				r.write(CandidateFile, data)
			}
			if _, err := r.s.LastKnownGood(); !errors.Is(err, ErrAbsent) {
				t.Errorf("LastKnownGood = %v, want ErrAbsent", err)
			}
			if _, err := r.s.Candidate(); !errors.Is(err, ErrAbsent) {
				t.Errorf("Candidate = %v, want ErrAbsent", err)
			}
			cand, last, err := r.s.Pointers()
			if err != nil || !cand.IsZero() || !last.IsZero() {
				t.Errorf("Pointers = %s, %s, %v; want both zero and no error", cand, last, err)
			}
		})
	}
}

func TestReadIgnoresUnknownPointerMembers(t *testing.T) {
	// Readers ignore unknown members: a newer release may add some.
	r := newRig(t)
	a := rev("A")
	r.write(ContentName(a.Digest), "A")
	r.write(LKGFile, `{"format":"ruralz.lkg.v1","digest":"`+a.Digest.String()+`","writtenAt":"2026-09-26T12:00:00Z","version":"9.0.0","signer":"x"}`)
	e, err := r.s.LastKnownGood()
	if err != nil || e.Digest != a.Digest || e.Pointer.Version != "9.0.0" {
		t.Fatalf("LastKnownGood = %+v, %v", e, err)
	}
}

func TestMissingContent(t *testing.T) {
	r := newRig(t)
	a := rev("A")
	r.write(LKGFile, `{"format":"ruralz.lkg.v1","digest":"`+a.Digest.String()+`"}`)
	if _, err := r.s.LastKnownGood(); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("LastKnownGood = %v, want fs.ErrNotExist for a missing content file", err)
	}
	if _, err := r.s.Revision(a.Digest); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Revision = %v, want fs.ErrNotExist", err)
	}
}

func TestContentReadStopsPastTheLimit(t *testing.T) {
	// Content past the Node's limit is read one byte past it, so the
	// pipeline reports RZ-CFG-001 rather than the Node reading it all.
	r := newRig(t, func(c *Config) { c.MaxBytes = 16 })
	big := rev(string(bytes.Repeat([]byte("x"), 100)))
	r.write(ContentName(big.Digest), string(big.Content))
	e, err := r.s.Revision(big.Digest)
	if err != nil || len(e.Content) != 17 {
		t.Fatalf("Revision = %d bytes, %v; want 17 bytes and no error", len(e.Content), err)
	}
	exact := rev(string(bytes.Repeat([]byte("y"), 16)))
	r.write(ContentName(exact.Digest), string(exact.Content))
	if e, err := r.s.Revision(exact.Digest); err != nil || len(e.Content) != 16 {
		t.Fatalf("Revision at the limit = %d bytes, %v", len(e.Content), err)
	}
}

func TestCandidateRereadsAMovedPointer(t *testing.T) {
	// A successor read candidate.json naming A; the holder then wrote a
	// newer candidate B and deleted A's content: Candidate reads B.
	r := newRig(t)
	a, b := rev("A"), rev("B")
	r.write(CandidateFile, `{"format":"ruralz.lkg.v1","digest":"`+a.Digest.String()+`"}`)
	r.write(ContentName(b.Digest), "B")
	reads := 0
	r.s.afterPointer = func() {
		reads++
		if reads == 1 {
			r.write(CandidateFile, `{"format":"ruralz.lkg.v1","digest":"`+b.Digest.String()+`"}`)
		}
	}
	e, err := r.s.Candidate()
	if err != nil || e.Digest != b.Digest || string(e.Content) != "B" {
		t.Fatalf("Candidate = %s %q, %v; want B", e.Digest.Short(), e.Content, err)
	}
	// A pointer that stays on missing content is an error.
	r.s.afterPointer = nil
	r.write(CandidateFile, `{"format":"ruralz.lkg.v1","digest":"`+a.Digest.String()+`"}`)
	if _, err := r.s.Candidate(); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Candidate = %v, want fs.ErrNotExist", err)
	}
}

func TestRetryDelay(t *testing.T) {
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second, time.Minute, time.Minute}
	for i, w := range want {
		if got := RetryDelay(i + 1); got != w {
			t.Errorf("RetryDelay(%d) = %v, want %v", i+1, got, w)
		}
	}
	if got := RetryDelay(0); got != time.Second {
		t.Errorf("RetryDelay(0) = %v, want 1s", got)
	}
}

func TestFormatIsTheHandoverMarker(t *testing.T) {
	if Format != handover.LKGFormat {
		t.Fatalf("Format = %q, handover marker %q", Format, handover.LKGFormat)
	}
}

func TestNewNeedsDir(t *testing.T) {
	if _, err := New(Config{}); !errors.Is(err, ErrConfig) {
		t.Fatalf("New(no Dir) = %v, want ErrConfig", err)
	}
}

func TestAdminPort(t *testing.T) {
	tests := []struct {
		name    string
		content string
		port    int
		ok      bool
	}{
		{"gateway", `{"format":"ruralz.canonical.v1","resources":[{"kind":"Gateway","metadata":{"name":"edge"},"spec":{"admin":{"port":19901},"listeners":[]}}]}`, 19901, true},
		{"gateway after others", `{"resources":[{"kind":"Route","spec":{"admin":{"port":1}}},{"kind":"Gateway","spec":{"admin":{"port":9901}}}]}`, 9901, true},
		{"no gateway", `{"resources":[{"kind":"Route","spec":{}}]}`, 0, false},
		{"no port", `{"resources":[{"kind":"Gateway","spec":{}}]}`, 0, false},
		{"port 0", `{"resources":[{"kind":"Gateway","spec":{"admin":{"port":0}}}]}`, 0, false},
		{"port too large", `{"resources":[{"kind":"Gateway","spec":{"admin":{"port":65536}}}]}`, 0, false},
		{"fraction", `{"resources":[{"kind":"Gateway","spec":{"admin":{"port":99.5}}}]}`, 0, false},
		{"not JSON", `{"resources":`, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			port, ok := AdminPort([]byte(tt.content))
			if port != tt.port || ok != tt.ok {
				t.Errorf("AdminPort = %d, %v; want %d, %v", port, ok, tt.port, tt.ok)
			}
			if p, ok := (Entry{Content: []byte(tt.content)}).AdminPort(); p != tt.port || ok != tt.ok {
				t.Errorf("Entry.AdminPort = %d, %v", p, ok)
			}
		})
	}
}
