// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package lab

import (
	"context"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/faultproxy"
	"github.com/ravindu-rev/ruralz/internal/testkit/loadgen"
	"github.com/ravindu-rev/ruralz/internal/testkit/mockidp"
	"github.com/ravindu-rev/ruralz/internal/testkit/mockup"
	"github.com/ravindu-rev/ruralz/internal/testkit/otlpsink"
)

// newHistogram returns an empty histogram at the run settings.
func newHistogram() *loadgen.Histogram {
	h, err := loadgen.NewHistogram(loadgen.DefaultLowest, loadgen.DefaultHighest, loadgen.DefaultDigits)
	if err != nil {
		panic(err) // constant, valid settings
	}
	return h
}

// serverTLS loads a server certificate and key; nil for empty paths.
func serverTLS(certFile, keyFile string) (*tls.Config, error) {
	if certFile == "" && keyFile == "" {
		return nil, nil
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("lab: TLS key pair: %w", err)
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, nil
}

// Sink modes of OpMode.
const (
	SinkStall  = "stall"
	SinkAccept = "accept"
)

// MockConfig configures the mock Upstream.
type MockConfig struct {
	// Listen is the listen address.
	Listen string
	// CertFile and KeyFile serve HTTPS when set.
	CertFile, KeyFile string
	// BodySize is every response body's size; S1's is 1 KiB.
	BodySize int64
	// Delay is waited before every response, O1a's 1 s.
	Delay time.Duration
	// Header is added to every response, such as the Cache-Control the
	// cache key flood's entries need.
	Header http.Header
	// Name, when set, is carried in every response's mockup.HeaderServer,
	// as the chaos runs' mock Upstreams name themselves.
	Name string
}

// MockStats are the mock's counters since the last reset.
type MockStats struct {
	// Requests counts the requests answered.
	Requests int64 `json:"requests"`
	// Connections counts the connections accepted (all time); Open is the
	// number open now.
	Connections int64 `json:"connections"`
	Open        int64 `json:"open"`
	// Excess is the mock's own latency beyond its configured delay, HdrHistogram
	// V2 base64; ExcessP99 its p99.
	Excess    string        `json:"excess"`
	ExcessP99 time.Duration `json:"excessP99Ns"`
}

// Mock is the mock Upstream service.
type Mock struct {
	srv   *mockup.Server
	cpu   *cpuMeter
	mu    sync.Mutex
	h     *loadgen.Histogram
	count int64
}

// StartMock starts the mock Upstream.
func StartMock(ctx context.Context, c MockConfig) (*Mock, error) {
	tc, err := serverTLS(c.CertFile, c.KeyFile)
	if err != nil {
		return nil, err
	}
	b := mockup.Behavior{BodySize: c.BodySize, Header: c.Header}
	if c.Delay > 0 {
		b.Delay = mockup.Fixed(c.Delay)
	}
	m := &Mock{cpu: newCPUMeter(), h: newHistogram()}
	m.srv, err = mockup.Start(ctx, mockup.Config{
		Name: c.Name, Listen: c.Listen, TLS: tc, Behavior: b, LogSize: -1, MaxConcurrentStreams: 1000,
		OnServed: m.served,
	})
	if err != nil {
		return nil, err
	}
	return m, nil
}

// served records one answered request's latency beyond its delay.
func (m *Mock) served(elapsed, delay time.Duration) {
	m.mu.Lock()
	m.h.Record(max(elapsed-delay, 0))
	m.count++
	m.mu.Unlock()
}

// Ready describes the mock.
func (m *Mock) Ready() Ready { return ready("mock", m.srv.Addr()) }

// Handle answers stats and reset.
func (m *Mock) Handle(_ context.Context, r Request) Response {
	switch r.Op {
	case OpReset:
		m.mu.Lock()
		m.h.Reset()
		m.count = 0
		m.mu.Unlock()
		m.cpu.reset()
		return Response{OK: true}
	case OpStats:
		st := m.srv.Stats()
		m.mu.Lock()
		ms := &MockStats{
			Requests: m.count, Connections: st.Connections, Open: st.OpenConnections,
			Excess: EncodeHistogram(m.h), ExcessP99: m.h.ValueAtPercentile(99),
		}
		m.mu.Unlock()
		return Response{OK: true, Process: m.cpu.stats(), Mock: ms}
	default:
		return unknown(r)
	}
}

// Close stops the mock.
func (m *Mock) Close() error { return m.srv.Close() }

// SinkConfig configures the OTLP sink.
type SinkConfig struct {
	Listen            string
	CertFile, KeyFile string
}

// SinkStats are the sink's counters.
type SinkStats struct {
	// Records counts the access records received; Missing those without
	// a gateway_duration; Outside those in no window.
	Records int64 `json:"records"`
	Missing int64 `json:"missing"`
	Outside int64 `json:"outside"`
	// Latest is the latest access record timestamp received.
	Latest time.Time `json:"latest"`
	// LogRequests, MetricRequests and TraceRequests count the exports.
	LogRequests    int64 `json:"logRequests"`
	MetricRequests int64 `json:"metricRequests"`
	TraceRequests  int64 `json:"traceRequests"`
	// Rejected counts exports answered with an error.
	Rejected int64 `json:"rejected"`
}

// Window is what one fold window holds.
type Window struct {
	Count     int64  `json:"count"`
	Missing   int64  `json:"missing"`
	Histogram string `json:"histogram"`
}

// Sink is the OTLP sink service, folding each access record's
// gateway_duration into the declared windows.
type Sink struct {
	s   *otlpsink.Sink
	cpu *cpuMeter
}

// StartSink starts the sink.
func StartSink(ctx context.Context, c SinkConfig) (*Sink, error) {
	tc, err := serverTLS(c.CertFile, c.KeyFile)
	if err != nil {
		return nil, err
	}
	s, err := otlpsink.Start(ctx, otlpsink.Config{
		Listen: c.Listen, TLS: tc,
		Fold: &otlpsink.Fold{Event: "ruralz.access", Attribute: "gateway_duration"},
	})
	if err != nil {
		return nil, err
	}
	return &Sink{s: s, cpu: newCPUMeter()}, nil
}

// Ready describes the sink.
func (s *Sink) Ready() Ready { return ready("sink", s.s.Addr()) }

// Handle answers window, result, remove, stats and reset.
func (s *Sink) Handle(ctx context.Context, r Request) Response {
	switch r.Op {
	case OpWindow:
		if err := s.s.AddFilteredWindow(r.Name, r.Start, r.End, r.Where); err != nil {
			return failed(err)
		}
		return Response{OK: true}
	case OpResult:
		// Records reach the sink after their request ended: wait until one
		// at or after the window's end arrived, within 30 s.
		if !r.End.IsZero() {
			wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			err := s.s.WaitFolded(wctx, r.End)
			cancel()
			if err != nil {
				return failed(err)
			}
		}
		w, err := s.s.Window(r.Name)
		if err != nil {
			return failed(err)
		}
		return Response{OK: true, Window: &Window{Count: w.Count, Missing: w.Missing, Histogram: EncodeHistogram(w.Histogram)}}
	case OpRemove:
		s.s.RemoveWindow(r.Name)
		return Response{OK: true}
	case OpMode:
		var m otlpsink.Mode
		switch r.Name {
		case SinkStall:
			m = otlpsink.Stall{}
		case SinkAccept:
			m = otlpsink.Accept{}
		default:
			return failed(fmt.Errorf("lab: sink mode %q is not %s or %s", r.Name, SinkStall, SinkAccept))
		}
		if err := s.s.SetMode(m); err != nil { //nolint:contextcheck // a sink leaving Down listens again for its own lifetime, not the request's
			return failed(err)
		}
		return Response{OK: true}
	case OpReset:
		s.cpu.reset()
		return Response{OK: true}
	case OpStats:
		f, st := s.s.FoldStats(), s.s.Stats()
		return Response{OK: true, Process: s.cpu.stats(), Sink: &SinkStats{
			Records: f.Records, Missing: f.Missing, Outside: f.Outside, Latest: f.Latest,
			LogRequests: st.LogRequests, MetricRequests: st.MetricRequests, TraceRequests: st.TraceRequests,
			Rejected: st.Rejected,
		}}
	default:
		return unknown(r)
	}
}

// Close stops the sink.
func (s *Sink) Close() error { return s.s.Close() }

// KeyFile is the lab's signing key file: the IdP publishes the keys and
// the load generator mints the run's bearer tokens with them.
type KeyFile struct {
	Keys []KeyEntry `json:"keys"`
}

// KeyEntry is one signing key: its algorithm, key ID and PKCS #8 private
// key in PEM.
type KeyEntry struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	PEM string `json:"pem"`
}

