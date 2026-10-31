// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package compile

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/statestore"
)

// httpsListener is an https listener with http3, served from M3, and one
// certificate.
const httpsListener = "    - name: https\n      protocol: https\n      port: 8443\n      http3: true\n      tls:\n        certificates:\n" +
	"          - name: c\n            certificate: {secretRef: {provider: file, name: /etc/ruralz/c.crt}}\n" +
	"            privateKey: {secretRef: {provider: file, name: /etc/ruralz/c.key}}\n"

// TestServeCheck covers step 7: every resource, whether a Route reaches it
// or not, is checked against the served-feature table and the Policy type
// registry; every RZ-CFG-040 finding comes back, sorted and located in its
// source, and the compile ends there: no secret resolves, no State Store
// opens, no metric label set is admitted and no Filter builds.
func TestServeCheck(t *testing.T) {
	h := newHarness(t)
	res := h.validate(
		gateway(httpsListener),
		upstreamDoc("up", "127.0.0.1:9000", ""),
		doc("Upstream", "legacy", "  protocol: grpc\n  endpoints:\n    - address: \"127.0.0.1:9100\"\n"),
		routeDoc("r", "/", "up", attach("hdr")),
		policyDoc("hdr", "headers", headersConfig, ""),
		policyDoc("geo", "authz.geoip", `{"deny":["XX"]}`, ""),
	)
	got := h.rejects(res)
	want := []string{
		`ruralz.yaml:13:14 error RZ-CFG-040 Gateway/edge spec.listeners[name=https].http3: field "http3" is not served by this release (Planned (M3))`,
		`ruralz.yaml:34:13 error RZ-CFG-040 Upstream/legacy spec.protocol: protocol "grpc" is not served by this release (Planned (M3))`,
		`ruralz.yaml:64:9 error RZ-CFG-040 Policy/geo spec.type: Policy type "authz.geoip" is not served by this release (Planned (M2))`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("findings\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if n := h.stores.calls(); n != 0 {
		t.Errorf("Acquire ran %d times after the serve check refused", n)
	}
	if n := len(h.builds()); n != 0 {
		t.Errorf("%d Builds ran after the serve check refused", n)
	}
	if len(h.meter.shapes) != 0 || len(h.cel.caps) != 0 {
		t.Errorf("%d Admits and %d Builders after the serve check refused", len(h.meter.shapes), len(h.cel.caps))
	}
	// Serve runs the same check alone, with the same findings, and
	// without a file table locates them by each resource's own source.
	ds, err := Serve(h.tc.pipe, res.Validated, res.Files)
	if err != nil || !slices.Equal(lines(ds), want) {
		t.Errorf("Serve = %v\n%s\nwant\n%s", err, text(ds), strings.Join(want, "\n"))
	}
	if ds, err := Serve(h.tc.pipe, res.Validated, nil); err != nil || !slices.Equal(lines(ds), want) {
		t.Errorf("Serve without files = %v\n%s\nwant\n%s", err, text(ds), strings.Join(want, "\n"))
	}
	clean := h.validate(gateway(""), upstreamDoc("up", "127.0.0.1:9000", ""), routeDoc("r", "/", "up", ""))
	if ds, err := Serve(h.tc.pipe, clean.Validated, clean.Files); err != nil || len(ds) != 0 {
		t.Errorf("Serve of a served Revision = %v\n%s", err, text(ds))
	}
	for name, call := range map[string]func() error{
		"no pipeline": func() error { _, err := Serve(nil, res.Validated, res.Files); return err },
		"no Revision": func() error { _, err := Serve(h.tc.pipe, nil, nil); return err },
		"no Bundle":   func() error { _, err := Serve(h.tc.pipe, &hub.Validated{}, nil); return err },
	} {
		if err := call(); !errors.Is(err, ErrInput) {
			t.Errorf("Serve with %s = %v, want ErrInput", name, err)
		}
	}
}

// TestSecretRefusals covers step 9's resolution: every secretRef use of the
// Revision resolves, a reached resource's or not; each failing use is a
// finding at its field, all reported and sorted, and the compile ends
// before the State Store opens or any Filter builds.
func TestSecretRefusals(t *testing.T) {
	h := newHarness(t)
	h.env.set("RURALZ_SECRET_KEY_OK", strings.Repeat("k", 32))
	h.env.set("RURALZ_SECRET_SHORT", "short")
	res := h.validate(
		gateway(""),
		upstreamDoc("up", "127.0.0.1:9000", ""),
		upstreamDoc("idle", "127.0.0.1:9001", "  tls:\n    caCertificate: {secretRef: {provider: env, name: RURALZ_SECRET_IDLE_CA}}\n"),
		routeDoc("r", "/", "up", attach("hdr")),
		policyDoc("hdr", "headers", headersConfig, ""),
		doc("Consumer", "acme", "  credentials:\n    apiKeys:\n"+
			"      - name: k1\n        secretRef: {provider: env, name: RURALZ_SECRET_KEY_OK}\n"+
			"      - name: k2\n        secretRef: {provider: env, name: RURALZ_SECRET_SHORT}\n"),
	)
	got := h.rejects(res)
	if len(got) != 2 ||
		!strings.Contains(got[0], "error RZ-CFG-026 Upstream/idle spec.tls.caCertificate") ||
		!strings.Contains(got[1], "error RZ-CFG-026 Consumer/acme spec.credentials.apiKeys[name=k2].secretRef") {
		t.Errorf("findings\n%s\nwant the CA of idle, then key k2", strings.Join(got, "\n"))
	}
	if n := h.stores.calls(); n != 0 {
		t.Errorf("Acquire ran %d times after a secret failed", n)
	}
	if n := len(h.builds()); n != 0 {
		t.Errorf("%d Builds ran after a secret failed", n)
	}
}

// TestStateStoreFallback covers the State Store of a Gateway without
// stateStore: RURALZ_STATE_STORE_URL, when set, resolves with the
// Revision's secrets and opens the redis driver as the env source, and a
// value that does not resolve is a finding at the Gateway's spec, never a
// silent fallback; unset or blank, the memory driver serves. A Gateway
// with stateStore ignores the variable.
func TestStateStoreFallback(t *testing.T) {
	for _, tc := range []struct {
		name, env string
		set       bool
		spec      string
		driver    statestore.DriverKind
		source    statestore.Source
		url       string
		finding   string
	}{
		{name: "unset", driver: statestore.DriverMemory, source: statestore.SourceFallback},
		{name: "blank", env: " \t\r\n", set: true, driver: statestore.DriverMemory, source: statestore.SourceFallback},
		{name: "loopback", env: "redis://127.0.0.1:6379", set: true, driver: statestore.DriverRedis, source: statestore.SourceEnv, url: "redis://127.0.0.1:6379"},
		{name: "credentials", env: " redis://u:p@state.internal:6379\n", set: true, driver: statestore.DriverRedis, source: statestore.SourceEnv, url: "redis://u:p@state.internal:6379"}, //nolint:gosec // G101: a test URL's placeholder user and password, not a credential.
		{name: "no credentials", env: "redis://state.internal:6379", set: true, finding: "error RZ-CFG-042 Gateway/edge spec:"},
		{name: "not a URL", env: "state.internal", set: true, finding: "error RZ-CFG-026 Gateway/edge spec:"},
		{name: "stateStore set", env: "redis://state.internal:6379", set: true, spec: "  stateStore:\n    driver: memory\n", driver: statestore.DriverMemory, source: statestore.SourceGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			if tc.set {
				h.env.set("RURALZ_STATE_STORE_URL", tc.env)
			}
			res := h.validate(gateway(tc.spec), upstreamDoc("up", "127.0.0.1:9000", ""), routeDoc("r", "/", "up", ""))
			if tc.finding != "" {
				got := h.rejects(res)
				if len(got) != 1 || !strings.HasPrefix(got[0], "ruralz.yaml:6:3 "+tc.finding) {
					t.Errorf("findings = %q, want one %q at the Gateway's spec", got, tc.finding)
				}
				if n := h.stores.calls(); n != 0 {
					t.Errorf("Acquire ran %d times", n)
				}
				return
			}
			h.mustCompile(res, nil)
			spec, _ := h.stores.last()
			m := spec.Main
			if m.Driver != tc.driver || m.Source != tc.source || m.URL != tc.url || spec.Cache != nil {
				t.Errorf("Spec.Main = driver %v, source %v, URL %q, cache %v; want %v, %v, %q, none", m.Driver, m.Source, m.URL, spec.Cache, tc.driver, tc.source, tc.url)
			}
			if opened := h.opens.opened(); len(opened) != 1 || opened[0].Driver != tc.driver {
				t.Errorf("drivers opened = %+v, want one %v", opened, tc.driver)
			}
		})
	}
}

