// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package respproxy

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/testkit/faultproxy"
)

// conn drives one connection's tap the way the fault proxy's pumps do.
type conn struct {
	tap faultproxy.TapFunc
}

func newConn(c *Counter, id uint64) conn { return conn{c.ConnTap(faultproxy.ConnInfo{ID: id})} }

func (cn conn) up(s string)   { cn.tap(faultproxy.Up, []byte(s)) }
func (cn conn) down(s string) { cn.tap(faultproxy.Down, []byte(s)) }

// downAsync runs down on its own goroutine, as a held reply blocks its
// pump, and returns a channel closed once the tap returned.
func (cn conn) downAsync(t *testing.T, s string) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		cn.down(s)
	}()
	t.Cleanup(func() { <-done })
	return done
}

func names(log []Command) []string {
	out := make([]string, len(log))
	for i, c := range log {
		out[i] = c.Name
	}
	return out
}

// TestCounterCountsCommandsAndReplies checks the totals and the log of
// one connection: commands by name, replies, error replies by code, and
// each command's arguments and reply.
func TestCounterCountsCommandsAndReplies(t *testing.T) {
	c := NewCounter(CounterConfig{})
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
	log := c.Log()
	if got := names(log); !slices.Equal(got, []string{"SET", "EVALSHA", "SCRIPT LOAD", "GET"}) {
		t.Fatalf("log names = %q", got)
	}
	for i, want := range []Command{
		{Seq: 1, Conn: 7, RoundTrip: 1, Name: "SET", Args: []string{"SET", "k", "v"}, Argc: 3, Reply: Reply{Kind: '+'}},
		{
			Seq: 2, Conn: 7, RoundTrip: 2, Name: "EVALSHA", Args: []string{"EVALSHA", "abc", "1", "rl:{x}"}, Argc: 4,
			Reply: Reply{Kind: '-', Err: "NOSCRIPT No matching script. Please use EVAL."},
		},
		{Seq: 3, Conn: 7, RoundTrip: 3, Name: "SCRIPT LOAD", Args: []string{"script", "load", "return 1"}, Argc: 3, Reply: Reply{Kind: '$'}},
		{Seq: 4, Conn: 7, RoundTrip: 4, Name: "GET", Args: []string{"GET", "k"}, Argc: 2, Reply: Reply{Kind: '$'}},
	} {
		got := log[i]
		if got.Seq != want.Seq || got.Conn != want.Conn || got.RoundTrip != want.RoundTrip || got.Name != want.Name ||
			!slices.Equal(got.Args, want.Args) || got.Argc != want.Argc || got.Cut || got.Reply != want.Reply {
			t.Errorf("log[%d] = %+v, want %+v", i, got, want)
		}
	}
}

