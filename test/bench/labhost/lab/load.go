// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package lab

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/loadgen"
)

// Key distributions of a KeySpec.
const (
	KeysCycle  = "cycle"
	KeysZipf   = "zipf"
	KeysRotate = "rotate"
	KeysSingle = "single"
)

// KeySpec is a scenario's key source (PBB "Scenarios").
type KeySpec struct {
	// Kind is cycle, zipf, rotate or single.
	Kind string `json:"kind"`
	// Prefix starts every key.
	Prefix string `json:"prefix,omitempty"`
	// N is the key cardinality (cycle, zipf).
	N int `json:"n,omitempty"`
	// Exponent is the Zipf exponent (zipf).
	Exponent float64 `json:"exponent,omitempty"`
	// Seed seeds the Zipf draw.
	Seed uint64 `json:"seed,omitempty"`
	// PerKey is the requests per key (rotate).
	PerKey uint64 `json:"perKey,omitempty"`
}

// source returns the loadgen key source.
func (k KeySpec) source() (loadgen.KeySource, error) {
	switch k.Kind {
	case KeysCycle:
		return loadgen.Cycle(k.Prefix, k.N)
	case KeysZipf:
		return loadgen.Zipf(k.Prefix, k.N, k.Exponent, k.Seed)
	case KeysRotate:
		return loadgen.Rotate(k.Prefix, k.PerKey)
	case KeysSingle:
		return loadgen.Single(k.Prefix), nil
	default:
		return nil, fmt.Errorf("lab: unknown key distribution %q", k.Kind)
	}
}

// TokenSpec mints the bearer tokens a scenario rotates: Count tokens
// signed in turn by the key file's keys (S2: 10,000 from two keys).
type TokenSpec struct {
	Count int `json:"count"`
	// Keys is the key file, shared with the IdP.
	Keys     KeyFile   `json:"keys"`
	Issuer   string    `json:"issuer"`
	Audience string    `json:"audience"`
	Expires  time.Time `json:"expires"`
}

// mint returns the tokens.
func (t TokenSpec) mint() ([]string, error) {
	keys, err := t.Keys.Signers()
	if err != nil {
		return nil, err
	}
	if len(keys) == 0 || t.Count < 1 {
		return nil, errors.New("lab: tokens need a key and a positive count")
	}
	out := make([]string, t.Count)
	for i := range out {
		k := keys[i%len(keys)]
		tok, err := k.Sign(map[string]any{"alg": string(k.Alg), "typ": "JWT", "kid": k.ID}, map[string]any{
			"iss": t.Issuer, "aud": t.Audience, "sub": fmt.Sprintf("bench-%d", i), "exp": t.Expires.Unix(),
		})
		if err != nil {
			return nil, err
		}
		out[i] = tok
	}
	return out, nil
}

// LoadSpec is one open-loop load generator run.
type LoadSpec struct {
	// Tool is loadgen (the default), oha or vegeta; ToolPath is the
	// external tool's executable on the load host.
	Tool     string `json:"tool,omitempty"`
	ToolPath string `json:"toolPath,omitempty"`
	// URL is the request URL; Host replaces the Host field when set.
	URL    string              `json:"url"`
	Method string              `json:"method,omitempty"`
	Host   string              `json:"host,omitempty"`
	Header map[string][]string `json:"header,omitempty"`
	// Protocol is "http1", "h2c" or "h2".
	Protocol string `json:"protocol"`
	// CA is the PEM CA bundle https URLs are verified with.
	CA string `json:"ca,omitempty"`
	// Connections and Streams set the connection shape (S1: 256 HTTP/1.1
	// connections; S3: 64 connections of up to 64 streams).
	Connections int `json:"connections"`
	Streams     int `json:"streams,omitempty"`
	// Rate is the offered rate; Warmup is offered first and discarded;
	// Duration is the measured phase.
	Rate     float64       `json:"rate"`
	Warmup   time.Duration `json:"warmupNs"`
	Duration time.Duration `json:"durationNs"`
	// Settle extends the warm-up by PBB's rule (loadgen.BenchmarkSettle).
	Settle bool `json:"settle,omitempty"`
	// KeyHeader and Keys set a per-request key (S2's x-bench-key).
	KeyHeader string   `json:"keyHeader,omitempty"`
	Keys      *KeySpec `json:"keys,omitempty"`
	// Tokens rotate as bearer credentials.
	Tokens *TokenSpec `json:"tokens,omitempty"`
	// Count names the RZ codes counted as achieved (RZ-RT-005 in O1).
	Count []string `json:"count,omitempty"`
	// Timeout bounds each request; 0 is loadgen's 30 s.
	Timeout time.Duration `json:"timeoutNs,omitempty"`
}

