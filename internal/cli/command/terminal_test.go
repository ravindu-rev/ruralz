// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package command

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// TestColorsDecision: color needs a terminal that takes it, TERM other
// than dumb and an unset or empty NO_COLOR.
func TestColorsDecision(t *testing.T) {
	yes := func(io.Writer) bool { return true }
	no := func(io.Writer) bool { return false }
	cases := []struct {
		name     string
		environ  []string
		terminal func(io.Writer) bool
		want     bool
	}{
		{"terminal", []string{"TERM=xterm-256color"}, yes, true},
		{"terminal without TERM", nil, yes, true},
		{"pipe", []string{"TERM=xterm-256color"}, no, false},
		{"dumb terminal", []string{"TERM=dumb"}, yes, false},
		{"NO_COLOR", []string{"NO_COLOR=1"}, yes, false},
		{"empty NO_COLOR", []string{"NO_COLOR="}, yes, true},
	}
	for _, c := range cases {
		sio := NewIO(io.Discard, io.Discard, c.environ, nil, "linux")
		if got := Colors(sio, io.Discard, c.terminal); got != c.want {
			t.Errorf("%s: Colors = %v, want %v", c.name, got, c.want)
		}
	}
	if Colors(IO{}, &bytes.Buffer{}, nil) {
		t.Error("Colors over a buffer with the real terminal check = true")
	}
}

// TestIsTerminal: a buffer, a regular file and a pipe are no terminal.
func TestIsTerminal(t *testing.T) {
	if IsTerminal(&bytes.Buffer{}) || IsTerminal(nil) {
		t.Error("a buffer is a terminal")
	}
	f, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if IsTerminal(f) {
		t.Error("a regular file is a terminal")
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close(); _ = w.Close() }()
	if IsTerminal(w) {
		t.Error("a pipe is a terminal")
	}
	var nilFile *os.File
	if IsTerminal(nilFile) {
		t.Error("a nil file is a terminal")
	}
}
