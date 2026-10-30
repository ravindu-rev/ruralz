// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package mock

import (
	"bytes"
	"encoding/json"
	"encoding/pem"
	"flag"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestProbeConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		c       probeConfig
		args    []string
		wantErr string
	}{
		{"nothing to probe", probeConfig{timeout: time.Second, interval: time.Second}, nil, "at least one of"},
		{"an argument", probeConfig{ready: "u", timeout: time.Second, interval: time.Second}, []string{"x"}, "unexpected argument"},
		{"a host without -get", probeConfig{ready: "u", host: "h", timeout: time.Second, interval: time.Second}, nil, "-host needs -get"},
		{"a sink without a name", probeConfig{sinkCheck: "a:53", timeout: time.Second, interval: time.Second}, nil, "go together"},
		{"a relative sink name", probeConfig{sinkCheck: "a:53", sinkName: "x.invalid", timeout: time.Second, interval: time.Second}, nil, "absolute"},
		{"no timeout", probeConfig{ready: "u", interval: time.Second}, nil, "positive"},
		{"every part", probeConfig{ready: "u", get: "g", host: "h", sinkCheck: "a:53", sinkName: "x.invalid.", dnsLog: "d", timeout: time.Second, interval: time.Second}, nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := flag.NewFlagSet("t", flag.ContinueOnError)
			if err := fs.Parse(tt.args); err != nil {
				t.Fatal(err)
			}
			err := tt.c.validate(fs)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("validate() = %v, want nil", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("validate() = %v, want an error containing %q", err, tt.wantErr)
			}
		})
	}
}

// nodeStandIn answers /readyz 503 until it has been asked notReady times,
// then 200, and / with a fixed body and header, recording the Host.
type nodeStandIn struct {
	notReady int64
	readyz   atomic.Int64
	host     atomic.Value
}

func (n *nodeStandIn) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/readyz":
		if n.readyz.Add(1) <= n.notReady {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "ready\n")
	case "/":
		n.host.Store(r.Host)
		w.Header().Set("X-Quickstart", "ruralz")
		_, _ = io.WriteString(w, "proxied\n")
	default:
		http.NotFound(w, r)
	}
}

func TestRunProbe(t *testing.T) {
	node := &nodeStandIn{notReady: 2}
	ns := httptest.NewServer(node)
	defer ns.Close()
	sink := newTestSink(t)
	ctl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, sink.Log())
	}))
	defer ctl.Close()

	c := probeConfig{
		ready: ns.URL + "/readyz", get: ns.URL + "/", host: "api.shop.example",
		sinkCheck: sink.Addr(), sinkName: "rz-sink-check.invalid.", dnsLog: ctl.URL,
		timeout: 30 * time.Second, interval: time.Millisecond,
	}
	client, err := probeClient("")
	if err != nil {
		t.Fatal(err)
	}
	rep := runProbe(t.Context(), c, client)
	if rep.Ready == nil || rep.Ready.Status != http.StatusOK || rep.Ready.Attempts != 3 {
		t.Errorf("Ready = %+v, want 200 after 3 attempts", rep.Ready)
	}
	if rep.Request == nil || rep.Request.Status != http.StatusOK || rep.Request.Body != "proxied\n" || rep.Request.Header.Get("X-Quickstart") != "ruralz" {
		t.Errorf("Request = %+v, want the proxied 200 with its header", rep.Request)
	}
	if got, _ := node.host.Load().(string); got != "api.shop.example" {
		t.Errorf("Host of the request = %q, want api.shop.example", got)
	}
	if rep.SinkCheck == nil || !rep.SinkCheck.NotFound || rep.SinkCheck.Name != "rz-sink-check.invalid." {
		t.Errorf("SinkCheck = %+v, want a not-found lookup of rz-sink-check.invalid.", rep.SinkCheck)
	}
	if rep.DNS == nil || rep.DNSError != "" {
		t.Fatalf("DNS = %+v, %q; want the sink's log", rep.DNS, rep.DNSError)
	}
	found := false
	for _, q := range rep.DNS.Queries {
		found = found || q.Name == "rz-sink-check.invalid."
	}
	if !found {
		t.Errorf("DNS log %+v misses the sink check, which the probe must read after it", rep.DNS.Queries)
	}
}