// LoadResult is a run's outcome.
type LoadResult struct {
	Offered      int64   `json:"offered"`
	Achieved     int64   `json:"achieved"`
	AchievedRate float64 `json:"achievedRate"`
	RateError    float64 `json:"rateError"`
	ErrorRate    float64 `json:"errorRate"`
	// Settled reports a warm-up that settled.
	Settled bool `json:"settled"`
	// Valid reports loadgen's validity rule; Invalid says why not.
	Valid   bool   `json:"valid"`
	Invalid string `json:"invalid,omitempty"`
	// MeasureStart and MeasureEnd bound the measured phase by intended
	// send time.
	MeasureStart time.Time        `json:"measureStart"`
	MeasureEnd   time.Time        `json:"measureEnd"`
	Outcomes     map[string]int64 `json:"outcomes"`
	Statuses     map[int]int64    `json:"statuses"`
	Codes        map[string]int64 `json:"codes"`
	NewConns     int64            `json:"newConns"`
	// Histogram is the measured phase's latency from intended send time;
	// Warmup the warm-up's.
	Histogram string `json:"histogram"`
	Warmup    string `json:"warmup"`
	// IntervalLog is the HdrHistogram interval log of the run's 1 s
	// windows, base64.
	IntervalLog string `json:"intervalLog"`
	// Failures are the first failures, one line each.
	Failures []string `json:"failures,omitempty"`
	// KeyCardinality and KeyDistribution describe the keys.
	KeyCardinality  int    `json:"keyCardinality"`
	KeyDistribution string `json:"keyDistribution"`
	// Process is the generator's CPU over the run.
	Process ProcessStats `json:"process"`
}

// maxReportedFailures bounds LoadResult.Failures.
const maxReportedFailures = 20

// RunLoad runs s and returns its result.
func RunLoad(ctx context.Context, s LoadSpec) (*LoadResult, error) {
	switch s.Tool {
	case "", ToolLoadgen:
	case ToolOha, ToolVegeta:
		if s.ToolPath == "" {
			return nil, fmt.Errorf("lab: %s needs its executable", s.Tool)
		}
		return RunTool(ctx, s)
	default:
		return nil, fmt.Errorf("lab: unknown tool %q", s.Tool)
	}
	cfg := loadgen.HTTPConfig{
		URL: s.URL, Method: s.Method, Host: s.Host, Header: http.Header(s.Header),
		Connections: s.Connections, StreamsPerConn: s.Streams, KeyHeader: s.KeyHeader,
	}
	switch s.Protocol {
	case "http1", "":
		cfg.Protocol = loadgen.HTTP1
	case "h2c":
		cfg.Protocol = loadgen.H2C
	case "h2":
		cfg.Protocol = loadgen.H2
	default:
		return nil, fmt.Errorf("lab: unknown protocol %q", s.Protocol)
	}
	if s.CA != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(s.CA)) {
			return nil, errors.New("lab: the CA bundle holds no certificate")
		}
		cfg.TLS = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	res := &LoadResult{KeyDistribution: "none"}
	if s.Keys != nil {
		src, err := s.Keys.source()
		if err != nil {
			return nil, err
		}
		cfg.Keys = src
		res.KeyCardinality, res.KeyDistribution = src.Cardinality(), src.Distribution()
	}
	if s.Tokens != nil {
		toks, err := s.Tokens.mint()
		if err != nil {
			return nil, err
		}
		cfg.Tokens = toks
	}
	target, err := loadgen.NewHTTP(cfg)
	if err != nil {
		return nil, err
	}
	defer func() { _ = target.Close() }()
	lc := loadgen.Config{Rate: s.Rate, Warmup: s.Warmup, Duration: s.Duration, Timeout: s.Timeout}
	if s.Settle {
		lc.Settle = loadgen.BenchmarkSettle()
	}
	cpu := newCPUMeter()
	r, err := loadgen.Run(ctx, target, lc)
	res.Process = cpu.stats()
	if err != nil {
		return nil, err
	}
	fillResult(res, r, s.Count)
	return res, nil
}

// fillResult copies a loadgen result into res.
func fillResult(res *LoadResult, r *loadgen.Result, count []string) {
	m := r.Measured
	res.Offered, res.Achieved = m.Offered, m.Achieved(count...)
	res.AchievedRate, res.RateError, res.ErrorRate = r.AchievedRate(count...), r.RateError(count...), m.ErrorRate(count...)
	res.Settled, res.MeasureStart, res.MeasureEnd = r.Settled, r.MeasureStart, r.MeasureEnd
	if err := r.Valid(loadgen.Validity{Count: count}); err != nil {
		res.Invalid = err.Error()
	} else {
		res.Valid = true
	}
	res.Outcomes = map[string]int64{}
	for o := range loadgen.NumOutcomes {
		if n := m.Count(o); n > 0 {
			res.Outcomes[o.String()] = n
		}
	}
	res.Statuses, res.Codes, res.NewConns = m.Statuses, m.Codes, m.NewConns
	res.Histogram, res.Warmup = EncodeHistogram(m.Latency), EncodeHistogram(r.Warmup.Latency)
	var log bytes.Buffer
	if lw, err := loadgen.NewLogWriter(&log, r.Start, "ruralz bench interval log"); err == nil {
		for _, w := range r.Windows {
			_ = lw.Write("", w.Start, w.Start.Add(r.Window), w.Latency)
		}
	}
	res.IntervalLog = base64.StdEncoding.EncodeToString(log.Bytes())
	for _, f := range r.Failures[:min(len(r.Failures), maxReportedFailures)] {
		res.Failures = append(res.Failures, fmt.Sprintf("%v %s %d %s %v", f.Intended.UTC().Format(time.RFC3339Nano), f.Outcome, f.Status, f.Code, f.Err))
	}
	slices.Sort(res.Failures)
}
