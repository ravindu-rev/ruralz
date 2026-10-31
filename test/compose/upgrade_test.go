// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build compose

package compose

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Load of the Zero-Downtime Upgrade.
const (
	// h1Workers and h2cWorkers send requests over HTTP/1.1 keep-alive and
	// h2c connections, which honor Connection: close and GOAWAY.
	h1Workers  = 4
	h2cWorkers = 2
	// workerPace spaces one worker's requests: about 300 requests per
	// second in all.
	workerPace = 20 * time.Millisecond
	// loadWindow is how many answers the suite waits for before the
	// handover starts and again after the old process exited.
	loadWindow = 100
	// loadTimeout bounds each of those waits.
	loadTimeout = 60 * time.Second
	// maxFailureSamples bounds the failures a load keeps for the report.
	maxFailureSamples = 20
)

// TestZeroDowntimeUpgrade hands over from the old image's container to the
// image under test's on one data volume in one network namespace, as user
// 65532, under load: no request fails on either protocol, the old process
// exits 0 after releasing the lock, the new one takes it and serves the
// admin port, and stops cleanly in its turn.
func TestZeroDowntimeUpgrade(t *testing.T) {
	s := requireSuite(t)
	ctx := t.Context()
	p := s.newProject(t, "upgrade", upgradeFile, map[string]string{varOldImage: s.oldImage})
	ca := newTestPKI(t)
	sec := newSecrets(t, "admin token")
	nodeFixture(t, p.run, ca, sec.get("admin token"), "127.0.0.1", "localhost")
	quickstartFixture(t, s.root, p.run)

	p.up(ctx, svcUpstream, svcOld)
	admin := p.port(ctx, svcUpstream, portAdmin)
	client := ca.client("")
	waitReady(t, client, admin)
	p.checkHardening(ctx, svcOld)

	l := startLoad(ctx, "http://"+p.port(ctx, svcUpstream, portHTTP)+"/")
	defer l.stop()
	l.waitAnswers(t, loadWindow)

	p.up(ctx, "--no-deps", svcNew)
	old := p.waitExit(ctx, svcOld, exitTimeout)
	if old.State.ExitCode != 0 || old.State.OOMKilled {
		t.Errorf("%s after the handover: state %+v, want exit 0", svcOld, old.State)
	}
	waitReady(t, client, admin)
	l.waitAnswers(t, loadWindow)
	l.stop()

	h1, h2c, failed, samples := l.result()
	t.Logf("load across the handover: %d HTTP/1.1 and %d h2c answers, %d failed", h1, h2c, failed)
	if failed != 0 {
		t.Errorf("%d requests failed across the Zero-Downtime Upgrade, want none: %s", failed, strings.Join(samples, "; "))
	}
	if h1 == 0 || h2c == 0 {
		t.Errorf("answers over HTTP/1.1 %d and h2c %d, want both", h1, h2c)
	}
	oldLogs, newLogs := p.logs(ctx, svcOld), p.logs(ctx, svcNew)
	if !strings.Contains(oldLogs, "handover released the lock to the new process") {
		t.Errorf("%s logs do not show the lock released to the new process:\n%s", svcOld, tail([]byte(oldLogs)))
	}
	if !strings.Contains(newLogs, "handover took the lock") {
		t.Errorf("%s logs do not show it took the lock:\n%s", svcNew, tail([]byte(newLogs)))
	}
	p.checkHardening(ctx, svcNew)
	p.stop(ctx, svcNew)
	sec.scan(t, "ruralzd-old logs", []byte(oldLogs))
	sec.scan(t, "ruralzd-new logs", []byte(p.logs(ctx, svcNew)))
}

// load sends paced GET requests from HTTP/1.1 and h2c workers and checks
// every answer: the quickstart Route's 200, body and header.
type load struct {
	cancel context.CancelFunc
	wg     sync.WaitGroup
	once   sync.Once

	h1, h2c, failed atomic.Int64

	mu      sync.Mutex
	samples []string
}

// startLoad starts the workers against url.
func startLoad(ctx context.Context, url string) *load {
	ctx, cancel := context.WithCancel(ctx)
	l := &load{cancel: cancel}
	h1 := &http.Client{Transport: &http.Transport{Proxy: nil, MaxIdleConnsPerHost: h1Workers}, Timeout: requestTimeout}
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	h2c := &http.Client{Transport: &http.Transport{Proxy: nil, Protocols: protocols}, Timeout: requestTimeout}
	for i := range h1Workers + h2cWorkers {
		client, count, proto := h1, &l.h1, "HTTP/1.1"
		if i >= h1Workers {
			client, count, proto = h2c, &l.h2c, "h2c"
		}
		l.wg.Add(1)
		go func() {
			defer l.wg.Done()
			l.work(ctx, client, url, proto, count)
		}()
	}
	return l
}

// work sends one request per workerPace until ctx ends.
func (l *load) work(ctx context.Context, client *http.Client, url, proto string, count *atomic.Int64) {
	tick := time.NewTicker(workerPace)
	defer tick.Stop()
	defer client.CloseIdleConnections()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			l.fail(proto, err.Error())
			continue
		}
		ex, err := do(client, req)
		switch {
		case ctx.Err() != nil:
			return // the load stopped mid-request
		case err != nil:
			l.fail(proto, err.Error())
		case ex.status != http.StatusOK || string(ex.body) != quickstartBody || ex.header.Get("X-Quickstart") != "ruralz":
			l.fail(proto, fmt.Sprintf("%d %q x-quickstart %q", ex.status, ex.body, ex.header.Get("X-Quickstart")))
		default:
			count.Add(1)
		}
	}
}

func (l *load) fail(proto, what string) {
	l.failed.Add(1)
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.samples) < maxFailureSamples {
		l.samples = append(l.samples, proto+": "+what)
	}
}

// waitAnswers waits until n more requests were answered as expected.
func (l *load) waitAnswers(t *testing.T, n int64) {
	t.Helper()
	want := l.h1.Load() + l.h2c.Load() + n
	deadline := time.Now().Add(loadTimeout)
	for l.h1.Load()+l.h2c.Load() < want {
		if time.Now().After(deadline) {
			_, _, failed, samples := l.result()
			t.Fatalf("fewer than %d answers within %v (%d failed: %s)", n, loadTimeout, failed, strings.Join(samples, "; "))
		}
		sleepCtx(t.Context())
	}
}

// stop ends the workers and waits for them.
func (l *load) stop() {
	l.once.Do(func() {
		l.cancel()
		l.wg.Wait()
	})
}

// result returns the answers per protocol, the failures and their first
// samples.
func (l *load) result() (h1, h2c, failed int64, samples []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.h1.Load(), l.h2c.Load(), l.failed.Load(), append([]string(nil), l.samples...)
}
