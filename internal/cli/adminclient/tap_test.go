// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package adminclient

import (
	"bufio"
	"errors"
	"io"
	"math"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/cli/command"
)

// Tests for the /tap stream: ?sample=, lines up to the Node's 1 MiB
// per-subscriber buffer with their newline, refusals as problem documents,
// the Node ending the stream, Close and Ctrl-C.

// tapServer streams the lines of its channel as NDJSON until the channel
// closes, then ends the response; it records each subscription.
type tapServer struct {
	url   string
	lines chan string
	rec   recorder
}

func newTapServer(t *testing.T) *tapServer {
	t.Helper()
	s := &tapServer{lines: make(chan string, 16)}
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		s.rec.record(r)
		w.Header().Set("Content-Type", "application/x-ndjson")
		rc := http.NewResponseController(w)
		_ = rc.Flush()
		for {
			select {
			case l, ok := <-s.lines:
				if !ok {
					return
				}
				_, _ = io.WriteString(w, l)
				_ = rc.Flush()
			case <-r.Context().Done():
				return
			}
		}
	})
	s.url = srv.URL
	return s
}

// TestTapLines checks that lines arrive in order without their newlines,
// events and {"dropped":N} notices alike, and that a line longer than the
// read buffer is assembled whole.
func TestTapLines(t *testing.T) {
	s := newTapServer(t)
	c, _ := newTestClient(t, Config{URL: s.url})
	st, err := c.Tap(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	long := `{"path":"/` + strings.Repeat("a", 3*tapReadBuffer) + `"}`
	want := []string{
		`{"time":"2026-09-26T12:00:00.123Z","route":"orders","status":200}`,
		`{"dropped":3}`,
		long,
		`{"time":"2026-09-26T12:00:01Z","route":"orders","status":404}` + "\r",
	}
	for _, l := range want {
		s.lines <- l + "\n"
	}
	for i, w := range want {
		got, err := st.Next(t.Context())
		if err != nil || string(got) != w {
			t.Fatalf("line %d = %.80q, %v, want %.80q", i, got, err, w)
		}
	}
	if got := s.rec.all(); len(got) != 1 || got[0].uri != "/tap?sample=1" {
		t.Errorf("subscriptions = %+v", got)
	}
}

// TestTapSample checks the ?sample= value sent, and that a rate outside
// (0, 1] is a usage error before any request.
func TestTapSample(t *testing.T) {
	s := newTapServer(t)
	for rate, want := range map[float64]string{1: "sample=1", 0.25: "sample=0.25", 1e-05: "sample=1e-05", 0.1: "sample=0.1"} {
		c, _ := newTestClient(t, Config{URL: s.url + "/prefix/"})
		st, err := c.Tap(t.Context(), rate)
		if err != nil {
			t.Fatalf("Tap(%v) = %v", rate, err)
		}
		_ = st.Close()
		got := s.rec.all()
		if last := got[len(got)-1].uri; last != "/prefix/tap?"+want {
			t.Errorf("Tap(%v) asked %q, want /prefix/tap?%s", rate, last, want)
		}
	}
	n := len(s.rec.all())
	c, _ := newTestClient(t, Config{URL: s.url})
	for _, rate := range []float64{0, -0.5, 1.5, math.NaN(), math.Inf(1)} {
		if _, err := c.Tap(t.Context(), rate); !isUsage(t, err) {
			t.Errorf("Tap(%v) = %v, want a usage error", rate, err)
		}
	}
	if len(s.rec.all()) != n {
		t.Error("an invalid rate sent a request")
	}
}

// TestTapLineBound checks the bound of a /tap line: 1 MiB with its
// newline is accepted, one byte more exits 2.
func TestTapLineBound(t *testing.T) {
	s := newTapServer(t)
	c, _ := newTestClient(t, Config{URL: s.url})
	st, err := c.Tap(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	fits := strings.Repeat("x", MaxTapLineBytes-1)
	s.lines <- fits + "\n"
	s.lines <- fits + "y\n"
	if got, err := st.Next(t.Context()); err != nil || len(got) != MaxTapLineBytes-1 {
		t.Fatalf("a line of exactly 1 MiB: %d bytes, %v", len(got), err)
	}
	_, err = st.Next(t.Context())
	if want := "GET " + s.url + "/tap?sample=1: a /tap line exceeds 1 MiB"; err == nil || err.Error() != want ||
		command.Code(t.Context(), err) != command.ExitNoResult || errors.Is(err, ErrStreamEnded) {
		t.Fatalf("a line over 1 MiB: %v, want %q", err, want)
	}
	if _, again := st.Next(t.Context()); !errors.Is(again, err) {
		t.Errorf("Next after the error = %v, want the same error", again)
	}
}

// TestTapStreamEnded checks the ends of a stream the Node closes: at a
// line boundary, inside a line, and without its final chunk.
func TestTapStreamEnded(t *testing.T) {
	t.Run("at a line boundary", func(t *testing.T) {
		s := newTapServer(t)
		c, _ := newTestClient(t, Config{URL: s.url})
		st, err := c.Tap(t.Context(), 1)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = st.Close() }()
		s.lines <- "{}\n"
		close(s.lines)
		if got, err := st.Next(t.Context()); err != nil || string(got) != "{}" {
			t.Fatalf("Next = %q, %v", got, err)
		}
		_, err = st.Next(t.Context())
		if !errors.Is(err, ErrStreamEnded) || command.Code(t.Context(), err) != command.ExitNoResult ||
			err.Error() != "GET "+s.url+"/tap?sample=1: the Node ended the /tap stream" {
			t.Errorf("Next at the end = %v", err)
		}
		if _, again := st.Next(t.Context()); !errors.Is(again, ErrStreamEnded) {
			t.Errorf("Next after the end = %v", again)
		}
	})
	t.Run("inside a line", func(t *testing.T) {
		s := newTapServer(t)
		c, _ := newTestClient(t, Config{URL: s.url})
		st, err := c.Tap(t.Context(), 1)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = st.Close() }()
		s.lines <- `{"time":"2026`
		close(s.lines)
		_, err = st.Next(t.Context())
		if !errors.Is(err, ErrStreamEnded) || !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("Next of a truncated line = %v", err)
		}
	})
	t.Run("connection lost", func(t *testing.T) {
		srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
			conn, buf, err := http.NewResponseController(w).Hijack()
			if err != nil {
				return
			}
			defer func() { _ = conn.Close() }()
			_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/x-ndjson\r\nTransfer-Encoding: chunked\r\n\r\n3\r\n{}\n\r\n")
			_ = buf.Flush()
		})
		c, _ := newTestClient(t, Config{URL: srv.URL})
		st, err := c.Tap(t.Context(), 1)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = st.Close() }()
		if got, err := st.Next(t.Context()); err != nil || string(got) != "{}" {
			t.Fatalf("Next = %q, %v", got, err)
		}
		if _, err := st.Next(t.Context()); !errors.Is(err, ErrStreamEnded) {
			t.Errorf("Next after the connection was lost = %v", err)
		}
	})
}

