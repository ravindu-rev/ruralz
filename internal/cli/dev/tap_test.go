// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package dev

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/adminapi"
	"github.com/ravindu-rev/ruralz/internal/cli/adminclient"
	"github.com/ravindu-rev/ruralz/internal/cli/command"
	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
)

// Tests for ruralz dev tap against a fake Node: an httptest server whose
// /tap answers each subscription with the next scripted reply.

// Lines a fake Node streams, as /tap writes them.
const (
	fullEvent = `{"time":"2026-09-26T12:00:00.123Z","traceId":"0af7651916cd43dd8448eb211c80319c","listener":"public",` +
		`"protocol":"http2","route":"orders","method":"GET","host":"api.example","path":"/orders/7","status":429,` +
		`"code":"RZ-RL-002","durationMs":1.25,"requestBytes":0,"responseBytes":121,"upstream":"inventory",` +
		`"endpoint":"10.0.0.7:8080","consumer":"acme","requestHeaders":{"accept":["application/json"],` +
		`"authorization":["[REDACTED]"]},"responseHeaders":{"retry-after":["1"],"x-a":["1","2"]}}`
	sparseEvent = `{"time":"2026-09-26T12:00:01Z","traceId":"4bf92f3577b34da6a3ce929d0e0e4736","listener":"public",` +
		`"protocol":"http1","route":"_unmatched","method":"POST","host":"api.example","path":"/nowhere","status":404,` +
		`"durationMs":0.05,"requestBytes":12,"responseBytes":110}`
	okEvent = `{"time":"2026-09-26T12:00:02Z","traceId":"1","listener":"public","protocol":"http1","route":"orders",` +
		`"method":"GET","host":"api.example","path":"/orders","status":200,"durationMs":2,"requestBytes":0,"responseBytes":2,"upstream":"inventory"}`
	droppedThree = `{"dropped":3}`
	droppedOne   = `{"dropped":1}`
)

// Text forms of the lines above.
const (
	fullText = "2026-09-26T12:00:00.123Z 429 GET api.example/orders/7 route=orders upstream=inventory code=RZ-RL-002 1.25ms\n" +
		"  > accept: application/json\n" +
		"  > authorization: [REDACTED]\n" +
		"  < retry-after: 1\n" +
		"  < x-a: 1\n" +
		"  < x-a: 2\n"
	sparseText = "2026-09-26T12:00:01Z 404 POST api.example/nowhere route=_unmatched 0.05ms\n"
	okText     = "2026-09-26T12:00:02Z 200 GET api.example/orders route=orders upstream=inventory 2ms\n"
)

// reply is how the fake Node answers one subscription.
type reply struct {
	// status is the answer's status; 0 is 200 with the stream.
	status int
	// problem is the problem document of a refusal.
	problem string
	// lines are streamed after the headers; the stream then ends, unless
	// hold keeps it open until the client leaves.
	lines []string
	hold  bool
	// drop closes the connection without an answer.
	drop bool
}

// fakeNode is a Node's admin server with a scripted /tap.
type fakeNode struct {
	t   *testing.T
	srv *httptest.Server

	mu      sync.Mutex
	replies []reply
	// requests records each subscription's query and Authorization.
	requests []http.Header
	queries  []string
}

// newFakeNode serves the replies in order; when they run out, every
// further subscription holds an empty stream.
func newFakeNode(t *testing.T, replies ...reply) *fakeNode {
	t.Helper()
	n := &fakeNode{t: t, replies: replies}
	n.srv = httptest.NewServer(http.HandlerFunc(n.serve))
	t.Cleanup(n.srv.Close)
	return n
}

func (n *fakeNode) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != adminclient.PathTap {
		http.NotFound(w, r)
		return
	}
	n.mu.Lock()
	n.requests = append(n.requests, r.Header.Clone())
	n.queries = append(n.queries, r.URL.RawQuery)
	rep := reply{hold: true}
	if len(n.replies) > 0 {
		rep, n.replies = n.replies[0], n.replies[1:]
	}
	n.mu.Unlock()
	switch {
	case rep.drop:
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
		return
	case rep.status != 0:
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(rep.status)
		_, _ = io.WriteString(w, rep.problem)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	for _, l := range rep.lines {
		_, _ = io.WriteString(w, l+"\n")
	}
	w.(http.Flusher).Flush()
	if rep.hold {
		<-r.Context().Done()
	}
}

// subscriptions returns how many subscriptions the Node saw.
func (n *fakeNode) subscriptions() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.requests)
}

