// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package tap

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/problem"
)

// tapServer serves h on a loopback test server; served receives once per
// finished ServeHTTP call.
func tapServer(t *testing.T, h *Hub) (url string, served <-chan struct{}) {
	t.Helper()
	done := make(chan struct{}, MaxSubscribers+2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r)
		done <- struct{}{}
	}))
	t.Cleanup(srv.Close)
	return srv.URL, done
}

// get sends GET to url with the test's context.
func get(t *testing.T, url string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// TestStreamDeliversLines checks the subscription: a 200
// application/x-ndjson response that is never sniffed or cached,
// committed at once, then one line per published event in order, and the
// subscriber gone once the client leaves.
func TestStreamDeliversLines(t *testing.T) {
	h := New(Config{})
	url, served := tapServer(t, h)
	resp := get(t, url)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	for name, want := range map[string]string{
		"Content-Type":           ContentType,
		"X-Content-Type-Options": "nosniff",
		"Cache-Control":          "no-store",
	} {
		if got := resp.Header.Get(name); got != want {
			t.Errorf("%s %q, want %q", name, got, want)
		}
	}
	// The headers arrived, so the subscription exists.
	if !h.Active() {
		t.Fatal("not Active after the response headers")
	}
	full, _ := fullEvent()
	sparse, _ := sparseEvent()
	events := []Event{full, sparse, full}
	for i := range events {
		h.Publish(0, &events[i])
	}
	rd := bufio.NewReader(resp.Body)
	for i := range events {
		line, err := rd.ReadBytes('\n')
		if err != nil {
			t.Fatalf("line %d: %v", i, err)
		}
		if want := appendLine(nil, &events[i]); string(line) != string(want) {
			t.Fatalf("line %d = %s, want %s", i, line, want)
		}
	}
	_ = resp.Body.Close()
	<-served
	if h.Active() {
		t.Fatal("subscriber kept after the client left")
	}
}

// TestStreamRefusals checks the problem documents: 503 RZ-RT-019 once
// MaxSubscribers subscribe, 400 for an invalid ?sample=, each with the
// requestId of the hub's RequestID and no subscription left behind.
func TestStreamRefusals(t *testing.T) {
	h := New(Config{RequestID: func(*http.Request) string { return "4bf92f3577b34da6a3ce929d0e0e4736" }})
	url, served := tapServer(t, h)
	checkProblem(t, url+"?sample=1.5", http.StatusBadRequest, CodeInvalidSample)
	<-served
	if h.Active() {
		t.Fatal("an invalid sample subscribed")
	}
	var open []io.ReadCloser
	for range MaxSubscribers {
		open = append(open, subscribe(t, url+"?sample=0.5"))
	}
	checkProblem(t, url, http.StatusServiceUnavailable, CodeLimit)
	<-served
	if got := h.Subscribers(); got != MaxSubscribers {
		t.Fatalf("%d subscribers, want %d", got, MaxSubscribers)
	}
	_ = open[0].Close()
	<-served
	subscribe(t, url)
}

// subscribe opens a subscription that must be accepted; it is closed when
// the test ends.
func subscribe(t *testing.T, url string) io.ReadCloser {
	t.Helper()
	resp := get(t, url)
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("subscription refused: status %d", resp.StatusCode)
	}
	return resp.Body
}

// checkProblem sends GET to url and checks the problem document answer.
func checkProblem(t *testing.T, url string, status int, code string) {
	t.Helper()
	resp := get(t, url)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != status {
		t.Fatalf("status %d, want %d (%s)", resp.StatusCode, status, body)
	}
	if got := resp.Header.Get("Content-Type"); got != problem.ContentType {
		t.Errorf("Content-Type %q, want %q", got, problem.ContentType)
	}
	d, err := problem.Decode(body)
	if err != nil {
		t.Fatal(err)
	}
	if d.Code != code || d.Status != status || d.RequestID != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("problem %+v, want code %s status %d with the requestId", d, code, status)
	}
}

// TestInvalidSampleCode checks that an invalid ?sample= is 400 with
// RZ-RT-022.
func TestInvalidSampleCode(t *testing.T) {
	h := New(Config{RequestID: func(*http.Request) string { return "4bf92f3577b34da6a3ce929d0e0e4736" }})
	url, served := tapServer(t, h)
	checkProblem(t, url+"?sample=0", http.StatusBadRequest, "RZ-RT-022")
	<-served
}

// errNoDeadline is a write a deadlineWriter refuses: it had no deadline.
var errNoDeadline = errors.New("write without a deadline")

// deadlineWriter is a ResponseWriter that takes write deadlines on a fake
// clock, as a net/http connection does on the real one. It reports each
// write and flush on events, and each deadline it takes on set. With
// stall set, a write blocks until the clock passes its deadline and then
// fails with os.ErrDeadlineExceeded, as it does for a client that stopped
// reading.
type deadlineWriter struct {
	t      *testing.T
	clk    *clocktest.Fake
	header http.Header
	stall  bool
	events chan string
	set    chan struct{}

	mu        sync.Mutex
	deadline  time.Time
	deadlines []time.Time
	body      bytes.Buffer
}

func newDeadlineWriter(t *testing.T, clk *clocktest.Fake, stall bool) *deadlineWriter {
	return &deadlineWriter{t: t, clk: clk, header: http.Header{}, stall: stall, events: make(chan string, 16), set: make(chan struct{}, 16)}
}

func (w *deadlineWriter) Header() http.Header { return w.header }

func (w *deadlineWriter) WriteHeader(int) {}

