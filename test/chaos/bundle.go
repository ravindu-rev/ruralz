// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package chaos

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing/fstest"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/loader"
	"github.com/ravindu-rev/ruralz/internal/config/pipeline"
	"github.com/ravindu-rev/ruralz/internal/config/render"
)

// bundleFS holds the chaos Bundle
// (docs/engineering/03-testing-and-quality-strategy.md "Chaos Bundle").
//
//go:embed testdata/bundle
var bundleFS embed.FS

// bundleRoot is the chaos Bundle's directory in bundleFS.
const bundleRoot = "testdata/bundle"

// Overlays of the chaos Bundle, each a structure a run may need.
const (
	// OverlayCacheConnection adds a stateStore.cache connection (CE-6).
	OverlayCacheConnection = "cache-connection"
	// OverlayCleartext drops the main connection's TLS block, for a State
	// Store behind the counting RESP proxy, which reads the commands.
	OverlayCleartext = "cleartext-state-store"
	// OverlayCacheCleartext adds a stateStore.cache connection without
	// TLS, for a cache State Store behind the counting RESP proxy (CE-16).
	OverlayCacheCleartext = "cache-connection-cleartext"
	// OverlayRouteTimeout gives /plain a timeout, the field V-6's N-1
	// variant does not know.
	OverlayRouteTimeout = "route-timeout"
	// OverlayJWT adds the auth.jwt Policy jwt-chaos, whose one issuer is
	// the run's mock IdP (BundleParams.JWTIssuer and JWKSURL), and the
	// Route /jwt it guards (FC-16).
	OverlayJWT = "jwt"
	// OverlayDiscovery adds the dns discovery Upstreams discovered-a, A
	// and AAAA records of DiscoveryAName on BundleParams.DiscoveryPort,
	// and discovered-srv, SRV records of DiscoverySRVName's named port
	// DiscoverySRVPort, with their Routes, and drops the main connection's
	// TLS block, for a State Store reached by a DNS name behind the
	// counting RESP proxy (FC-19).
	OverlayDiscovery = "dns-discovery"
	// OverlayEnvStateStore reads the main connection's URL from the
	// process variable EnvStateStoreURL, which changes only with a
	// restart, and drops its TLS block, so the URL can name each
	// replacement deployment directly (GD-5).
	OverlayEnvStateStore = "env-state-store"
)

// Overlays returns the chaos Bundle's overlays.
func Overlays() []string {
	return []string{
		OverlayCacheConnection, OverlayCleartext, OverlayCacheCleartext, OverlayRouteTimeout,
		OverlayJWT, OverlayDiscovery, OverlayEnvStateStore,
	}
}

// Names the overlays add: the Routes, the jwt-chaos audience, the DNS
// names of the discovery Upstreams and the State Store URL's variable.
const (
	PathJWT               = "/jwt"
	PathDiscoveredA       = "/discovered-a"
	PathDiscoveredSRV     = "/discovered-srv"
	JWTAudience           = "chaos"
	UpstreamDiscoveredA   = "discovered-a"
	UpstreamDiscoveredSRV = "discovered-srv"
	DiscoveryAName        = "a.discovery.chaos.test"
	DiscoverySRVName      = "srv.discovery.chaos.test"
	// DiscoverySRVPort is discovered-srv's named port: its records are
	// those of _http._tcp.srv.discovery.chaos.test.
	DiscoverySRVPort = "http"
	EnvStateStoreURL = "RURALZ_STATE_STORE_URL"
	// RefStateStoreEnv is OverlayEnvStateStore's reference to it, which a
	// run keeps as authored (topology.NodeOptions.AuthoredSecrets).
	RefStateStoreEnv = "env:" + EnvStateStoreURL
)

// Secret references of the chaos Bundle, as NodeOptions.Secrets and
// NodeOptions.StateStoreURLs key them.
const (
	RefTLSCertificate = "file:/etc/ruralz/tls/tls.crt"
	RefTLSKey         = "file:/etc/ruralz/tls/tls.key"
	RefStateStoreURL  = "file:/etc/ruralz/state/url"
	RefStateStoreCA   = "file:/etc/ruralz/state/ca.crt"
	RefCacheURL       = "file:/etc/ruralz/state/cache-url"
	RefCacheCA        = "file:/etc/ruralz/state/cache-ca.crt"
)

// Consumers returns the chaos Bundle's Consumers, each holding one API
// key under ConsumerKeyRef and the chaos-requests quota.
func Consumers() []string { return []string{"chaos-1", "chaos-2", "chaos-3", "chaos-4"} }

// ConsumerKeyRef returns the secret reference of a Consumer's API key.
func ConsumerKeyRef(consumer string) string { return "file:/etc/ruralz/consumers/" + consumer }

