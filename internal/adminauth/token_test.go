// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package adminauth

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/errcode"
)

// Tests for the WP-17 "Done when" item "token rotation picked up": a
// changed token file is re-read on the next request presenting a token
// (or on Reload) and the new token replaces the old one; a rotation to an
// invalid token keeps the last valid one and is reported once; removing
// the file revokes its token. The start rules of spec 06 requirement 93
// hold for every re-read.

// authAs returns the kind a is admitting tok as on path, KindNone on error.
func authAs(t *testing.T, a *Authenticator, path, tok string) Kind {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
	r.RemoteAddr = "127.0.0.1:9"
	r.Header.Set("Authorization", "Bearer "+tok)
	p, err := a.Authenticate(r)
	if err != nil {
		return KindNone
	}
	return p.Kind
}

// replace writes content to a temporary file beside path and renames it
// over path (an atomic rotation).
func replace(t *testing.T, path, content string) {
	t.Helper()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

// rewrite writes content in place and moves the modification time on, so
// the change is visible whatever the file system's time granularity.
func rewrite(t *testing.T, path, content string) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	next := fi.ModTime().Add(time.Second)
	if err := os.Chtimes(path, next, next); err != nil {
		t.Fatal(err)
	}
}

func TestTokenRotationPickedUp(t *testing.T) {
	dir := t.TempDir()
	logs := &logBuffer{}
	opPath := writeFile(t, dir, "operator", operatorToken+"\n")
	mPath := writeFile(t, dir, "metrics", metricsToken)
	a, err := New(Settings{TokenFile: opPath, MetricsTokenFile: mPath}, Options{Logger: newLogger(logs)})
	if err != nil {
		t.Fatal(err)
	}
	if authAs(t, a, "/tap", operatorToken) != KindOperator || authAs(t, a, "/metrics", metricsToken) != KindMetrics {
		t.Fatal("initial tokens refused")
	}

	// Atomic rename.
	tokB := "rotated-B-" + strings.Repeat("b", 30)
	replace(t, opPath, tokB+"\n")
	if authAs(t, a, "/tap", tokB) != KindOperator {
		t.Fatal("rotated token refused after a rename")
	}
	if authAs(t, a, "/tap", operatorToken) != KindNone {
		t.Fatal("the old operator token still works")
	}

	// In place, another length.
	tokC := "rotated-C-" + strings.Repeat("c", 44)
	rewrite(t, opPath, tokC)
	if authAs(t, a, "/tap", tokC) != KindOperator || authAs(t, a, "/tap", tokB) != KindNone {
		t.Fatal("in-place rotation not picked up")
	}

	// In place, same length (the modification time moves).
	tokD := "rotated-D-" + strings.Repeat("d", 44)
	rewrite(t, opPath, tokD)
	if authAs(t, a, "/tap", tokD) != KindOperator || authAs(t, a, "/tap", tokC) != KindNone {
		t.Fatal("same-length rotation not picked up")
	}

	// The metrics token rotates independently.
	tokM := "metrics-rotated-" + strings.Repeat("m", 30)
	replace(t, mPath, tokM)
	if authAs(t, a, "/metrics", tokM) != KindMetrics || authAs(t, a, "/metrics", metricsToken) != KindNone {
		t.Fatal("metrics rotation not picked up")
	}
	if authAs(t, a, "/metrics", tokD) != KindOperator {
		t.Fatal("the operator token no longer admits /metrics")
	}
	if n := len(logs.lines("admin token rotated")); n != 4 {
		t.Fatalf("%d rotation records:\n%s", n, logs.String())
	}

	// A rotation to an invalid token keeps the last valid one, reported
	// once however many requests follow.
	for _, bad := range []string{"", "too-short", " leading-space-" + strings.Repeat("x", 30)} {
		before := len(logs.lines("admin token rotation failed, keeping the last valid token"))
		rewrite(t, opPath, bad)
		for range 3 {
			if authAs(t, a, "/tap", tokD) != KindOperator {
				t.Fatalf("rotation to %q dropped the last valid token", bad)
			}
		}
		if bad != "" && authAs(t, a, "/tap", strings.TrimSpace(bad)) != KindNone {
			t.Fatalf("invalid token %q admitted", bad)
		}
		if got := len(logs.lines("admin token rotation failed, keeping the last valid token")); got != before+1 {
			t.Fatalf("rotation to %q: %d failure records, want %d:\n%s", bad, got, before+1, logs.String())
		}
	}
	// Back to a valid token.
	tokE := "rotated-E-" + strings.Repeat("e", 40)
	rewrite(t, opPath, tokE)
	if authAs(t, a, "/tap", tokE) != KindOperator || authAs(t, a, "/tap", tokD) != KindNone {
		t.Fatal("recovery rotation not picked up")
	}

	// A deleted file revokes its token (RZ-AUTH-002, reported once at
	// warn); the metrics token is unaffected and a new file is picked up.
	if err := os.Remove(opPath); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/tap", nil)
		r.RemoteAddr = "127.0.0.1:9"
		r.Header.Set("Authorization", "Bearer "+tokE)
		_, err := a.Authenticate(r)
		if code, _ := errcode.CodeOf(err); code != CodeInvalid || !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("a deleted file's token: %v", err)
		}
	}
	revocations := logs.lines("admin token file removed, token revoked")
	if len(revocations) != 1 || !strings.Contains(revocations[0], `"level":"WARN"`) || !strings.Contains(revocations[0], EnvTokenFile) {
		t.Fatalf("revocation records:\n%s", logs.String())
	}
	if authAs(t, a, "/metrics", tokM) != KindMetrics {
		t.Fatal("revoking the operator token dropped the metrics token")
	}
	// Re-created with an invalid token while revoked: nothing is admitted,
	// and the failure record says that no token is in use rather than
	// claiming to keep the last valid one.
	kept := len(logs.lines("admin token rotation failed, keeping the last valid token"))
	writeFile(t, dir, "operator", "too-short")
	if authAs(t, a, "/tap", tokE) != KindNone {
		t.Fatal("the revoked token admitted after an invalid re-create")
	}
	if authAs(t, a, "/tap", "too-short") != KindNone {
		t.Fatal("an invalid re-created token admitted")
	}
	if n := len(logs.lines("admin token rotation failed, no token in use")); n != 1 {
		t.Fatalf("%d no-token failure records:\n%s", n, logs.String())
	}
	if n := len(logs.lines("admin token rotation failed, keeping the last valid token")); n != kept {
		t.Fatalf("%d keeping-the-last-token records, want %d:\n%s", n, kept, logs.String())
	}
	tokF := "rotated-F-" + strings.Repeat("f", 40)
	writeFile(t, dir, "operator", tokF)
	if authAs(t, a, "/tap", tokF) != KindOperator || authAs(t, a, "/tap", tokE) != KindNone {
		t.Fatal("re-created file not picked up")
	}
	// Deleted, then re-created with the same token: back in use.
	if err := os.Remove(opPath); err != nil {
		t.Fatal(err)
	}
	if authAs(t, a, "/tap", tokF) != KindNone {
		t.Fatal("a deleted file's token admitted")
	}
	writeFile(t, dir, "operator", tokF)
	if authAs(t, a, "/tap", tokF) != KindOperator {
		t.Fatal("re-created file with the same token not picked up")
	}
	if n := len(logs.lines("admin token file removed, token revoked")); n != 2 {
		t.Fatalf("%d revocation records", n)
	}
	// A re-created file with the same token is not a rotation.
	rotations := len(logs.lines("admin token rotated"))
	replace(t, opPath, tokF+"\n\n")
	if authAs(t, a, "/tap", tokF) != KindOperator || len(logs.lines("admin token rotated")) != rotations {
		t.Fatal("an unchanged token counted as a rotation")
	}
	for _, tok := range []string{operatorToken, metricsToken, tokB, tokC, tokD, tokE, tokF, tokM, "too-short"} {
		if strings.Contains(logs.String(), tok) {
			t.Fatalf("a log record holds a token:\n%s", logs.String())
		}
	}
}

