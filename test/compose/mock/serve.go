// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package mock

import (
	"bytes"
	"context"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/mockidp"
	"github.com/ravindu-rev/ruralz/internal/testkit/mockup"
)

// Control API timeouts.
const (
	controlReadHeaderTimeout = 10 * time.Second
	controlWriteTimeout      = 30 * time.Second
	// maxFileBytes bounds every file serve reads: a Routes file, a
	// certificate, a key or a client secret.
	maxFileBytes = 1 << 20
)

// serveConfig is the parsed flags of "mock serve".
type serveConfig struct {
	upstream       string
	upstreamName   string
	upstreamRoutes string
	upstreamBody   string

	idp             string
	idpCert, idpKey string
	idpIssuer       string
	idpSigningKey   string
	idpAlg          string
	idpKID          string
	idpClient       string
	idpClientSecret string // a file
	idpAudience     string

	dns     string
	control string
}

// serveFlags returns the flag set of "mock serve" filling c.
func serveFlags(c *serveConfig, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("mock serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&c.upstream, "upstream", "", "mock Upstream listen address, such as 127.0.0.1:9000 (empty: no mock Upstream)")
	fs.StringVar(&c.upstreamName, "upstream-name", "", "name the mock Upstream sets in X-Mockup-Server")
	fs.StringVar(&c.upstreamRoutes, "upstream-routes", "", "JSON Routes file programming the mock Upstream's answers")
	fs.StringVar(&c.upstreamBody, "upstream-body", "", "body of the default 200 answer when no Routes file is given")
	fs.StringVar(&c.idp, "idp", "", "mock IdP listen address, served over TLS (empty: no mock IdP)")
	fs.StringVar(&c.idpCert, "idp-cert", "", "PEM certificate chain the mock IdP serves")
	fs.StringVar(&c.idpKey, "idp-key", "", "PEM private key of -idp-cert")
	fs.StringVar(&c.idpIssuer, "idp-issuer", "", "iss of the tokens the mock IdP issues (default its URL)")
	fs.StringVar(&c.idpSigningKey, "idp-signing-key", "", "PKCS #8 PEM private key the mock IdP signs with and publishes in its JWKS (default a generated RS256 and ES256 pair)")
	fs.StringVar(&c.idpAlg, "idp-alg", string(mockidp.ES256), "JWS algorithm of -idp-signing-key: RS256, PS256, ES256 or EdDSA")
	fs.StringVar(&c.idpKID, "idp-kid", "", "kid of -idp-signing-key (default its RFC 7638 thumbprint)")
	fs.StringVar(&c.idpClient, "idp-client", "", "the one client ID the token endpoint accepts (empty: any client)")
	fs.StringVar(&c.idpClientSecret, "idp-client-secret-file", "", "file holding -idp-client's secret, read byte for byte")
	fs.StringVar(&c.idpAudience, "idp-token-audience", "", "aud of the access tokens the token endpoint issues")
	fs.StringVar(&c.dns, "dns", "", "DNS sink listen address for UDP and TCP, such as 0.0.0.0:53 (empty: no DNS sink)")
	fs.StringVar(&c.control, "control", "", "control API listen address, such as 0.0.0.0:9100 (empty: no control API)")
	return fs
}

// serve runs "mock serve" until SIGTERM, SIGINT or the end of ctx.
func serve(ctx context.Context, args []string, stderr io.Writer) int {
	var c serveConfig
	fs := serveFlags(&c, stderr)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "mock serve: unexpected argument %q\n", fs.Arg(0))
		return ExitUsage
	}
	if err := c.validate(); err != nil {
		_, _ = fmt.Fprintf(stderr, "mock serve: %v\n", err)
		return ExitUsage
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	p, err := startParts(ctx, c)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "mock serve: %v\n", err)
		return ExitFailure
	}
	_, _ = fmt.Fprintf(stderr, "mock serve: serving %s\n", p.describe())
	<-ctx.Done()
	if err := p.Close(); err != nil {
		_, _ = fmt.Fprintf(stderr, "mock serve: %v\n", err)
	}
	return ExitOK
}

