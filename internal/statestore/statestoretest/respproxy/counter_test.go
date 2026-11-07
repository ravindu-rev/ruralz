// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package respproxy

import (
	"errors"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/testkit/faultproxy"
)

// conn drives one connection's hooks the way the fault proxy's pumps do:
// each chunk read goes to Hold first, then to Tap once every chunk read
// before it went and its own hold, if Hold took one, was released.
type conn struct {
	h faultproxy.ConnHooks

	mu        sync.Mutex
	queue     []*chunk // chunks read down, not yet forwarded
	forwarded []string // chunks forwarded down, in order
}

// chunk is one chunk read down: held until released.
type chunk struct {
	b        string
	released bool
}

func newConn(c *Counter, id uint64) *conn { return &conn{h: c.ConnTap(faultproxy.ConnInfo{ID: id})} }

func (cn *conn) up(s string) {
	if cn.h.Hold(faultproxy.Up, []byte(s), func() {}) {
		panic("respproxy: a command chunk was held")
	}
	cn.h.Tap(faultproxy.Up, []byte(s))
}

// down reads s from the server: it is forwarded at once unless Hold holds
// it or a chunk read before it still waits.
func (cn *conn) down(s string) {
	ch := &chunk{b: s}
	cn.mu.Lock()
	cn.queue = append(cn.queue, ch)
	cn.mu.Unlock()
	if !cn.h.Hold(faultproxy.Down, []byte(s), func() { cn.release(ch) }) {
		cn.release(ch)
	}
}

// release ends ch's hold and forwards every chunk it no longer blocks.
func (cn *conn) release(ch *chunk) {
	cn.mu.Lock()
	defer cn.mu.Unlock()
	ch.released = true
	for len(cn.queue) > 0 && cn.queue[0].released {
		next := cn.queue[0]
		cn.queue = cn.queue[1:]
		cn.h.Tap(faultproxy.Down, []byte(next.b))
		cn.forwarded = append(cn.forwarded, next.b)
	}
}

// sent returns the chunks forwarded down and how many still wait.
func (cn *conn) sent() ([]string, int) {
	cn.mu.Lock()
	defer cn.mu.Unlock()
	return slices.Clone(cn.forwarded), len(cn.queue)
}

// expectSent fails t unless the chunks forwarded down are want and wait
// chunks still wait.
func (cn *conn) expectSent(t *testing.T, when string, want []string, wait int) {
	t.Helper()
	if got, n := cn.sent(); !slices.Equal(got, want) || n != wait {
		t.Fatalf("%s: forwarded %q with %d waiting, want %q with %d", when, got, n, want, wait)
	}
}

func names(log []Command) []string {
	out := make([]string, len(log))
	for i, c := range log {
		out[i] = c.Name
	}
	return out
}

// fakeClock returns a clock that moves only when a test advances it.
func fakeClock() *clocktest.Fake { return clocktest.New(time.Unix(0, 0)) }

// held returns the chunks of replies c's reply delays hold right now.
func held(c *Counter) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.holds)
}

