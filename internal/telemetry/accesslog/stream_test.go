// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package accesslog_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/telemetry/accesslog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/logsink"
)

var _ logsink.Stream = (*accesslog.Writer)(nil)

// gate blocks every Write until opened, like a pipe nobody reads.
type gate struct {
	open    chan struct{}
	entered chan struct{}
	once    sync.Once
	mu      sync.Mutex
	buf     bytes.Buffer
}

func (g *gate) Write(p []byte) (int, error) {
	g.once.Do(func() { close(g.entered) })
	<-g.open
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.buf.Write(p)
}

func (g *gate) String() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.buf.String()
}

// ADR-0018 "Blocked stdout test", 09 req 67 and 70, WP-11 done-when: with
// stdout blocked, Submit and process logging never block; the access log
// drops beyond its bounds with queue_full; one worker writes both streams
// as whole lines once stdout drains.
func TestBlockedStdoutBothStreams_Req67(t *testing.T) {
	w := accesslog.New(accesslog.Options{NodeID: "01J9Z8Q4W6X3V5T7R2N0M1K8H4", QueueRecords: 256})
	g := &gate{open: make(chan struct{}), entered: make(chan struct{})}
	s, err := logsink.New(logsink.Options{Stdout: g, Access: w, QueueRecords: 64})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	log := s.Logger("gateway")
	log.InfoContext(ctx, "first")
	<-g.entered

	const n = 20_000
	start := time.Now()
	for i := range n {
		r := w.Acquire()
		r.Route = "orders"
		r.Path = "/orders?secret=1"
		r.Status = 200 + i%300
		w.Submit(r)
		if i%10 == 0 {
			log.InfoContext(ctx, "m")
		}
	}
	elapsed := time.Since(start)
	if elapsed > 5*time.Second {
		t.Fatalf("%d requests took %v with stdout blocked", n, elapsed)
	}
	as, ps := w.Stats(), s.Stats()
	if as.Produced != n || as.QueueFull < n-256-1 {
		t.Fatalf("access stats = %+v", as)
	}
	if ps.QueueFull == 0 {
		t.Fatalf("process stats = %+v, want queue_full drops", ps)
	}

	close(g.open)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
	}
	w.Close()
	s.Close()

	access, process := 0, 0
	for _, line := range strings.Split(strings.TrimSuffix(g.String(), "\n"), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("line is not one JSON object (interleaved?): %q", line)
		}
		switch m["msg"] {
		case accesslog.Message:
			access++
			if m["path"] != "/orders" {
				t.Fatalf("path = %v", m["path"])
			}
		default:
			process++
		}
	}
	as, ps = w.Stats(), s.Stats()
	if uint64(access) != as.Produced-as.QueueFull-as.ExportError {
		t.Fatalf("wrote %d access lines, stats %+v", access, as)
	}
	if uint64(process) != ps.Produced-ps.QueueFull-ps.ExportError {
		t.Fatalf("wrote %d process lines, stats %+v", process, ps)
	}
}

// The worker writes access lines as they arrive and on cancel.
func TestWorkerDrainsAccess(t *testing.T) {
	w := accesslog.New(accesslog.Options{})
	var mu sync.Mutex
	var out bytes.Buffer
	s, err := logsink.New(logsink.Options{Stdout: writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return out.Write(p)
	}), Access: w, Level: slog.LevelInfo})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	for range 3 {
		w.Submit(w.Acquire())
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := strings.Count(out.String(), "\n")
		mu.Unlock()
		if n == 3 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if n := strings.Count(out.String(), "\n"); n != 3 {
		t.Fatalf("wrote %d lines", n)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// lineCounter counts the process and access lines written, failing every
// seventh write so export_error is exercised too.
type lineCounter struct {
	mu              sync.Mutex
	writes          int
	access, process uint64
	leaked          bool
}

var errInjected = errors.New("injected write failure")

func (c *lineCounter) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writes++
	if c.writes%7 == 0 {
		return 0, errInjected
	}
	for _, line := range bytes.Split(bytes.TrimSuffix(p, []byte{'\n'}), []byte{'\n'}) {
		if bytes.Contains(line, []byte(`"msg":"access"`)) {
			c.access++
		} else {
			c.process++
		}
		if bytes.Contains(line, []byte("leak")) {
			c.leaked = true
		}
	}
	return len(p), nil
}

type processBridge struct{ n atomic.Uint64 }

func (b *processBridge) Export(*logsink.Exported) error { b.n.Add(1); return nil }

type accessBridge struct{ n atomic.Uint64 }

func (b *accessBridge) Export(*accesslog.Exported) error { b.n.Add(1); return nil }

// 09 req 61, 65 and 67 under -race: producers log and submit on many
// goroutines while another changes the Revision, the credential headers
// and the bridges, and the worker writes with some writes failing. After
// Run returns and both streams close, every record of each stream is
// accounted for exactly once: written + queue_full + export_error =
// produced.
func TestConcurrentProducersAndSetters_Req65(t *testing.T) {
	w := accesslog.New(accesslog.Options{QueueRecords: 128})
	out := &lineCounter{}
	s, err := logsink.New(logsink.Options{Stdout: out, Access: w, QueueRecords: 128, BatchBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	stop := make(chan struct{})
	var setter sync.WaitGroup
	setter.Go(func() {
		pb, ab := &processBridge{}, &accessBridge{}
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			s.SetRevision(fmt.Sprintf("rev-%012x", i))
			s.SetCredentialHeaders([]string{fmt.Sprintf("x-key-%d", i%4)})
			if i%2 == 0 {
				s.SetBridge(pb)
				w.SetBridge(ab)
			} else {
				s.SetBridge(nil)
				w.SetBridge(nil)
			}
		}
	})
	const producers, each = 8, 1000
	var wg sync.WaitGroup
	for p := range producers {
		wg.Go(func() {
			log := s.Logger(fmt.Sprintf("p%d", p))
			for i := range each {
				log.WarnContext(ctx, "m", slog.Int("i", i),
					slog.Any("headers", http.Header{"Authorization": {"Bearer leak"}, "X-Key-1": {"leak"}}))
				r := w.Acquire()
				r.Route = "orders"
				r.Path = "/orders?token=leak"
				r.Status = 200
				w.Submit(r)
			}
		})
	}
	wg.Wait()
	close(stop)
	setter.Wait()
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	w.Close()
	s.Close()

	out.mu.Lock()
	defer out.mu.Unlock()
	if out.leaked {
		t.Fatal("a credential or query string reached stdout")
	}
	as, ps := w.Stats(), s.Stats()
	if as.Produced != producers*each || ps.Produced != producers*each {
		t.Fatalf("produced: access %d, process %d", as.Produced, ps.Produced)
	}
	if out.access+as.QueueFull+as.ExportError != as.Produced {
		t.Fatalf("access: wrote %d, stats %+v", out.access, as)
	}
	if out.process+ps.QueueFull+ps.ExportError != ps.Produced {
		t.Fatalf("process: wrote %d, stats %+v", out.process, ps)
	}
}