// validate checks the flag combinations.
func (c *serveConfig) validate() error {
	switch {
	case c.upstream == "" && c.idp == "" && c.dns == "":
		return errors.New("give at least one of -upstream, -idp and -dns")
	case c.upstream == "" && (c.upstreamRoutes != "" || c.upstreamBody != "" || c.upstreamName != ""):
		return errors.New("-upstream-routes, -upstream-body and -upstream-name need -upstream")
	case c.upstreamRoutes != "" && c.upstreamBody != "":
		return errors.New("give -upstream-routes or -upstream-body, not both")
	case c.idp != "" && (c.idpCert == "" || c.idpKey == ""):
		return errors.New("-idp needs -idp-cert and -idp-key")
	case c.idp == "" && (c.idpCert != "" || c.idpKey != "" || c.idpSigningKey != "" || c.idpClient != "" ||
		c.idpClientSecret != "" || c.idpIssuer != "" || c.idpAudience != "" || c.idpKID != ""):
		return errors.New("the -idp-* flags need -idp")
	case (c.idpClient == "") != (c.idpClientSecret == ""):
		return errors.New("-idp-client and -idp-client-secret-file go together")
	}
	return nil
}

// parts are the running parts of "mock serve".
type parts struct {
	upstream *mockup.Server
	idp      *mockidp.Server
	dns      *dnsSink
	control  *http.Server
	ctlAddr  string
	ctlDone  chan struct{}
}

// startParts starts every part c names; on error it closes what it
// started.
func startParts(ctx context.Context, c serveConfig) (_ *parts, err error) {
	p := &parts{}
	defer func() {
		if err != nil {
			_ = p.Close()
		}
	}()
	if c.upstream != "" {
		if p.upstream, err = startUpstream(ctx, c); err != nil {
			return nil, err
		}
	}
	if c.idp != "" {
		if p.idp, err = startIDP(ctx, c); err != nil {
			return nil, err
		}
	}
	if c.dns != "" {
		if p.dns, err = startDNSSink(ctx, c.dns); err != nil {
			return nil, err
		}
	}
	if c.control != "" {
		if err = p.startControl(ctx, c.control); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// describe names the listening parts and their addresses.
func (p *parts) describe() string {
	var out []string
	if p.upstream != nil {
		out = append(out, "upstream "+p.upstream.Addr())
	}
	if p.idp != nil {
		out = append(out, "idp "+p.idp.Addr())
	}
	if p.dns != nil {
		out = append(out, "dns "+p.dns.Addr())
	}
	if p.control != nil {
		out = append(out, "control "+p.ctlAddr)
	}
	return strings.Join(out, ", ")
}

// Close stops every running part and waits for it.
func (p *parts) Close() error {
	var errs []error
	if p.control != nil {
		errs = append(errs, p.control.Close())
		<-p.ctlDone
	}
	if p.dns != nil {
		errs = append(errs, p.dns.Close())
	}
	if p.idp != nil {
		errs = append(errs, p.idp.Close())
	}
	if p.upstream != nil {
		errs = append(errs, p.upstream.Close())
	}
	return errors.Join(errs...)
}

// startUpstream starts the mock Upstream programmed by c.
func startUpstream(ctx context.Context, c serveConfig) (*mockup.Server, error) {
	routes := Routes{Default: Answer{Body: c.upstreamBody}}
	if c.upstreamRoutes != "" {
		data, err := readFile(c.upstreamRoutes)
		if err != nil {
			return nil, err
		}
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.DisallowUnknownFields()
		routes = Routes{}
		if err := dec.Decode(&routes); err != nil {
			return nil, fmt.Errorf("%s: %w", c.upstreamRoutes, err)
		}
	}
	s, err := mockup.Start(ctx, mockup.Config{Name: c.upstreamName, Listen: c.upstream, Behavior: routes.Default.behavior()})
	if err != nil {
		return nil, err
	}
	for path, a := range routes.Paths {
		if err := s.SetRoute(path, a.behavior()); err != nil {
			_ = s.Close()
			return nil, err
		}
	}
	return s, nil
}

// behavior returns the mockup Behavior of a.
func (a Answer) behavior() mockup.Behavior {
	b := mockup.Behavior{Status: a.Status, Body: []byte(a.Body), ContentType: a.ContentType}
	if b.ContentType == "" {
		b.ContentType = "text/plain; charset=utf-8"
	}
	if len(a.Header) > 0 {
		b.Header = http.Header{}
		for k, v := range a.Header {
			b.Header.Set(k, v)
		}
	}
	return b
}

// startIDP starts the mock IdP over TLS as c sets it up.
func startIDP(ctx context.Context, c serveConfig) (*mockidp.Server, error) {
	certPEM, err := readFile(c.idpCert)
	if err != nil {
		return nil, err
	}
	keyPEM, err := readFile(c.idpKey)
	if err != nil {
		return nil, err
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("-idp-cert and -idp-key: %w", err)
	}
	cfg := mockidp.Config{
		Listen:        c.idp,
		TLS:           &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		Issuer:        c.idpIssuer,
		TokenAudience: c.idpAudience,
	}
	if c.idpSigningKey != "" {
		k, err := signingKey(c.idpSigningKey, mockidp.Alg(c.idpAlg), c.idpKID)
		if err != nil {
			return nil, err
		}
		cfg.Keys = []*mockidp.Key{k}
	}
	if c.idpClient != "" {
		secret, err := readFile(c.idpClientSecret)
		if err != nil {
			return nil, err
		}
		cfg.Clients = map[string]string{c.idpClient: string(secret)}
	}
	return mockidp.Start(ctx, cfg)
}

// signingKey reads the PKCS #8 PEM private key at path as a mockidp key.
func signingKey(path string, alg mockidp.Alg, kid string) (*mockidp.Key, error) {
	data, err := readFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("%s: no PKCS #8 PRIVATE KEY block", path)
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("%s: %T is not a signing key", path, key)
	}
	return mockidp.NewKey(alg, kid, signer)
}

// readFile reads a file a flag names, at most maxFileBytes.
func readFile(path string) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // G304: a file the mock's own command line names
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(data) > maxFileBytes {
		return nil, fmt.Errorf("%s: larger than %d bytes", path, maxFileBytes)
	}
	return data, nil
}

