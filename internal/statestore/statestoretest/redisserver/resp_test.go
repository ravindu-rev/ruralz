// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redisserver

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func read(s string) (Value, error) {
	return readValue(bufio.NewReaderSize(strings.NewReader(s), maxLine))
}

// TestReadValue checks the admin client's reader on every RESP2 and RESP3
// type: nesting, nulls, maps as alternating keys and values, and
// attributes, which are dropped for the value they annotate.
func TestReadValue(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string // Value.String
		kind byte
	}{
		{"+OK\r\n", `"OK"`, '+'},
		{"-ERR bad\r\n", `"ERR bad"`, '-'},
		{":-42\r\n", "-42", ':'},
		{"$5\r\nhe\r\no\r\n", `"he\r\no"`, '$'},
		{"$0\r\n\r\n", `""`, '$'},
		{"$-1\r\n", "(nil)", '$'},
		{"*-1\r\n", "(nil)", '*'},
		{"_\r\n", "(nil)", '_'},
		{",1.5\r\n", `"1.5"`, ','},
		{"#t\r\n", `"t"`, '#'},
		{"(123456789012345678901234567890\r\n", `"123456789012345678901234567890"`, '('},
		{"!9\r\nERR inner\r\n", `"ERR inner"`, '!'},
		{"=8\r\ntxt:info\r\n", `"txt:info"`, '='},
		{"*3\r\n:1\r\n*1\r\n+a\r\n$-1\r\n", `[1 ["a"] (nil)]`, '*'},
		{"%2\r\n+a\r\n:1\r\n+b\r\n:2\r\n", `["a" 1 "b" 2]`, '%'},
		{"~1\r\n+x\r\n", `["x"]`, '~'},
		{">2\r\n+message\r\n+x\r\n", `["message" "x"]`, '>'},
		{"|1\r\n+ttl\r\n:9\r\n+value\r\n", `"value"`, '+'},
		{"|0\r\n|0\r\n:7\r\n", "7", ':'},
		{"*2\r\n|1\r\n+a\r\n+b\r\n:1\r\n:2\r\n", "[1 2]", '*'},
		{"+no CR\n", `"no CR"`, '+'},
	} {
		v, err := read(tc.in)
		if err != nil || v.String() != tc.want || v.Kind != tc.kind {
			t.Errorf("readValue(%q) = %s (kind %q), %v; want %s (kind %q)", tc.in, v, v.Kind, err, tc.want, tc.kind)
		}
	}
}

// TestReadValueErrors checks that malformed replies and replies past the
// reader's bounds are errors, never allocations without bound.
func TestReadValueErrors(t *testing.T) {
	for _, in := range []string{
		"", "\r\n", "?x\r\n", ":12a\r\n", "$x\r\n", "$-2\r\n", "!-1\r\n", "$3\r\nabcd\r\n", "$3\r\nab",
		"*x\r\n", "%-1\r\n", "*2\r\n:1\r\n",
		fmt.Sprintf("$%d\r\n", maxBulk+1),
		fmt.Sprintf("*%d\r\n", maxElems+1),
		strings.Repeat("*1\r\n", maxDepth+1) + ":1\r\n",
		"+" + strings.Repeat("a", maxLine) + "\r\n",
	} {
		if v, err := read(in); err == nil {
			t.Errorf("readValue(%.40q) = %s, want an error", in, v)
		}
	}
	if _, err := read(strings.Repeat("*1\r\n", maxDepth) + ":1\r\n"); err != nil {
		t.Errorf("nesting %d deep: %v", maxDepth, err)
	}
	if _, err := read("+" + strings.Repeat("a", maxLine-3) + "\r\n"); err != nil {
		t.Errorf("a line of %d bytes: %v", maxLine, err)
	}
	if _, err := read("?x\r\n"); !errors.Is(err, errProtocol) {
		t.Errorf("unknown type: %v, want errProtocol", err)
	}
}

// TestReplyError checks that error replies, simple and blob, surface as a
// *ReplyError with their code.
func TestReplyError(t *testing.T) {
	for in, code := range map[string]string{"-NOSCRIPT No matching script\r\n": "NOSCRIPT", "!11\r\nSYNTAX bad!\r\n": "SYNTAX", "-ERR\r\n": "ERR"} {
		v, err := read(in)
		if err != nil {
			t.Fatal(err)
		}
		var re *ReplyError
		if !errors.As(v.Err(), &re) || re.Code() != code || !strings.Contains(re.Error(), v.Str) {
			t.Errorf("%q: Err() = %v, want a ReplyError with code %s", in, v.Err(), code)
		}
	}
	if v, _ := read("+OK\r\n"); v.Err() != nil {
		t.Errorf("+OK: Err() = %v", v.Err())
	}
}

// TestWriteCommand checks that commands go out as arrays of bulk strings,
// binary-safe.
func TestWriteCommand(t *testing.T) {
	var b bytes.Buffer
	w := bufio.NewWriter(&b)
	if err := writeCommand(w, []string{"SET", "k\r\n", ""}); err != nil {
		t.Fatal(err)
	}
	if want := "*3\r\n$3\r\nSET\r\n$3\r\nk\r\n\r\n$0\r\n\r\n"; b.String() != want {
		t.Errorf("writeCommand = %q, want %q", b.String(), want)
	}
}
