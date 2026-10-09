// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package nodedir

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/ulid"
)

// testULID is a fixed node.id.
func testULID(t testing.TB) ulid.ULID {
	t.Helper()
	id, err := ulid.Parse("01ARYZ6S41TSV4RRFFQ69G5FAV")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// assertPerm checks a Unix mode; Windows has no such bits.
func assertPerm(t *testing.T, path string, want fs.FileMode) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != want {
		t.Errorf("%s: mode %v, want %v", path, got, want)
	}
}

// TestLayoutNames pins the layout of spec 04 requirement 5 and the names
// the CLI relies on (spec 10 requirements 94 and 95).
func TestLayoutNames(t *testing.T) {
	root := filepath.Join(t.TempDir(), "data")
	d, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ got, want string }{
		{DefaultRoot, "/var/lib/ruralz"},
		{HolderFormat, "ruralz.holder.v1"},
		{d.Root(), root},
		{d.LockPath(), filepath.Join(root, "lock")},
		{d.HolderPath(), filepath.Join(root, "holder.json")},
		{d.HandoverSocketPath(), filepath.Join(root, "handover.sock")},
		{d.NodeIDPath(), filepath.Join(root, "identity", "node-id")},
		{d.LKGPath(), filepath.Join(root, "lkg")},
		{d.Path(OCICacheDir), filepath.Join(root, "cache", "oci", "sha256")},
	} {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
}

// TestOpenModes covers spec 04 requirement 5 and test plan item 3 "modes
// 0700/0600": Open creates root, identity/ and lkg/ owner-only, missing
// parents included, and does not create the reserved OCI cache.
func TestOpenModes(t *testing.T) {
	root := filepath.Join(t.TempDir(), "a", "b", "data")
	d, err := Open(root + string(filepath.Separator) + ".")
	if err != nil {
		t.Fatal(err)
	}
	if d.Root() != root {
		t.Errorf("Root() = %q, want the cleaned %q", d.Root(), root)
	}
	for _, p := range []string{filepath.Dir(root), root, d.Path(IdentityDir), d.LKGPath()} {
		assertPerm(t, p, 0o700)
	}
	if _, err := os.Stat(d.Path("cache")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("cache/ exists or stat failed: %v", err)
	}
	// Open is idempotent and writes no files.
	if _, err := Open(root); err != nil {
		t.Fatalf("second Open: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if got := strings.Join(names, ","); got != "identity,lkg" {
		t.Errorf("root holds %s, want identity,lkg", got)
	}
}

// TestOpenErrors covers the root checks of Open.
func TestOpenErrors(t *testing.T) {
	base := t.TempDir()
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	identityFile := filepath.Join(base, "idfile")
	if err := os.Mkdir(identityFile, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(identityFile, IdentityDir), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		root string
		want error
	}{
		{"relative", "var/lib/ruralz", ErrRelativeRoot},
		{"empty", "", ErrRelativeRoot},
		{"root is a file", file, ErrNotDirectory},
		{"identity is a file", identityFile, ErrNotDirectory},
		{"parent is a file", filepath.Join(file, "data"), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := Open(tc.root)
			if err == nil {
				t.Fatalf("Open(%q) = %v, want an error", tc.root, d)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("Open(%q) err = %v, want %v", tc.root, err, tc.want)
			}
		})
	}
}

// TestWriteFileAtomic covers the atomic write of spec 04 requirement 5
// and test plan item 3 "atomic write survives a simulated crash (temp file
// left behind is ignored)".
func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	if err := WriteFileAtomic(dir, "f.json", []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A crash between the temporary write and the rename leaves a
	// temporary file behind; readers and later writes ignore it.
	crash := filepath.Join(dir, tempPrefix+"f.json-999")
	if err := os.WriteFile(crash, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(dir, "f.json", []byte("two"), 0o640); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "f.json")) //nolint:gosec // G304: a test file.
	if err != nil || string(got) != "two" {
		t.Fatalf("content = %q, %v", got, err)
	}
	assertPerm(t, filepath.Join(dir, "f.json"), 0o640)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("dir holds %d entries, want f.json and the crash leftover", len(entries))
	}

	for _, name := range []string{"", ".", "..", "a/b", tempPrefix + "x", string(filepath.Separator) + "x"} {
		if err := WriteFileAtomic(dir, name, nil, 0o600); !errors.Is(err, ErrBadName) {
			t.Errorf("WriteFileAtomic(%q) err = %v, want ErrBadName", name, err)
		}
	}
	missing := filepath.Join(dir, "missing")
	if err := WriteFileAtomic(missing, "f", nil, 0o600); err == nil {
		t.Error("write into a missing directory succeeded")
	}
	// A failed rename (target is a non-empty directory) removes the
	// temporary file.
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(filepath.Join(sub, "busy", "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(sub, "busy", []byte("x"), 0o600); err == nil {
		t.Fatal("rename over a non-empty directory succeeded")
	}
	if entries, _ := os.ReadDir(sub); len(entries) != 1 {
		t.Errorf("temporary file left after a failed rename: %d entries", len(entries))
	}
}