// TestTokenRotationKubernetesSymlinks rotates a token the way a Kubernetes
// Secret volume does: token -> ..data/token, ..data -> a timestamped
// directory swapped atomically.
func TestTokenRotationKubernetesSymlinks(t *testing.T) {
	dir := t.TempDir()
	mount := func(name, tok string) {
		d := filepath.Join(dir, name)
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
		writeFile(t, d, "token", tok)
		tmp := filepath.Join(dir, "..data_tmp")
		if err := os.Symlink(name, tmp); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, filepath.Join(dir, "..data")); err != nil {
			t.Fatal(err)
		}
	}
	mount("..2026_09_26_01", operatorToken)
	if err := os.Symlink(filepath.Join("..data", "token"), filepath.Join(dir, "token")); err != nil {
		t.Fatal(err)
	}
	a, err := New(Settings{TokenFile: filepath.Join(dir, "token")}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if authAs(t, a, "/tap", operatorToken) != KindOperator {
		t.Fatal("initial token refused")
	}
	next := "k8s-rotated-" + strings.Repeat("k", 22) // same length as operatorToken
	if len(next) != len(operatorToken) {
		t.Fatalf("lengths %d and %d", len(next), len(operatorToken))
	}
	mount("..2026_09_26_02", next)
	if authAs(t, a, "/tap", next) != KindOperator || authAs(t, a, "/tap", operatorToken) != KindNone {
		t.Fatal("symlink swap not picked up")
	}
}