// Quota is the name of every Consumer's requests quota, and QuotaWindow
// its window.
const (
	Quota       = "chaos-requests"
	QuotaWindow = "24h0m0s"
)

// Routes of the chaos Bundle, by path.
const (
	PathPlain         = "/plain"
	PathLimited       = "/limited"
	PathLimitedClosed = "/limited-closed"
	PathRefunded      = "/refunded"
	PathUndeclared    = "/undeclared"
	PathCached        = "/cached"
	PathHot           = "/hot"
	PathHotLocal      = "/hot-local"
	PathCotenant      = "/cotenant"
	PathFlood         = "/flood"
)

// Policies with State Store keys, as their GCRA keys name them.
const (
	PolicyLimited       = "ratelimit-limited"
	PolicyLimitedClosed = "ratelimit-limited-closed"
	PolicyUndeclared    = "ratelimit-undeclared"
	PolicyHot           = "ratelimit-hot"
	PolicyHotLocal      = "ratelimit-hot-local"
	PolicyCotenant      = "ratelimit-cotenant"
	PolicyFlood         = "ratelimit-flood"
	PolicyQuota         = "quota-chaos"
	PolicyQuotaClosed   = "quota-chaos-closed"
)

// CodeBodyRejected is the code of /refunded's answers: its
// validation.json-schema Policy rejects every body after the quota
// charged the request, which onLog then refunds.
const CodeBodyRejected = "RZ-RT-009"

// Constant keys of the CE-12 Policies, and the header the per-request
// keys of /undeclared and /flood come from.
const (
	HotKey      = "hot"
	HotLocalKey = "hot-local"
	CotenantKey = "cotenant-2"
	KeyHeader   = "x-chaos-key"
	APIKeyField = "x-api-key"
)

// Limit is one ratelimit limits[] entry per 1 s window.
type Limit struct {
	// Requests is the Cell-wide limit per second.
	Requests int
	// Ceiling is the declared perNodeCeiling; 0 leaves it undeclared
	// where the Policy allows that.
	Ceiling int
}

// BundleParams are the values the harness renders the chaos Bundle with
// for one run: the profile's Node count and the run's loads decide the
// limits.
type BundleParams struct {
	// Backends are the addresses of the three mock Upstreams of
	// chaos-backend.
	Backends [3]string
	// Topology is the stateStore.topology of the run's State Store:
	// standalone, cluster or sentinel.
	Topology string
	// Overlay is "" or one of Overlays().
	Overlay string
	// Limited is /limited's and /limited-closed's ratelimit, with a
	// declared ceiling.
	Limited Limit
	// Undeclared is /undeclared's limit, without a ceiling.
	Undeclared int
	// Hot is /hot's constant-key limit with its ceiling of requests /
	// N_serving, and HotLocal /hot-local's localOnly one.
	Hot, HotLocal Limit
	// Cotenant is /cotenant's low limit.
	Cotenant int
	// Flood is /flood's limit, keyed by a header that changes on every
	// request.
	Flood Limit
	// CacheTimeout is the cache Policy's stateStoreTimeout, the bound of
	// its lookups and post-commit writes.
	CacheTimeout time.Duration
	// JWTIssuer and JWKSURL are jwt-chaos's issuer and its https jwksUrl,
	// the run's mock IdP, which OverlayJWT requires.
	JWTIssuer, JWKSURL string
	// DiscoveryPort is discovered-a's port, which OverlayDiscovery
	// requires.
	DiscoveryPort int
}

// DefaultBundleParams returns parameters every limit of which stays far
// from a run's load, for runs that exercise none of them; a run sets the
// limits it asserts.
func DefaultBundleParams(nodes int) BundleParams {
	return BundleParams{
		Topology:     "standalone",
		Limited:      Limit{Requests: 1_000_000, Ceiling: max(1, 1_000_000/max(1, nodes))},
		Undeclared:   1_000_000,
		Hot:          Limit{Requests: 1_000_000, Ceiling: max(1, 1_000_000/max(1, nodes))},
		HotLocal:     Limit{Requests: 1_000_000, Ceiling: max(1, 1_000_000/max(1, nodes))},
		Cotenant:     1_000_000,
		Flood:        Limit{Requests: 1_000_000, Ceiling: max(1, 1_000_000/max(1, nodes))},
		CacheTimeout: 50 * time.Millisecond,
	}
}