// TestCounterCountsCommandsAndReplies checks the totals and the log of
// one connection: commands by name, replies, error replies by code, and
// each command's connection, round trip and reply.
func TestCounterCountsCommandsAndReplies(t *testing.T) {
	c := newCounter(fakeClock(), logSize)
	cn := newConn(c, 7)
	cn.up(cmd("SET", "k", "v"))
	cn.down("+OK\r\n")
	cn.up(cmd("EVALSHA", "abc", "1", "rl:{x}"))
	cn.down("-NOSCRIPT No matching script. Please use EVAL.\r\n")
	cn.up(cmd("script", "load", "return 1"))
	cn.down("$40\r\ne0e1f9fabfc9d4800c877a703b823ac0578ff8db\r\n")
	cn.up("GET k\r\n")
	cn.down("$-1\r\n")

	s := c.Snapshot()
	if s.Commands != 4 || s.Replies != 4 || s.RoundTrips != 4 || s.Seq != 4 {
		t.Errorf("Snapshot = %+v, want 4 commands, replies and round trips", s)
	}
	if want := map[string]int64{"SET": 1, "EVALSHA": 1, "SCRIPT LOAD": 1, "GET": 1}; !maps.Equal(s.ByName, want) {
		t.Errorf("ByName = %v, want %v", s.ByName, want)
	}
	if s.Count("script  load") != 1 || s.Count("evalsha") != 1 || s.Count("EVAL") != 0 {
		t.Errorf("Count: SCRIPT LOAD %d, EVALSHA %d, EVAL %d; want 1, 1, 0", s.Count("script  load"), s.Count("evalsha"), s.Count("EVAL"))
	}
	if want := map[string]int64{"NOSCRIPT": 1}; !maps.Equal(s.Errors, want) {
		t.Errorf("Errors = %v, want %v", s.Errors, want)
	}
	log := c.LogSince(0)
	if got := names(log); !slices.Equal(got, []string{"SET", "EVALSHA", "SCRIPT LOAD", "GET"}) {
		t.Fatalf("log names = %q", got)
	}
	for i, want := range []Command{
		{seq: 1, Conn: 7, RoundTrip: 1, Name: "SET", Reply: Reply{Kind: '+'}},
		{
			seq: 2, Conn: 7, RoundTrip: 2, Name: "EVALSHA",
			Reply: Reply{Kind: '-', Err: "NOSCRIPT No matching script. Please use EVAL."},
		},
		{seq: 3, Conn: 7, RoundTrip: 3, Name: "SCRIPT LOAD", Reply: Reply{Kind: '$'}},
		{seq: 4, Conn: 7, RoundTrip: 4, Name: "GET", Reply: Reply{Kind: '$'}},
	} {
		got := log[i]
		if got != want {
			t.Errorf("log[%d] = %+v, want %+v", i, got, want)
		}
	}
}

// TestCounterCountsEachCommandAndReplyOnce checks that a pipelined batch
// counts each command and each reply exactly once, matched in order,
// however the proxy's reads cut the bytes and whether reply delays hold
// some of them or none: the read-time scan that decides the holds counts
// nothing. The batch is fed whole, a byte at a time and in uneven chunks,
// with no delay, a delay on EVALSHA and a delay on every reply; a RESP3
// push between the replies answers no command.
func TestCounterCountsEachCommandAndReplyOnce(t *testing.T) {
	var batch strings.Builder
	for i := range 5 {
		batch.WriteString(cmd("EVALSHA", "sha", "1", "rz:rl:flood:{"+strconv.Itoa(i)+"}", "1"))
	}
	batch.WriteString(cmd("GET", "g") + cmd("HMGET", "p", "n", "t", "e") + cmd("TIME") + cmd("EVALSHA", "sha", "0"))
	commands := batch.String()
	replies := ":1\r\n" + ":0\r\n" + "*3\r\n:1\r\n:0\r\n:1500\r\n" + "$5\r\nhello\r\n" +
		">2\r\n$10\r\ninvalidate\r\n_\r\n" + "-ERR one\r\n" +
		"_\r\n" + "*3\r\n_\r\n$3\r\nabc\r\n_\r\n" + "*2\r\n$10\r\n1700000000\r\n$6\r\n123456\r\n" +
		"-NOSCRIPT No matching script.\r\n"
	wantNames := []string{"EVALSHA", "EVALSHA", "EVALSHA", "EVALSHA", "EVALSHA", "GET", "HMGET", "TIME", "EVALSHA"}
	wantReplies := []Reply{
		{Kind: ':'},
		{Kind: ':'},
		{Kind: '*'},
		{Kind: '$'},
		{Kind: '-', Err: "ERR one"},
		{Kind: '_'},
		{Kind: '*'},
		{Kind: '*'},
		{Kind: '-', Err: "NOSCRIPT No matching script."},
	}
	cut := func(s string, sizes []int) []string {
		var out []string
		for i := 0; s != ""; i++ {
			n := min(len(s), sizes[i%len(sizes)])
			out = append(out, s[:n])
			s = s[n:]
		}
		return out
	}
	for _, chunking := range []struct {
		name  string
		sizes []int
	}{
		{"whole", []int{1 << 20}},
		{"a byte at a time", []int{1}},
		{"uneven chunks", []int{3, 7, 1, 11, 5, 2}},
	} {
		for _, delays := range []struct {
			name   string
			delays []ReplyDelay
		}{
			{"no delay", nil},
			{"EVALSHA held", []ReplyDelay{{Command: "EVALSHA", Delay: time.Second}}},
			{"every reply held", []ReplyDelay{{Delay: time.Second}}},
		} {
			t.Run(chunking.name+"/"+delays.name, func(t *testing.T) {
				clk := clocktest.New(time.Unix(0, 0))
				c := newCounter(clk, logSize)
				if err := c.SetReplyDelays(delays.delays...); err != nil {
					t.Fatal(err)
				}
				cn := newConn(c, 1)
				for _, b := range cut(commands, chunking.sizes) {
					cn.up(b)
				}
				for _, b := range cut(replies, chunking.sizes) {
					cn.down(b)
				}
				clk.Advance(time.Second)
				if got, waiting := cn.sent(); strings.Join(got, "") != replies || waiting != 0 {
					t.Fatalf("forwarded %q with %d chunks waiting, want every reply byte once", strings.Join(got, ""), waiting)
				}
				s := c.Snapshot()
				if s.Commands != 9 || s.Replies != 9 || s.Pushes != 1 || s.RoundTrips != 1 || s.Desyncs != 0 || held(c) != 0 {
					t.Errorf("Snapshot = %+v, want 9 commands and replies in one round trip, 1 push, none held", s)
				}
				if want := map[string]int64{"EVALSHA": 6, "GET": 1, "HMGET": 1, "TIME": 1}; !maps.Equal(s.ByName, want) {
					t.Errorf("ByName = %v, want %v", s.ByName, want)
				}
				if want := map[string]int64{"ERR": 1, "NOSCRIPT": 1}; !maps.Equal(s.Errors, want) {
					t.Errorf("Errors = %v, want %v", s.Errors, want)
				}
				log := c.LogSince(0)
				if got := names(log); !slices.Equal(got, wantNames) {
					t.Fatalf("log names = %q, want %q", got, wantNames)
				}
				for i, e := range log {
					if e.Reply != wantReplies[i] {
						t.Errorf("log[%d] %s reply = %+v, want %+v", i, e.Name, e.Reply, wantReplies[i])
					}
				}
			})
		}
	}
}

