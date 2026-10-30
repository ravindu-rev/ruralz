// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package mock

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// Probe defaults and bounds.
const (
	// DefaultProbeTimeout bounds the wait for -ready and the -get request
	// together.
	DefaultProbeTimeout = 60 * time.Second
	// DefaultProbeInterval spaces the attempts of a poll; it only paces
	// the polls, never decides an outcome.
	DefaultProbeInterval = 250 * time.Millisecond
	// probeAttemptTimeout bounds one HTTP attempt.
	probeAttemptTimeout = 5 * time.Second
	// probeReadTimeout bounds the sink check and the DNS log read, each
	// on its own budget so a slow readiness wait cannot starve them.
	probeReadTimeout = 10 * time.Second
	// maxProbeBody bounds the body a ProbeResult keeps.
	maxProbeBody = 64 << 10
)

// probeConfig is the parsed flags of "mock probe".
type probeConfig struct {
	ca        string
	ready     string
	get       string
	host      string
	sinkCheck string
	sinkName  string
	dnsLog    string
	timeout   time.Duration
	interval  time.Duration
}

// probe runs "mock probe" and prints its ProbeReport on stdout.
func probe(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	var c probeConfig
	fs := flag.NewFlagSet("mock probe", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&c.ca, "ca", "", "PEM trust anchors for https URLs (default the system roots)")
	fs.StringVar(&c.ready, "ready", "", "URL polled until it answers 200, such as https://upstream:9901/readyz")
	fs.StringVar(&c.get, "get", "", "URL requested, after -ready answered 200, until it answers 200")
	fs.StringVar(&c.host, "host", "", "Host header of the -get request")
	fs.StringVar(&c.sinkCheck, "sink-check", "", "DNS server, host:port, to look -sink-check-name up through")
	fs.StringVar(&c.sinkName, "sink-check-name", "", "absolute name, ending in a dot, of the -sink-check lookup")
	fs.StringVar(&c.dnsLog, "dns-log", "", "control API URL of a DNS sink's record, read last, such as http://upstream:9100/dns")
	fs.DurationVar(&c.timeout, "timeout", DefaultProbeTimeout, "bound of the -ready wait and the -get request together")
	fs.DurationVar(&c.interval, "interval", DefaultProbeInterval, "pause between the attempts of a poll")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	if err := c.validate(fs); err != nil {
		_, _ = fmt.Fprintf(stderr, "mock probe: %v\n", err)
		return ExitUsage
	}
	client, err := probeClient(c.ca)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "mock probe: %v\n", err)
		return ExitFailure
	}
	defer client.CloseIdleConnections()
	rep := runProbe(ctx, c, client)
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rep); err != nil {
		_, _ = fmt.Fprintf(stderr, "mock probe: %v\n", err)
		return ExitFailure
	}
	return ExitOK
}

// validate checks the flag combinations.
func (c *probeConfig) validate(fs *flag.FlagSet) error {
	switch {
	case fs.NArg() > 0:
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	case c.ready == "" && c.get == "" && c.sinkCheck == "" && c.dnsLog == "":
		return errors.New("give at least one of -ready, -get, -sink-check and -dns-log")
	case c.host != "" && c.get == "":
		return errors.New("-host needs -get")
	case (c.sinkCheck == "") != (c.sinkName == ""):
		return errors.New("-sink-check and -sink-check-name go together")
	case c.sinkName != "" && c.sinkName[len(c.sinkName)-1] != '.':
		return errors.New("-sink-check-name must be absolute, ending in a dot")
	case c.timeout <= 0 || c.interval <= 0:
		return errors.New("-timeout and -interval must be positive")
	}
	return nil
}

