// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package topology

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// captureStore keeps the bytes the leak scan reads beyond the logs and
// data directories: one append-only file per source under dir.
type captureStore struct {
	dir string
	mu  sync.Mutex
}

// add appends data and a newline to the file of source.
func (s *captureStore) add(source string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(filepath.Join(s.dir, fileName(source)), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("topology: capture %s: %w", source, err)
	}
	_, werr := f.Write(append(bytes.Clone(data), '\n'))
	if err := f.Close(); werr == nil {
		werr = err
	}
	if werr != nil {
		return fmt.Errorf("topology: capture %s: %w", source, werr)
	}
	return nil
}

// fileName turns a source or test name into one path element: letters,
// digits, '.', '-' and '_' kept, anything else '_'.
func fileName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" || out == "." || out == ".." {
		return "_" + out
	}
	return out
}

// Capture records bytes a test got from the topology, such as a ruralz
// command's output or a client response, under source, for the teardown
// leak scan.
func (t *Topology) Capture(source string, data []byte) {
	if err := t.captures.add("capture-"+source, data); err != nil {
		t.t.Errorf("%v", err)
	}
}

// HTTPClient returns a client for requests to the Nodes, the balancers or
// the mocks: it trusts the harness's CAs, follows no redirect and
// captures every response body it reads for the teardown leak scan.
func (t *Topology) HTTPClient() *http.Client {
	tr := &http.Transport{
		TLSClientConfig:   t.ClientTLS(),
		ForceAttemptHTTP2: true,
	}
	t.track(func() { t.transports = append(t.transports, tr) })
	return &http.Client{
		Transport:     &capturingTransport{base: tr, topo: t},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// capturingTransport captures response bodies as they are read.
type capturingTransport struct {
	base http.RoundTripper
	topo *Topology
}

func (c *capturingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := c.base.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	resp.Body = &capturedBody{rc: resp.Body, topo: c.topo, source: "client-" + r.URL.Host}
	return resp, nil
}

// capturedBody records what is read from a response body, at Close.
type capturedBody struct {
	rc     io.ReadCloser
	topo   *Topology
	source string
	buf    bytes.Buffer
	once   sync.Once
}

func (b *capturedBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	b.buf.Write(p[:n])
	return n, err
}

func (b *capturedBody) Close() error {
	err := b.rc.Close()
	b.once.Do(func() {
		if cerr := b.topo.captures.add(b.source, b.buf.Bytes()); cerr != nil {
			b.topo.t.Errorf("%v", cerr)
		}
	})
	return err
}

// adminTLS is the client configuration of the admin port and its probes.
func (t *Topology) adminTLS() *tls.Config {
	return &tls.Config{RootCAs: t.ca.Pool(), MinVersion: tls.VersionTLS12}
}