// request returns the headers and query of subscription i.
func (n *fakeNode) request(i int) (http.Header, string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.requests[i], n.queries[i]
}

// problemDoc is a problem document of the admin API.
func problemDoc(status int, code, title string) string {
	return fmt.Sprintf(`{"title":%q,"status":%d,"code":%q,"requestId":"4bf92f3577b34da6a3ce929d0e0e4736"}`, title, status, code)
}

// tapArgs returns ruralz dev tap against n with more flags.
func tapArgs(n *fakeNode, more ...string) []string {
	return append([]string{"dev", "tap", "--admin", n.srv.URL}, more...)
}

// TestTapTextForm checks the text form: one line per event, members that
// are empty left out, then one indented line per header value, request
// headers first, names sorted.
func TestTapTextForm(t *testing.T) {
	n := newFakeNode(t, reply{lines: []string{fullEvent, sparseEvent, okEvent}, hold: true})
	inv := start(t, testOptions(clocktest.New(epoch())), nil, tapArgs(n)...)
	want := fullText + sparseText + okText
	waitFor(t, "the events", func() bool { return inv.stdout.String() == want })
	inv.interrupt(os.Interrupt)
	if code := inv.wait(t); code != command.ExitInterrupted || inv.stderr.String() != "" {
		t.Errorf("dev tap = exit %d, stderr %q; want 130 and nothing", code, inv.stderr)
	}
	if got := inv.stdout.String(); got != want {
		t.Errorf("stdout =\n%s\nwant\n%s", got, want)
	}
}

// TestTapJSONForm checks --output json: every line as received, drop
// notices included, each of which also warns on standard error.
func TestTapJSONForm(t *testing.T) {
	n := newFakeNode(t, reply{lines: []string{fullEvent, droppedThree, sparseEvent, droppedOne}, hold: true})
	inv := start(t, testOptions(clocktest.New(epoch())), nil, tapArgs(n, "--output", "json")...)
	want := fullEvent + "\n" + droppedThree + "\n" + sparseEvent + "\n" + droppedOne + "\n"
	waitFor(t, "the lines", func() bool { return inv.stdout.String() == want })
	inv.interrupt(os.Interrupt)
	inv.wait(t)
	wantErr := "warning: the Node dropped 3 /tap events\nwarning: the Node dropped 1 /tap event\n"
	if got := inv.stderr.String(); got != wantErr {
		t.Errorf("stderr = %q, want %q", got, wantErr)
	}
	if got := inv.stdout.String(); got != want {
		t.Errorf("stdout =\n%s\nwant\n%s", got, want)
	}
}

// TestTapDroppedText checks that the text form warns on each drop notice
// and prints no line for it.
func TestTapDroppedText(t *testing.T) {
	n := newFakeNode(t, reply{lines: []string{droppedThree, okEvent}, hold: true})
	inv := start(t, testOptions(clocktest.New(epoch())), nil, tapArgs(n)...)
	waitFor(t, "the event", func() bool { return inv.stdout.String() == okText })
	inv.interrupt(os.Interrupt)
	inv.wait(t)
	if got := inv.stderr.String(); got != "warning: the Node dropped 3 /tap events\n" {
		t.Errorf("stderr = %q", got)
	}
}

// TestTapRouteFilter checks --route: only the events of that Route print,
// in both forms; drop notices are kept.
func TestTapRouteFilter(t *testing.T) {
	stream := []string{fullEvent, sparseEvent, droppedOne, okEvent}
	t.Run("text", func(t *testing.T) {
		n := newFakeNode(t, reply{lines: stream, hold: true})
		inv := start(t, testOptions(clocktest.New(epoch())), nil, tapArgs(n, "--route", "orders")...)
		want := fullText + okText
		waitFor(t, "the events", func() bool { return inv.stdout.String() == want })
		inv.interrupt(os.Interrupt)
		inv.wait(t)
	})
	t.Run("json", func(t *testing.T) {
		n := newFakeNode(t, reply{lines: stream, hold: true})
		inv := start(t, testOptions(clocktest.New(epoch())), nil, tapArgs(n, "--route", "_unmatched", "--output", "json")...)
		want := sparseEvent + "\n" + droppedOne + "\n"
		waitFor(t, "the lines", func() bool { return inv.stdout.String() == want })
		inv.interrupt(os.Interrupt)
		inv.wait(t)
	})
}