// TestStateStoreRefusals covers the end of step 9: a driver the opener
// refuses is a finding at the connection's block, spec.stateStore for the
// main connection, spec.stateStore.cache for the cache one and the
// Gateway's spec for RURALZ_STATE_STORE_URL; an opener error without an RZ
// code is a fault, and the compile deadline ending during Acquire is the
// context's error.
func TestStateStoreRefusals(t *testing.T) {
	const (
		redisSpec = "  stateStore:\n    driver: redis\n    url: {secretRef: {provider: env, name: RURALZ_SECRET_REDIS}}\n"
		cacheSpec = "    cache:\n      url: {secretRef: {provider: env, name: RURALZ_SECRET_CACHE}}\n"
	)
	refused := errcode.Errorf("RZ-CFG-026", "the URL does not fit the topology")
	for _, tc := range []struct {
		name, spec string
		role       statestore.Role
		fallback   bool
		want       string
	}{
		{name: "main", spec: redisSpec, role: statestore.RoleMain, want: "ruralz.yaml:11:5 error RZ-CFG-026 Gateway/edge spec.stateStore: the URL does not fit the topology"},
		{name: "cache", spec: redisSpec + cacheSpec, role: statestore.RoleCache, want: "ruralz.yaml:14:7 error RZ-CFG-026 Gateway/edge spec.stateStore.cache: the URL does not fit the topology"},
		{name: "fallback", role: statestore.RoleMain, fallback: true, want: "ruralz.yaml:6:3 error RZ-CFG-026 Gateway/edge spec: the URL does not fit the topology"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.env.set("RURALZ_SECRET_REDIS", "redis://u:p@main.internal:6379")
			h.env.set("RURALZ_SECRET_CACHE", "redis://u:p@cache.internal:6379")
			if tc.fallback {
				h.env.set("RURALZ_STATE_STORE_URL", "redis://127.0.0.1:6379")
			}
			h.opens.fail[tc.role] = refused
			got := h.rejects(h.validate(gateway(tc.spec), upstreamDoc("up", "127.0.0.1:9000", ""), routeDoc("r", "/", "up", attach("hdr")), policyDoc("hdr", "headers", headersConfig, "")))
			if len(got) != 1 || got[0] != tc.want {
				t.Errorf("findings = %q, want %q", got, tc.want)
			}
			if n := len(h.builds()); n != 0 {
				t.Errorf("%d Builds ran after the State Store was refused", n)
			}
		})
	}

	t.Run("fault", func(t *testing.T) {
		h := newHarness(t)
		h.env.set("RURALZ_SECRET_REDIS", "redis://u:p@main.internal:6379")
		h.opens.fail[statestore.RoleMain] = errors.New("dial refused")
		c, ds, err := h.compile(h.validate(gateway(redisSpec), upstreamDoc("up", "127.0.0.1:9000", ""), routeDoc("r", "/", "up", "")), nil)
		if c != nil || len(ds) != 0 || err == nil || !strings.Contains(err.Error(), "dial refused") {
			t.Errorf("Compile = %v, %s, %v; want a fault naming the cause", c, text(ds), err)
		}
		if _, coded := errcode.CodeOf(err); coded {
			t.Errorf("fault %v carries an RZ code", err)
		}
	})

	t.Run("deadline", func(t *testing.T) {
		h := newHarness(t)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		h.stores.before = cancel
		h.stores.err = context.Canceled
		res := h.validate(gateway(""), upstreamDoc("up", "127.0.0.1:9000", ""), routeDoc("r", "/", "up", ""))
		c, ds, err := h.comp.Compile(ctx, Input{Validated: res.Validated, Files: res.Files})
		if c != nil || len(ds) != 0 || !errors.Is(err, context.Canceled) {
			t.Errorf("Compile = %v, %s, %v; want context.Canceled", c, text(ds), err)
		}
	})
}

