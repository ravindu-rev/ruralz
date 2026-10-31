// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package respproxy

import (
	"bytes"
	"fmt"
	"strconv"
)

// Parser bounds. They keep a broken or hostile stream from holding
// unbounded memory; a stream past one stops parsing (a desync).
const (
	// maxLine bounds one line: a header, a simple value or an inline
	// command. It is Redis's own bound on inline commands and headers
	// (PROTO_INLINE_MAX_SIZE, 64 KiB).
	maxLine = 64 << 10
	// maxDepth bounds the nesting of a reply's aggregates.
	maxDepth = 128
	// maxArgs is the number of arguments kept per command; later ones
	// are counted, not kept.
	maxArgs = 64
	// maxArgBytes is the number of bytes kept per argument.
	maxArgBytes = 256
	// maxErrBytes is the number of bytes kept of an error reply.
	maxErrBytes = 256
)

// sink receives what a parser reads, synchronously and in stream order.
// Byte slices are valid only during the call.
type sink interface {
	// command is one complete command: its first maxArgs arguments, each
	// cut to maxArgBytes, the full argument count, and whether anything
	// was cut.
	command(args [][]byte, argc int64, cut bool)
	// replyStart is the first line of a reply (an attribute ahead of it
	// excluded).
	replyStart()
	// replyEnd is a complete reply: its type byte and, for an error
	// reply, its message.
	replyEnd(kind byte, msg []byte)
	// push is a complete RESP3 push frame, which answers no command.
	push()
}

// state is where a parser stands in the byte stream.
type state uint8

const (
	stType    state = iota // expecting a type byte, or an inline command
	stLine                 // reading a line up to LF
	stBlob                 // reading a blob payload
	stBlobEnd              // expecting the CRLF after a blob payload
)

// frame is one open aggregate of a reply.
type frame struct {
	// left counts the elements still to read (twice the pairs of a map
	// or attribute).
	left int64
	// attr marks an attribute, which annotates the value after it and
	// is not an element of its parent.
	attr bool
}

// parser reads one direction of a RESP connection incrementally: chunks
// may split anything anywhere, and nothing but the current line and the
// kept bytes is buffered. Commands are arrays of bulk strings or inline
// lines (client to server); replies are RESP2 and RESP3 values (server to
// client). RESP3 streamed strings and aggregates, which servers do not
// send, stop the parse. A parser is not safe for concurrent use.
type parser struct {
	replies bool
	sink    sink
	// err is set once the stream stopped parsing; later bytes are
	// ignored.
	err error

	st   state
	kind byte // type byte of the current line or blob; 0 for an inline command
	line []byte
	left int64 // blob payload bytes still to read
	end  int   // bytes of the CRLF after a blob payload seen
	keep int   // bytes of the current blob to keep
	kept []byte
	cut  bool // the current blob had more bytes than keep

	// Commands.
	argsLeft int64 // bulk strings still to read in the current command
	argc     int64
	args     [][]byte // storage of the kept arguments, reused
	nargs    int      // kept arguments of the current command
	cmdCut   bool

	// Replies.
	stack []frame
	top   byte // type byte of the top-level value being read; 0 between values
	msg   []byte
}

func newParser(replies bool, s sink) *parser {
	return &parser{replies: replies, sink: s}
}

// replyOpen reports whether a reply has started and not ended.
func (p *parser) replyOpen() bool { return p.top != 0 && p.top != '>' }

// feed parses b.
func (p *parser) feed(b []byte) {
	for len(b) > 0 && p.err == nil {
		switch p.st {
		case stType:
			c := b[0]
			switch {
			case !p.replies && p.argsLeft == 0 && c != '*':
				p.kind = 0 // an inline command: this byte starts its line
			case !p.replies && p.argsLeft > 0 && c != '$':
				p.fail("command argument of type %q, want '$'", c)
				return
			case p.replies && !isReplyType(c):
				p.fail("unknown type byte %q", c)
				return
			default:
				p.kind = c
				b = b[1:]
			}
			p.st, p.line = stLine, p.line[:0]
		case stLine:
			i := bytes.IndexByte(b, '\n')
			n := i
			if i < 0 {
				n = len(b)
			}
			if len(p.line)+n > maxLine {
				p.fail("line longer than %d bytes", maxLine)
				return
			}
			p.line = append(p.line, b[:n]...)
			if i < 0 {
				return
			}
			b = b[i+1:]
			p.st = stType
			p.onLine(bytes.TrimSuffix(p.line, []byte{'\r'}))
		case stBlob:
			n := int(min(p.left, int64(len(b))))
			p.keepBytes(b[:n])
			p.left -= int64(n)
			b = b[n:]
			if p.left == 0 {
				p.st, p.end = stBlobEnd, 0
			}
		case stBlobEnd:
			if b[0] != "\r\n"[p.end] {
				p.fail("blob not followed by CRLF")
				return
			}
			b = b[1:]
			if p.end++; p.end == 2 {
				p.st = stType
				p.onBlob()
			}
		}
	}
}

func (p *parser) fail(format string, args ...any) {
	p.err = fmt.Errorf("respproxy: "+format, args...)
}

// startBlob begins a blob of n payload bytes, keeping the first keep.
func (p *parser) startBlob(n int64, keep int) {
	p.left, p.keep, p.kept, p.cut = n, keep, p.kept[:0], false
	p.st, p.end = stBlob, 0
	if n == 0 {
		p.st = stBlobEnd
	}
}