func (w *deadlineWriter) SetWriteDeadline(d time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.deadline = d
	w.deadlines = append(w.deadlines, d)
	select {
	case w.set <- struct{}{}:
	default: // a reader waits for the count, not each signal
	}
	return nil
}

func (w *deadlineWriter) current() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.deadline
}

func (w *deadlineWriter) Write(b []byte) (int, error) {
	d := w.current()
	if d.IsZero() {
		w.t.Error("a write without a deadline")
		return 0, errNoDeadline
	}
	if w.stall {
		timer := w.clk.NewTimer(d.Sub(w.clk.Now()))
		w.events <- "stalled"
		<-timer.C()
		return 0, os.ErrDeadlineExceeded
	}
	w.mu.Lock()
	w.body.Write(b)
	w.mu.Unlock()
	w.events <- "write"
	return len(b), nil
}

func (w *deadlineWriter) FlushError() error {
	if w.current().IsZero() {
		w.t.Error("a flush without a deadline")
		return errNoDeadline
	}
	w.events <- "flush"
	return nil
}

func (w *deadlineWriter) recorded() []time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.deadlines)
}

// recordedN returns the deadlines once w took n of them: the handler
// clears a batch's deadline after its flush returned, so the flush event
// comes before that clear.
func (w *deadlineWriter) recordedN(n int) []time.Time {
	w.t.Helper()
	guard := time.NewTimer(10 * time.Second) // a hang guard only
	defer guard.Stop()
	for {
		if got := w.recorded(); len(got) >= n {
			return got
		}
		select {
		case <-w.set:
		case <-guard.C:
			w.t.Fatalf("deadlines %v, want %d of them", w.recorded(), n)
		}
	}
}

// serveTap runs h.ServeHTTP for w until the returned cancel; returned is
// closed when ServeHTTP returns.
func serveTap(t *testing.T, h *Hub, w http.ResponseWriter) (cancel context.CancelFunc, returned <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/tap", nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(w, req)
	}()
	return cancel, done
}

// next returns the next event of w.
func (w *deadlineWriter) next(want string) {
	w.t.Helper()
	if got := <-w.events; got != want {
		w.t.Fatalf("event %q, want %q", got, want)
	}
}

// TestEveryWriteHasADeadline checks the /tap write deadline: every write
// and flush to a subscriber runs under a WriteTimeout (10 s) deadline from
// the hub's clock, and the deadline is cleared between batches, so an idle
// subscription is never cut.
func TestEveryWriteHasADeadline(t *testing.T) {
	if WriteTimeout != 10*time.Second {
		t.Fatalf("WriteTimeout %v, want 10s", WriteTimeout)
	}
	start := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	clk := clocktest.New(start)
	h := New(Config{Clock: clk})
	w := newDeadlineWriter(t, clk, false)
	cancel, returned := serveTap(t, h, w)
	defer cancel()
	w.next("flush") // the commit
	clk.Advance(time.Minute)
	ev, _ := sparseEvent()
	h.Publish(0, &ev)
	w.next("write")
	w.next("flush")
	later := start.Add(time.Minute + WriteTimeout)
	want := []time.Time{start.Add(WriteTimeout), {}, later, later, {}}
	if got := w.recordedN(len(want)); !slices.EqualFunc(got, want, time.Time.Equal) {
		t.Fatalf("deadlines %v, want %v", got, want)
	}
	cancel()
	<-returned
	if got, want := w.body.String(), string(appendLine(nil, &ev)); got != want {
		t.Fatalf("body %q, want %q", got, want)
	}
}

// TestStalledSubscriberDropped checks the deadline's effect: a subscriber
// that stops reading keeps its write blocked until the deadline passes,
// then it is dropped and its slot freed.
func TestStalledSubscriberDropped(t *testing.T) {
	start := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	clk := clocktest.New(start)
	h := New(Config{Clock: clk})
	w := newDeadlineWriter(t, clk, true)
	cancel, returned := serveTap(t, h, w)
	defer cancel()
	w.next("flush")
	ev, _ := sparseEvent()
	h.Publish(0, &ev)
	w.next("stalled")
	select {
	case <-returned:
		t.Fatal("the subscriber was dropped before its deadline")
	default:
	}
	if got := w.current(); !got.Equal(start.Add(WriteTimeout)) {
		t.Fatalf("deadline %v, want %v", got, start.Add(WriteTimeout))
	}
	clk.Advance(WriteTimeout)
	<-returned
	if h.Active() {
		t.Fatal("the slot of a dropped subscriber was kept")
	}
}

// TestNoWriteWithoutDeadline checks that a response that cannot take a
// write deadline ends the subscription before anything is written.
func TestNoWriteWithoutDeadline(t *testing.T) {
	h := New(Config{})
	rec := httptest.NewRecorder() // no SetWriteDeadline
	cancel, returned := serveTap(t, h, rec)
	defer cancel()
	<-returned
	if h.Active() || rec.Flushed {
		t.Fatalf("subscription kept %v, flushed %v; want neither", h.Active(), rec.Flushed)
	}
}

// TestStreamEndsWithRequestContext checks the subscription's owner: when
// the request context ends (the admin server shutting down), ServeHTTP
// returns and the subscriber is removed.
func TestStreamEndsWithRequestContext(t *testing.T) {
	clk := clocktest.New(time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC))
	h := New(Config{Clock: clk})
	w := newDeadlineWriter(t, clk, false)
	cancel, returned := serveTap(t, h, w)
	defer cancel()
	w.next("flush")
	if !h.Active() {
		t.Fatal("no subscriber after the commit")
	}
	cancel()
	<-returned
	if h.Active() {
		t.Fatal("subscriber kept after the request context ended")
	}
}