// TestCounterRoundTrips checks that commands pipelined behind a command
// still awaiting its reply share its round trip, that a command sent
// after every reply starts a new one, and that connections never share
// one.
func TestCounterRoundTrips(t *testing.T) {
	c := newCounter(fakeClock(), logSize)
	a, b := newConn(c, 1), newConn(c, 2)
	a.up(cmd("GET", "x") + cmd("HMGET", "h", "f")) // one write: pipelined
	b.up(cmd("PING"))
	a.down("$-1\r\n")
	a.up(cmd("PING")) // HMGET still awaits its reply
	a.down("*1\r\n$-1\r\n+PONG\r\n")
	b.down("+PONG\r\n")
	a.up(cmd("EVALSHA", "s", "0"))
	a.down(":1\r\n")
	b.up(cmd("PING"))
	b.down("+PONG\r\n")

	var got []uint64
	for _, e := range c.LogSince(0) {
		got = append(got, e.RoundTrip)
	}
	// GET, HMGET, PING (b), PING, EVALSHA, PING (b).
	if want := []uint64{1, 1, 2, 1, 3, 4}; !slices.Equal(got, want) {
		t.Errorf("round trips = %v, want %v", got, want)
	}
	if s := c.Snapshot(); s.RoundTrips != 4 || s.Replies != 6 {
		t.Errorf("Snapshot = %+v, want 4 round trips and 6 replies", s)
	}
}

// TestCounterSplitReplies checks that a reply split across chunks is
// counted once, when complete, and matched to its command.
func TestCounterSplitReplies(t *testing.T) {
	c := newCounter(fakeClock(), logSize)
	cn := newConn(c, 1)
	cn.up(cmd("GET", "k"))
	cn.down("$5\r\nhel")
	if s := c.Snapshot(); s.Replies != 0 {
		t.Fatalf("Replies = %d after half a reply, want 0", s.Replies)
	}
	cn.down("lo\r\n")
	if s := c.Snapshot(); s.Replies != 1 || c.LogSince(0)[0].Reply.Kind != '$' {
		t.Fatalf("Replies = %d, reply %+v; want 1 and a bulk string", s.Replies, c.LogSince(0)[0].Reply)
	}
}