func TestRunProbeNeverReady(t *testing.T) {
	ns := httptest.NewServer(&nodeStandIn{notReady: 1 << 30})
	defer ns.Close()
	client, err := probeClient("")
	if err != nil {
		t.Fatal(err)
	}
	c := probeConfig{ready: ns.URL + "/readyz", get: ns.URL + "/", dnsLog: ns.URL + "/missing", timeout: 200 * time.Millisecond, interval: time.Millisecond}
	rep := runProbe(t.Context(), c, client)
	if rep.Ready == nil || rep.Ready.Status != http.StatusServiceUnavailable || rep.Ready.Attempts == 0 {
		t.Errorf("Ready = %+v, want the last 503", rep.Ready)
	}
	if rep.Request == nil || rep.Request.Attempts != 0 || !strings.Contains(rep.Request.Error, "not sent") {
		t.Errorf("Request = %+v, want it not sent", rep.Request)
	}
	// The DNS log is read on its own budget even after the wait used its
	// whole timeout.
	if rep.DNS != nil || !strings.Contains(rep.DNSError, "status 404") {
		t.Errorf("DNS = %+v, %q; want the 404 of the missing log", rep.DNS, rep.DNSError)
	}
}

func TestProbeTrustsCA(t *testing.T) {
	ts := httptest.NewUnstartedServer(&nodeStandIn{})
	ts.Config.ErrorLog = slog.NewLogLogger(slog.DiscardHandler, slog.LevelError) // the refused handshakes are expected
	ts.StartTLS()
	defer ts.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ts.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Run("with -ca", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if got := probe(t.Context(), []string{"-ca", ca, "-ready", ts.URL + "/readyz", "-timeout", "30s"}, &stdout, &stderr); got != ExitOK {
			t.Fatalf("probe = %d, want %d (%s)", got, ExitOK, stderr.String())
		}
		var rep ProbeReport
		if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil {
			t.Fatal(err)
		}
		if rep.Ready == nil || rep.Ready.Status != http.StatusOK {
			t.Errorf("Ready = %+v, want 200 over TLS", rep.Ready)
		}
	})
	t.Run("without -ca", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if got := probe(t.Context(), []string{"-ready", ts.URL + "/readyz", "-timeout", "300ms", "-interval", "10ms"}, &stdout, &stderr); got != ExitOK {
			t.Fatalf("probe = %d, want %d (%s)", got, ExitOK, stderr.String())
		}
		var rep ProbeReport
		if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil {
			t.Fatal(err)
		}
		if rep.Ready == nil || rep.Ready.Status != 0 || !strings.Contains(rep.Ready.Error, "certificate") {
			t.Errorf("Ready = %+v, want a certificate error", rep.Ready)
		}
	})
	t.Run("a -ca file without certificates", func(t *testing.T) {
		empty := filepath.Join(t.TempDir(), "empty.pem")
		if err := os.WriteFile(empty, []byte("none"), 0o600); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		if got := probe(t.Context(), []string{"-ca", empty, "-ready", ts.URL}, &stdout, &stderr); got != ExitFailure {
			t.Errorf("probe with an empty -ca = %d, want %d", got, ExitFailure)
		}
	})
}

func TestProbeUsage(t *testing.T) {
	for _, args := range [][]string{{}, {"-bogus"}, {"-host", "h", "-ready", "u"}, {"-sink-check", "a:53"}} {
		var stdout, stderr bytes.Buffer
		if got := probe(t.Context(), args, &stdout, &stderr); got != ExitUsage {
			t.Errorf("probe(%q) = %d, want %d", args, got, ExitUsage)
		}
	}
	var stdout, stderr bytes.Buffer
	if got := probe(t.Context(), []string{"-h"}, &stdout, &stderr); got != ExitOK || !strings.Contains(stderr.String(), "-sink-check-name") {
		t.Errorf("probe -h = %d, %q; want 0 and the flags", got, stderr.String())
	}
}