// NewKeyFile generates n keys of alg.
func NewKeyFile(alg mockidp.Alg, n int) (*KeyFile, error) {
	kf := &KeyFile{}
	for range n {
		k, err := mockidp.GenerateKey(alg)
		if err != nil {
			return nil, err
		}
		der, err := x509.MarshalPKCS8PrivateKey(k.Signer())
		if err != nil {
			return nil, err
		}
		kf.Keys = append(kf.Keys, KeyEntry{Alg: string(alg), Kid: k.ID, PEM: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))})
	}
	return kf, nil
}

// ReadKeyFile reads a key file.
func ReadKeyFile(path string) (*KeyFile, error) {
	b, err := os.ReadFile(path) //nolint:gosec // G304: the key file the harness names
	if err != nil {
		return nil, err
	}
	var kf KeyFile
	if err := json.Unmarshal(b, &kf); err != nil {
		return nil, fmt.Errorf("lab: key file: %w", err)
	}
	return &kf, nil
}

// Signers returns the mockidp keys.
func (kf *KeyFile) Signers() ([]*mockidp.Key, error) {
	var out []*mockidp.Key
	for _, e := range kf.Keys {
		blk, _ := pem.Decode([]byte(e.PEM))
		if blk == nil {
			return nil, fmt.Errorf("lab: key %s: no PEM block", e.Kid)
		}
		priv, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
		if err != nil {
			return nil, fmt.Errorf("lab: key %s: %w", e.Kid, err)
		}
		signer, ok := priv.(crypto.Signer)
		if !ok {
			return nil, fmt.Errorf("lab: key %s is not a signer", e.Kid)
		}
		k, err := mockidp.NewKey(mockidp.Alg(e.Alg), e.Kid, signer)
		if err != nil {
			return nil, fmt.Errorf("lab: key %s: %w", e.Kid, err)
		}
		out = append(out, k)
	}
	return out, nil
}