// probeClient returns a client that trusts the PEM certificates of ca, or
// the system roots without one, and ignores proxy variables.
func probeClient(ca string) (*http.Client, error) {
	tc := &tls.Config{MinVersion: tls.VersionTLS12}
	if ca != "" {
		data, err := readFile(ca)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(data) {
			return nil, fmt.Errorf("%s: no PEM certificate", ca)
		}
		tc.RootCAs = pool
	}
	return &http.Client{
		Transport: &http.Transport{Proxy: nil, TLSClientConfig: tc, ForceAttemptHTTP2: true},
		Timeout:   probeAttemptTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}, nil
}

// runProbe runs each part c asks for, in order: the readiness wait, the
// request, the sink check, then the DNS log, so the log holds every
// lookup the other parts could have caused.
func runProbe(ctx context.Context, c probeConfig, client *http.Client) ProbeReport {
	var rep ProbeReport
	wait, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if c.ready != "" {
		rep.Ready = poll(wait, client, c.ready, "", c.interval)
	}
	if c.get != "" {
		if rep.Ready == nil || rep.Ready.Status == http.StatusOK {
			rep.Request = poll(wait, client, c.get, c.host, c.interval)
		} else {
			rep.Request = &ProbeResult{URL: c.get, Error: "not sent: -ready never answered 200"}
		}
	}
	if c.sinkCheck != "" {
		rep.SinkCheck = lookupThrough(ctx, c.sinkCheck, c.sinkName)
	}
	if c.dnsLog != "" {
		log, err := fetchDNSLog(ctx, client, c.dnsLog)
		if err != nil {
			rep.DNSError = err.Error()
		} else {
			rep.DNS = &log
		}
	}
	return rep
}

// poll requests url until it answers 200 or ctx ends. The result counts
// every attempt and holds the last one that ctx's end did not cut short,
// so a wait that runs out reports the last answer or error it saw.
func poll(ctx context.Context, client *http.Client, url, host string, interval time.Duration) *ProbeResult {
	res := &ProbeResult{URL: url}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			if res.Attempts == 0 {
				res.Error = ctx.Err().Error()
			}
			return res
		case <-timer.C:
		}
		var cur ProbeResult
		attempt(ctx, client, url, host, &cur)
		res.Attempts++
		if cur.Status != 0 || ctx.Err() == nil || res.Attempts == 1 {
			res.Status, res.Header, res.Body, res.Error = cur.Status, cur.Header, cur.Body, cur.Error
		}
		if res.Status == http.StatusOK {
			return res
		}
		timer.Reset(interval)
	}
}

// attempt sends one GET and records its answer or error in res.
func attempt(ctx context.Context, client *http.Client, url, host string, res *ProbeResult) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		res.Error = err.Error()
		return
	}
	if host != "" {
		req.Host = host
	}
	resp, err := client.Do(req)
	if err != nil {
		res.Error = err.Error()
		return
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxProbeBody))
	res.Status, res.Header, res.Body = resp.StatusCode, resp.Header, string(body)
	if err != nil {
		res.Error = "reading the body: " + err.Error()
		res.Status = 0
	}
}

// lookupThrough looks name up through the DNS server at server only.
func lookupThrough(ctx context.Context, server, name string) *SinkCheck {
	ctx, cancel := context.WithTimeout(ctx, probeReadTimeout)
	defer cancel()
	r := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, server)
		},
	}
	out := &SinkCheck{Name: name, Server: server}
	if _, err := r.LookupHost(ctx, name); err != nil {
		out.Error = err.Error()
		var de *net.DNSError
		out.NotFound = errors.As(err, &de) && de.IsNotFound
	}
	return out
}

// fetchDNSLog reads a DNS sink's record from the control API URL u.
func fetchDNSLog(ctx context.Context, client *http.Client, u string) (DNSLog, error) {
	ctx, cancel := context.WithTimeout(ctx, probeReadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return DNSLog{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return DNSLog{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return DNSLog{}, fmt.Errorf("GET %s: status %d", u, resp.StatusCode)
	}
	var log DNSLog
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxFileBytes)).Decode(&log); err != nil {
		return DNSLog{}, fmt.Errorf("GET %s: %w", u, err)
	}
	return log, nil
}
