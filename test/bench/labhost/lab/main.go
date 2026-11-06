// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package lab

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Exit statuses of Main.
const (
	ExitOK    = 0
	ExitError = 1
	ExitUsage = 2
)

// usage is Main's usage text.
const usage = `usage: labhost <command> [flags]

commands:
  mock        the mock Upstream (-listen, -cert, -key, -body-size, -delay, -header, -name)
  sink        the OTLP sink folding gateway_duration (-listen, -cert, -key)
  idp         the mock IdP (-listen, -cert, -key, -issuer, -keys)
  faultproxy  the fault proxy (-listen, -upstream, -mean, -stddev, -pass-first)
  conns       held client connections (-kind, -target, -count, -rate, -ca, -server-name, -host, -path, -header-bytes, -timeout)
  echo        the TCP echo of the round-trip check (-listen)
  run         a supervised child that stops with the session and takes signals on request
              (-ready ADDR, -ready-line REGEXP, -container NAME -- COMMAND ARGS...)
  load        the chaos runs' load: open-loop loads started on request, every request's record
              kept until no load runs, then returned on request
  loadgen     one load generator run: a LoadSpec line on stdin, a LoadResult line on stdout
  port        a TCP port free on the -listen address's host a moment ago, as JSON
  facts       the host's fingerprint and frequency and throttle state as JSON (-root)
  roundtrip   round trips to an echo service (-addr, -n): a histogram on stdout

Services write a ready line, answer request lines on stdin, and stop at the
end of stdin or on SIGTERM.
`