// TestStateStoreSpec covers what the compile tells the State Store
// manager: whether the Revision reaches a limit Policy (ratelimit, quota)
// and a cache Policy, the main and cache connections of stateStore, and
// one new handle per role.
func TestStateStoreSpec(t *testing.T) {
	h := newHarness(t)
	h.env.set("RURALZ_SECRET_REDIS", "redis://u:p@main.internal:6379")
	h.env.set("RURALZ_SECRET_CACHE", "redis://u:p@cache.internal:6379")
	c := h.mustCompile(h.validate(
		gateway("  stateStore:\n    driver: redis\n    url: {secretRef: {provider: env, name: RURALZ_SECRET_REDIS}}\n"+
			"    cache:\n      url: {secretRef: {provider: env, name: RURALZ_SECRET_CACHE}}\n"),
		upstreamDoc("up", "127.0.0.1:9000", ""),
		routeDoc("r", "/", "up", attach("rl", "cache")),
		policyDoc("rl", "ratelimit", rateLimitConfig, ""),
		policyDoc("cache", "cache", cacheConfig, ""),
	), nil)
	spec, _ := h.stores.last()
	if !spec.Limits || !spec.Caches || spec.Cache == nil || spec.Main.URL != "redis://u:p@main.internal:6379" || spec.Cache.URL != "redis://u:p@cache.internal:6379" { //nolint:gosec // G101: test URLs' placeholder user and password, not credentials.
		t.Errorf("Spec = limits %v, caches %v, main %q, cache %+v", spec.Limits, spec.Caches, spec.Main.URL, spec.Cache)
	}
	s := c.Snapshot
	if s.StateStore == nil || s.CacheStore == nil || s.StateStore == s.CacheStore || s.StateStore.Store() == s.CacheStore.Store() {
		t.Errorf("StateStore %v, CacheStore %v; want two handles on two drivers", s.StateStore, s.CacheStore)
	}

	h = newHarness(t)
	h.mustCompile(h.validate(gateway(""), upstreamDoc("up", "127.0.0.1:9000", ""), routeDoc("r", "/", "up", attach("hdr")),
		policyDoc("hdr", "headers", headersConfig, ""), policyDoc("rl", "ratelimit", rateLimitConfig, "")), nil)
	if spec, _ := h.stores.last(); spec.Limits || spec.Caches {
		t.Errorf("Spec = limits %v, caches %v; want neither, as no Route reaches rl", spec.Limits, spec.Caches)
	}
}