// IDPConfig configures the mock IdP.
type IDPConfig struct {
	Listen            string
	CertFile, KeyFile string
	// Issuer is the iss the tokens carry, the Bundle's issuer.
	Issuer string
	// Keys is the key file to publish.
	Keys string
}

// IDP is the mock IdP service: it publishes the key file's keys.
type IDP struct {
	s   *mockidp.Server
	cpu *cpuMeter
}

// StartIDP starts the mock IdP.
func StartIDP(ctx context.Context, c IDPConfig) (*IDP, error) {
	tc, err := serverTLS(c.CertFile, c.KeyFile)
	if err != nil {
		return nil, err
	}
	kf, err := ReadKeyFile(c.Keys)
	if err != nil {
		return nil, err
	}
	keys, err := kf.Signers()
	if err != nil {
		return nil, err
	}
	s, err := mockidp.Start(ctx, mockidp.Config{Listen: c.Listen, TLS: tc, Issuer: c.Issuer, Keys: keys})
	if err != nil {
		return nil, err
	}
	return &IDP{s: s, cpu: newCPUMeter()}, nil
}

// Ready describes the IdP.
func (i *IDP) Ready() Ready { return ready("idp", i.s.Addr()) }

// Handle answers stats and reset.
func (i *IDP) Handle(_ context.Context, r Request) Response {
	switch r.Op {
	case OpReset:
		i.cpu.reset()
		return Response{OK: true}
	case OpStats:
		return Response{OK: true, Process: i.cpu.stats()}
	default:
		return unknown(r)
	}
}

// Close stops the IdP.
func (i *IDP) Close() error { return i.s.Close() }

// ProxyConfig configures the fault proxy: S5x's 500 µs ± 100 µs of normal
// delay each way (target).
type ProxyConfig struct {
	Listen, Upstream string
	Mean, StdDev     time.Duration
	// PassFirst forwards unchanged until an OpMode request names
	// ProxyDelay, so connection setup, a TLS handshake's round trips
	// included, is not delayed: O2's replies are.
	PassFirst bool
}

// Fault proxy modes of OpMode.
const (
	ProxyDelay = "delay"
	ProxyPass  = "pass"
)

// ProxyStats are the fault proxy's counters.
type ProxyStats struct {
	Connections int64 `json:"connections"`
	Open        int64 `json:"open"`
}

// Proxy is the fault proxy service.
type Proxy struct {
	p     *faultproxy.Proxy
	delay faultproxy.Mode
	cpu   *cpuMeter
}

// StartProxy starts the fault proxy.
func StartProxy(ctx context.Context, c ProxyConfig) (*Proxy, error) {
	var delay faultproxy.Mode = faultproxy.Pass{}
	if c.Mean > 0 || c.StdDev > 0 {
		delay = faultproxy.Delay{Mean: c.Mean, StdDev: c.StdDev}
	}
	mode := delay
	if c.PassFirst {
		mode = faultproxy.Pass{}
	}
	p, err := faultproxy.Start(ctx, faultproxy.Config{Listen: c.Listen, Upstream: c.Upstream, Mode: mode})
	if err != nil {
		return nil, err
	}
	return &Proxy{p: p, delay: delay, cpu: newCPUMeter()}, nil
}