// TestTapSample checks ?sample=: 1 by default, --sample's value when
// given.
func TestTapSample(t *testing.T) {
	for _, c := range []struct {
		args  []string
		query string
	}{
		{nil, "sample=1"},
		{[]string{"--sample", "0.25"}, "sample=0.25"},
		{[]string{"--sample=1"}, "sample=1"},
		{[]string{"--sample", "1e-3"}, "sample=0.001"},
	} {
		n := newFakeNode(t)
		inv := start(t, testOptions(clocktest.New(epoch())), nil, tapArgs(n, c.args...)...)
		waitFor(t, "the subscription", func() bool { return n.subscriptions() == 1 })
		inv.interrupt(os.Interrupt)
		inv.wait(t)
		if _, q := n.request(0); q != c.query {
			t.Errorf("%q: query %q, want %q", c.args, q, c.query)
		}
	}
}

// TestTapUsage checks the usage errors: a --sample outside (0, 1] and an
// empty --route exit 2 before any subscription.
func TestTapUsage(t *testing.T) {
	n := newFakeNode(t)
	for _, c := range []struct {
		args   []string
		stderr string
	}{
		{[]string{"--sample", "0"}, `invalid value "0" for --sample: want a number in (0, 1]`},
		{[]string{"--sample", "1.5"}, `invalid value "1.5" for --sample: want a number in (0, 1]`},
		{[]string{"--sample", "-0.5"}, `invalid value "-0.5" for --sample: want a number in (0, 1]`},
		{[]string{"--sample", "NaN"}, `invalid value "NaN" for --sample: want a number in (0, 1]`},
		{[]string{"--sample", "all"}, `invalid value "all" for --sample: want a number in (0, 1]`},
		{[]string{"--route="}, "--route needs a Route name"},
	} {
		r := execute(t, testOptions(clocktest.New(epoch())), nil, tapArgs(n, c.args...)...)
		want := "ruralz dev tap: " + c.stderr + "\nRun 'ruralz dev tap --help' for usage.\n"
		r.want(t, fmt.Sprint(c.args), command.ExitNoResult, "", want)
	}
	if n.subscriptions() != 0 {
		t.Errorf("a usage error subscribed %d times", n.subscriptions())
	}
}

// TestTapFirstSubscriptionRefused checks that the first subscription must
// succeed: 503 RZ-RT-019 at the subscriber limit and 401 exit 2 with the
// problem line, the document on standard output with --output json.
func TestTapFirstSubscriptionRefused(t *testing.T) {
	limit := problemDoc(503, "RZ-RT-019", "The /tap subscriber limit is reached")
	unauth := problemDoc(401, "RZ-AUTH-001", "Unauthorized")
	for _, c := range []struct {
		name   string
		rep    reply
		json   bool
		stdout string
		line   string
	}{
		{"subscriber limit", reply{status: 503, problem: limit}, false, "", "503 RZ-RT-019 The /tap subscriber limit is reached"},
		{"subscriber limit, json", reply{status: 503, problem: limit}, true, limit + "\n", "503 RZ-RT-019 The /tap subscriber limit is reached"},
		{"unauthenticated", reply{status: 401, problem: unauth}, false, "", "401 RZ-AUTH-001 Unauthorized"},
	} {
		t.Run(c.name, func(t *testing.T) {
			n := newFakeNode(t, c.rep)
			args := tapArgs(n)
			if c.json {
				args = append(args, "--output", "json")
			}
			r := execute(t, testOptions(clocktest.New(epoch())), nil, args...)
			want := "ruralz dev tap: " + c.line + " (requestId 4bf92f3577b34da6a3ce929d0e0e4736)\n"
			r.want(t, c.name, command.ExitNoResult, c.stdout, want)
		})
	}
}