// TestTapRefused checks that a refused subscription is a ProblemError the
// caller can tell apart: 503 RZ-RT-019 at the subscriber limit, 401
// without a valid credential.
func TestTapRefused(t *testing.T) {
	for code, status := range map[string]int{"RZ-RT-019": http.StatusServiceUnavailable, "RZ-AUTH-001": http.StatusUnauthorized} {
		srv := serve(t, func(w http.ResponseWriter, r *http.Request) { writeProblem(w, r, code) })
		c, _ := newTestClient(t, Config{URL: srv.URL})
		st, err := c.Tap(t.Context(), 1)
		pe, ok := errors.AsType[*ProblemError](err)
		if st != nil || !ok || pe.Doc.Code != code || pe.Status != status || command.Code(t.Context(), err) != command.ExitNoResult {
			t.Errorf("Tap refused with %s = %v, %v", code, st, err)
		}
	}
}

// TestTapRereadsTokenPerSubscription checks that each subscription reads
// the token file again, as ruralz dev tap's resubscriptions need.
func TestTapRereadsTokenPerSubscription(t *testing.T) {
	s := newTapServer(t)
	path := tokenFile(t, testToken)
	c, _ := newTestClient(t, Config{URL: s.url, TokenFile: path})
	for _, tok := range []string{testToken, rotatedToken} {
		if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		st, err := c.Tap(t.Context(), 1)
		if err != nil {
			t.Fatal(err)
		}
		_ = st.Close()
	}
	got := s.rec.all()
	if len(got) != 2 || got[0].header.Get("Authorization") != "Bearer "+testToken ||
		got[1].header.Get("Authorization") != "Bearer "+rotatedToken {
		t.Errorf("subscriptions = %+v", got)
	}
}

