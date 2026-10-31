// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package listener

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// captureHandler is a slog.Handler keeping every record's level and
// message.
type captureHandler struct {
	mu   sync.Mutex
	recs []string
}

func (*captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recs = append(h.recs, r.Level.String()+" "+r.Message)
	return nil
}
func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

// has reports whether a record at level contains part.
func (h *captureHandler) has(level slog.Level, part string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.recs {
		if strings.HasPrefix(r, level.String()+" ") && strings.Contains(r, part) {
			return true
		}
	}
	return false
}

// TestServerErrorLogBridged checks that Server.ErrorLog writes to the
// error-log handler of the Options at WARN, as the telemetry bridge
// expects: a failed handshake's net/http log line arrives there.
func TestServerErrorLogBridged(t *testing.T) {
	p := newTestPKI(t)
	logs := &captureHandler{}
	h := newHarness(t, func(o *Options) { o.ErrorLog = logs })
	h.activate(t, snap(httpsListener("api", false, p.cert(t, "api", "127.0.0.1"))), p.secrets)
	if _, err := dialRaw(t, h.addr(t, "api"), nil).roundTrip(t.Context(), "/", nil); err != nil {
		t.Fatalf("cleartext request to https: %v", err)
	}
	waitFor(t, "the handshake error record", func() bool { return logs.has(slog.LevelWarn, "TLS handshake error") })
}

// TestServerSettings checks the Server of each listener protocol against
// "Bounded resources": ReadHeaderTimeout 10 s, IdleTimeout 120 s,
// MaxHeaderBytes 256 KiB, HTTP/2 limits only through HTTP2Config (250
// streams, 256 KiB and 64 KiB receive buffers, 16 KiB frames), ReadTimeout
// and WriteTimeout unset; https serves HTTP/1.1 and HTTP/2, http serves
// HTTP/1.1 and h2c, never both HTTP/2 forms.
func TestServerSettings(t *testing.T) {
	tests := []struct {
		protocol         v1alpha1.ListenerProtocol
		http2, cleartext bool
	}{
		{protocol: v1alpha1.ListenerProtocolHTTPS, http2: true},
		{protocol: v1alpha1.ListenerProtocolHTTP, cleartext: true},
	}
	for _, tt := range tests {
		t.Run(string(tt.protocol), func(t *testing.T) {
			srv := newHTTPServer(tt.protocol, http.NotFoundHandler(), slog.DiscardHandler)
			if srv.ReadHeaderTimeout != 10*time.Second || srv.IdleTimeout != 120*time.Second {
				t.Errorf("timeouts = header %v, idle %v; want 10s, 2m0s", srv.ReadHeaderTimeout, srv.IdleTimeout)
			}
			if srv.ReadTimeout != 0 || srv.WriteTimeout != 0 {
				t.Errorf("ReadTimeout, WriteTimeout = %v, %v; want unset", srv.ReadTimeout, srv.WriteTimeout)
			}
			if srv.MaxHeaderBytes != 256<<10 {
				t.Errorf("MaxHeaderBytes = %d, want %d", srv.MaxHeaderBytes, 256<<10)
			}
			h2 := srv.HTTP2
			if h2 == nil || h2.MaxConcurrentStreams != 250 || h2.MaxReceiveBufferPerConnection != 256<<10 ||
				h2.MaxReceiveBufferPerStream != 64<<10 || h2.MaxReadFrameSize != 16<<10 {
				t.Errorf("HTTP2 = %+v, want 250 streams, 256 KiB and 64 KiB receive buffers, 16 KiB frames", h2)
			}
			p := srv.Protocols
			if p == nil || !p.HTTP1() || p.HTTP2() != tt.http2 || p.UnencryptedHTTP2() != tt.cleartext {
				t.Errorf("Protocols = %v, want HTTP1, HTTP2 %t, UnencryptedHTTP2 %t", p, tt.http2, tt.cleartext)
			}
			if srv.ErrorLog == nil || srv.ConnContext == nil {
				t.Error("ErrorLog or ConnContext unset")
			}
			if srv.TLSConfig != nil {
				t.Error("TLSConfig set: the listener terminates TLS itself")
			}
		})
	}
}

