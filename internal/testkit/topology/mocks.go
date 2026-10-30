// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package topology

import (
	"bytes"
	"fmt"
	"net/http"
	"slices"
	"strconv"

	"github.com/ravindu-rev/ruralz/internal/testkit/mockidp"
	"github.com/ravindu-rev/ruralz/internal/testkit/mockup"
	"github.com/ravindu-rev/ruralz/internal/testkit/otlpsink"
)

// StartMockUpstream starts an in-process mock Upstream that lives until the
// teardown, which scans its request log for canaries: size
// mockup.Config.LogSize and MaxLoggedBody for the run, since a request the
// log dropped or cut fails the scan as incomplete. A Bundle that reaches it
// over TLS takes Config.TLS from ServerTLS.
func (t *Topology) StartMockUpstream(c mockup.Config) (*mockup.Server, error) {
	s, err := mockup.Start(t.ctx, c)
	if err != nil {
		return nil, fmt.Errorf("topology: %w", err)
	}
	if !t.track(func() { t.mocks = append(t.mocks, s) }) {
		_ = s.Close()
		return nil, errTorn
	}
	return s, nil
}

// StartIdP starts the in-process mock IdP that lives until the teardown.
// Its token endpoint is the designated destination of an
// auth.upstream-oauth2 client secret, so its records are not scanned. A
// Bundle's JWKS and token URLs must be https (RZ-CFG-037): Config.TLS
// defaults to a ServerTLS configuration.
func (t *Topology) StartIdP(c mockidp.Config) (*mockidp.Server, error) {
	if c.TLS == nil {
		cfg, err := t.ServerTLS()
		if err != nil {
			return nil, err
		}
		c.TLS = cfg
	}
	s, err := mockidp.Start(t.ctx, c)
	if err != nil {
		return nil, fmt.Errorf("topology: %w", err)
	}
	if !t.track(func() { t.idps = append(t.idps, s) }) {
		_ = s.Close()
		return nil, errTorn
	}
	return s, nil
}

// StartOTLPSink starts an in-process OTLP collector that lives until the
// teardown, which scans its records and keeps them as failure evidence.
// It serves TLS from the harness's CA (an https endpoint) unless
// Config.TLS is set or Transport.CleartextOTLP.
func (t *Topology) StartOTLPSink(c otlpsink.Config) (*otlpsink.Sink, error) {
	if c.TLS == nil && !t.cfg.Transport.CleartextOTLP {
		cfg, err := t.ServerTLS()
		if err != nil {
			return nil, err
		}
		c.TLS = cfg
	}
	s, err := otlpsink.Start(t.ctx, c)
	if err != nil {
		return nil, fmt.Errorf("topology: %w", err)
	}
	if !t.track(func() { t.sinks = append(t.sinks, s) }) {
		_ = s.Close()
		return nil, errTorn
	}
	return s, nil
}

// dumpMock writes a mock Upstream's request log as the leak scan reads it:
// each request's line, fields, trailers and body. It reports an error when
// the log dropped or cut a request, which leaves the scan incomplete.
func dumpMock(s *mockup.Server) ([]byte, error) {
	var b bytes.Buffer
	reqs := s.Requests()
	for _, r := range reqs {
		fmt.Fprintf(&b, "%s %s %s host=%s remote=%s\n", r.Method, r.URI, r.Proto, r.Host, r.RemoteAddr)
		writeHeader(&b, r.Header)
		writeHeader(&b, r.Trailer)
		b.Write(r.Body)
		b.WriteByte('\n')
	}
	var incomplete []string
	if n := s.Stats().LogEvicted; n > 0 {
		incomplete = append(incomplete, strconv.FormatInt(n, 10)+" requests dropped from the log (raise mockup.Config.LogSize)")
	}
	for _, r := range reqs {
		if r.BodyTruncated {
			incomplete = append(incomplete, "request bodies cut (raise mockup.Config.MaxLoggedBody)")
			break
		}
	}
	if len(incomplete) > 0 {
		return b.Bytes(), fmt.Errorf("topology: the leak scan of mock Upstream %q is incomplete: %v", s.Name(), incomplete)
	}
	return b.Bytes(), nil
}

// writeHeader writes header fields, one per line, in name order.
func writeHeader(b *bytes.Buffer, h http.Header) {
	names := make([]string, 0, len(h))
	for k := range h {
		names = append(names, k)
	}
	slices.Sort(names)
	for _, k := range names {
		for _, v := range h[k] {
			fmt.Fprintf(b, "%s: %s\n", k, v)
		}
	}
}
