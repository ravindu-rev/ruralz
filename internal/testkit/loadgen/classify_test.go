// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package loadgen

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"testing"
)

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o wait expired" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return false }

func TestClassify(t *testing.T) {
	wrap := func(err error) error { return &url.Error{Op: "Get", URL: "http://x", Err: err} }
	for _, c := range []struct {
		name string
		err  error
		want Outcome
	}{
		{"nil", nil, Success},
		{"request timeout", wrap(ErrTimeout), Timeout},
		{"deadline", wrap(context.DeadlineExceeded), Timeout},
		{"socket deadline", wrap(os.ErrDeadlineExceeded), Timeout},
		{"net timeout", wrap(timeoutErr{}), Timeout},
		{"canceled", wrap(context.Canceled), Canceled},
		{"EOF", wrap(io.EOF), Closed},
		{"unexpected EOF", wrap(io.ErrUnexpectedEOF), Closed},
		{"closed connection", wrap(net.ErrClosed), Closed},
		{"record header", wrap(tls.RecordHeaderError{Msg: "bad"}), TLSError},
		{"alert", wrap(tls.AlertError(40)), TLSError},
		{"unknown authority", wrap(x509.UnknownAuthorityError{}), TLSError},
		{"hostname", wrap(x509.HostnameError{Certificate: &x509.Certificate{}, Host: "x"}), TLSError},
		{"HTTP/2 lost connection", errors.New("http2: client connection lost"), Closed},
		{"HTTP/2 GOAWAY", errors.New("http2: server sent GOAWAY and closed the connection"), Closed},
		{"HTTP/2 stream reset", errors.New("stream error: stream ID 3; INTERNAL_ERROR; received from peer"), Reset},
		{"reset text", fmt.Errorf("read: %s", "connection reset by peer"), Reset},
		{"refused text", errors.New("dial tcp: connection refused"), Refused},
		{"tls text", errors.New("tls: handshake failure"), TLSError},
		{"timeout text", errors.New("net/http: TLS handshake timeout"), Timeout},
		{"other", errors.New("malformed HTTP response"), Other},
	} {
		if got := Classify(c.err); got != c.want {
			t.Errorf("%s: Classify(%v) = %v, want %v", c.name, c.err, got, c.want)
		}
	}
}