// variables returns the Environment variables of p.
func (p BundleParams) variables() (map[string]string, error) {
	for i, b := range p.Backends {
		if b == "" {
			return nil, fmt.Errorf("chaos: bundle: backend %d has no address", i+1)
		}
	}
	if !slices.Contains([]string{"standalone", "cluster", "sentinel"}, p.Topology) {
		return nil, fmt.Errorf("chaos: bundle: topology %q", p.Topology)
	}
	if p.Overlay != "" && !slices.Contains(Overlays(), p.Overlay) {
		return nil, fmt.Errorf("chaos: bundle: overlay %q", p.Overlay)
	}
	if p.CacheTimeout <= 0 {
		return nil, fmt.Errorf("chaos: bundle: cache stateStoreTimeout %v", p.CacheTimeout)
	}
	itoa := strconv.Itoa
	vars := map[string]string{
		"BACKEND_1":                 p.Backends[0],
		"BACKEND_2":                 p.Backends[1],
		"BACKEND_3":                 p.Backends[2],
		"STATE_STORE_TOPOLOGY":      p.Topology,
		"LIMITED_REQUESTS":          itoa(p.Limited.Requests),
		"LIMITED_CEILING":           itoa(p.Limited.Ceiling),
		"UNDECLARED_REQUESTS":       itoa(p.Undeclared),
		"HOT_REQUESTS":              itoa(p.Hot.Requests),
		"HOT_CEILING":               itoa(p.Hot.Ceiling),
		"HOT_LOCAL_REQUESTS":        itoa(p.HotLocal.Requests),
		"HOT_LOCAL_CEILING":         itoa(p.HotLocal.Ceiling),
		"COTENANT_REQUESTS":         itoa(p.Cotenant),
		"FLOOD_REQUESTS":            itoa(p.Flood.Requests),
		"FLOOD_CEILING":             itoa(p.Flood.Ceiling),
		"CACHE_STATE_STORE_TIMEOUT": p.CacheTimeout.String(),
	}
	// The variables only an overlay reads are set only when given, so an
	// overlay rendered without them fails on its unresolved variable.
	if p.JWTIssuer != "" {
		vars["JWT_ISSUER"] = p.JWTIssuer
	}
	if p.JWKSURL != "" {
		vars["JWKS_URL"] = p.JWKSURL
	}
	if p.DiscoveryPort > 0 {
		vars["DISCOVERY_PORT"] = itoa(p.DiscoveryPort)
	}
	return vars, nil
}

// environmentName is the Environment the harness renders.
const environmentName = "chaos"

// environments returns the environments file of p: one Environment
// selecting p's overlay, or none, with p's variables.
func (p BundleParams) environments() ([]byte, error) {
	vars, err := p.variables()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(vars))
	for k := range vars {
		names = append(names, k)
	}
	slices.Sort(names)
	var b strings.Builder
	b.WriteString("apiVersion: ruralz/v1alpha1\nkind: Environment\nmetadata:\n  name: " + environmentName + "\nspec:\n")
	if p.Overlay != "" {
		b.WriteString("  overlay: " + p.Overlay + "\n")
	}
	b.WriteString("  variables:\n")
	for _, k := range names {
		b.WriteString("    " + k + ": " + strconv.Quote(vars[k]) + "\n")
	}
	return []byte(b.String()), nil
}

// RenderBundle renders the chaos Bundle for p through the configuration
// pipeline, as ruralz bundle render --env does with an Environment
// holding p's overlay and variables, and returns the rendered one-file
// Bundle a file-mode Node loads. Diagnostics with an error, or a run
// without a Revision, fail it; warnings pass.
func RenderBundle(ctx context.Context, pl *pipeline.Pipeline, p BundleParams) ([]byte, error) {
	env, err := p.environments()
	if err != nil {
		return nil, err
	}
	res, err := pl.Run(ctx, pipeline.Request{
		Source:       loader.Source{FS: bundleFS, Path: bundleRoot},
		Environments: loader.Source{FS: fstest.MapFS{"environments.yaml": &fstest.MapFile{Data: env}}, Path: "environments.yaml"},
		Environment:  environmentName,
		Render:       true,
	})
	if err != nil {
		return nil, fmt.Errorf("chaos: bundle: %w", err)
	}
	if res.HasErrors() || res.Validated == nil {
		return nil, fmt.Errorf("chaos: bundle: %w", diagError(res.Diagnostics))
	}
	var b bytes.Buffer
	ds, err := pl.WriteRendered(ctx, &b, res, render.FormatYAML)
	if err != nil {
		return nil, fmt.Errorf("chaos: bundle: %w", err)
	}
	if len(ds) > 0 {
		return nil, fmt.Errorf("chaos: bundle: %w", diagError(ds))
	}
	return b.Bytes(), nil
}

// diagError joins diagnostics into one error.
func diagError(ds diag.List) error {
	if len(ds) == 0 {
		return errors.New("no Revision")
	}
	var errs []error
	for _, d := range ds {
		errs = append(errs, fmt.Errorf("%s %s:%d:%d %s", d.Code, d.File, d.Line, d.Column, d.Message))
	}
	return errors.Join(errs...)
}
