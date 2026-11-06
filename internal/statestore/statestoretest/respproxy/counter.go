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
	// At is when the Counter read the command, on its clock.
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
// the slow reply fault. Each reply is held for Delay from the moment the
// proxy read it, as a server answering that much late would be.
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
	// Held counts the chunks of replies a reply delay holds right now.
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
	clk clock.Clock

	mu     sync.Mutex
	closed bool
	rules  []ReplyDelay // replaced, never changed in place
	// gen counts the changes of the rules: a hold the rules of an older
	// generation decided is never armed.
	gen uint64
	// holds are the armed holds, which a change of the rules and Close end.
	holds      map[*heldChunk]struct{}
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
		clk:    clk,
		holds:  map[*heldChunk]struct{}{},
		byName: map[string]int64{},
		errors: map[string]int64{},
		log:    make([]Command, max(size, 0)),
	}
}

// ConnTap returns the hooks of one proxied connection; pass it as
// faultproxy.Config.ConnTap. The tap parses the commands flowing up and
// counts the replies flowing down as they are forwarded; the hold reads
// each chunk of replies as the proxy reads it and holds the chunks that
// start a reply a reply delay matches.
func (c *Counter) ConnTap(info faultproxy.ConnInfo) faultproxy.ConnHooks {
	cs := &connState{c: c, id: info.ID}
	cs.up = newParser(false, cmdSink{cs})
	cs.down = newParser(true, replySink{cs})
	cs.scan = newParser(true, scanSink{cs})
	return faultproxy.ConnHooks{Tap: cs.tap, Hold: cs.hold}
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
// replaced delays are released at once. A reply is held from the moment
// the proxy read it, while the proxy goes on reading, so the replies to
// pipelined commands are each held their delay side by side, never one
// after another. Replies are forwarded in the chunks the proxy reads and
// in the order read, so a reply sharing a chunk with a held one, or read
// after it, waits for it, as replies queued behind a slow reply do.
func (c *Counter) SetReplyDelays(delays ...ReplyDelay) error {
	var rules []ReplyDelay
	for _, d := range delays {
		if d.Delay < 0 {
			return fmt.Errorf("respproxy: negative reply delay %v for %q", d.Delay, d.Command)
		}
		rules = append(rules, ReplyDelay{Command: normalizeName(d.Command), Delay: d.Delay})
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
	c.rules = rules
	held := c.endHolds()
	c.mu.Unlock()
	releaseAll(held)
	return nil
}

// Close releases every held reply and turns reply delays off for good;
// counting goes on. It is idempotent. A Counter used as the ConnTap of a
// faultproxy.Proxy is closed before or with it, so no hold of the Counter
// outlives the proxy.
func (c *Counter) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.rules = nil
	held := c.endHolds()
	c.mu.Unlock()
	releaseAll(held)
}

// heldChunk is one chunk of replies a reply delay holds: release, the
// fault proxy's, forwards it once the chunks read before it went.
type heldChunk struct {
	timer   clock.Timer
	release func()
}

// rulesNow returns the reply delays in force and their generation.
func (c *Counter) rulesNow() ([]ReplyDelay, uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rules, c.gen
}

// arm holds one chunk of replies for d on the Counter's clock, from now:
// release runs once d passes, the delays change or the Counter closes,
// whichever comes first. False, holding nothing, means the delays of gen,
// which decided the hold, changed since or the Counter closed. The timer
// is armed before Held counts the chunk, so a test that sees Held may
// advance a fake clock past it.
func (c *Counter) arm(d time.Duration, gen uint64, release func()) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || gen != c.gen {
		return false
	}
	h := &heldChunk{release: release}
	h.timer = c.clk.AfterFunc(d, func() { c.unhold(h) })
	c.holds[h] = struct{}{}
	c.held++
	return true
}

// unhold ends h's hold once its delay passed, unless a change of the
// delays or Close ended it first.
func (c *Counter) unhold(h *heldChunk) {
	c.mu.Lock()
	_, ok := c.holds[h]
	if ok {
		delete(c.holds, h)
		c.held--
	}
	c.mu.Unlock()
	if ok {
		h.release()
	}
}

// endHolds ends every hold at once and moves the delays to a new
// generation; c.mu is held. The caller passes the holds to releaseAll once
// it no longer holds c.mu.
func (c *Counter) endHolds() []*heldChunk {
	c.gen++
	held := make([]*heldChunk, 0, len(c.holds))
	for h := range c.holds {
		held = append(held, h)
	}
	clear(c.holds)
	c.held = 0
	return held
}

