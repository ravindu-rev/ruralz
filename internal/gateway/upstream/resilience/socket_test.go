// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package resilience

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"syscall"
	"testing"
	"time"
)

// Real-socket classification (05 req 39 "MUST be table-tested against real
// sockets", 05 section 6 test 1). Each case runs one attempt through an
// http.Transport shaped like the Upstream layer's (05 req 27) with a
// StageTrace, and classifies the RoundTrip error.

// attempt runs one GET through tr and classifies its error.
func attempt(ctx context.Context, t *testing.T, tr http.RoundTripper, url string) (Kind, error) {
	t.Helper()
	k, err := classifyAttempt(ctx, tr, url)
	if errors.Is(err, errGotResponse) {
		t.Fatal(err)
	}
	return k, err
}

// errGotResponse reports an attempt that unexpectedly got a response.
var errGotResponse = errors.New("RoundTrip got a response")

// classifyAttempt runs one GET through tr and classifies its error; it is
// safe to call from any goroutine.
func classifyAttempt(ctx context.Context, tr http.RoundTripper, url string) (Kind, error) {
	var st StageTrace
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, st.ClientTrace()), http.MethodGet, url, http.NoBody)
	if err != nil {
		return KindNone, err
	}
	resp, err := tr.RoundTrip(req)
	if resp != nil {
		_ = resp.Body.Close()
		return KindNone, errGotResponse
	}
	return ClassifyStage(ctx, err, st.Stage()), err
}

// transport returns a Transport with the fixed dial and TLS bounds,
// cleartext HTTP/1 and HTTP/1 plus HTTP/2 over TLS.
func transport(t *testing.T, tlsConf *tls.Config, control func(network, address string, c syscall.RawConn) error) *http.Transport {
	t.Helper()
	d := &net.Dialer{Timeout: DialTimeout, Control: control}
	tr := &http.Transport{
		DialContext:         d.DialContext,
		TLSHandshakeTimeout: TLSHandshakeTimeout,
		TLSClientConfig:     tlsConf,
		DisableCompression:  true,
		Proxy:               nil,
	}
	t.Cleanup(tr.CloseIdleConnections)
	return tr
}

// listen opens a loopback TCP listener.
func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

// closedAddr returns an address nothing listens on.
func closedAddr(t *testing.T) string {
	t.Helper()
	ln := listen(t)
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// silentListener accepts connections and reads them without answering.
func silentListener(t *testing.T, ln net.Listener) string {
	t.Helper()
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				_, _ = io.Copy(io.Discard, c)
			}()
		}
	}()
	return ln.Addr().String()
}

func TestClassifyRealSocketsConnect_05Req39(t *testing.T) {
	t.Parallel()
	t.Run("closed port", func(t *testing.T) {
		k, err := attempt(context.Background(), t, transport(t, nil, nil), "http://"+closedAddr(t)+"/")
		if k != KindConnect {
			t.Fatalf("kind %v for %v", k, err)
		}
	})
	t.Run("egress guard refusal", func(t *testing.T) {
		refuse := func(string, string, syscall.RawConn) error {
			return errors.New("egress: 127.0.0.1 is not allowed (RURALZ_FETCH_ALLOW)")
		}
		srv := httptest.NewServer(http.NotFoundHandler())
		t.Cleanup(srv.Close)
		k, err := attempt(context.Background(), t, transport(t, nil, refuse), srv.URL)
		if k != KindConnect {
			t.Fatalf("kind %v for %v", k, err)
		}
	})
}

func TestClassifyRealSocketsTLS_05Req39(t *testing.T) {
	t.Parallel()
	t.Run("plaintext server", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		t.Cleanup(srv.Close)
		k, err := attempt(context.Background(), t, transport(t, nil, nil), "https://"+srv.Listener.Addr().String()+"/")
		if k != KindTLS {
			t.Fatalf("kind %v for %v", k, err)
		}
	})
	t.Run("untrusted certificate", func(t *testing.T) {
		srv := httptest.NewTLSServer(http.NotFoundHandler())
		t.Cleanup(srv.Close)
		k, err := attempt(context.Background(), t, transport(t, nil, nil), srv.URL)
		if k != KindTLS {
			t.Fatalf("kind %v for %v", k, err)
		}
	})
	t.Run("handshake never completes", func(t *testing.T) {
		ln := listen(t)
		addr := silentListener(t, ln)
		tr := transport(t, nil, nil)
		tr.TLSHandshakeTimeout = 100 * time.Millisecond // 2 s in production
		k, err := attempt(context.Background(), t, tr, "https://"+addr+"/")
		if k != KindTLS {
			t.Fatalf("kind %v for %v", k, err)
		}
	})
}