// TestTapFirstSubscriptionUnreachable: a Node that cannot be reached at
// the first subscription exits 2.
func TestTapFirstSubscriptionUnreachable(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	r := execute(t, testOptions(clocktest.New(epoch())), nil, "dev", "tap", "--admin", "http://"+addr)
	if r.code != command.ExitNoResult || r.stdout != "" || !strings.HasPrefix(r.stderr, "ruralz dev tap: GET http://"+addr+"/tap?sample=1: ") {
		t.Errorf("dev tap = exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
}

// TestTapResubscribes checks the reconnection: when the Node ends the
// stream the command says so, waits 1 s, reads the token file again and
// subscribes again; after an accepted subscription the backoff starts
// over at 1 s.
func TestTapResubscribes(t *testing.T) {
	clk := clocktest.New(epoch())
	tok := tokenFile(t, testToken)
	n := newFakeNode(t,
		reply{lines: []string{okEvent}},
		reply{lines: []string{sparseEvent}},
		reply{lines: []string{fullEvent}, hold: true},
	)
	inv := start(t, testOptions(clk), nil, tapArgs(n, "--admin-token-file", tok)...)
	ended := "ruralz dev tap: GET " + n.srv.URL + "/tap?sample=1: the Node ended the /tap stream; subscribing again in 1s\n"
	again := "ruralz dev tap: subscribed again to " + n.srv.URL + "\n"

	waitFor(t, "the first end", func() bool { return inv.stderr.String() == ended && clk.Pending() == 1 })
	if err := os.WriteFile(tok, []byte(rotatedToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Second)
	waitFor(t, "the second end", func() bool { return inv.stderr.String() == ended+again+ended && clk.Pending() == 1 })
	clk.Advance(time.Second)
	waitFor(t, "the third stream", func() bool { return inv.stdout.String() == okText+sparseText+fullText })
	inv.interrupt(syscall.SIGTERM)
	if code := inv.wait(t); code != command.ExitTerminated {
		t.Errorf("dev tap = exit %d, want 143", code)
	}
	if got := inv.stderr.String(); got != ended+again+ended+again {
		t.Errorf("stderr =\n%s", got)
	}
	for i, want := range []string{testToken, rotatedToken, rotatedToken} {
		h, _ := n.request(i)
		if got := h.Get("Authorization"); got != "Bearer "+want {
			t.Errorf("subscription %d: Authorization %q, want the token file's then", i, got)
		}
	}
}

// TestTapBackoff checks the backoff while the Node cannot be reached: from
// 1 s, doubling to at most 30 s, and back to 1 s once a subscription is
// accepted.
func TestTapBackoff(t *testing.T) {
	clk := clocktest.New(epoch())
	replies := []reply{{lines: []string{okEvent}}}
	for range 7 {
		replies = append(replies, reply{drop: true})
	}
	replies = append(replies, reply{lines: []string{sparseEvent}}, reply{drop: true})
	n := newFakeNode(t, replies...)
	inv := start(t, testOptions(clk), nil, tapArgs(n)...)
	waits := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second, 30 * time.Second, time.Second, 2 * time.Second}
	for i, d := range waits {
		want := fmt.Sprintf("; subscribing again in %s\n", command.FormatDuration(d))
		waitFor(t, fmt.Sprintf("wait %d", i), func() bool {
			return strings.Count(inv.stderr.String(), "subscribing again") == i+1 && clk.Pending() == 1
		})
		if got := lines(inv.stderr.String()); !strings.HasSuffix(got[len(got)-1]+"\n", want) {
			t.Fatalf("wait %d: last line %q, want it to end %q", i, got[len(got)-1], want)
		}
		if i == len(waits)-1 {
			break
		}
		clk.Advance(d)
	}
	if got := inv.stdout.String(); got != okText+sparseText {
		t.Errorf("stdout = %q", got)
	}
	inv.interrupt(os.Interrupt)
	if code := inv.wait(t); code != command.ExitInterrupted {
		t.Errorf("dev tap = exit %d, want 130", code)
	}
}

// TestTapLaterRefusal checks that a later subscription the Node refuses,
// with 401 or 503 RZ-RT-019, exits 2.
func TestTapLaterRefusal(t *testing.T) {
	for _, c := range []struct {
		name string
		rep  reply
		line string
	}{
		{"unauthenticated", reply{status: 401, problem: problemDoc(401, "RZ-AUTH-002", "Unauthorized")}, "401 RZ-AUTH-002 Unauthorized"},
		{"subscriber limit", reply{status: 503, problem: problemDoc(503, "RZ-RT-019", "Busy")}, "503 RZ-RT-019 Busy"},
	} {
		t.Run(c.name, func(t *testing.T) {
			clk := clocktest.New(epoch())
			n := newFakeNode(t, reply{lines: []string{okEvent}}, c.rep)
			inv := start(t, testOptions(clk), nil, tapArgs(n)...)
			waitFor(t, "the end of the stream", func() bool {
				return strings.Contains(inv.stderr.String(), "subscribing again in 1s") && clk.Pending() == 1
			})
			clk.Advance(time.Second)
			if code := inv.wait(t); code != command.ExitNoResult {
				t.Errorf("dev tap = exit %d, want 2", code)
			}
			if got := lines(inv.stderr.String()); got[len(got)-1] != "ruralz dev tap: "+c.line+" (requestId 4bf92f3577b34da6a3ce929d0e0e4736)" {
				t.Errorf("stderr =\n%s", inv.stderr)
			}
		})
	}
}

// TestTapInterruptWhileWaiting: Ctrl-C ends the backoff wait, exit 130.
func TestTapInterruptWhileWaiting(t *testing.T) {
	clk := clocktest.New(epoch())
	n := newFakeNode(t, reply{lines: []string{okEvent}})
	inv := start(t, testOptions(clk), nil, tapArgs(n)...)
	waitFor(t, "the backoff", func() bool { return clk.Pending() == 1 && strings.Contains(inv.stderr.String(), "subscribing again") })
	inv.interrupt(os.Interrupt)
	if code := inv.wait(t); code != command.ExitInterrupted {
		t.Errorf("dev tap = exit %d, want 130", code)
	}
}

// TestTapEnvironmentDefaults checks that RURALZ_ADMIN_URL and
// RURALZ_ADMIN_TOKEN_FILE stand in for absent flags, and that a flag wins.
func TestTapEnvironmentDefaults(t *testing.T) {
	n := newFakeNode(t)
	tok := tokenFile(t, testToken)
	inv := start(t, testOptions(clocktest.New(epoch())), []string{"RURALZ_ADMIN_URL=" + n.srv.URL, "RURALZ_ADMIN_TOKEN_FILE=" + tok}, "dev", "tap")
	waitFor(t, "the subscription", func() bool { return n.subscriptions() == 1 })
	inv.interrupt(os.Interrupt)
	inv.wait(t)
	if h, _ := n.request(0); h.Get("Authorization") != "Bearer "+testToken {
		t.Errorf("Authorization %q, want the environment's token", h.Get("Authorization"))
	}
	inv = start(t, testOptions(clocktest.New(epoch())), []string{"RURALZ_ADMIN_URL=http://127.0.0.1:1", "RURALZ_ADMIN_TOKEN_FILE=" + tok},
		"dev", "tap", "--admin", n.srv.URL, "--admin-token-file=")
	waitFor(t, "the second subscription", func() bool { return n.subscriptions() == 2 })
	inv.interrupt(os.Interrupt)
	inv.wait(t)
	if h, _ := n.request(1); h.Get("Authorization") != "" {
		t.Errorf("an empty --admin-token-file sent %q", h.Get("Authorization"))
	}
}

// TestTapColors checks the status colors on a terminal that takes color:
// 2xx and 3xx green, 4xx yellow, 5xx red; NO_COLOR, TERM=dumb and JSON
// stay plain.
func TestTapColors(t *testing.T) {
	ev := func(status int) string {
		return fmt.Sprintf(`{"time":"t","route":"r","method":"GET","host":"h","path":"/","status":%d,"durationMs":1}`, status)
	}
	stream := []string{ev(200), ev(302), ev(404), ev(503), ev(101)}
	colored := "t \x1b[32m200\x1b[0m GET h/ route=r 1ms\n" +
		"t \x1b[32m302\x1b[0m GET h/ route=r 1ms\n" +
		"t \x1b[33m404\x1b[0m GET h/ route=r 1ms\n" +
		"t \x1b[31m503\x1b[0m GET h/ route=r 1ms\n" +
		"t 101 GET h/ route=r 1ms\n"
	plain := strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(colored,
		"\x1b[32m", ""), "\x1b[33m", ""), "\x1b[31m", ""), "\x1b[0m", "")
	terminal := func(clk *clocktest.Fake) Options {
		o := testOptions(clk)
		o.terminal = func(io.Writer) bool { return true }
		return o
	}
	for _, c := range []struct {
		name    string
		environ []string
		json    bool
		want    string
	}{
		{"terminal", []string{"TERM=xterm-256color"}, false, colored},
		{"NO_COLOR", []string{"NO_COLOR=1"}, false, plain},
		{"dumb terminal", []string{"TERM=dumb"}, false, plain},
		{"json", nil, true, strings.Join(stream, "\n") + "\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			n := newFakeNode(t, reply{lines: stream, hold: true})
			args := tapArgs(n)
			if c.json {
				args = append(args, "--output", "json")
			}
			inv := start(t, terminal(clocktest.New(epoch())), c.environ, args...)
			waitFor(t, "the events", func() bool { return inv.stdout.String() == c.want })
			inv.interrupt(os.Interrupt)
			inv.wait(t)
		})
	}
}