// TestStateStoreZonesSpec covers the zone maps the compile hands the State
// Store manager: stateStore.zones for the main connection and
// stateStore.cache.zones for the cache connection, each its own, keyed by
// netaddr.CanonicalHost so hosts compare case-insensitively and IP
// literals in canonical form.
func TestStateStoreZonesSpec(t *testing.T) {
	h := newHarness(t)
	h.env.set("RURALZ_SECRET_REDIS", "redis://u:p@main.internal:6379")
	h.env.set("RURALZ_SECRET_CACHE", "redis://u:p@cache.internal:6379")
	h.mustCompile(h.validate(
		gateway("  stateStore:\n    driver: redis\n    url: {secretRef: {provider: env, name: RURALZ_SECRET_REDIS}}\n"+
			"    zones:\n      - {host: Main.INTERNAL, zone: eu-west-1a}\n      - {host: \"2001:0DB8::1\", zone: eu-west-1b}\n"+
			"    cache:\n      url: {secretRef: {provider: env, name: RURALZ_SECRET_CACHE}}\n"+
			"      zones: [{host: cache.internal, zone: eu-west-1b}]\n"),
		upstreamDoc("up", "127.0.0.1:9000", ""), routeDoc("r", "/", "up", ""),
	), nil)
	spec, _ := h.stores.last()
	if want := (statestore.Zones{"main.internal": "eu-west-1a", "2001:db8::1": "eu-west-1b"}); !maps.Equal(spec.Main.Zones, want) {
		t.Errorf("main zones = %v, want %v", spec.Main.Zones, want)
	}
	if spec.Cache == nil || !maps.Equal(spec.Cache.Zones, statestore.Zones{"cache.internal": "eu-west-1b"}) {
		t.Errorf("cache = %+v, want the cache connection's own zone map", spec.Cache)
	}
}
