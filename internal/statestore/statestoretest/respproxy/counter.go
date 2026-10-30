// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package respproxy

import (
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/testkit/faultproxy"
)

// DefaultLogSize is the command log capacity.
const DefaultLogSize = 1024

// maxPending bounds the commands of one connection awaiting replies; past
// it the Counter stops matching that connection's replies to commands.
const maxPending = 1 << 16

// ErrClosed is returned by SetReplyDelays on a closed Counter.
var ErrClosed = errors.New("respproxy: closed")

// CounterConfig configures a Counter.
type CounterConfig struct {
	// LogSize bounds the command log; default DefaultLogSize. A negative
	// size keeps no log.
	LogSize int
	// Clock times reply delays and stamps each command's At; default
	// clock.Real().
	Clock clock.Clock
}

// Command is one command a client sent.
type Command struct {
	// Seq numbers the commands of the Counter from 1, in the order read.
	Seq uint64
	// Conn is the faultproxy connection ID.
	Conn uint64
	// RoundTrip numbers the Counter's round trips from 1: a command sent
	// while an earlier command of its connection still awaits its reply
	// shares that command's round trip; otherwise it starts a new one.
	RoundTrip uint64
	// Name is the upper-cased command name; a container command carries
	// its subcommand too, as in "SCRIPT LOAD".
	Name string
	// Args are the first 64 arguments as sent, the name included, each
	// cut to 256 bytes. Argc counts every argument, and Cut reports that
	// an argument was dropped or cut.
	Args []string
	Argc int64
	Cut  bool
	// At is when the Counter read the command, on its clock: a chaos run
	// counts a key's calls per window by it.
	At time.Time
	// Reply is the reply, once read.
	Reply Reply
}

// Reply sums up the reply to a Command.
type Reply struct {
	// Kind is the RESP type byte of the reply ('+', '-', ':', '$', '*',
	// '%' and so on); 0 until it is read, and for good once the Counter
	// stops matching the connection's replies.
	Kind byte
	// Err is the message of an error reply ('-' or '!'), cut to 256 bytes.
	Err string
}

// ReplyDelay holds the replies to one command before they are forwarded:
// the slow reply fault.
type ReplyDelay struct {
	// Command is the Name of the command whose replies are held, such as
	// "EVALSHA" or "SCRIPT LOAD"; "" holds every reply.
	Command string
	// Delay is how long each matching reply is held.
	Delay time.Duration
}

// Snapshot is a Counter's totals at one moment.
type Snapshot struct {
	// Seq is the Seq of the last command read; 0 before any.
	Seq uint64
	// Commands counts the commands read; ByName counts them by Name.
	Commands int64
	ByName   map[string]int64
	// RoundTrips is the number of round trips, the last Command.RoundTrip.
	RoundTrips uint64
	// Replies counts the replies read; Errors counts the error replies
	// among them by code, the first word of the message ("NOSCRIPT").
	Replies int64
	Errors  map[string]int64
	// Pushes counts RESP3 push frames, which answer no command.
	Pushes int64
	// Desyncs counts connection directions whose bytes stopped parsing.
	Desyncs int64
	// Held counts the connections whose replies a reply delay holds right
	// now.
	Held int64
	// LogEvicted counts the commands the log dropped to stay in bounds.
	LogEvicted int64
}

// Count returns the number of commands named name (case and spacing as
// in Command.Name do not matter).
func (s Snapshot) Count(name string) int64 { return s.ByName[normalizeName(name)] }

// Sub returns the counts accumulated since earlier, a Snapshot of the
// same Counter; Seq and Held stay s's.
func (s Snapshot) Sub(earlier Snapshot) Snapshot {
	d := s
	d.Commands -= earlier.Commands
	d.ByName = subCounts(s.ByName, earlier.ByName)
	d.RoundTrips -= earlier.RoundTrips
	d.Replies -= earlier.Replies
	d.Errors = subCounts(s.Errors, earlier.Errors)
	d.Pushes -= earlier.Pushes
	d.Desyncs -= earlier.Desyncs
	d.LogEvicted -= earlier.LogEvicted
	return d
}