// TestFormatEvent checks the text form's edge cases: members that are
// empty left out, a zero duration kept, control characters in text the
// Node chose replaced, so a line cannot break or drive a terminal.
func TestFormatEvent(t *testing.T) {
	for _, c := range []struct {
		name string
		ev   adminapi.TapEvent
		want string
	}{
		{"empty", adminapi.TapEvent{}, "0ms"},
		{"no host or path", adminapi.TapEvent{Time: "t", Status: 200, Method: "GET", Route: "r"}, "t 200 GET route=r 0ms"},
		{"control characters", adminapi.TapEvent{Time: "t", Path: "/a\x1b[31mb", Route: "r\nx", DurationMs: 0.5}, "t /a�[31mb route=r�x 0.5ms"},
		{"invalid UTF-8", adminapi.TapEvent{Route: "r\xff"}, "route=r� 0ms"},
		{"header value", adminapi.TapEvent{Route: "r", RequestHeaders: map[string][]string{"x": {"a\rb"}}}, "route=r 0ms\n  > x: a�b"},
	} {
		if got := string(formatEvent(c.ev, false)); got != c.want {
			t.Errorf("%s: formatEvent = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestDecodeTapLine checks the two shapes of a /tap line and the rest.
func TestDecodeTapLine(t *testing.T) {
	ev, dropped, ok := decodeTapLine([]byte(okEvent))
	if !ok || dropped != -1 || ev.Route != "orders" || ev.Status != 200 {
		t.Errorf("event = %+v, %d, %v", ev, dropped, ok)
	}
	if _, dropped, ok := decodeTapLine([]byte(droppedThree)); !ok || dropped != 3 {
		t.Errorf("drop notice = %d, %v", dropped, ok)
	}
	for _, bad := range []string{"", "not json", "[1]", `{"status":"x"}`, `{"dropped":`} {
		if _, _, ok := decodeTapLine([]byte(bad)); ok {
			t.Errorf("decodeTapLine(%q) accepted", bad)
		}
	}
}

// TestTapSkipsUndecodableLines: the text form warns on a line that is not
// an event and goes on; --output json re-emits it as received.
func TestTapSkipsUndecodableLines(t *testing.T) {
	n := newFakeNode(t, reply{lines: []string{"garbage", okEvent}, hold: true})
	inv := start(t, testOptions(clocktest.New(epoch())), nil, tapArgs(n)...)
	waitFor(t, "the event", func() bool { return inv.stdout.String() == okText })
	inv.interrupt(os.Interrupt)
	inv.wait(t)
	if got := inv.stderr.String(); got != "warning: skipping a /tap line that is not an event\n" {
		t.Errorf("stderr = %q", got)
	}
	n = newFakeNode(t, reply{lines: []string{"garbage", okEvent}, hold: true})
	inv = start(t, testOptions(clocktest.New(epoch())), nil, tapArgs(n, "--output", "json")...)
	waitFor(t, "the lines", func() bool { return inv.stdout.String() == "garbage\n"+okEvent+"\n" })
	inv.interrupt(os.Interrupt)
	inv.wait(t)
}

// TestUnreachable checks which failed subscriptions dev tap waits out.
func TestUnreachable(t *testing.T) {
	wrap := func(err error) error { return command.NoResultf("GET u: %w", err) }
	for _, c := range []struct {
		name string
		err  error
		want bool
	}{
		{"refused connection", wrap(&net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}), true},
		{"reset", wrap(syscall.ECONNRESET), true},
		{"end of stream", wrap(io.EOF), true},
		{"connect deadline", command.NoResult(&adminclient.DeadlineError{Kind: adminclient.DeadlineConnect}), true},
		{"problem document", command.NoResult(&adminclient.ProblemError{Status: 401}), false},
		{"redirect", command.NoResult(&adminclient.RedirectError{Status: 302}), false},
		{"status", command.NoResult(&adminclient.StatusError{Status: 500}), false},
		{"cleartext", wrap(adminclient.ErrCleartext), false},
		{"token file", command.NoResultf("--admin-token-file x: %w", os.ErrNotExist), false},
		{"usage", command.Usagef("bad"), false},
		{"other", errors.New("x"), false},
	} {
		if got := unreachable(c.err); got != c.want {
			t.Errorf("%s: unreachable = %v, want %v", c.name, got, c.want)
		}
	}
}