// TestNodeID covers spec 04 requirements 5 and 6 and test plan item 3
// "node-id created once and reused".
func TestNodeID(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	want := testULID(t)
	calls := 0
	gen := func() (ulid.ULID, error) { calls++; return want, nil }
	id, err := d.NodeID(gen)
	if err != nil || id != want {
		t.Fatalf("NodeID = %s, %v", id, err)
	}
	data, err := os.ReadFile(d.NodeIDPath())
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "01ARYZ6S41TSV4RRFFQ69G5FAV\n" {
		t.Errorf("node-id content %q, want the ULID and a newline", data)
	}
	assertPerm(t, d.NodeIDPath(), 0o600)
	again, err := d.NodeID(func() (ulid.ULID, error) {
		t.Error("generator called for an existing node-id")
		return ulid.ULID{}, nil
	})
	if err != nil || again != want || calls != 1 {
		t.Fatalf("second NodeID = %s, %v (calls %d)", again, err, calls)
	}
	// A Dir opened later (the next boot) reads the same identity.
	d2, err := Open(d.Root())
	if err != nil {
		t.Fatal(err)
	}
	if id, err := d2.NodeID(nil); err != nil || id != want {
		t.Fatalf("NodeID after reopen = %s, %v", id, err)
	}
	if entries, _ := os.ReadDir(d.Path(IdentityDir)); len(entries) != 1 {
		t.Errorf("identity/ holds %d entries, want node-id only", len(entries))
	}
}

// TestNodeIDErrors covers invalid files and generator failures.
func TestNodeIDErrors(t *testing.T) {
	boom := errors.New("boom")
	cases := []struct {
		name    string
		content string // "" leaves the file absent
		gen     func() (ulid.ULID, error)
		want    error
	}{
		{"generator fails", "", func() (ulid.ULID, error) { return ulid.ULID{}, boom }, boom},
		{"no generator", "", nil, nil},
		{"garbage", "not a ulid\n", nil, ErrNodeIDInvalid},
		{"empty file", "\n", nil, ErrNodeIDInvalid},
		{"two newlines", "01ARYZ6S41TSV4RRFFQ69G5FAV\n\n", nil, ErrNodeIDInvalid},
		{"too large", strings.Repeat("0", 100), nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := Open(filepath.Join(t.TempDir(), "data"))
			if err != nil {
				t.Fatal(err)
			}
			if tc.content != "" {
				if err := os.WriteFile(d.NodeIDPath(), []byte(tc.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			id, err := d.NodeID(tc.gen)
			if err == nil {
				t.Fatalf("NodeID = %s, want an error", id)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if tc.content != "" {
				got, _ := os.ReadFile(d.NodeIDPath())
				if string(got) != tc.content {
					t.Error("an invalid node-id file was rewritten")
				}
			}
		})
	}
	// Without a trailing newline the file is still accepted.
	d, err := Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(d.NodeIDPath(), []byte("01aryz6s41tsv4rrffq69g5fav"), 0o600); err != nil {
		t.Fatal(err)
	}
	if id, err := d.NodeID(nil); err != nil || id != testULID(t) {
		t.Fatalf("NodeID = %s, %v", id, err)
	}
}

// TestNodeIDConcurrentFirstBoot: processes starting at once agree on one
// node.id; the file is never rewritten (spec 04 requirement 5).
func TestNodeIDConcurrentFirstBoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "data")
	if _, err := Open(root); err != nil {
		t.Fatal(err)
	}
	const n = 16
	ids := make([]ulid.ULID, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			d, err := Open(root)
			if err != nil {
				errs[i] = err
				return
			}
			ids[i], errs[i] = d.NodeID(func() (ulid.ULID, error) {
				return ulid.New(time.UnixMilli(int64(1000+i)), nil)
			})
		})
	}
	wg.Wait()
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		if ids[i] != ids[0] {
			t.Fatalf("goroutine %d got %s, goroutine 0 got %s", i, ids[i], ids[0])
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(root, IdentityDir)); len(entries) != 1 {
		t.Errorf("identity/ holds %d entries, want node-id only", len(entries))
	}
}

// TestReadLimited covers the bounded reader.
func TestReadLimited(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, bytes.Repeat([]byte("x"), 10), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, err := readLimited(p, 10, false); err != nil || len(b) != 10 {
		t.Fatalf("readLimited(10) = %d bytes, %v", len(b), err)
	}
	if _, err := readLimited(p, 9, false); err == nil {
		t.Fatal("readLimited(9) accepted 10 bytes")
	}
	if _, err := readLimited(filepath.Dir(p), 9, false); err == nil {
		t.Fatal("reading a directory succeeded")
	}
}