// TestCounterRoundTrips checks that commands pipelined behind a command
// still awaiting its reply share its round trip, that a command sent
// after every reply starts a new one, and that connections never share
// one.
func TestCounterRoundTrips(t *testing.T) {
	c := NewCounter(CounterConfig{})
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
	for _, e := range c.Log() {
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
	c := NewCounter(CounterConfig{})
	cn := newConn(c, 1)
	cn.up(cmd("GET", "k"))
	cn.down("$5\r\nhel")
	if s := c.Snapshot(); s.Replies != 0 {
		t.Fatalf("Replies = %d after half a reply, want 0", s.Replies)
	}
	cn.down("lo\r\n")
	if s := c.Snapshot(); s.Replies != 1 || c.Log()[0].Reply.Kind != '$' {
		t.Fatalf("Replies = %d, reply %+v; want 1 and a bulk string", s.Replies, c.Log()[0].Reply)
	}
}

// TestCounterPushesAnswerNoCommand checks that a RESP3 push between
// replies is counted apart and leaves the command-to-reply matching in
// step.
func TestCounterPushesAnswerNoCommand(t *testing.T) {
	c := NewCounter(CounterConfig{})
	cn := newConn(c, 1)
	cn.up(cmd("GET", "a") + cmd("INCR", "n"))
	cn.down(">2\r\n$10\r\ninvalidate\r\n*1\r\n$1\r\na\r\n$3\r\nbar\r\n:5\r\n")
	s := c.Snapshot()
	if s.Pushes != 1 || s.Replies != 2 {
		t.Errorf("Pushes %d, Replies %d; want 1 and 2", s.Pushes, s.Replies)
	}
	if log := c.Log(); log[0].Reply.Kind != '$' || log[1].Reply.Kind != ':' {
		t.Errorf("replies %+v, %+v; want GET a bulk string and INCR an integer", log[0].Reply, log[1].Reply)
	}
}

// TestCounterDesync checks that a direction whose bytes stop parsing is
// counted once and then ignored, while other connections go on.
func TestCounterDesync(t *testing.T) {
	c := NewCounter(CounterConfig{})
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

// TestCounterLogBound checks that the log keeps the newest LogSize
// commands and counts the evicted ones, that a reply to an evicted
// command is still counted, and that a negative size keeps no log.
func TestCounterLogBound(t *testing.T) {
	c := NewCounter(CounterConfig{LogSize: 2})
	cn := newConn(c, 1)
	cn.up(cmd("A") + cmd("B") + cmd("C"))
	cn.down("+1\r\n+2\r\n+3\r\n")
	if got := names(c.Log()); !slices.Equal(got, []string{"B", "C"}) {
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
	for _, e := range c.Log() {
		if e.Reply.Kind != '+' {
			t.Errorf("%s reply %+v, want a simple string", e.Name, e.Reply)
		}
	}

	none := NewCounter(CounterConfig{LogSize: -1})
	cn = newConn(none, 1)
	cn.up(cmd("PING"))
	cn.down("+PONG\r\n")
	if s := none.Snapshot(); len(none.Log()) != 0 || s.Commands != 1 || s.Replies != 1 || s.LogEvicted != 0 {
		t.Errorf("no log: Log %v, Snapshot %+v; want no entries, one command and one reply", none.Log(), s)
	}
}

// TestCounterUnmatchedAfterOverflow checks that a connection with more
// than maxPending commands awaiting replies stops matching replies to
// commands but goes on counting them.
func TestCounterUnmatchedAfterOverflow(t *testing.T) {
	c := NewCounter(CounterConfig{LogSize: 4})
	cn := newConn(c, 1)
	n := maxPending + 1
	cn.up(strings.Repeat(cmd("PING"), n))
	cn.down(strings.Repeat("+PONG\r\n", n))
	if s := c.Snapshot(); s.Commands != int64(n) || s.Replies != int64(n) || s.RoundTrips != 1 {
		t.Errorf("Snapshot = %+v, want %d commands and replies in one round trip", s, n)
	}
	for _, e := range c.Log() {
		if e.Reply.Kind != 0 {
			t.Errorf("command %d matched reply %+v after the overflow", e.Seq, e.Reply)
		}
	}
	cn.up(cmd("PING"))
	if s := c.Snapshot(); s.RoundTrips != 2 {
		t.Errorf("RoundTrips = %d after every reply, want a new round trip", s.RoundTrips)
	}
}

// TestSnapshotSub checks that Sub keeps only what changed, and keeps the
// later Seq and Held.
func TestSnapshotSub(t *testing.T) {
	c := NewCounter(CounterConfig{})
	cn := newConn(c, 1)
	cn.up(cmd("GET", "a"))
	cn.down("-ERR x\r\n")
	before := c.Snapshot()
	cn.up(cmd("GET", "b") + cmd("EVALSHA", "s", "0"))
	cn.down("$-1\r\n-NOSCRIPT x\r\n")
	d := c.Snapshot().Sub(before)
	if d.Commands != 2 || d.Replies != 2 || d.RoundTrips != 1 || d.Seq != 3 || d.Held != 0 {
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

// startHeld sends command on cn, then its reply on a goroutine, and
// waits until the Counter holds that reply.
func startHeld(t *testing.T, c *Counter, cn conn, command, reply string) <-chan struct{} {
	t.Helper()
	cn.up(command)
	done := cn.downAsync(t, reply)
	waitFor(t, "a held reply", func() bool { return c.Snapshot().Held == 1 })
	return done
}

func isDone(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// TestReplyDelayHoldsMatchingReplies checks the slow reply fault: a reply
// to the named command is held for its delay on the injected clock and
// counted only once released, while replies to other commands pass.
func TestReplyDelayHoldsMatchingReplies(t *testing.T) {
	clk := clocktest.New(time.Unix(0, 0))
	c := NewCounter(CounterConfig{Clock: clk})
	if err := c.SetReplyDelays(ReplyDelay{Command: "evalsha", Delay: 200 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	cn := newConn(c, 1)
	cn.up(cmd("GET", "k"))
	cn.down("$-1\r\n") // not held: this goroutine would block
	done := startHeld(t, c, cn, cmd("EVALSHA", "s", "0"), ":1\r\n")
	clk.Advance(199 * time.Millisecond)
	if isDone(done) {
		t.Fatal("reply released before its delay")
	}
	if s := c.Snapshot(); s.Replies != 1 {
		t.Fatalf("Replies = %d while held, want 1", s.Replies)
	}
	clk.Advance(time.Millisecond)
	<-done
	if s := c.Snapshot(); s.Replies != 2 || s.Held != 0 {
		t.Fatalf("Snapshot = %+v after the delay, want 2 replies, none held", s)
	}
}

// TestReplyDelayEveryReplyAndLongest checks that a delay without a
// command holds every reply and that the longest matching delay wins.
func TestReplyDelayEveryReplyAndLongest(t *testing.T) {
	clk := clocktest.New(time.Unix(0, 0))
	c := NewCounter(CounterConfig{Clock: clk})
	if err := c.SetReplyDelays(ReplyDelay{Delay: 100 * time.Millisecond}, ReplyDelay{Command: "GET", Delay: 300 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	cn := newConn(c, 1)
	done := startHeld(t, c, cn, cmd("PING"), "+PONG\r\n")
	clk.Advance(100 * time.Millisecond)
	<-done
	done = startHeld(t, c, cn, cmd("GET", "k"), "$-1\r\n")
	clk.Advance(299 * time.Millisecond)
	if isDone(done) {
		t.Fatal("GET reply released before the longest delay")
	}
	clk.Advance(time.Millisecond)
	<-done
}

// TestReplyDelayMatchesAcrossChunks checks that a reply starting in a
// chunk that also ends an earlier reply is matched to its own command,
// not the earlier one.
func TestReplyDelayMatchesAcrossChunks(t *testing.T) {
	clk := clocktest.New(time.Unix(0, 0))
	c := NewCounter(CounterConfig{Clock: clk})
	if err := c.SetReplyDelays(ReplyDelay{Command: "EVALSHA", Delay: time.Second}); err != nil {
		t.Fatal(err)
	}
	cn := newConn(c, 1)
	cn.up(cmd("GET", "k") + cmd("EVALSHA", "s", "0"))
	cn.down("$5\r\nhel") // GET's reply starts: not held
	done := cn.downAsync(t, "lo\r\n:1\r\n")
	waitFor(t, "the EVALSHA reply held", func() bool { return c.Snapshot().Held == 1 })
	clk.Advance(time.Second)
	<-done
	if s := c.Snapshot(); s.Replies != 2 {
		t.Fatalf("Replies = %d, want 2", s.Replies)
	}
}

// TestReplyDelayReleasedOnChangeAndClose checks that replacing the
// delays and closing the Counter release held replies at once, that a
// closed Counter holds nothing and refuses new delays, and that negative
// delays are refused.
func TestReplyDelayReleasedOnChangeAndClose(t *testing.T) {
	clk := clocktest.New(time.Unix(0, 0))
	c := NewCounter(CounterConfig{Clock: clk})
	if err := c.SetReplyDelays(ReplyDelay{Delay: time.Hour}); err != nil {
		t.Fatal(err)
	}
	cn := newConn(c, 1)
	done := startHeld(t, c, cn, cmd("PING"), "+PONG\r\n")
	if err := c.SetReplyDelays(ReplyDelay{Delay: time.Hour}); err != nil {
		t.Fatal(err)
	}
	<-done // released by the change, not by the clock

	done = startHeld(t, c, cn, cmd("PING"), "+PONG\r\n")
	c.Close()
	<-done
	c.Close() // idempotent
	cn.up(cmd("PING"))
	cn.down("+PONG\r\n") // not held: this goroutine would block
	if err := c.SetReplyDelays(ReplyDelay{Delay: time.Hour}); !errors.Is(err, ErrClosed) {
		t.Errorf("SetReplyDelays after Close = %v, want ErrClosed", err)
	}
	if s := c.Snapshot(); s.Replies != 3 || s.Held != 0 {
		t.Errorf("Snapshot = %+v, want 3 replies, none held", s)
	}

	d := NewCounter(CounterConfig{Clock: clk})
	if err := d.SetReplyDelays(ReplyDelay{Command: "GET", Delay: -time.Millisecond}); err == nil {
		t.Error("negative delay: want an error")
	}
	cn = newConn(d, 1)
	cn.up(cmd("GET", "k"))
	cn.down("$-1\r\n") // the refused delay set nothing
}
