// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package logsink

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
)

// syncBuffer is a goroutine-safe bytes.Buffer recording each Write.
type syncBuffer struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	writes int
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.writes++
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *syncBuffer) Writes() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.writes
}

// lines splits the output into lines, requiring a trailing newline.
func (b *syncBuffer) lines(t *testing.T) []string {
	t.Helper()
	s := b.String()
	if s == "" {
		return nil
	}
	if !strings.HasSuffix(s, "\n") {
		t.Fatalf("output does not end with a newline: %q", s)
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// newSink returns a Sink over a syncBuffer; tests drive the worker with
// drain instead of Run unless they test Run.
func newSink(t *testing.T, o Options) (*Sink, *syncBuffer) {
	t.Helper()
	out := &syncBuffer{}
	if o.Stdout == nil {
		o.Stdout = out
	}
	s, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return s, out
}

// drain runs the worker's pump on the test goroutine: everything queued
// is encoded and written.
func drain(s *Sink) { s.pump(nil) }

// keys returns the member names of a JSON object line, in order, at the
// top level.
func keys(t *testing.T, line string) []string {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(line))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		t.Fatalf("not an object: %q (%v)", line, err)
	}
	var out []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			t.Fatalf("token: %v in %q", err, line)
		}
		k, ok := tok.(string)
		if !ok {
			t.Fatalf("key %v is not a string in %q", tok, line)
		}
		out = append(out, k)
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatalf("value of %q: %v", k, err)
		}
	}
	return out
}

// object decodes a JSON line.
func object(t *testing.T, line string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("invalid JSON line %q: %v", line, err)
	}
	return m
}

// traceCtx is a context key carrying fixed trace IDs for TraceContext.
type traceKey struct{}

type traceIDs struct {
	trace [16]byte
	span  [8]byte
}

func withTrace(ctx context.Context, ids traceIDs) context.Context {
	return context.WithValue(ctx, traceKey{}, ids)
}

func traceFromContext(ctx context.Context) ([16]byte, [8]byte, bool) {
	ids, ok := ctx.Value(traceKey{}).(traceIDs)
	return ids.trace, ids.span, ok
}

// failWriter fails every write after writing the first n bytes of it.
type failWriter struct {
	mu sync.Mutex
	n  int
	w  bytes.Buffer
}

var errWrite = errors.New("write failed")

func (f *failWriter) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := min(f.n, len(p))
	f.w.Write(p[:k])
	return k, errWrite
}

// blockingWriter blocks every Write until release is closed.
type blockingWriter struct {
	release chan struct{}
	entered chan struct{}
	once    sync.Once
	out     syncBuffer
}

func newBlockingWriter() *blockingWriter {
	return &blockingWriter{release: make(chan struct{}), entered: make(chan struct{})}
}

func (b *blockingWriter) Write(p []byte) (int, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return b.out.Write(p)
}

var _ io.Writer = (*blockingWriter)(nil)