// TestCounterPushesAnswerNoCommand checks that a RESP3 push between
// replies is counted apart and leaves the command-to-reply matching in
// step.
func TestCounterPushesAnswerNoCommand(t *testing.T) {
	c := newCounter(fakeClock(), logSize)
	cn := newConn(c, 1)
	cn.up(cmd("GET", "a") + cmd("INCR", "n"))
	cn.down(">2\r\n$10\r\ninvalidate\r\n*1\r\n$1\r\na\r\n$3\r\nbar\r\n:5\r\n")
	s := c.Snapshot()
	if s.Pushes != 1 || s.Replies != 2 {
		t.Errorf("Pushes %d, Replies %d; want 1 and 2", s.Pushes, s.Replies)
	}
	if log := c.LogSince(0); log[0].Reply.Kind != '$' || log[1].Reply.Kind != ':' {
		t.Errorf("replies %+v, %+v; want GET a bulk string and INCR an integer", log[0].Reply, log[1].Reply)
	}
}

// TestCounterDesync checks that a direction whose bytes stop parsing is
// counted once and then ignored, while other connections go on.
func TestCounterDesync(t *testing.T) {
	c := newCounter(fakeClock(), logSize)
	a, b := newConn(c, 1), newConn(c, 2)
	a.up(cmd("PING") + "*1\r\n:1\r\n")
	a.up(cmd("PING"))
	b.up(cmd("PING"))
	b.down("?garbage\r\n")
	b.down("+PONG\r\n")
	s := c.Snapshot()
	if s.Desyncs != 2 || s.Commands != 2 || s.Replies != 0 {
		t.Errorf("Snapshot = %+v, want 2 desyncs, 2 commands and no reply", s)
	}
}

// TestCounterLogBound checks that the log keeps the newest commands its
// size allows and counts the evicted ones, and that a reply to an evicted
// command is still counted.
func TestCounterLogBound(t *testing.T) {
	c := newCounter(fakeClock(), 2)
	cn := newConn(c, 1)
	cn.up(cmd("A") + cmd("B") + cmd("C"))
	cn.down("+1\r\n+2\r\n+3\r\n")
	if got := names(c.LogSince(0)); !slices.Equal(got, []string{"B", "C"}) {
		t.Errorf("Log = %q, want [B C]", got)
	}
	if got := names(c.LogSince(2)); !slices.Equal(got, []string{"C"}) {
		t.Errorf("LogSince(2) = %q, want [C]", got)
	}
	if got := c.LogSince(3); len(got) != 0 {
		t.Errorf("LogSince(3) = %v, want none", got)
	}
	if s := c.Snapshot(); s.LogEvicted != 1 || s.Replies != 3 || s.Commands != 3 {
		t.Errorf("Snapshot = %+v, want 1 evicted, 3 commands and 3 replies", s)
	}
	for _, e := range c.LogSince(0) {
		if e.Reply.Kind != '+' {
			t.Errorf("%s reply %+v, want a simple string", e.Name, e.Reply)
		}
	}
}

// TestCounterUnmatchedAfterOverflow checks that a connection with more
// than maxPending commands awaiting replies stops matching replies to
// commands but goes on counting them.
func TestCounterUnmatchedAfterOverflow(t *testing.T) {
	c := newCounter(fakeClock(), 4)
	cn := newConn(c, 1)
	n := maxPending + 1
	cn.up(strings.Repeat(cmd("PING"), n))
	cn.down(strings.Repeat("+PONG\r\n", n))
	if s := c.Snapshot(); s.Commands != int64(n) || s.Replies != int64(n) || s.RoundTrips != 1 {
		t.Errorf("Snapshot = %+v, want %d commands and replies in one round trip", s, n)
	}
	for _, e := range c.LogSince(0) {
		if e.Reply.Kind != 0 {
			t.Errorf("command %d matched reply %+v after the overflow", e.seq, e.Reply)
		}
	}
	cn.up(cmd("PING"))
	if s := c.Snapshot(); s.RoundTrips != 2 {
		t.Errorf("RoundTrips = %d after every reply, want a new round trip", s.RoundTrips)
	}
}