func TestReload(t *testing.T) {
	dir := t.TempDir()
	opPath := writeFile(t, dir, "operator", operatorToken)
	mPath := writeFile(t, dir, "metrics", metricsToken)
	logs := &logBuffer{}
	a, err := New(Settings{TokenFile: opPath, MetricsTokenFile: mPath}, Options{Logger: newLogger(logs)})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Reload(); err != nil {
		t.Fatalf("Reload of unchanged files: %v", err)
	}
	// Reload re-reads even when the file info did not change.
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	f, err := root.OpenFile("operator", os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	fi, _ := f.Stat()
	tokX := "X" + operatorToken[1:]
	if _, err := f.WriteAt([]byte("X"), 0); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if err := os.Chtimes(opPath, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := a.Reload(); err != nil {
		t.Fatal(err)
	}
	if authAs(t, a, "/tap", tokX) != KindOperator {
		t.Fatal("Reload did not pick up the rewritten token")
	}
	// Failures are returned, joined; an invalid file keeps its last
	// valid token, a removed file revokes its token.
	rewrite(t, opPath, "short")
	if err := os.Remove(mPath); err != nil {
		t.Fatal(err)
	}
	err = a.Reload()
	if !errors.Is(err, ErrTokenShort) || !errors.Is(err, os.ErrNotExist) || !errors.Is(err, ErrTokenRevoked) {
		t.Fatalf("Reload = %v", err)
	}
	if strings.Contains(err.Error(), "short\"") || authAs(t, a, "/tap", tokX) != KindOperator {
		t.Fatal("a failed Reload dropped the last valid operator token")
	}
	if authAs(t, a, "/metrics", metricsToken) != KindNone {
		t.Fatal("a removed metrics token file still admits its token")
	}
	// Every Reload reports a file that is still absent.
	if err := a.Reload(); !errors.Is(err, ErrTokenRevoked) {
		t.Fatalf("second Reload = %v", err)
	}
	// Reload never logs; the caller does.
	if strings.Contains(logs.String(), "rotation failed") && len(logs.lines("admin token rotation failed, keeping the last valid token")) > 1 {
		t.Fatalf("unexpected records:\n%s", logs.String())
	}
	// A settings-free authenticator reloads nothing.
	empty, err := New(Settings{}, Options{})
	if err != nil || empty.Reload() != nil {
		t.Fatal(err)
	}
}

func TestTryRefreshWhileBusy(t *testing.T) {
	p := writeFile(t, t.TempDir(), "o", operatorToken)
	tf, err := loadTokenFile(EnvTokenFile, p)
	if err != nil {
		t.Fatal(err)
	}
	rewrite(t, p, "busy-rotation-"+strings.Repeat("z", 30))
	tf.mu.Lock()
	res, err := tf.tryRefresh()
	tf.mu.Unlock()
	if res != busy || err != nil {
		t.Fatalf("tryRefresh while busy = %v, %v", res, err)
	}
	if res, err := tf.tryRefresh(); res != rotated || err != nil {
		t.Fatalf("tryRefresh = %v, %v", res, err)
	}
}

func TestRotationUnderConcurrentRequests(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "operator", operatorToken)
	a, err := New(Settings{TokenFile: p}, Options{Logger: newLogger(&logBuffer{})})
	if err != nil {
		t.Fatal(err)
	}
	tokens := []string{operatorToken, "concurrent-" + strings.Repeat("q", 30), "concurrent-" + strings.Repeat("r", 40)}
	var stop atomic.Bool
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			for n := 0; !stop.Load(); n++ {
				tok := tokens[(i+n)%len(tokens)]
				if k := authAs(t, a, "/metrics", tok); k != KindNone && k != KindOperator {
					t.Errorf("kind %v", k)
					return
				}
			}
		})
	}
	for i := range 30 {
		replace(t, p, tokens[i%len(tokens)])
	}
	stop.Store(true)
	wg.Wait()
	last := tokens[29%len(tokens)]
	if authAs(t, a, "/tap", last) != KindOperator {
		t.Fatal("the last rotation was not picked up")
	}
}