// Ready describes the proxy.
func (p *Proxy) Ready() Ready { return ready("faultproxy", p.p.Addr()) }

// Handle answers stats and reset.
func (p *Proxy) Handle(_ context.Context, r Request) Response {
	switch r.Op {
	case OpReset:
		p.cpu.reset()
		return Response{OK: true}
	case OpStats:
		st := p.p.Stats()
		return Response{OK: true, Process: p.cpu.stats(), Proxy: &ProxyStats{Connections: st.Accepted, Open: st.Active}}
	case OpMode:
		var m faultproxy.Mode
		switch r.Name {
		case ProxyDelay:
			m = p.delay
		case ProxyPass:
			m = faultproxy.Pass{}
		default:
			return failed(fmt.Errorf("lab: fault proxy mode %q is not %s or %s", r.Name, ProxyDelay, ProxyPass))
		}
		if err := p.p.SetMode(m); err != nil { //nolint:contextcheck // a proxy leaving Refuse listens again for its own lifetime, not the request's
			return failed(err)
		}
		return Response{OK: true}
	default:
		return unknown(r)
	}
}

// Close stops the proxy.
func (p *Proxy) Close() error { return p.p.Close() }

// Echo is the TCP echo service the round-trip host check measures.
type Echo struct {
	ln    net.Listener
	wg    sync.WaitGroup
	conns sync.Map
	n     atomic.Int64
	cpu   *cpuMeter
}

// StartEcho listens on addr and echoes every byte back.
func StartEcho(ctx context.Context, addr string) (*Echo, error) {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("lab: echo: %w", err)
	}
	e := &Echo{ln: ln, cpu: newCPUMeter()}
	e.wg.Go(e.accept)
	return e, nil
}

func (e *Echo) accept() {
	for {
		c, err := e.ln.Accept()
		if err != nil {
			return
		}
		e.n.Add(1)
		e.conns.Store(c, struct{}{})
		e.wg.Go(func() {
			defer e.conns.Delete(c)
			defer func() { _ = c.Close() }()
			_, _ = io.Copy(c, c)
		})
	}
}

// Ready describes the echo service.
func (e *Echo) Ready() Ready { return ready("echo", e.ln.Addr().String()) }

// Handle answers stats and reset.
func (e *Echo) Handle(_ context.Context, r Request) Response {
	switch r.Op {
	case OpReset:
		e.cpu.reset()
		return Response{OK: true}
	case OpStats:
		return Response{OK: true, Process: e.cpu.stats(), Proxy: &ProxyStats{Connections: e.n.Load()}}
	default:
		return unknown(r)
	}
}

// Close stops the echo service and its connections.
func (e *Echo) Close() error {
	err := e.ln.Close()
	e.conns.Range(func(k, _ any) bool {
		_ = k.(net.Conn).Close()
		return true
	})
	e.wg.Wait()
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

// RoundTrip measures n round trips of one byte to an echo service at
// addr, one after another on one connection, and returns their histogram.
func RoundTrip(ctx context.Context, addr string, n int) (*loadgen.Histogram, error) {
	c, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("lab: round trip: %w", err)
	}
	defer func() { _ = c.Close() }()
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	h := newHistogram()
	b := []byte{0}
	for range n {
		start := time.Now()
		if _, err := c.Write(b); err != nil {
			return nil, fmt.Errorf("lab: round trip: %w", err)
		}
		if _, err := io.ReadFull(c, b); err != nil {
			return nil, fmt.Errorf("lab: round trip: %w", err)
		}
		h.Record(time.Since(start))
	}
	return h, nil
}

func ready(service, addr string) Ready {
	return Ready{Service: service, Addr: addr, PID: os.Getpid(), CPUs: newCPUMeter().stats().CPUs}
}

func unknown(r Request) Response {
	return Response{Error: fmt.Sprintf("lab: unknown operation %q", r.Op)}
}

func failed(err error) Response { return Response{Error: err.Error()} }

// FreePort returns a TCP port free on listen's host a moment ago, listen
// being "host:0": the port a server the harness starts on a lab host
// binds, which binds it again on a retry when another process took it
// first.
func FreePort(ctx context.Context, listen string) (int, error) {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", listen)
	if err != nil {
		return 0, fmt.Errorf("lab: port: %w", err)
	}
	defer func() { _ = ln.Close() }()
	a, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("lab: port: address %v", ln.Addr())
	}
	return a.Port, nil
}