func subCounts(a, b map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(a))
	for k, v := range a {
		if v -= b[k]; v != 0 {
			out[k] = v
		}
	}
	return out
}

// Counter parses the RESP traffic of faultproxy connections and counts
// it. ConnTap is its faultproxy.Config.ConnTap. Methods are safe for
// concurrent use.
type Counter struct {
	clk     clock.Clock
	closing chan struct{} // closed by Close

	mu         sync.Mutex
	closed     bool
	rules      []ReplyDelay  // replaced, never changed in place
	release    chan struct{} // closed when the rules change
	seq        uint64
	commands   int64
	byName     map[string]int64
	roundTrips uint64
	replies    int64
	errors     map[string]int64
	pushes     int64
	desyncs    int64
	held       int64
	log        []Command // a ring; Seq s sits at (s-1) % len(log)
	logLen     uint64
	evicted    int64
}

// NewCounter returns a Counter.
func NewCounter(c CounterConfig) *Counter {
	size := c.LogSize
	if size == 0 {
		size = DefaultLogSize
	}
	clk := c.Clock
	if clk == nil {
		clk = clock.Real()
	}
	return &Counter{
		clk:     clk,
		closing: make(chan struct{}),
		release: make(chan struct{}),
		byName:  map[string]int64{},
		errors:  map[string]int64{},
		log:     make([]Command, max(size, 0)),
	}
}

// ConnTap returns the tap of one proxied connection; pass it as
// faultproxy.Config.ConnTap. The tap parses commands flowing up and
// replies flowing down, and holds the replies that reply delays match
// before they are forwarded.
func (c *Counter) ConnTap(info faultproxy.ConnInfo) faultproxy.TapFunc {
	cs := &connState{c: c, id: info.ID}
	cs.up = newParser(false, cmdSink{cs})
	cs.down = newParser(true, replySink{cs})
	return cs.tap
}

// Snapshot returns the totals.
func (c *Counter) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Snapshot{
		Seq: c.seq, Commands: c.commands, ByName: maps.Clone(c.byName), RoundTrips: c.roundTrips,
		Replies: c.replies, Errors: maps.Clone(c.errors), Pushes: c.pushes, Desyncs: c.desyncs,
		Held: c.held, LogEvicted: c.evicted,
	}
}

// Log returns the logged commands, oldest first.
func (c *Counter) Log() []Command { return c.LogSince(0) }

// LogSince returns the logged commands with a Seq above seq (a
// Snapshot's Seq), oldest first.
func (c *Counter) LogSince(seq uint64) []Command {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []Command
	for s := max(c.seq-c.logLen, seq) + 1; s <= c.seq; s++ {
		out = append(out, c.log[(s-1)%uint64(len(c.log))])
	}
	return out
}

// SetReplyDelays replaces the reply delays; none clears them. A reply
// that several match is held for the longest, and replies held under the
// replaced delays are released at once. Replies are forwarded in the
// chunks the proxy reads, so a reply sharing a chunk with a held one
// waits with it, as replies queued behind a slow command do.
func (c *Counter) SetReplyDelays(delays ...ReplyDelay) error {
	var rules []ReplyDelay
	for _, d := range delays {
		if d.Delay < 0 {
			return fmt.Errorf("respproxy: negative reply delay %v for %q", d.Delay, d.Command)
		}
		rules = append(rules, ReplyDelay{Command: normalizeName(d.Command), Delay: d.Delay})
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrClosed
	}
	c.rules = rules
	close(c.release)
	c.release = make(chan struct{})
	return nil
}

// Close releases every held reply and turns reply delays off for good;
// counting goes on. It is idempotent. A Counter used as the ConnTap of a
// faultproxy.Proxy is closed before or with it, so no held reply delays
// the proxy's Close.
func (c *Counter) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		c.rules = nil
		close(c.closing)
	}
}

// currentRules returns the reply delays in force and the channel closed
// when they change.
func (c *Counter) currentRules() ([]ReplyDelay, <-chan struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rules, c.release
}