// TestTapClose checks that Close ends a waiting Next at once and is
// idempotent.
func TestTapClose(t *testing.T) {
	s := newTapServer(t)
	c, _ := newTestClient(t, Config{URL: s.url})
	st, err := c.Tap(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	ch := make(chan error, 1)
	go func() {
		_, err := st.Next(t.Context())
		ch <- err
	}()
	_ = st.Close()
	select {
	case err := <-ch:
		if !errors.Is(err, ErrStreamEnded) {
			t.Errorf("Next after Close = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not end Next")
	}
	if err := st.Close(); err != nil {
		t.Errorf("second Close = %v", err)
	}
}

// TestTapInterrupt checks that Ctrl-C ends a waiting Next, exit 130, and
// that a canceled context passed to Next alone ends the stream too.
func TestTapInterrupt(t *testing.T) {
	for _, sameContext := range []bool{true, false} {
		s := newTapServer(t)
		c, _ := newTestClient(t, Config{URL: s.url})
		ctx, interrupt := interruptible(t)
		subscribe := ctx
		if !sameContext {
			subscribe = t.Context()
		}
		st, err := c.Tap(subscribe, 1)
		if err != nil {
			t.Fatal(err)
		}
		ch := make(chan error, 1)
		go func() {
			_, err := st.Next(ctx)
			ch <- err
		}()
		interrupt()
		select {
		case err := <-ch:
			wantInterrupted(ctx, t, err)
		case <-time.After(10 * time.Second):
			t.Fatal("Ctrl-C did not end Next")
		}
		_ = st.Close()
	}
}

// TestTapLineReader covers the line reader's edge cases directly.
func TestTapLineReader(t *testing.T) {
	read := func(input string, limit int64) ([]string, error) {
		s := &TapStream{r: bufio.NewReaderSize(strings.NewReader(input), 16), limit: limit}
		var out []string
		for {
			l, err := s.readLine()
			if err != nil {
				return out, err
			}
			out = append(out, string(l))
		}
	}
	cases := []struct {
		input string
		limit int64
		lines []string
		err   error
	}{
		{"", 8, nil, io.EOF},
		{"a\n\nbc\n", 8, []string{"a", "", "bc"}, io.EOF},
		{"1234567\n", 8, []string{"1234567"}, io.EOF},
		{"12345678\n", 8, nil, errLineTooLarge},
		{"0123456789abcdefghij\n", 64, []string{"0123456789abcdefghij"}, io.EOF},
		{"ok\npart", 64, []string{"ok"}, io.ErrUnexpectedEOF},
	}
	for _, c := range cases {
		lines, err := read(c.input, c.limit)
		if strings.Join(lines, "|") != strings.Join(c.lines, "|") || !errors.Is(err, c.err) {
			t.Errorf("readLine(%q, %d) = %q, %v, want %q, %v", c.input, c.limit, lines, err, c.lines, c.err)
		}
	}
}