// TestHTTPListenerServesHTTP1AndH2C checks an http listener: HTTP/1.1, and
// HTTP/2 by prior knowledge (h2c), both reaching the handler with the
// listener's connection record.
func TestHTTPListenerServesHTTP1AndH2C(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.ConnStripe = func() emit.Stripe { return 3 } })
	h.activate(t, snap(httpListener("web", 0)), nil)
	url := "http://" + h.addr(t, "web") + "/"

	c1, _ := client(t, nil, true, false, false)
	status, body := get(t, c1, url)
	if status != http.StatusOK || field(body, 0) != "HTTP/1.1" || field(body, 1) != "web" || field(body, 5) != "3" {
		t.Errorf("HTTP/1.1: %d %q, want HTTP/1.1 on web with stripe 3", status, body)
	}
	if peer := field(body, 4); !strings.HasPrefix(peer, "127.0.0.1:") {
		t.Errorf("peer = %q, want the TCP peer", peer)
	}
	c2, _ := client(t, nil, false, false, true)
	if status, body := get(t, c2, url); status != http.StatusOK || field(body, 0) != "HTTP/2.0" || field(body, 1) != "web" {
		t.Errorf("h2c: %d %q", status, body)
	}
}

// TestHTTPListenerIgnoresH2CUpgrade checks that h2c is prior knowledge
// only: an HTTP/1.1 request asking Upgrade: h2c is answered over HTTP/1.1,
// never 101.
func TestHTTPListenerIgnoresH2CUpgrade(t *testing.T) {
	h := newHarness(t)
	h.activate(t, snap(httpListener("web", 0)), nil)
	c := dialRaw(t, h.addr(t, "web"), nil)
	r, err := c.roundTrip(t.Context(), "/", http.Header{
		"Connection":     {"Upgrade, HTTP2-Settings"},
		"Upgrade":        {"h2c"},
		"Http2-Settings": {"AAMAAABkAARAAAAAAAIAAAAA"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.status != http.StatusOK || field(r.body, 0) != "HTTP/1.1" {
		t.Fatalf("Upgrade: h2c answered %d %q, want 200 over HTTP/1.1", r.status, r.body)
	}
}

// TestHTTPSListenerNegotiatesALPN checks an https listener: ALPN h2 gives
// HTTP/2, a client offering only http/1.1 gets HTTP/1.1.
func TestHTTPSListenerNegotiatesALPN(t *testing.T) {
	p := newTestPKI(t)
	h := newHarness(t)
	h.activate(t, snap(httpsListener("api", false, p.cert(t, "api", "127.0.0.1"))), p.secrets)
	url := "https://" + h.addr(t, "api") + "/"
	cfg := &tls.Config{RootCAs: p.roots(), MinVersion: tls.VersionTLS12}

	c2, _ := client(t, cfg, true, true, false)
	if status, body := get(t, c2, url); status != http.StatusOK || field(body, 0) != "HTTP/2.0" || field(body, 1) != "api" {
		t.Errorf("h2: %d %q", status, body)
	}
	c1, _ := client(t, cfg, true, false, false)
	if status, body := get(t, c1, url); status != http.StatusOK || field(body, 0) != "HTTP/1.1" {
		t.Errorf("http/1.1: %d %q", status, body)
	}
}

// TestHeaderBlockCeiling checks MaxHeaderBytes: a header block below
// 256 KiB reaches the handler, one above it gets net/http's plain 431
// without reaching it.
func TestHeaderBlockCeiling(t *testing.T) {
	h := newHarness(t)
	h.activate(t, snap(httpListener("web", 0)), nil)
	addr := h.addr(t, "web")
	for _, tt := range []struct {
		size int
		want int
	}{
		{size: 200 << 10, want: http.StatusOK},
		{size: 300 << 10, want: http.StatusRequestHeaderFieldsTooLarge},
	} {
		c := dialRaw(t, addr, nil)
		r, err := c.roundTrip(t.Context(), "/", http.Header{"X-Pad": {strings.Repeat("a", tt.size)}})
		if err != nil {
			t.Fatalf("%d-byte header: %v", tt.size, err)
		}
		if r.status != tt.want {
			t.Errorf("%d-byte header: status %d, want %d", tt.size, r.status, tt.want)
		}
		if tt.want != http.StatusOK && strings.Contains(r.body, "web") {
			t.Errorf("%d-byte header reached the handler: %q", tt.size, r.body)
		}
	}
}
