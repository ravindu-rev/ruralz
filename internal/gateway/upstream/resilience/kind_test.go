// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package resilience

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// TestKindLabels_05Req39 holds the kind spellings and their alignment with
// the error label of ruralz_upstream_attempts_total and the emit indexes.
func TestKindLabels_05Req39(t *testing.T) {
	fam, ok := catalog.Lookup(catalog.UpstreamAttemptsTotal)
	if !ok {
		t.Fatal("catalog has no ruralz_upstream_attempts_total")
	}
	i := slices.IndexFunc(fam.Labels, func(l catalog.Label) bool { return l.Name == "error" })
	if i < 0 {
		t.Fatal("ruralz_upstream_attempts_total has no error label")
	}
	var names []string
	for _, k := range Kinds() {
		names = append(names, k.String())
	}
	if !slices.Equal(names, fam.Labels[i].Values) {
		t.Fatalf("Kinds() = %v, want the error label values %v", names, fam.Labels[i].Values)
	}
	if len(Kinds()) != NumKinds || NumKinds != emit.NumAttemptErrors {
		t.Fatalf("NumKinds = %d, emit.NumAttemptErrors = %d", NumKinds, emit.NumAttemptErrors)
	}
	idx := map[Kind]int{KindNone: emit.ErrNone, KindConnect: emit.ErrConnect, KindTimeout: emit.ErrTimeout, KindReset: emit.ErrReset, KindTLS: emit.ErrTLS}
	for k, want := range idx {
		if int(k) != want {
			t.Errorf("int(%v) = %d, want emit index %d", k, int(k), want)
		}
		got, ok := ParseKind(k.String())
		if !ok || got != k {
			t.Errorf("ParseKind(%q) = %v, %v", k.String(), got, ok)
		}
	}
	if _, ok := ParseKind("bogus"); ok {
		t.Error(`ParseKind("bogus") succeeded`)
	}
	if got := Kind(200).String(); got != "none" {
		t.Errorf("Kind(200).String() = %q", got)
	}
}

// wrapped nests err under n levels of %w.
func wrapped(err error, n int) error {
	for range n {
		err = fmt.Errorf("level: %w", err)
	}
	return err
}

