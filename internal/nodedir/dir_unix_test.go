// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package nodedir

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/ulid"
)

// TestOpenRejectsUnsafeRoot covers Open's ownership and permission checks:
// a root owned by another user (spec 04 section 3) or writable by all
// users could let someone else replace or squat the lock, holder.json or
// the Last-Known-Good. A group-writable root is the operator's choice and
// accepted (systemd StateDirectoryMode=0770, a volume shared through its
// group ID, setgid included).
func TestOpenRejectsUnsafeRoot(t *testing.T) {
	cases := []struct {
		name string
		mode os.FileMode
		want error
	}{
		{"0700", 0o700, nil},
		{"0755 readable", 0o755, nil},
		{"0750", 0o750, nil},
		{"0770 group-writable", 0o770, nil},
		{"0775 group-writable", 0o775, nil},
		{"2770 setgid group-writable", os.ModeSetgid | 0o770, nil},
		{"0702 writable by all users only", 0o702, ErrWritableByOthers},
		{"0777 world-writable", 0o777, ErrWritableByOthers},
		{"1777 sticky", os.ModeSticky | 0o777, ErrWritableByOthers},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "data")
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(root, tc.mode); err != nil {
				t.Fatal(err)
			}
			_, err := Open(root)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Open err = %v, want %v", err, tc.want)
			}
		})
	}
	t.Run("subdirectory writable by others", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "data")
		if _, err := Open(root); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(root, LKGDir), 0o777); err != nil { //nolint:gosec // G302: the unsafe mode under test.
			t.Fatal(err)
		}
		if _, err := Open(root); !errors.Is(err, ErrWritableByOthers) {
			t.Fatalf("Open err = %v, want ErrWritableByOthers", err)
		}
	})
	t.Run("subdirectory writable by group", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "data")
		if _, err := Open(root); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(root, IdentityDir), os.ModeSetgid|0o770); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(root); err != nil {
			t.Fatalf("Open err = %v, want nil", err)
		}
	})
}

// readDeadline bounds a read in tests, so a blocking open fails the test
// instead of hanging it.
const readDeadline = 30 * time.Second

// within runs read and fails the test when it does not return within
// readDeadline (a FIFO opened without O_NONBLOCK waits for a writer).
func within[T any](t *testing.T, read func() (T, error)) (T, error) {
	t.Helper()
	type result struct {
		v   T
		err error
	}
	done := make(chan result, 1)
	go func() {
		v, err := read()
		done <- result{v, err}
	}()
	timer := time.NewTimer(readDeadline)
	defer timer.Stop()
	select {
	case r := <-done:
		return r.v, r.err
	case <-timer.C:
		t.Fatalf("read still blocked after %v", readDeadline)
		var zero T
		return zero, nil
	}
}

// TestReadersRefuseSpecialFiles covers the readers of holder.json (spec 10
// requirements 95 and 96.1) and node-id (spec 04 requirement 5): a FIFO,
// a symbolic link or a directory in their place is refused without
// blocking and without reading through the link. ruralz node drain may run
// as root against a data dir another user owns, so a planted link must not
// let it read, or echo in a JSON error, another file; ReadHolderAt reports
// ErrHolderInvalid, never ErrNoHolder, and NodeID neither generates nor
// rewrites an identity.
func TestReadersRefuseSpecialFiles(t *testing.T) {
	validHolder := `{"format":"ruralz.holder.v1","pid":7,"nodeId":"01ARYZ6S41TSV4RRFFQ69G5FAV"}`
	cases := []struct {
		name  string
		plant func(t *testing.T, path, target string)
		msg   string
	}{
		{"FIFO", func(t *testing.T, path, _ string) {
			if !mkfifo(t, path) {
				t.Skip("no syscall.Mkfifo on this platform")
			}
		}, "not a regular file"},
		{"symbolic link to a valid file", func(t *testing.T, path, target string) {
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		}, ""},
		{"dangling symbolic link", func(t *testing.T, path, target string) {
			if err := os.Symlink(target+".none", path); err != nil {
				t.Fatal(err)
			}
		}, ""},
		{"directory", func(t *testing.T, path, _ string) {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}, "not a regular file"},
	}
	for _, tc := range cases {
		t.Run("holder.json "+tc.name, func(t *testing.T) {
			root := t.TempDir()
			target := filepath.Join(t.TempDir(), "other.json")
			if err := os.WriteFile(target, []byte(validHolder), 0o600); err != nil {
				t.Fatal(err)
			}
			tc.plant(t, filepath.Join(root, HolderFile), target)
			h, err := within(t, func() (Holder, error) { return ReadHolderAt(root) })
			if !errors.Is(err, ErrHolderInvalid) || errors.Is(err, ErrNoHolder) {
				t.Fatalf("ReadHolderAt = %+v, %v; want ErrHolderInvalid", h, err)
			}
			if !strings.Contains(err.Error(), tc.msg) {
				t.Errorf("err %q lacks %q", err, tc.msg)
			}
		})
		t.Run("node-id "+tc.name, func(t *testing.T) {
			d, err := Open(filepath.Join(t.TempDir(), "data"))
			if err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(t.TempDir(), "other-id")
			if err := os.WriteFile(target, []byte("01ARYZ6S41TSV4RRFFQ69G5FAV\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			tc.plant(t, d.NodeIDPath(), target)
			before, err := os.Lstat(d.NodeIDPath())
			if err != nil {
				t.Fatal(err)
			}
			id, err := within(t, func() (ulid.ULID, error) {
				return d.NodeID(func() (ulid.ULID, error) {
					t.Error("generator called with a node-id in place")
					return ulid.ULID{}, errors.New("unexpected")
				})
			})
			if err == nil {
				t.Fatalf("NodeID = %s through a %s, want an error", id, tc.name)
			}
			after, err := os.Lstat(d.NodeIDPath())
			if err != nil || !os.SameFile(before, after) {
				t.Errorf("node-id was replaced: %v", err)
			}
		})
	}
}

// TestReadHolderAtRefusesHardLink: a hard link planted in place of
// holder.json is refused before its content is parsed or echoed.
func TestReadHolderAtRefusesHardLink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), "other.json")
	if err := os.WriteFile(target, []byte(`{"format":"TOPSECRET-VALUE"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(target, filepath.Join(root, HolderFile)); err != nil {
		t.Skipf("hard link: %v", err)
	}
	_, err := ReadHolderAt(root)
	if !errors.Is(err, ErrHolderInvalid) || strings.Contains(err.Error(), "TOPSECRET") || !strings.Contains(err.Error(), "hard links") {
		t.Errorf("ReadHolderAt = %v; want ErrHolderInvalid naming the link count, without the content", err)
	}
}

// TestOpenRejectsForeignOwner needs root to hand the directory to another
// user.
func TestOpenRejectsForeignOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to chown")
	}
	root := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(root, 65534, 65534); err != nil {
		t.Skipf("chown: %v", err)
	}
	if _, err := Open(root); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("Open err = %v, want ErrNotOwner", err)
	}
}

// TestSyncDirMissing covers the fsync error path.
func TestSyncDirMissing(t *testing.T) {
	if err := syncDir(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("syncDir of a missing directory succeeded")
	}
}