// TestSnapshotSub checks that Sub keeps only what changed, and keeps the
// later Seq.
func TestSnapshotSub(t *testing.T) {
	c := newCounter(fakeClock(), logSize)
	cn := newConn(c, 1)
	cn.up(cmd("GET", "a"))
	cn.down("-ERR x\r\n")
	before := c.Snapshot()
	cn.up(cmd("GET", "b") + cmd("EVALSHA", "s", "0"))
	cn.down("$-1\r\n-NOSCRIPT x\r\n")
	d := c.Snapshot().Sub(before)
	if d.Commands != 2 || d.Replies != 2 || d.RoundTrips != 1 || d.Seq != 3 {
		t.Errorf("Sub = %+v, want 2 commands and replies, 1 round trip, Seq 3", d)
	}
	if want := map[string]int64{"GET": 1, "EVALSHA": 1}; !maps.Equal(d.ByName, want) {
		t.Errorf("Sub ByName = %v, want %v", d.ByName, want)
	}
	if want := map[string]int64{"NOSCRIPT": 1}; !maps.Equal(d.Errors, want) {
		t.Errorf("Sub Errors = %v, want %v (ERR unchanged, so absent)", d.Errors, want)
	}
}

// TestCommandName checks names: upper case, with the subcommand of a
// container command.
func TestCommandName(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"get", "k"}, "GET"},
		{[]string{"EVALSHA", "s", "0"}, "EVALSHA"},
		{[]string{"script", "load", "return 1"}, "SCRIPT LOAD"},
		{[]string{"SCRIPT"}, "SCRIPT"},
		{[]string{"Client", "SetInfo", "LIB-NAME", "rueidis"}, "CLIENT SETINFO"},
		{[]string{"cluster", "slots"}, "CLUSTER SLOTS"},
		{[]string{"FT.SEARCH", "idx", "*"}, "FT.SEARCH"},
		{[]string{"HELLO", "3"}, "HELLO"},
	} {
		args := make([][]byte, len(tc.args))
		for i, a := range tc.args {
			args[i] = []byte(a)
		}
		if got := commandName(args); got != tc.want {
			t.Errorf("commandName(%q) = %q, want %q", tc.args, got, tc.want)
		}
	}
}

