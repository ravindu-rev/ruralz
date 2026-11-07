// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package raft

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/controlstore"
)

// object returns a test object.
func object(kind controlstore.Kind, data string) controlstore.Object {
	return controlstore.Object{Kind: kind, Format: "test.v1", Data: []byte(data)}
}

// TestContentPutGet stores, indexes and verifies objects.
func TestContentPutGet(t *testing.T) {
	dir := t.TempDir()
	c, err := openContent(dir, func(controlstore.Digest) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	o := object(controlstore.KindRevision, "canonical")
	if err := c.put(o); err != nil {
		t.Fatal(err)
	}
	if err := c.put(o); err != nil {
		t.Errorf("a repeated put = %v", err)
	}
	if !c.has(o.Digest()) {
		t.Fatal("has = false after put")
	}
	got, err := c.get(o.Digest())
	if err != nil || got.Kind != o.Kind || got.Format != o.Format || !bytes.Equal(got.Data, o.Data) {
		t.Fatalf("get = %+v, %v", got, err)
	}
	conflict := controlstore.Object{Kind: controlstore.KindDiff, Format: "test.v1", Data: o.Data}
	if err := c.put(conflict); !errors.Is(err, controlstore.ErrInvalid) {
		t.Errorf("a put of the same bytes as another kind = %v", err)
	}
	if _, err := c.get(controlstore.Sum([]byte("absent"))); !errors.Is(err, controlstore.ErrNotFound) {
		t.Errorf("get of an absent object = %v", err)
	}
	reopened, err := openContent(dir, func(controlstore.Digest) bool { return false })
	if err != nil || !reopened.has(o.Digest()) {
		t.Errorf("a reopened store lost the object: %v", err)
	}
}

// TestContentSelfVerifies: a stored object whose bytes no longer hash to
// its name is removed and reported missing, so the replica fetches it.
func TestContentSelfVerifies(t *testing.T) {
	c, err := openContent(t.TempDir(), func(controlstore.Digest) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	o := object(controlstore.KindPlan, "plan")
	if err := c.put(o); err != nil {
		t.Fatal(err)
	}
	path := c.path(o.Digest())
	data, err := os.ReadFile(path) //nolint:gosec // G304: a test object under t.TempDir.
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes.Replace(data, []byte("plan"), []byte("PLAN"), 1), 0o600); err != nil { //nolint:gosec // G703: a test object under t.TempDir.
		t.Fatal(err)
	}
	if _, err := c.get(o.Digest()); !errors.Is(err, controlstore.ErrNotFound) {
		t.Errorf("get of a corrupted object = %v, want ErrNotFound", err)
	}
	if c.has(o.Digest()) {
		t.Error("a corrupted object is still indexed")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the corrupted file remains: %v", err)
	}
}

// TestContentWriteVerifies: streamed bytes that do not hash to the digest
// leave nothing behind.
func TestContentWriteVerifies(t *testing.T) {
	c, err := openContent(t.TempDir(), func(controlstore.Digest) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	d := controlstore.Sum([]byte("expected"))
	err = c.write(d, objectHeader{Kind: controlstore.KindDiff, Format: "f", Size: 8}, strings.NewReader("tampered"))
	if !errors.Is(err, controlstore.ErrInvalid) {
		t.Errorf("write = %v, want ErrInvalid", err)
	}
	if c.has(d) {
		t.Error("a tampered object was stored")
	}
	entries, _ := os.ReadDir(filepath.Dir(c.path(d)))
	if len(entries) != 0 {
		t.Errorf("leftovers: %v", entries)
	}
}

// TestContentReleaseAndPins: a released object goes at once without a
// pin; with one it waits for the last unpin and stays when a later entry
// referenced it again.
func TestContentReleaseAndPins(t *testing.T) {
	referenced := map[controlstore.Digest]bool{}
	c, err := openContent(t.TempDir(), func(d controlstore.Digest) bool { return referenced[d] })
	if err != nil {
		t.Fatal(err)
	}
	a, b, x := object(controlstore.KindRevision, "a"), object(controlstore.KindRevision, "b"), object(controlstore.KindRevision, "x")
	for _, o := range []controlstore.Object{a, b, x} {
		if err := c.put(o); err != nil {
			t.Fatal(err)
		}
	}
	c.release([]controlstore.Digest{x.Digest()})
	if c.has(x.Digest()) {
		t.Error("an unpinned release kept the object")
	}
	unpin1 := c.pin()
	unpin2 := c.pin()
	c.release([]controlstore.Digest{a.Digest(), b.Digest()})
	if !c.has(a.Digest()) || !c.has(b.Digest()) {
		t.Fatal("a pinned store deleted released objects")
	}
	referenced[b.Digest()] = true
	unpin1()
	unpin1()
	if !c.has(a.Digest()) {
		t.Error("the first unpin of two deleted objects")
	}
	unpin2()
	if c.has(a.Digest()) {
		t.Error("the last unpin kept an unreferenced object")
	}
	if !c.has(b.Digest()) {
		t.Error("the last unpin deleted an object a later entry referenced")
	}
}

// TestContentOpenRemovesTemporaries removes interrupted writes.
func TestContentOpenRemovesTemporaries(t *testing.T) {
	dir := t.TempDir()
	tmp := filepath.Join(dir, "sha256", "ab", "abcd.123.tmp")
	if err := os.MkdirAll(filepath.Dir(tmp), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmp, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openContent(dir, func(controlstore.Digest) bool { return false }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tmp); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a temporary file survived: %v", err)
	}
}