// TestClassifyStage_05Req39 classifies synthetic errors of every shape.
func TestClassifyStage_05Req39(t *testing.T) {
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	live := context.Background()
	dial := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	tests := []struct {
		name  string
		ctx   context.Context
		err   error
		stage Stage
		want  Kind
	}{
		{"nil", live, nil, StageExchange, KindNone},
		{"nil with ended context", ended, nil, StageExchange, KindNone},
		{"ended context wins over dial", ended, dial, StageDial, KindTimeout},
		{"ended context wins over EOF", ended, io.EOF, StageExchange, KindTimeout},
		{"dial refused", live, dial, StageUnknown, KindConnect},
		{"dial wrapped", live, wrapped(dial, 3), StageExchange, KindConnect},
		{"static host DNS", live, &net.DNSError{Err: "no such host", Name: "api.internal", IsNotFound: true}, StageUnknown, KindConnect},
		{"record header", live, tls.RecordHeaderError{Msg: "tls: first record does not look like a TLS handshake"}, StageUnknown, KindTLS},
		{"alert", live, tls.AlertError(42), StageUnknown, KindTLS},
		{"verification", live, &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}, StageUnknown, KindTLS},
		{"unknown authority", live, x509.UnknownAuthorityError{}, StageUnknown, KindTLS},
		{"hostname", live, x509.HostnameError{Host: "x"}, StageUnknown, KindTLS},
		{"invalid certificate", live, x509.CertificateInvalidError{Reason: x509.Expired}, StageUnknown, KindTLS},
		{"ECH rejection", live, &tls.ECHRejectionError{}, StageUnknown, KindTLS},
		{"remote alert", live, &net.OpError{Op: "remote error", Err: errors.New("tls: bad certificate")}, StageUnknown, KindTLS},
		{"local alert", live, &net.OpError{Op: "local error", Err: errors.New("tls: internal error")}, StageUnknown, KindTLS},
		{"tls text", live, wrapped(errors.New("tls: handshake failure"), 2), StageUnknown, KindTLS},
		{"handshake timeout text", live, errors.New("net/http: TLS handshake timeout"), StageUnknown, KindTLS},
		{"scheme mismatch", live, wrapped(http.ErrSchemeMismatch, 1), StageUnknown, KindTLS},
		{"joined tls", live, errors.Join(io.EOF, errors.New("tls: bad record MAC")), StageUnknown, KindTLS},
		{"EOF during dial stage", live, io.EOF, StageDial, KindConnect},
		{"EOF during TLS stage", live, io.EOF, StageTLS, KindTLS},
		{"EOF on the exchange", live, io.EOF, StageExchange, KindReset},
		{"unexpected EOF", live, io.ErrUnexpectedEOF, StageUnknown, KindReset},
		{"read reset", live, &net.OpError{Op: "read", Err: errors.New("connection reset by peer")}, StageExchange, KindReset},
		{"HTTP/2 stream reset text", live, errors.New("stream error: stream ID 1; INTERNAL_ERROR; received from peer"), StageExchange, KindReset},
		{"HTTP/2 connection lost", live, errors.New("http2: client connection lost"), StageExchange, KindReset},
		{"protocol error", live, errors.New(`malformed HTTP status code "x"`), StageExchange, KindReset},
		{"caller context deadline", live, wrapped(context.DeadlineExceeded, 1), StageExchange, KindTimeout},
		{"caller context canceled", live, context.Canceled, StageUnknown, KindTimeout},
		{"tls text beyond the walk depth", live, wrapped(errors.New("tls: deep"), maxChainDepth+2), StageExchange, KindReset},
		{"nil context", nil, io.EOF, StageExchange, KindReset},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyStage(tt.ctx, tt.err, tt.stage); got != tt.want {
				t.Fatalf("ClassifyStage(%v, stage %d) = %v, want %v", tt.err, tt.stage, got, tt.want)
			}
		})
	}
	if got := Classify(live, dial); got != KindConnect {
		t.Errorf("Classify(dial) = %v", got)
	}
}

// TestExpiredCanceled_05Req39 separates a deadline expiry (error kind
// timeout, an Upstream failure) from a cancellation that is none: the
// client went away or a sibling composition step canceled the shared
// context.
func TestExpiredCanceled_05Req39(t *testing.T) {
	clk := newClock()
	past := time.Now().Add(-time.Second)
	errSibling := errors.New("sibling step failed")
	tests := []struct {
		name     string
		ctx      func(t *testing.T) context.Context
		expired  bool
		canceled bool
	}{
		{"live", func(*testing.T) context.Context { return context.Background() }, false, false},
		{"nil", func(*testing.T) context.Context { return nil }, false, false},
		{"attempt deadline", func(*testing.T) context.Context {
			ctx, _ := WithDeadline(context.Background(), clk, clk.Now(), ErrAttemptTimeout)
			return ctx
		}, true, false},
		{"leg deadline", func(*testing.T) context.Context {
			ctx, _ := WithDeadline(context.Background(), clk, clk.Now(), ErrLegTimeout)
			return ctx
		}, true, false},
		{"Route deadline", func(t *testing.T) context.Context {
			ctx, cancel := context.WithDeadline(context.Background(), past)
			t.Cleanup(cancel)
			return ctx
		}, true, false},
		{"Route deadline with its own cause", func(t *testing.T) context.Context {
			ctx, cancel := context.WithDeadlineCause(context.Background(), past, errors.New("route timeout"))
			t.Cleanup(cancel)
			return ctx
		}, true, false},
		{"attempt context under an expired Route context", func(t *testing.T) context.Context {
			route, cancel := context.WithDeadline(context.Background(), past)
			t.Cleanup(cancel)
			ctx, d := WithDeadline(route, clk, time.Time{}, ErrAttemptTimeout)
			t.Cleanup(d.Cancel)
			return ctx
		}, true, false},
		{"client gone", func(*testing.T) context.Context {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx
		}, false, true},
		{"sibling composition step", func(*testing.T) context.Context {
			ctx, cancel := context.WithCancelCause(context.Background())
			cancel(errSibling)
			return ctx
		}, false, true},
		{"attempt context under a canceled request", func(t *testing.T) context.Context {
			req, cancel := context.WithCancel(context.Background())
			cancel()
			ctx, d := WithDeadline(req, clk, clk.Now().Add(time.Hour), ErrAttemptTimeout)
			t.Cleanup(d.Cancel)
			return ctx
		}, false, true},
		{"Deadline.Cancel", func(*testing.T) context.Context {
			ctx, d := WithDeadline(context.Background(), clk, clk.Now().Add(time.Hour), ErrAttemptTimeout)
			d.Cancel()
			return ctx
		}, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := tt.ctx(t)
			if got := Expired(ctx); got != tt.expired {
				t.Errorf("Expired = %v", got)
			}
			if got := Canceled(ctx); got != tt.canceled {
				t.Errorf("Canceled = %v", got)
			}
			if ctx != nil && ctx.Err() != nil && ClassifyStage(ctx, context.Canceled, StageExchange) != KindTimeout {
				t.Error("an ended context does not classify as timeout")
			}
		})
	}
}