// releaseAll stops the timers of held and releases their chunks.
func releaseAll(held []*heldChunk) {
	for _, h := range held {
		h.timer.Stop()
		h.release()
	}
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

// eventKind is what the counting reply parser read.
type eventKind uint8

const (
	evEnd eventKind = iota + 1
	evPush
)

// event is one thing read from a chunk of replies.
type event struct {
	kind  eventKind
	reply byte   // evEnd: the reply's type byte
	err   string // evEnd: an error reply's message
}

// connState is the RESP state of one proxied connection. Its tap and hold
// run on the connection's two pump goroutines; mu serializes them. Two
// parsers read the replies: scan as the proxy reads them, to decide their
// holds, and down as they are forwarded, to count them.
type connState struct {
	c              *Counter
	id             uint64
	up, down, scan *parser

	mu        sync.Mutex
	pending   []pendingCmd // commands awaiting replies, oldest at head
	head      int
	unmatched bool  // pending overflowed: replies are no longer matched
	due       int64 // commands read minus replies forwarded
	roundTrip uint64
	events    []event // the replies of the chunk being counted
	// ahead counts the replies scan saw start that the counting has not
	// seen end: the next reply scan sees answers pending[head+ahead].
	ahead int
	// rules and delay are the scan's of the chunk being scanned: the
	// reply delays in force and the longest matching a reply so far.
	rules []ReplyDelay
	delay time.Duration
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

// readReplies counts the replies of one chunk as it is forwarded.
func (cs *connState) readReplies(b []byte) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	failed := cs.down.err != nil
	cs.events = cs.events[:0]
	cs.down.feed(b)
	if !failed && cs.down.err != nil {
		cs.c.desync()
	}
	for _, e := range cs.events {
		switch e.kind {
		case evEnd:
			cs.due = max(cs.due-1, 0)
			cs.ahead = max(cs.ahead-1, 0)
			cs.c.reply(cs.popPending(), e.reply, e.err)
		case evPush:
			cs.c.push()
		}
	}
}

// hold is the connection's faultproxy.HoldFunc. It reads each chunk of
// replies as the proxy reads it, ahead of the counting, and holds the
// chunk for the longest reply delay matching a reply that starts in it,
// from now on the Counter's clock (arm); a chunk carrying only the rest
// of a reply waits for the chunk that started it, as the proxy forwards
// in order. Commands flowing up hold nothing.
func (cs *connState) hold(dir faultproxy.Direction, b []byte, release func()) bool {
	if dir != faultproxy.Down {
		return false
	}
	rules, gen := cs.c.rulesNow()
	cs.mu.Lock()
	cs.rules, cs.delay = rules, 0
	cs.scan.feed(b)
	d := cs.delay
	cs.rules = nil
	cs.mu.Unlock()
	return d > 0 && cs.c.arm(d, gen, release)
}

// scanned matches the reply scan saw start to its command, the oldest
// pending command whose reply scan had not seen, and keeps the longest
// delay of the chunk's rules that matches it; cs.mu is held. A reply to a
// command the connection no longer matches (unmatched) meets only the
// delays of every command.
func (cs *connState) scanned() {
	i := cs.head + cs.ahead
	cs.ahead++
	if len(cs.rules) == 0 {
		return
	}
	name, known := "", !cs.unmatched && i < len(cs.pending)
	if known {
		name = cs.pending[i].name
	}
	for _, r := range cs.rules {
		if r.Command == "" || (known && r.Command == name) {
			cs.delay = max(cs.delay, r.Delay)
		}
	}
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

// replySink collects the events of a chunk of replies being counted.
type replySink struct{ cs *connState }

func (replySink) command([][]byte, int64, bool) {}
func (replySink) replyStart()                   {}

func (s replySink) replyEnd(kind byte, msg []byte) {
	s.cs.events = append(s.cs.events, event{kind: evEnd, reply: kind, err: string(msg)})
}

func (s replySink) push() { s.cs.events = append(s.cs.events, event{kind: evPush}) }

// scanSink matches the replies of a chunk being scanned for its hold.
type scanSink struct{ cs *connState }

func (scanSink) command([][]byte, int64, bool) {}
func (s scanSink) replyStart()                 { s.cs.scanned() }
func (scanSink) replyEnd(byte, []byte)         {}
func (scanSink) push()                         {}

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
