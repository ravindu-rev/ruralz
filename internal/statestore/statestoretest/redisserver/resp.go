// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redisserver

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Reader bounds. A reply past one is an error, so a broken server cannot
// make the admin client allocate without bound.
const (
	// maxLine bounds one line; Redis bounds its own the same (64 KiB).
	maxLine = 64 << 10
	// maxBulk bounds one blob.
	maxBulk = 64 << 20
	// maxElems bounds the elements of one aggregate.
	maxElems = 1 << 20
	// maxDepth bounds the nesting of aggregates.
	maxDepth = 64
)

// Value is one RESP2 or RESP3 reply.
type Value struct {
	// Kind is the RESP type byte: '+', '-', ':', '$' and '*', and with
	// RESP3 '_', ',', '#', '!', '=', '(', '%', '~' and '>'.
	Kind byte
	// Str holds a simple string, an error message, a blob, a verbatim
	// string (its format prefix included), a double, a big number or a
	// boolean ("t" or "f").
	Str string
	// Int holds an integer.
	Int int64
	// Elems holds an array, a set or a push; a map's keys and values
	// alternate.
	Elems []Value
	// Null marks a null: RESP3 '_', or RESP2 "$-1" and "*-1".
	Null bool
}

// Err returns the error of an error reply ('-' or '!'), else nil.
func (v Value) Err() error {
	if v.Kind == '-' || v.Kind == '!' {
		return &ReplyError{Msg: v.Str}
	}
	return nil
}

// String renders v for messages.
func (v Value) String() string {
	switch {
	case v.Null:
		return "(nil)"
	case v.Kind == ':':
		return strconv.FormatInt(v.Int, 10)
	case v.Kind == '*' || v.Kind == '%' || v.Kind == '~' || v.Kind == '>':
		parts := make([]string, len(v.Elems))
		for i, e := range v.Elems {
			parts[i] = e.String()
		}
		return "[" + strings.Join(parts, " ") + "]"
	}
	return strconv.Quote(v.Str)
}

// ReplyError is an error reply.
type ReplyError struct {
	// Msg is the message, such as "NOSCRIPT No matching script".
	Msg string
}

func (e *ReplyError) Error() string { return "redisserver: error reply: " + e.Msg }

// Code returns the first word of the message, such as "NOSCRIPT".
func (e *ReplyError) Code() string {
	code, _, _ := strings.Cut(e.Msg, " ")
	return code
}

// writeCommand writes args as an array of bulk strings.
func writeCommand(w *bufio.Writer, args []string) error {
	_, _ = fmt.Fprintf(w, "*%d\r\n", len(args))
	for _, a := range args {
		_, _ = fmt.Fprintf(w, "$%d\r\n", len(a))
		_, _ = w.WriteString(a)
		_, _ = w.WriteString("\r\n")
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("redisserver: write: %w", err)
	}
	return nil
}

// errProtocol marks a reply that breaks RESP or a reader bound.
var errProtocol = errors.New("redisserver: protocol error")

// readValue reads one reply from r, whose buffer holds maxLine bytes.
// An attribute is read and dropped: the value it annotates is returned.
func readValue(r *bufio.Reader) (Value, error) { return readDepth(r, 0) }

func readDepth(r *bufio.Reader, depth int) (Value, error) {
	for {
		v, err := readOne(r, depth)
		if err != nil || v.Kind != '|' {
			return v, err
		}
	}
}

// readOne reads one value, an attribute included.
func readOne(r *bufio.Reader, depth int) (Value, error) {
	line, err := r.ReadSlice('\n')
	switch {
	case errors.Is(err, bufio.ErrBufferFull):
		return Value{}, fmt.Errorf("%w: line longer than %d bytes", errProtocol, maxLine)
	case err != nil:
		return Value{}, fmt.Errorf("redisserver: read: %w", err)
	}
	line = bytes.TrimSuffix(bytes.TrimSuffix(line, []byte{'\n'}), []byte{'\r'})
	if len(line) == 0 {
		return Value{}, fmt.Errorf("%w: empty line", errProtocol)
	}
	kind, body := line[0], string(line[1:])
	v := Value{Kind: kind}
	switch kind {
	case '+', '-', ',', '(', '#':
		v.Str = body
		return v, nil
	case '_':
		v.Null = true
		return v, nil
	case ':':
		if v.Int, err = strconv.ParseInt(body, 10, 64); err != nil {
			return Value{}, fmt.Errorf("%w: integer %q", errProtocol, body)
		}
		return v, nil
	case '$', '!', '=':
		n, err := strconv.Atoi(body)
		switch {
		case err != nil || n < -1 || (n == -1 && kind != '$') || n > maxBulk:
			return Value{}, fmt.Errorf("%w: blob length %q", errProtocol, body)
		case n == -1:
			v.Null = true
			return v, nil
		}
		buf := make([]byte, n+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return Value{}, fmt.Errorf("redisserver: read: %w", err)
		}
		if buf[n] != '\r' || buf[n+1] != '\n' {
			return Value{}, fmt.Errorf("%w: blob not followed by CRLF", errProtocol)
		}
		v.Str = string(buf[:n])
		return v, nil
	case '*', '~', '>', '%', '|':
		n, err := strconv.Atoi(body)
		switch {
		case err != nil || n < -1 || (n == -1 && kind != '*') || n > maxElems:
			return Value{}, fmt.Errorf("%w: aggregate length %q", errProtocol, body)
		case n == -1:
			v.Null = true
			return v, nil
		case depth == maxDepth:
			return Value{}, fmt.Errorf("%w: aggregates nested deeper than %d", errProtocol, maxDepth)
		}
		if kind == '%' || kind == '|' {
			n *= 2
		}
		v.Elems = make([]Value, 0, min(n, 1024))
		for range n {
			e, err := readDepth(r, depth+1)
			if err != nil {
				return Value{}, err
			}
			v.Elems = append(v.Elems, e)
		}
		return v, nil
	}
	return Value{}, fmt.Errorf("%w: type byte %q", errProtocol, kind)
}