func TestClassifyRealSocketsReset_05Req39(t *testing.T) {
	t.Parallel()
	t.Run("server closes after reading the headers", func(t *testing.T) {
		ln := listen(t)
		t.Cleanup(func() { _ = ln.Close() })
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				buf := make([]byte, 4096)
				_, _ = c.Read(buf)
				_ = c.Close()
			}
		}()
		k, err := attempt(context.Background(), t, transport(t, nil, nil), "http://"+ln.Addr().String()+"/")
		if k != KindReset {
			t.Fatalf("kind %v for %v", k, err)
		}
	})
	t.Run("HTTP/2 RST_STREAM", func(t *testing.T) {
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			panic(http.ErrAbortHandler) // resets the stream before headers
		}))
		srv.EnableHTTP2 = true
		srv.StartTLS()
		t.Cleanup(srv.Close)
		tr := transport(t, srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone(), nil)
		tr.Protocols = h2Only()
		k, err := attempt(context.Background(), t, tr, srv.URL)
		if k != KindReset {
			t.Fatalf("kind %v for %v", k, err)
		}
	})
	t.Run("HTTP/2 ping timeout on a silent server", func(t *testing.T) {
		srv := httptest.NewUnstartedServer(http.NotFoundHandler())
		srv.EnableHTTP2 = true
		srv.StartTLS()
		clientConf := srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
		certs := srv.TLS.Certificates
		srv.Close()
		ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: certs, NextProtos: []string{"h2"}, MinVersion: tls.VersionTLS12})
		if err != nil {
			t.Fatal(err)
		}
		addr := silentListener(t, ln)
		tr := transport(t, clientConf, nil)
		tr.Protocols = h2Only()
		// 1 s and 5 s in production (05 req 22).
		tr.HTTP2 = &http.HTTP2Config{SendPingTimeout: 50 * time.Millisecond, PingTimeout: 100 * time.Millisecond}
		k, err := attempt(context.Background(), t, tr, "https://"+addr+"/")
		if k != KindReset {
			t.Fatalf("kind %v for %v", k, err)
		}
	})
}

// h2Only returns the HTTP/2-only protocol set.
func h2Only() *http.Protocols {
	p := new(http.Protocols)
	p.SetHTTP2(true)
	return p
}

// TestClassifyRealSocketsTimeout_05Req39 expires the attempt, leg and
// Route contexts of an attempt waiting for headers (05 reqs 24 and 39:
// only these deadlines yield timeout).
func TestClassifyRealSocketsTimeout_05Req39(t *testing.T) {
	t.Parallel()
	arrived := make(chan struct{}, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	tests := []struct {
		name      string
		cause     error
		viaRoute  bool
		wantCause error
	}{
		{name: "per-try", cause: ErrAttemptTimeout, wantCause: ErrAttemptTimeout},
		{name: "leg", cause: ErrLegTimeout, wantCause: ErrLegTimeout},
		{name: "Route", viaRoute: true, wantCause: context.Canceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clk := newClock()
			route, cancelRoute := context.WithCancel(context.Background())
			defer cancelRoute()
			ctx, d := WithDeadline(route, clk, clk.Now().Add(time.Second), tt.cause)
			defer d.Cancel()
			type result struct {
				k   Kind
				err error
			}
			done := make(chan result, 1)
			tr := transport(t, nil, nil)
			go func() {
				k, err := classifyAttempt(ctx, tr, srv.URL)
				done <- result{k, err}
			}()
			<-arrived
			if tt.viaRoute {
				cancelRoute()
			} else {
				clk.Advance(time.Second)
			}
			r := <-done
			if r.k != KindTimeout {
				t.Fatalf("kind %v for %v", r.k, r.err)
			}
			if got := context.Cause(ctx); !errors.Is(got, tt.wantCause) {
				t.Fatalf("cause %v, want %v", got, tt.wantCause)
			}
			if d.Expired() == tt.viaRoute {
				t.Fatalf("Expired() = %v", d.Expired())
			}
		})
	}
}