// hold blocks for d, or until release (from currentRules) closes or the
// Counter closes. The timer is armed before Held counts the reply, so a
// test that sees Held may advance a fake clock past it.
func (c *Counter) hold(d time.Duration, release <-chan struct{}) {
	t := c.clk.NewTimer(d)
	defer t.Stop()
	c.mu.Lock()
	c.held++
	c.mu.Unlock()
	select {
	case <-t.C():
	case <-release:
	case <-c.closing:
	}
	c.mu.Lock()
	c.held--
	c.mu.Unlock()
}

// record logs one command and returns its Seq; c.mu is held.
func (c *Counter) record(cmd Command) uint64 {
	c.seq++
	cmd.Seq = c.seq
	c.commands++
	c.byName[cmd.Name]++
	if n := uint64(len(c.log)); n > 0 {
		if c.logLen == n {
			c.evicted++
		} else {
			c.logLen++
		}
		c.log[(cmd.Seq-1)%n] = cmd
	}
	return cmd.Seq
}

// reply counts one reply and records it on the logged command seq (0:
// none).
func (c *Counter) reply(seq uint64, kind byte, msg string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.replies++
	if kind == '-' || kind == '!' {
		code, _, _ := strings.Cut(msg, " ")
		c.errors[code]++
	}
	if n := uint64(len(c.log)); seq != 0 && n > 0 {
		if e := &c.log[(seq-1)%n]; e.Seq == seq {
			e.Reply = Reply{Kind: kind, Err: msg}
		}
	}
}

func (c *Counter) push() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pushes++
}

func (c *Counter) desync() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.desyncs++
}

// pendingCmd is a command awaiting its reply.
type pendingCmd struct {
	seq  uint64
	name string
}

// eventKind is what a reply parser read.
type eventKind uint8

const (
	evStart eventKind = iota + 1
	evEnd
	evPush
)

// event is one thing read from a chunk of replies.
type event struct {
	kind  eventKind
	reply byte   // evEnd: the reply's type byte
	err   string // evEnd: an error reply's message
}

// connState is the RESP state of one proxied connection. Its two taps run
// on the connection's two pump goroutines; mu serializes them.
type connState struct {
	c        *Counter
	id       uint64
	up, down *parser

	mu        sync.Mutex
	pending   []pendingCmd // commands awaiting replies, oldest at head
	head      int
	unmatched bool  // pending overflowed: replies are no longer matched
	due       int64 // commands read minus replies forwarded
	roundTrip uint64
	events    []event // the replies of the chunk being read
}

func (cs *connState) tap(dir faultproxy.Direction, b []byte) {
	switch dir {
	case faultproxy.Up:
		cs.readCommands(b)
	case faultproxy.Down:
		cs.readReplies(b)
	}
}

func (cs *connState) readCommands(b []byte) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	failed := cs.up.err != nil
	cs.up.feed(b)
	if !failed && cs.up.err != nil {
		cs.c.desync()
	}
}

// onCommand records one command; cs.mu is held.
func (cs *connState) onCommand(args [][]byte, argc int64, cut bool) {
	name := commandName(args)
	strs := make([]string, len(args))
	for i, a := range args {
		strs[i] = string(a)
	}
	c := cs.c
	c.mu.Lock()
	if cs.due == 0 {
		c.roundTrips++
		cs.roundTrip = c.roundTrips
	}
	seq := c.record(Command{Conn: cs.id, RoundTrip: cs.roundTrip, Name: name, Args: strs, Argc: argc, Cut: cut, At: c.clk.Now()})
	c.mu.Unlock()
	cs.due++
	switch {
	case cs.unmatched:
	case len(cs.pending)-cs.head >= maxPending:
		cs.unmatched, cs.pending, cs.head = true, nil, 0
	default:
		cs.pending = append(cs.pending, pendingCmd{seq: seq, name: name})
	}
}

