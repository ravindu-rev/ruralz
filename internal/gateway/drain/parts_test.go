// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package drain

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/gateway/admission"
	"github.com/ravindu-rev/ruralz/internal/gateway/listener"
	"github.com/ravindu-rev/ruralz/internal/gateway/readiness"
	"github.com/ravindu-rev/ruralz/internal/gateway/retire"
	"github.com/ravindu-rev/ruralz/internal/gateway/sdnotify"
	"github.com/ravindu-rev/ruralz/internal/gateway/setting"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/nodedir"
	"github.com/ravindu-rev/ruralz/internal/ulid"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// The Node's parts implement the Drainer's interfaces.
var (
	_ Readiness = (*readiness.Tracker)(nil)
	_ Notifier  = (*sdnotify.Notifier)(nil)
	_ Lock      = (*nodedir.Lock)(nil)
	_ Listeners = (*listener.Set)(nil)
	_ Retirer   = (*retire.Holder)(nil)
)

// conn is an HTTP/1.1 connection the test drives by hand.
type conn struct {
	net.Conn
	br *bufio.Reader
}

func dial(t *testing.T, addr string) (*conn, error) {
	t.Helper()
	var d net.Dialer
	c, err := d.DialContext(t.Context(), "tcp", addr)
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = c.Close() })
	return &conn{Conn: c, br: bufio.NewReader(c)}, nil
}

// keptAlive sends GET / and reports whether it was answered 200 without
// Connection: close.
func (c *conn) keptAlive(t *testing.T) bool {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://node/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := req.Write(c); err != nil {
		return false
	}
	resp, err := http.ReadResponse(c.br, req)
	if err != nil {
		return false
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK && !resp.Close
}

// closedByPeer reports whether the peer closed the connection; the
// real-time deadline only turns a hang into a failure.
func (c *conn) closedByPeer(t *testing.T) bool {
	t.Helper()
	if err := c.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	n, err := c.br.Read(make([]byte, 1))
	return n == 0 && err != nil && !errors.Is(err, os.ErrDeadlineExceeded)
}

// probe answers one /readyz request.
func probe(t *testing.T, tr *readiness.Tracker) (int, string) {
	t.Helper()
	w := httptest.NewRecorder()
	tr.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", nil))
	return w.Code, w.Body.String()
}