// Main runs one labhost command and returns its exit status.
func Main(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = io.WriteString(stderr, usage)
		return ExitUsage
	}
	cmd, rest := args[0], args[1:]
	fs := flag.NewFlagSet("labhost "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", "127.0.0.1:0", "listen address")
	cert := fs.String("cert", "", "TLS certificate file (PEM)")
	key := fs.String("key", "", "TLS key file (PEM)")
	var svc func() (Service, error)
	switch cmd {
	case "mock":
		body := fs.Int64("body-size", 1024, "response body size in bytes")
		delay := fs.Duration("delay", 0, "delay before every response")
		header := http.Header{}
		fs.Func("header", "a response header \"Name: value\" added to every response (repeatable)", func(v string) error {
			name, value, ok := strings.Cut(v, ":")
			if !ok || strings.TrimSpace(name) == "" {
				return fmt.Errorf("header %q is not \"Name: value\"", v)
			}
			header.Add(strings.TrimSpace(name), strings.TrimSpace(value))
			return nil
		})
		name := fs.String("name", "", "the name every response carries in the mock's server header")
		svc = func() (Service, error) {
			return StartMock(ctx, MockConfig{Listen: *listen, CertFile: *cert, KeyFile: *key, BodySize: *body, Delay: *delay, Header: header, Name: *name})
		}
	case "sink":
		svc = func() (Service, error) {
			return StartSink(ctx, SinkConfig{Listen: *listen, CertFile: *cert, KeyFile: *key})
		}
	case "idp":
		issuer := fs.String("issuer", "", "token issuer")
		keys := fs.String("keys", "", "key file to publish")
		svc = func() (Service, error) {
			return StartIDP(ctx, IDPConfig{Listen: *listen, CertFile: *cert, KeyFile: *key, Issuer: *issuer, Keys: *keys})
		}
	case "faultproxy":
		upstream := fs.String("upstream", "", "upstream address")
		mean := fs.Duration("mean", 0, "mean delay each way")
		stddev := fs.Duration("stddev", 0, "delay standard deviation")
		passFirst := fs.Bool("pass-first", false, "forward unchanged until a mode request switches to the delay")
		svc = func() (Service, error) {
			return StartProxy(ctx, ProxyConfig{Listen: *listen, Upstream: *upstream, Mean: *mean, StdDev: *stddev, PassFirst: *passFirst})
		}
	case "echo":
		svc = func() (Service, error) { return StartEcho(ctx, *listen) }
	case "conns":
		var c ConnsConfig
		fs.StringVar(&c.Kind, "kind", "", "connection kind: idle-http1, idle-h2, stall-h2c or slow-header")
		fs.StringVar(&c.Target, "target", "", "the Node listener (host:port)")
		fs.IntVar(&c.Count, "count", 0, "connections to open")
		fs.Float64Var(&c.Rate, "rate", 0, "connection attempts per second (0: all at once)")
		fs.StringVar(&c.CAFile, "ca", "", "CA file verifying the TLS listener")
		fs.StringVar(&c.ServerName, "server-name", "", "TLS server name")
		fs.StringVar(&c.Host, "host", "", "request host")
		fs.StringVar(&c.Path, "path", "", "request path")
		fs.IntVar(&c.HeaderBytes, "header-bytes", 0, "slow-header field size")
		fs.DurationVar(&c.Timeout, "timeout", 0, "per-connection setup bound")
		svc = func() (Service, error) { return StartConns(ctx, c) }
	case "run":
		readyAddr := fs.String("ready", "", "TCP address the child serves on")
		readyLine := fs.String("ready-line", "", "regular expression a line of the child's output matches once it serves")
		container := fs.String("container", "", "the container the child runs (docker run --name), which signals reach through docker kill")
		if err := fs.Parse(rest); err != nil {
			return ExitUsage
		}
		if fs.NArg() == 0 {
			_, _ = io.WriteString(stderr, "labhost: run: no command\n")
			return ExitUsage
		}
		argv := fs.Args()
		s, err := StartChild(ctx, ChildConfig{Argv: argv, ReadyAddr: *readyAddr, ReadyLine: *readyLine, Container: *container, Stderr: stderr})
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "labhost:", err)
			return ExitError
		}
		if err := Serve(ctx, s, stdin, stdout); err != nil {
			_, _ = fmt.Fprintln(stderr, "labhost:", err)
			return ExitError
		}
		return ExitOK
	case "load":
		svc = func() (Service, error) { return NewLoadService(), nil }
	case "port":
		if err := fs.Parse(rest); err != nil {
			return ExitUsage
		}
		port, err := FreePort(ctx, *listen)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "labhost:", err)
			return ExitError
		}
		return writeJSON(stdout, stderr, map[string]int{"port": port})
	case "loadgen":
		if err := fs.Parse(rest); err != nil {
			return ExitUsage
		}
		return runLoadCommand(ctx, stdin, stdout, stderr)
	case "facts":
		root := fs.String("root", "/", "file system root of /proc and /sys")
		if err := fs.Parse(rest); err != nil {
			return ExitUsage
		}
		return writeJSON(stdout, stderr, ReadHostFacts(*root))
	case "roundtrip":
		addr := fs.String("addr", "", "echo service address")
		n := fs.Int("n", 10000, "round trips")
		if err := fs.Parse(rest); err != nil {
			return ExitUsage
		}
		rctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		h, err := RoundTrip(rctx, *addr, *n)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "labhost:", err)
			return ExitError
		}
		return writeJSON(stdout, stderr, map[string]string{"histogram": EncodeHistogram(h)})
	default:
		_, _ = fmt.Fprintf(stderr, "labhost: unknown command %q\n%s", cmd, usage)
		return ExitUsage
	}
	if err := fs.Parse(rest); err != nil {
		return ExitUsage
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "labhost: unexpected arguments %q\n", fs.Args())
		return ExitUsage
	}
	s, err := svc()
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "labhost:", err)
		return ExitError
	}
	if err := Serve(ctx, s, stdin, stdout); err != nil {
		_, _ = fmt.Fprintln(stderr, "labhost:", err)
		return ExitError
	}
	return ExitOK
}

// runLoadCommand reads one LoadSpec line, runs it and writes the
// LoadResult line.
func runLoadCommand(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) int {
	sc := bufio.NewScanner(stdin)
	sc.Buffer(make([]byte, 0, 64<<10), MaxLine)
	if !sc.Scan() {
		_, _ = fmt.Fprintln(stderr, "labhost: loadgen: no spec on stdin")
		return ExitUsage
	}
	var spec LoadSpec
	if err := json.Unmarshal(sc.Bytes(), &spec); err != nil {
		_, _ = fmt.Fprintln(stderr, "labhost: loadgen: spec:", err)
		return ExitUsage
	}
	res, err := RunLoad(ctx, spec)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "labhost: loadgen:", err)
		return ExitError
	}
	return writeJSON(stdout, stderr, res)
}

func writeJSON(stdout, stderr io.Writer, v any) int {
	if err := json.NewEncoder(stdout).Encode(v); err != nil {
		_, _ = fmt.Fprintln(stderr, "labhost:", err)
		return ExitError
	}
	return ExitOK
}