// TestNoRefusalRightAfterRotation: requests presenting the new token right
// after an atomic rename are all admitted, also those whose refresh found
// another refresh in progress (they wait for it and then refresh the file
// themselves instead of comparing with the old digest). Background
// requests with the metrics token refresh the operator file too, so some
// of those refreshes ran their stat before a rename.
func TestNoRefusalRightAfterRotation(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "operator", operatorToken)
	m := writeFile(t, dir, "metrics", metricsToken)
	a, err := New(Settings{TokenFile: p, MetricsTokenFile: m}, Options{Logger: newLogger(&logBuffer{})})
	if err != nil {
		t.Fatal(err)
	}
	var stop atomic.Bool
	var bg sync.WaitGroup
	for range 4 {
		bg.Go(func() {
			for !stop.Load() {
				if authAs(t, a, "/metrics", metricsToken) != KindMetrics {
					t.Error("the metrics token refused")
					return
				}
			}
		})
	}
	stopBackground := sync.OnceFunc(func() {
		stop.Store(true)
		bg.Wait()
	})
	defer stopBackground()
	const rounds, workers = 200, 8
	var refused atomic.Int64
	for i := range rounds {
		tok := fmt.Sprintf("rotation-%04d-%s", i, strings.Repeat("n", 24))
		replace(t, p, tok)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for range workers {
			wg.Go(func() {
				<-start
				if authAs(t, a, "/tap", tok) != KindOperator {
					refused.Add(1)
				}
			})
		}
		close(start)
		wg.Wait()
	}
	stopBackground()
	if n := refused.Load(); n != 0 {
		t.Fatalf("%d of %d requests with the current token refused right after a rotation", n, rounds*workers)
	}
}

// TestWaitForRefreshInProgress drives the wait path deterministically: a
// request whose refresh finds another in progress, and whose token
// matches nothing yet, waits for it, then refreshes the file itself, and
// is admitted with the token that refresh published.
func TestWaitForRefreshInProgress(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "operator", operatorToken)
	a, err := New(Settings{TokenFile: p}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	next := "waited-for-" + strings.Repeat("w", 30)
	replace(t, p, next)
	a.operator.mu.Lock() // a refresh in progress
	got := make(chan Kind, 1)
	go func() { got <- authAs(t, a, "/tap", next) }()
	select {
	case k := <-got:
		t.Fatalf("the request did not wait for the refresh in progress: %v", k)
	case <-time.After(50 * time.Millisecond):
	}
	_, err = a.operator.refreshLocked(false)
	a.operator.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	select {
	case k := <-got:
		if k != KindOperator {
			t.Fatalf("kind %v after the refresh in progress published the new token", k)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the request never finished")
	}
	// A token that matches without waiting does not wait.
	a.operator.mu.Lock()
	defer a.operator.mu.Unlock()
	if authAs(t, a, "/tap", next) != KindOperator {
		t.Fatal("the current token refused while a refresh is in progress")
	}
}

// TestBusyRefreshThatMissedTheRename: a refresh in progress that ran its
// stat before the rename publishes nothing new. A request with the new
// token that found it busy waits for it and then refreshes the file
// itself, so it is admitted instead of refused with RZ-AUTH-002.
func TestBusyRefreshThatMissedTheRename(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "operator", operatorToken)
	a, err := New(Settings{TokenFile: p}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	a.operator.mu.Lock() // a refresh that already ran its stat
	next := "missed-rename-" + strings.Repeat("m", 30)
	replace(t, p, next)
	got := make(chan Kind, 1)
	go func() { got <- authAs(t, a, "/tap", next) }()
	select {
	case k := <-got:
		t.Fatalf("the request did not wait for the refresh in progress: %v", k)
	case <-time.After(50 * time.Millisecond):
	}
	a.operator.mu.Unlock() // that refresh ends without publishing the rename
	select {
	case k := <-got:
		if k != KindOperator {
			t.Fatalf("kind %v, want the renamed token admitted", k)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the request never finished")
	}
}

// TestStatFailureKeepsLastToken: a stat failure other than a missing file
// (here ENOTDIR, a path component replaced by a file) keeps the last valid
// token and is reported once per episode.
func TestStatFailureKeepsLastToken(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	p := writeFile(t, sub, "token", operatorToken)
	logs := &logBuffer{}
	a, err := New(Settings{TokenFile: p}, Options{Logger: newLogger(logs)})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(sub); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "sub", "a file where a directory was")
	if _, err := os.Stat(p); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Skipf("stat through a file gives %v here", err)
	}
	for range 3 {
		if authAs(t, a, "/tap", operatorToken) != KindOperator {
			t.Fatal("a stat failure dropped the last valid token")
		}
	}
	if n := len(logs.lines("admin token rotation failed, keeping the last valid token")); n != 1 {
		t.Fatalf("%d failure records:\n%s", n, logs.String())
	}
	if len(logs.lines("admin token file removed, token revoked")) != 0 {
		t.Fatal("a stat failure revoked the token")
	}
}