// TestRealPartsDrainInWaves runs a stop-signal Drain over the Node's real
// parts on a fake clock: at the signal /readyz turns 503 draining,
// STOPPING=1 with STATUS=draining reaches the service manager and the lock
// and holder.json are released while the listener still accepts; at 5 s
// new connections are refused; at each shard's draw exactly that shard's
// connections close, in K waves, while later shards keep serving; at 25 s
// the deadline finds no connection left, closes everything and Run returns
// 0 after the steps.
func TestRealPartsDrainInWaves(t *testing.T) {
	clk := clocktest.New(clock.Real().Now())
	start := clk.Now()
	ctx := t.Context()

	limiter := admission.NewConnLimiter(admission.DefaultConnections)
	set, err := listener.New(listener.Options{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }),
		Conns:   limiter,
		Clock:   clk,
		Host:    "127.0.0.1",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = set.Close() })
	plan, err := set.Prepare(ctx, &snapshot.Snapshot{
		Listeners: []*snapshot.Listener{{Name: "web", Protocol: v1alpha1.ListenerProtocolHTTP}},
		Gateway:   snapshot.Gateway{AdminPort: 9901},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Commit(); err != nil {
		t.Fatal(err)
	}
	addr := set.Addr("web").String()

	tracker := readiness.New()
	d, err := revision.Parse("sha256:162af81f5de482d540e09cf25205ffe39b705fd2dcf5eca88eb42e92b098a85e")
	if err != nil {
		t.Fatal(err)
	}
	tracker.SetSecrets()
	tracker.SetListeners()
	tracker.SetRevision(d)
	if status, _ := probe(t, tracker); status != http.StatusOK {
		t.Fatalf("readyz before the Drain = %d", status)
	}

	sinkPath := filepath.Join(t.TempDir(), "notify")
	if len(sinkPath) > 100 {
		dir, err := os.MkdirTemp("", "drn")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		sinkPath = filepath.Join(dir, "notify")
	}
	var lc net.ListenConfig
	sink, err := lc.ListenPacket(ctx, "unixgram", sinkPath)
	if err != nil {
		t.Skipf("no datagram Unix sockets: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close() })
	notifier, err := sdnotify.New(sdnotify.Options{Socket: sinkPath, Readiness: tracker.Readyz})
	if err != nil {
		t.Fatal(err)
	}

	dataDir, err := nodedir.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	lock, err := dataDir.TryLock()
	if errors.Is(err, errors.ErrUnsupported) {
		t.Skip("no flock on this platform")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Release() })
	nodeID, err := ulid.New(start, bytes.NewReader(make([]byte, 16)))
	if err != nil {
		t.Fatal(err)
	}
	holder, _ := nodedir.NewHolder(nodeID, "0.0.0-dev")
	if err := lock.WriteHolder(holder); err != nil {
		t.Fatal(err)
	}

	var steps []string
	logs := &captureHandler{}
	drainer, err := New(Config{
		Readiness: tracker,
		Listeners: set,
		Retirer:   retire.New(retire.Config{Clock: clk}),
		Notifier:  notifier,
		Lock:      lock,
		Steps: []Step{
			{Name: "admin", Run: func(context.Context) error { steps = append(steps, "admin"); return nil }},
			{Name: "telemetry", Run: func(context.Context) error { steps = append(steps, "telemetry"); return nil }},
		},
		Connections: limiter.Open,
		Clock:       clk,
		Rand:        half,
		Logger:      slog.New(logs),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Two idle connections per shard: connection k is on shard k mod K.
	conns := make([]*conn, 2*Waves)
	for k := range conns {
		c, err := dial(t, addr)
		if err != nil || !c.keptAlive(t) {
			t.Fatalf("connection %d not served: %v", k, err)
		}
		conns[k] = c
	}

	code := make(chan int, 1)
	go func() { code <- drainer.Run(context.WithoutCancel(ctx)) }()
	drainer.Signal()

	waitFor(t, "/readyz to fail", func() bool { s, _ := probe(t, tracker); return s == http.StatusServiceUnavailable })
	if _, body := probe(t, tracker); !bytes.Contains([]byte(body), []byte(`"reason":"draining"`)) {
		t.Errorf("readyz during the Drain = %s, want reason draining", body)
	}
	if err := sink.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	msg := make([]byte, sdnotify.MaxMessage)
	n, _, err := sink.ReadFrom(msg)
	if err != nil || string(msg[:n]) != "STOPPING=1\nSTATUS=draining" {
		t.Errorf("notification %q (%v), want STOPPING=1 with STATUS=draining", msg[:n], err)
	}
	waitFor(t, "the lock to be released", func() bool { return !lock.Held() })
	if _, err := os.Stat(dataDir.HolderPath()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("holder.json after the Drain's start: %v, want it removed", err)
	}
	if other, err := dataDir.TryLock(); err != nil {
		t.Errorf("TryLock after the Drain's start = %v, want the lock free", err)
	} else {
		_ = other.Release()
	}

	// The accept window: the listener still accepts. This connection is
	// the next in turn, on shard 0.
	extra, err := dial(t, addr)
	if err != nil || !extra.keptAlive(t) {
		t.Fatalf("a connection during the accept window was not served: %v", err)
	}
	byShard := make([][]*conn, Waves)
	for k, c := range append(conns, extra) {
		byShard[k%Waves] = append(byShard[k%Waves], c)
	}

	armed := func() {
		t.Helper()
		waitFor(t, "the Drain's next timer", func() bool { return clk.Pending() == 2 })
	}
	armed()
	clk.Set(start.Add(AcceptWindow))
	armed()
	if c, err := dial(t, addr); err == nil {
		_ = c.Close()
		t.Error("the listener accepted a connection after stop-accepting")
	}

	for i := range Waves {
		clk.Set(start.Add(waveAt(AcceptWindow, i)))
		for _, c := range byShard[i] {
			if !c.closedByPeer(t) {
				t.Errorf("wave %d: a connection of shard %d stayed open", i, i)
			}
		}
		if i+1 < Waves {
			for _, c := range byShard[i+1] {
				if !c.keptAlive(t) {
					t.Errorf("wave %d: a connection of shard %d, whose wave is next, was not served", i, i+1)
				}
			}
		}
		armed()
	}

	// A connection's slot returns just after its socket closes.
	waitFor(t, "every connection's slot", func() bool { return limiter.Open() == 0 })
	clk.Set(start.Add(AcceptWindow + Deadline))
	if got := recv(t, "Run to return", code); got != setting.ExitOK {
		t.Errorf("exit code = %d, want %d", got, setting.ExitOK)
	}
	if n := logs.count("drain deadline reached", "connections=0", "ended=0"); n != 1 {
		t.Errorf("%d deadline records with no connection left and no request ended, want 1", n)
	}
	if fmt.Sprint(steps) != "[admin telemetry]" {
		t.Errorf("steps = %v, want [admin telemetry]", steps)
	}
	if set.Bound() {
		t.Error("a listener socket is still bound after the Drain")
	}
}