// TestReplyDelayHoldsMatchingReplies checks the slow reply fault: a reply
// to the named command is held for its delay on the injected clock and
// counted only once released, while replies to other commands pass.
func TestReplyDelayHoldsMatchingReplies(t *testing.T) {
	clk := clocktest.New(time.Unix(0, 0))
	c := newCounter(clk, logSize)
	if err := c.SetReplyDelays(ReplyDelay{Command: "evalsha", Delay: 200 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	cn := newConn(c, 1)
	cn.up(cmd("GET", "k"))
	cn.down("$-1\r\n")
	cn.expectSent(t, "the GET reply", []string{"$-1\r\n"}, 0)
	cn.up(cmd("EVALSHA", "s", "0"))
	cn.down(":1\r\n")
	if s := c.Snapshot(); held(c) != 1 || s.Replies != 1 {
		t.Fatalf("Snapshot = %+v with the EVALSHA reply read, want it held and 1 reply", s)
	}
	clk.Advance(199 * time.Millisecond)
	cn.expectSent(t, "before the delay", []string{"$-1\r\n"}, 1)
	clk.Advance(time.Millisecond)
	cn.expectSent(t, "after the delay", []string{"$-1\r\n", ":1\r\n"}, 0)
	if s := c.Snapshot(); s.Replies != 2 || held(c) != 0 {
		t.Fatalf("Snapshot = %+v after the delay, want 2 replies, none held", s)
	}
}

// TestReplyDelayHoldsEachReplyFromItsRead checks that the slow reply fault
// holds each reply its delay from the moment the proxy read it, as a
// server answering that much late would: replies
// to pipelined commands read 1 s apart and held 4 s are forwarded at 4 s
// and 5 s, side by side, not at 4 s and 8 s, one hold after the other; a
// reply read between them that no delay matches waits behind the first,
// the order read, and for no delay of its own.
func TestReplyDelayHoldsEachReplyFromItsRead(t *testing.T) {
	clk := clocktest.New(time.Unix(0, 0))
	c := newCounter(clk, logSize)
	if err := c.SetReplyDelays(ReplyDelay{Command: "EVALSHA", Delay: 4 * time.Second}); err != nil {
		t.Fatal(err)
	}
	cn := newConn(c, 1)
	cn.up(cmd("EVALSHA", "a", "0") + cmd("GET", "k") + cmd("EVALSHA", "b", "0"))
	cn.down(":1\r\n") // read at 0 s: held until 4 s
	clk.Advance(500 * time.Millisecond)
	cn.down("$-1\r\n") // read at 0.5 s: waits behind the first
	clk.Advance(500 * time.Millisecond)
	cn.down(":0\r\n") // read at 1 s: held until 5 s
	if s := c.Snapshot(); held(c) != 2 || s.Replies != 0 {
		t.Fatalf("Snapshot = %+v with every reply read, want both EVALSHA replies held and none counted", s)
	}
	clk.Advance(3 * time.Second)
	cn.expectSent(t, "at 4 s", []string{":1\r\n", "$-1\r\n"}, 1)
	if s := c.Snapshot(); held(c) != 1 || s.Replies != 2 {
		t.Fatalf("Snapshot = %+v at 4 s, want the second EVALSHA reply held and 2 replies", s)
	}
	clk.Advance(time.Second)
	cn.expectSent(t, "at 5 s", []string{":1\r\n", "$-1\r\n", ":0\r\n"}, 0)
	if s := c.Snapshot(); held(c) != 0 || s.Replies != 3 {
		t.Fatalf("Snapshot = %+v at 5 s, want none held and 3 replies", s)
	}
	for i, e := range c.LogSince(0) {
		if e.Reply.Kind == 0 {
			t.Errorf("log[%d] %s has no reply once every reply was forwarded", i, e.Name)
		}
	}
}

// TestReplyDelayEveryReplyAndLongest checks that a delay without a
// command holds every reply and that the longest matching delay wins.
func TestReplyDelayEveryReplyAndLongest(t *testing.T) {
	clk := clocktest.New(time.Unix(0, 0))
	c := newCounter(clk, logSize)
	if err := c.SetReplyDelays(ReplyDelay{Delay: 100 * time.Millisecond}, ReplyDelay{Command: "GET", Delay: 300 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	cn := newConn(c, 1)
	cn.up(cmd("PING"))
	cn.down("+PONG\r\n")
	cn.expectSent(t, "with the PING reply read", nil, 1)
	clk.Advance(100 * time.Millisecond)
	cn.expectSent(t, "after the PING delay", []string{"+PONG\r\n"}, 0)
	cn.up(cmd("GET", "k"))
	cn.down("$-1\r\n")
	clk.Advance(299 * time.Millisecond)
	cn.expectSent(t, "before the longest delay", []string{"+PONG\r\n"}, 1)
	clk.Advance(time.Millisecond)
	cn.expectSent(t, "after the longest delay", []string{"+PONG\r\n", "$-1\r\n"}, 0)
}

// TestReplyDelayMatchesAcrossChunks checks that a reply starting in a
// chunk that also ends an earlier reply is matched to its own command,
// not the earlier one, and that a chunk carrying only the rest of a reply
// waits for the chunk that started it.
func TestReplyDelayMatchesAcrossChunks(t *testing.T) {
	clk := clocktest.New(time.Unix(0, 0))
	c := newCounter(clk, logSize)
	if err := c.SetReplyDelays(ReplyDelay{Command: "EVALSHA", Delay: time.Second}); err != nil {
		t.Fatal(err)
	}
	cn := newConn(c, 1)
	cn.up(cmd("GET", "k") + cmd("EVALSHA", "s", "0") + cmd("GET", "j"))
	cn.down("$5\r\nhel") // the first GET reply starts: not held
	cn.down("lo\r\n:1\r\n$2\r\nh")
	cn.down("i\r\n") // the rest of the second GET reply
	if n := held(c); n != 1 {
		t.Fatalf("%d chunks held, want the chunk starting the EVALSHA reply held", n)
	}
	cn.expectSent(t, "while the EVALSHA reply is held", []string{"$5\r\nhel"}, 2)
	clk.Advance(time.Second)
	cn.expectSent(t, "after its delay", []string{"$5\r\nhel", "lo\r\n:1\r\n$2\r\nh", "i\r\n"}, 0)
	if s := c.Snapshot(); s.Replies != 3 {
		t.Fatalf("Replies = %d, want 3", s.Replies)
	}
}

// TestReplyDelayReleasedOnChangeAndClose checks that replacing the
// delays and closing the Counter release held replies at once and stop
// their timers, that a closed Counter holds nothing and refuses new
// delays, and that negative delays are refused.
func TestReplyDelayReleasedOnChangeAndClose(t *testing.T) {
	clk := clocktest.New(time.Unix(0, 0))
	c := newCounter(clk, logSize)
	if err := c.SetReplyDelays(ReplyDelay{Delay: time.Hour}); err != nil {
		t.Fatal(err)
	}
	cn := newConn(c, 1)
	cn.up(cmd("PING"))
	cn.down("+PONG\r\n")
	cn.expectSent(t, "held for an hour", nil, 1)
	if err := c.SetReplyDelays(ReplyDelay{Delay: time.Hour}); err != nil {
		t.Fatal(err)
	}
	// Released by the change, not by the clock.
	cn.expectSent(t, "after the change", []string{"+PONG\r\n"}, 0)

	cn.up(cmd("PING"))
	cn.down("+PONG\r\n")
	cn.expectSent(t, "held under the new delays", []string{"+PONG\r\n"}, 1)
	c.Close()
	cn.expectSent(t, "after Close", []string{"+PONG\r\n", "+PONG\r\n"}, 0)
	c.Close() // idempotent
	cn.up(cmd("PING"))
	cn.down("+PONG\r\n")
	cn.expectSent(t, "on a closed Counter", []string{"+PONG\r\n", "+PONG\r\n", "+PONG\r\n"}, 0)
	if err := c.SetReplyDelays(ReplyDelay{Delay: time.Hour}); !errors.Is(err, ErrClosed) {
		t.Errorf("SetReplyDelays after Close = %v, want ErrClosed", err)
	}
	if s := c.Snapshot(); s.Replies != 3 || held(c) != 0 {
		t.Errorf("Snapshot = %+v, want 3 replies, none held", s)
	}
	if n := clk.Pending(); n != 0 {
		t.Errorf("%d hold timers armed after Close, want none", n)
	}

	d := newCounter(clk, logSize)
	if err := d.SetReplyDelays(ReplyDelay{Command: "GET", Delay: -time.Millisecond}); err == nil {
		t.Error("negative delay: want an error")
	}
	cn = newConn(d, 1)
	cn.up(cmd("GET", "k"))
	cn.down("$-1\r\n")
	cn.expectSent(t, "after the refused delay", []string{"$-1\r\n"}, 0)
}

// TestReplyDelayDecidedUnderReplacedDelays checks that a hold decided
// under delays replaced before it was armed is never armed: a chunk read
// as the delays change is released at once, as the replies the change
// releases are.
func TestReplyDelayDecidedUnderReplacedDelays(t *testing.T) {
	clk := clocktest.New(time.Unix(0, 0))
	c := newCounter(clk, logSize)
	if err := c.SetReplyDelays(ReplyDelay{Delay: time.Hour}); err != nil {
		t.Fatal(err)
	}
	_, gen := c.rulesNow()
	if err := c.SetReplyDelays(); err != nil {
		t.Fatal(err)
	}
	if c.arm(time.Hour, gen, func() { t.Error("released a hold that was never armed") }) {
		t.Fatal("armed a hold decided under replaced delays")
	}
	if held(c) != 0 || clk.Pending() != 0 {
		t.Errorf("%d replies held with %d timers armed, want none", held(c), clk.Pending())
	}
}