// startControl serves the control API on addr.
func (p *parts) startControl(ctx context.Context, addr string) error {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("control: %w", err)
	}
	p.ctlAddr = ln.Addr().String()
	p.control = &http.Server{
		Handler:           p.controlHandler(),
		ReadHeaderTimeout: controlReadHeaderTimeout,
		WriteTimeout:      controlWriteTimeout,
		BaseContext:       func(net.Listener) context.Context { return context.WithoutCancel(ctx) },
	}
	p.ctlDone = make(chan struct{})
	go func() {
		defer close(p.ctlDone)
		_ = p.control.Serve(ln)
	}()
	return nil
}

// controlHandler routes the control API.
func (p *parts) controlHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /requests", func(w http.ResponseWriter, _ *http.Request) {
		if p.upstream == nil {
			http.Error(w, "no mock Upstream", http.StatusNotFound)
			return
		}
		writeJSON(w, requestLog(p.upstream))
	})
	mux.HandleFunc("GET /dns", func(w http.ResponseWriter, _ *http.Request) {
		if p.dns == nil {
			http.Error(w, "no DNS sink", http.StatusNotFound)
			return
		}
		writeJSON(w, p.dns.Log())
	})
	mux.HandleFunc("GET /idp", func(w http.ResponseWriter, _ *http.Request) {
		if p.idp == nil {
			http.Error(w, "no mock IdP", http.StatusNotFound)
			return
		}
		writeJSON(w, idpLog(p.idp))
	})
	return mux
}

// requestLog converts the mock Upstream's log.
func requestLog(s *mockup.Server) RequestLog {
	out := RequestLog{Requests: []Request{}, Evicted: s.Stats().LogEvicted}
	for _, r := range s.Requests() {
		out.Requests = append(out.Requests, Request{
			Seq: r.Seq, Method: r.Method, Host: r.Host, URI: r.URI, Path: r.Path, RawQuery: r.RawQuery,
			Proto: r.Proto, Header: r.Header, RemoteAddr: r.RemoteAddr, BodyBytes: r.BodyBytes,
		})
	}
	return out
}

// idpLog converts the mock IdP's record, leaving the client secrets out.
func idpLog(s *mockidp.Server) IDPLog {
	out := IDPLog{JWKSFetches: s.Stats().JWKSFetches, TokenRequests: []TokenRequest{}}
	for _, r := range s.TokenRequests() {
		out.TokenRequests = append(out.TokenRequests, TokenRequest{
			ClientID: r.ClientID, AuthMethod: r.AuthMethod, GrantType: r.GrantType, Scope: r.Scope,
			Status: r.Status, AccessToken: r.AccessToken,
		})
	}
	return out
}

// writeJSON writes v as the JSON body of a 200 response.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(v)
}