func (p *parser) keepBytes(b []byte) {
	if room := p.keep - len(p.kept); room < len(b) {
		p.cut = true
		b = b[:max(room, 0)]
	}
	p.kept = append(p.kept, b...)
}

func (p *parser) onLine(line []byte) {
	if p.replies {
		p.replyLine(line)
	} else {
		p.commandLine(line)
	}
}

func (p *parser) onBlob() {
	if !p.replies {
		p.addArg(p.kept, p.cut)
		if p.argsLeft--; p.argsLeft == 0 {
			p.endCommand()
		}
		return
	}
	if p.kind == '!' && len(p.stack) == 0 {
		p.msg = append(p.msg[:0], p.kept...)
	}
	p.valueDone(false)
}

// commandLine handles a line of the client stream.
func (p *parser) commandLine(line []byte) {
	switch {
	case p.argsLeft == 0 && p.kind == 0:
		// An inline command; Redis skips an empty line.
		fields := bytes.Fields(line)
		if len(fields) == 0 {
			return
		}
		p.beginCommand(int64(len(fields)))
		for _, f := range fields {
			p.addArg(f[:min(len(f), maxArgBytes)], len(f) > maxArgBytes)
		}
		p.endCommand()
	case p.argsLeft == 0:
		n, ok := parseLen(line)
		if !ok || n < -1 {
			p.fail("bad command length %q", line)
			return
		}
		if n > 0 {
			p.beginCommand(n)
		} // Redis skips *0 and *-1.
	default:
		// The type byte was '$' (feed).
		n, ok := parseLen(line)
		if !ok || n < 0 {
			p.fail("bad argument length %q", line)
			return
		}
		keep := maxArgBytes
		if p.nargs == maxArgs {
			keep = 0
		}
		p.startBlob(n, keep)
	}
}

func (p *parser) beginCommand(n int64) {
	p.argsLeft, p.argc, p.nargs, p.cmdCut = n, n, 0, false
}

// addArg keeps one argument of the current command while fewer than
// maxArgs are kept.
func (p *parser) addArg(b []byte, cut bool) {
	if p.nargs == maxArgs {
		p.cmdCut = true
		return
	}
	if p.nargs == len(p.args) {
		p.args = append(p.args, nil)
	}
	p.args[p.nargs] = append(p.args[p.nargs][:0], b...)
	p.nargs++
	p.cmdCut = p.cmdCut || cut
}

func (p *parser) endCommand() {
	p.argsLeft = 0
	p.sink.command(p.args[:p.nargs], p.argc, p.cmdCut)
}

// replyLine handles a line of the server stream: a scalar, or the header
// of a blob or an aggregate.
func (p *parser) replyLine(line []byte) {
	kind := p.kind
	blob := kind == '$' || kind == '!' || kind == '='
	agg := kind == '*' || kind == '~' || kind == '>' || kind == '%' || kind == '|'
	var n int64
	if blob || agg {
		var ok bool
		n, ok = parseLen(line)
		// Only RESP2's null bulk string and null array are -1 long.
		if !ok || n < -1 || (n == -1 && kind != '$' && kind != '*') || n > 1<<61 {
			p.fail("bad length %q of type %q", line, kind)
			return
		}
		if agg && n > 0 && len(p.stack) == maxDepth {
			p.fail("aggregates nested deeper than %d", maxDepth)
			return
		}
	}
	if len(p.stack) == 0 && kind != '|' {
		p.top = kind
		if kind != '>' {
			p.sink.replyStart()
		}
	}
	switch {
	case kind == '-':
		if len(p.stack) == 0 {
			p.msg = append(p.msg[:0], line[:min(len(line), maxErrBytes)]...)
		}
		p.valueDone(false)
	case blob && n >= 0:
		keep := 0
		if kind == '!' && len(p.stack) == 0 {
			keep = maxErrBytes
		}
		p.startBlob(n, keep)
	case agg && n > 0:
		if kind == '%' || kind == '|' {
			n *= 2
		}
		p.stack = append(p.stack, frame{left: n, attr: kind == '|'})
	default:
		// A simple value, a null, or an empty aggregate.
		p.valueDone(kind == '|')
	}
}

// isReplyType reports whether c is a RESP2 or RESP3 type byte.
func isReplyType(c byte) bool {
	switch c {
	case '+', '-', ':', '$', '*', '_', ',', '#', '!', '=', '(', '%', '~', '>', '|':
		return true
	}
	return false
}

// valueDone records a complete value (attr: an attribute) and closes
// every aggregate it completes; a top-level value ends a reply or a push.
func (p *parser) valueDone(attr bool) {
	for len(p.stack) > 0 {
		if attr {
			return // an attribute is not an element of its parent
		}
		f := &p.stack[len(p.stack)-1]
		if f.left--; f.left > 0 {
			return
		}
		attr = f.attr
		p.stack = p.stack[:len(p.stack)-1]
	}
	if attr {
		return // a top-level attribute: the value it annotates follows
	}
	if p.top == '>' {
		p.sink.push()
	} else {
		p.sink.replyEnd(p.top, p.msg)
	}
	p.top, p.msg = 0, p.msg[:0]
}

// parseLen parses a RESP length: an optional minus sign and decimal
// digits, nothing else.
func parseLen(b []byte) (int64, bool) {
	if len(b) == 0 || len(b) > 20 {
		return 0, false
	}
	digits := b
	if digits[0] == '-' {
		digits = digits[1:]
	}
	if len(digits) == 0 {
		return 0, false
	}
	for _, c := range digits {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(string(b), 10, 64)
	return n, err == nil
}