// readReplies parses one chunk of replies, holds it while a reply delay
// matches a reply starting in it, then counts its replies, which are
// forwarded as soon as the tap returns.
func (cs *connState) readReplies(b []byte) {
	cs.mu.Lock()
	failed := cs.down.err != nil
	open := cs.down.replyOpen()
	cs.events = cs.events[:0]
	cs.down.feed(b)
	if !failed && cs.down.err != nil {
		cs.c.desync()
	}
	d, release := cs.holdFor(open)
	cs.mu.Unlock()
	if d > 0 {
		cs.c.hold(d, release)
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for _, e := range cs.events {
		switch e.kind {
		case evEnd:
			cs.due = max(cs.due-1, 0)
			cs.c.reply(cs.popPending(), e.reply, e.err)
		case evPush:
			cs.c.push()
		case evStart:
		}
	}
}

// holdFor returns how long to hold the chunk just parsed, the longest
// reply delay matching a reply that starts in it, and the channel that
// ends the hold early. open says whether a reply was already under way
// when the chunk began; the k-th reply starting in the chunk answers the
// k-th pending command after it.
func (cs *connState) holdFor(open bool) (time.Duration, <-chan struct{}) {
	rules, release := cs.c.currentRules()
	if len(rules) == 0 {
		return 0, nil
	}
	i := cs.head
	if open {
		i++
	}
	var d time.Duration
	for _, e := range cs.events {
		if e.kind != evStart {
			continue
		}
		name, known := "", !cs.unmatched && i < len(cs.pending)
		if known {
			name = cs.pending[i].name
		}
		i++
		for _, r := range rules {
			if r.Command == "" || (known && r.Command == name) {
				d = max(d, r.Delay)
			}
		}
	}
	return d, release
}

// popPending removes the oldest pending command and returns its Seq, or
// 0 when replies are not matched.
func (cs *connState) popPending() uint64 {
	if cs.unmatched || cs.head == len(cs.pending) {
		return 0
	}
	seq := cs.pending[cs.head].seq
	cs.pending[cs.head] = pendingCmd{}
	cs.head++
	switch {
	case cs.head == len(cs.pending):
		cs.pending, cs.head = cs.pending[:0], 0
	case cs.head >= 1024 && 2*cs.head >= len(cs.pending):
		n := copy(cs.pending, cs.pending[cs.head:])
		clear(cs.pending[n:])
		cs.pending, cs.head = cs.pending[:n], 0
	}
	return seq
}

// cmdSink receives a connection's commands.
type cmdSink struct{ cs *connState }

func (s cmdSink) command(args [][]byte, argc int64, cut bool) { s.cs.onCommand(args, argc, cut) }
func (cmdSink) replyStart()                                   {}
func (cmdSink) replyEnd(byte, []byte)                         {}
func (cmdSink) push()                                         {}

// replySink collects the events of a chunk of replies.
type replySink struct{ cs *connState }

func (replySink) command([][]byte, int64, bool) {}

func (s replySink) replyStart() { s.cs.events = append(s.cs.events, event{kind: evStart}) }

func (s replySink) replyEnd(kind byte, msg []byte) {
	s.cs.events = append(s.cs.events, event{kind: evEnd, reply: kind, err: string(msg)})
}

func (s replySink) push() { s.cs.events = append(s.cs.events, event{kind: evPush}) }

// commandName returns the Name of the command args.
func commandName(args [][]byte) string {
	if len(args) == 0 {
		return ""
	}
	name := strings.ToUpper(string(args[0]))
	if len(args) > 1 && hasSubcommands(name) {
		name += " " + strings.ToUpper(string(args[1]))
	}
	return name
}

// hasSubcommands reports whether name is a container command, whose
// first argument is a subcommand.
func hasSubcommands(name string) bool {
	switch name {
	case "ACL", "CLIENT", "CLUSTER", "COMMAND", "CONFIG", "DEBUG", "FUNCTION", "LATENCY", "MEMORY",
		"MODULE", "OBJECT", "PUBSUB", "SCRIPT", "SENTINEL", "SLOWLOG", "XGROUP", "XINFO":
		return true
	}
	return false
}

// normalizeName upper-cases a command name and joins its words with one
// space.
func normalizeName(name string) string {
	return strings.ToUpper(strings.Join(strings.Fields(name), " "))
}