// TestStageTrace covers the monotonic stage and its hooks, the stage input
// of classification (05 req 39).
func TestStageTrace(t *testing.T) {
	var s StageTrace
	if s.Stage() != StageUnknown {
		t.Fatal("zero StageTrace is not StageUnknown")
	}
	tr := s.ClientTrace()
	if tr != s.ClientTrace() {
		t.Fatal("ClientTrace is rebuilt per call")
	}
	tr.ConnectStart("tcp", "127.0.0.1:1")
	if s.Stage() != StageDial {
		t.Fatalf("after ConnectStart: %d", s.Stage())
	}
	tr.TLSHandshakeStart()
	tr.ConnectStart("tcp", "127.0.0.1:1") // a late second dial never moves it back
	if s.Stage() != StageTLS {
		t.Fatalf("after TLSHandshakeStart: %d", s.Stage())
	}
	tr.GotConn(httptrace.GotConnInfo{})
	if s.Stage() != StageExchange {
		t.Fatalf("after GotConn: %d", s.Stage())
	}
	s.Reset()
	if s.Stage() != StageUnknown {
		t.Fatal("Reset kept the stage")
	}
	tr.DNSStart(httptrace.DNSStartInfo{})
	if s.Stage() != StageDial {
		t.Fatalf("after DNSStart: %d", s.Stage())
	}
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			if i%2 == 0 {
				tr.GotConn(httptrace.GotConnInfo{})
			} else {
				tr.ConnectStart("tcp", "x")
			}
		})
	}
	wg.Wait()
	if s.Stage() != StageExchange {
		t.Fatalf("concurrent hooks ended at %d", s.Stage())
	}
}

// TestKindError covers the kind annotation of RZ-UP-007 (05 req 40).
func TestKindError(t *testing.T) {
	cause := errors.New("boom")
	err := fmt.Errorf("leg: %w", &KindError{Kind: KindReset, Err: cause})
	if KindOf(err) != KindReset || !errors.Is(err, cause) {
		t.Fatalf("KindOf/Is failed for %v", err)
	}
	if got := (&KindError{Kind: KindTLS}).Error(); got != "upstream tls error" {
		t.Errorf("Error() = %q", got)
	}
	if got := (&KindError{Kind: KindConnect, Err: cause}).Error(); !strings.HasSuffix(got, ": boom") {
		t.Errorf("Error() = %q", got)
	}
	if KindOf(cause) != KindNone {
		t.Error("KindOf of a plain error is not KindNone")
	}
}
