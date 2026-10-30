// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package compile

import (
	"cmp"
	"crypto/tls"
	"maps"
	"net/netip"
	"slices"

	"github.com/ravindu-rev/ruralz/internal/clientaddr"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter/header"
	"github.com/ravindu-rev/ruralz/internal/filter/transform/request"
	"github.com/ravindu-rev/ruralz/internal/gateway/listener"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/resilience"
	"github.com/ravindu-rev/ruralz/internal/identity"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// The schema defaults of the Gateway fields the compiler reads
// (docs/architecture/02-configuration-model.md "Gateway"). Stage G
// materializes them in every Revision; the compiler applies them only to
// a field it finds absent.
const (
	defaultMaxRequestBodyBytes   = 10 << 20
	defaultMaxRequestHeaderBytes = 64 << 10
	defaultMaxResponseBodyBytes  = 10 << 20
	defaultMaxCompositionSteps   = 16
	defaultMaxBufferedBytes      = 512 << 20
	defaultAdminPort             = 9901
	defaultTraceSampling         = 0.01
)

// claimsBytes bounds auth.claims for the CEL cost estimate: auth.jwt
// decodes a token of at most 16 KiB into at most four times that
// (expr.InputCaps.ClaimsBytes).
const claimsBytes = 4 * 16 << 10

// codeSchema is RZ-CFG-005, the code of a Gateway setting validation
// refuses first.
const codeSchema = "RZ-CFG-005"

// limitsOf returns the effective limits of Gateway spec.limits: each
// field, else its schema default, with limits.maxRequestHeaderBytes capped
// at the 256 KiB process ceiling, which the activation reports as
// header_limit_capped (docs/architecture/03-data-plane.md "Listeners and
// protocols").
func limitsOf(gl *v1alpha1.Limits) snapshot.Limits {
	l := snapshot.Limits{
		MaxRequestBodyBytes: defaultMaxRequestBodyBytes, MaxResponseBodyBytes: defaultMaxResponseBodyBytes,
		MaxBufferedBytes: defaultMaxBufferedBytes, MaxCompositionSteps: defaultMaxCompositionSteps,
		MaxRequestHeaderBytes: defaultMaxRequestHeaderBytes,
	}
	if gl != nil {
		if v := gl.MaxRequestBodyBytes; v != nil {
			l.MaxRequestBodyBytes = int64(*v)
		}
		if v := gl.MaxResponseBodyBytes; v != nil {
			l.MaxResponseBodyBytes = int64(*v)
		}
		if v := gl.MaxBufferedBytes; v != nil {
			l.MaxBufferedBytes = int64(*v)
		}
		if v := gl.MaxCompositionSteps; v != nil {
			l.MaxCompositionSteps = int(*v)
		}
		if v := gl.MaxRequestHeaderBytes; v != nil {
			l.MaxRequestHeaderBytes = int64(*v)
		}
	}
	if l.MaxRequestHeaderBytes > listener.MaxHeaderBytes {
		l.MaxRequestHeaderBytes, l.HeaderLimitCapped = listener.MaxHeaderBytes, true
	}
	return l
}

// prepare compiles what every program and Filter of the snapshot reads:
// the Consumers and Route views, each prepared for CEL once, the input
// caps of the snapshot's CEL Builder and the Builder itself, over the
// previous snapshot's programs.
func (b *build) prepare() {
	b.limits = limitsOf(b.gw.Spec.Limits)
	b.consumers = identity.CompileBundle(b.b).Map()
	for _, name := range slices.Sorted(maps.Keys(b.consumers)) {
		b.c.d.CEL.PrepareConsumer(b.consumers[name])
	}
	routes := b.b.Routes()
	b.views = make(map[string]*expr.Route, len(routes))
	caps := expr.InputCaps{
		RequestHeaderBytes: b.limits.MaxRequestHeaderBytes, AddedHeaderBytes: b.addedHeaderBytes(), ClaimsBytes: claimsBytes,
	}
	caps.Routes.Add(nil)
	for _, rt := range routes {
		v := &expr.Route{Name: rt.Metadata.Name, Labels: maps.Clone(rt.Metadata.Labels)}
		b.c.d.CEL.PrepareRoute(v)
		b.views[v.Name] = v
		caps.Routes.Add(v)
	}
	caps.Consumers.Add(nil)
	for _, c := range b.consumers {
		caps.Consumers.Add(c)
	}
	var prev expr.ProgramSet
	if p := b.in.Previous; p != nil {
		prev = p.Programs
	}
	b.builder = b.c.d.CEL.NewBuilder(prev, caps)
	b.shared = &shared{}
	b.carry = carryOverOf(b.in.Previous)
}

// addedHeaderBytes returns the most bytes the client-leg Policies of any
// one Route can add to its request's headers, query and path before an
// expression reads them (expr.InputCaps.AddedHeaderBytes): over the
// Route's effective chain, the request side of each headers Policy
// (header.AddedRequestBytes) and each transform.request Policy
// (request.AddedRequestBytes).
func (b *build) addedHeaderBytes() int64 {
	var most int64
	for _, ch := range b.v.Chains {
		if ch == nil {
			continue
		}
		var sum int64
		for _, e := range distinct(&ch.Client) {
			r := b.byKey[attachKey{policy: e.Policy, scope: e.Scope, phases: e.Phases}]
			if r == nil {
				continue
			}
			switch cfg := r.res.Config.(type) {
			case *v1alpha1.HeadersConfig:
				sum += header.AddedRequestBytes(cfg)
			case *v1alpha1.TransformRequestConfig:
				sum += request.AddedRequestBytes(cfg)
			}
		}
		most = max(most, sum)
	}
	return most
}

// compileGateway compiles the Node-wide settings of the Revision: the
// admin port, trusted proxies, limits, trace sampling, OTLP endpoint,
// access log condition and State Store timeout default.
func (b *build) compileGateway() error {
	sp := &b.gw.Spec
	g := snapshot.Gateway{AdminPort: defaultAdminPort, Limits: b.limits, TraceSampling: defaultTraceSampling, StateStoreTimeout: b.stateTimeout}
	if a := sp.Admin; a != nil && a.Port != nil {
		g.AdminPort = int(*a.Port)
	}
	for _, e := range sp.TrustedProxies {
		p, err := clientaddr.ParsePrefix(e)
		if err != nil {
			path := specPath("trustedProxies").Append(diag.Item(e))
			if err := b.refuse(refusal{res: b.gwRes, path: path, err: errcode.Wrap(codeSchema, err)}); err != nil {
				return err
			}
			continue
		}
		g.TrustedProxies = append(g.TrustedProxies, p)
	}
	slices.SortFunc(g.TrustedProxies, func(x, y netip.Prefix) int {
		return cmp.Or(x.Addr().Compare(y.Addr()), cmp.Compare(x.Bits(), y.Bits()))
	})
	if t := sp.Telemetry; t != nil {
		if t.TraceSampling != nil {
			g.TraceSampling = *t.TraceSampling
		}
		if t.OTLP != nil {
			g.OTLPEndpoint = t.OTLP.Endpoint
		}
		if al := t.AccessLog; al != nil && al.When != "" {
			prg, iss := b.builder.Compile(expr.Site{Place: expr.PlaceAccessLogWhen}, al.When)
			if prg == nil {
				path := specPath("telemetry", "accessLog", "when")
				if err := b.refuse(refusal{res: b.gwRes, path: path, err: celError(iss)}); err != nil {
					return err
				}
			}
			g.AccessLogWhen = prg
		}
	}
	b.gateway = g
	return nil
}

// compileListeners returns the client listeners in name order, an https
// listener with its TLS settings: the minimum version, the certificates
// in name order as secret references, and a client certificate request
// when a Route bound to it holds auth.mtls in its effective chain.
func (b *build) compileListeners() []*snapshot.Listener {
	mtls := map[string]bool{}
	for _, rt := range b.routes {
		for _, p := range rt.Chain.Policies {
			if p.Type == v1alpha1.PolicyTypeAuthMTLS && p.Scope != phase.ScopeUpstream {
				for _, l := range rt.Listeners {
					mtls[l] = true
				}
				break
			}
		}
	}
	out := make([]*snapshot.Listener, 0, len(b.gw.Spec.Listeners))
	for _, l := range b.gw.Spec.Listeners {
		sl := &snapshot.Listener{
			Name: l.Name, Protocol: l.Protocol, Port: int(l.Port), Hostnames: slices.Clone(l.Hostnames),
			Metrics: b.plan.Listener(l.Name),
		}
		if l.ProxyProtocol != nil {
			sl.ProxyProtocol = *l.ProxyProtocol
		}
		if l.Protocol == v1alpha1.ListenerProtocolHTTPS && l.TLS != nil {
			t := &snapshot.ListenerTLS{MinVersion: tls.VersionTLS13, RequestClientCert: mtls[l.Name]}
			if v := l.TLS.MinVersion; v != nil && *v == v1alpha1.TLSVersion12 {
				t.MinVersion = tls.VersionTLS12
			}
			for _, c := range l.TLS.Certificates {
				t.Certificates = append(t.Certificates, snapshot.Certificate{
					Name: c.Name, Certificate: secret.RefOf(c.Certificate.SecretRef), PrivateKey: secret.RefOf(c.PrivateKey.SecretRef),
				})
			}
			slices.SortFunc(t.Certificates, func(x, y snapshot.Certificate) int { return cmp.Compare(x.Name, y.Name) })
			sl.TLS = t
		}
		out = append(out, sl)
	}
	slices.SortFunc(out, func(x, y *snapshot.Listener) int { return cmp.Compare(x.Name, y.Name) })
	return out
}

// warningsOf returns the activation warnings: one when the Revision
// attaches cache and a limit Policy type without stateStore.cache, so the
// Response Cache shares the connection that holds limit keys
// (docs/architecture/09-traffic-management-and-resilience.md "Response
// caching"), then the balancer budget's warnings, by Upstream.
func (b *build) warningsOf() []Warning {
	var out []Warning
	if b.cachePolicies && b.limitPolicies && (b.gw.Spec.StateStore == nil || b.gw.Spec.StateStore.Cache == nil) {
		out = append(out, Warning{
			Resource: b.gwRes.ResourceID(),
			Message: "the Revision attaches cache and limit Policies (ratelimit, quota or ai.token-budget) without stateStore.cache: " +
				"Response Cache entries share the State Store connection that holds limit keys",
		})
	}
	if b.set != nil {
		for _, w := range b.set.Warnings() {
			out = append(out, Warning{Resource: &diag.ResourceID{Kind: string(v1alpha1.KindUpstream), Name: w.Upstream}, Message: w.Message})
		}
	}
	return out
}

// connectionsOf returns the summed circuitBreaker.maxConnections of the
// reached Upstreams, the default 1,024 for one that sets none.
func (b *build) connectionsOf() int {
	n := 0
	for _, name := range b.upstreams {
		u, ok := b.b.Upstream(name)
		if !ok {
			continue
		}
		c := resilience.DefaultMaxConnections
		if cb := u.Spec.CircuitBreaker; cb != nil && cb.MaxConnections != nil {
			c = int(*cb.MaxConnections)
		}
		n += c
	}
	return n
}
