// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package validate

import (
	"strconv"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/clientaddr"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/netaddr"
	"github.com/ravindu-rev/ruralz/internal/routematch"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// gateway checks the Gateway rules a Node would otherwise refuse or
// ignore at activation: listener ports, listener TLS, listener hostnames,
// trustedProxies, the OTLP endpoint and client TLS, and the client pairs
// of stateStore.tls and stateStore.cache.tls.
func (u *unit) gateway() {
	g, ok := hub.Object[v1alpha1.Gateway](u.r)
	if !ok || g == nil {
		return
	}
	u.listeners(g)
	for _, e := range g.Spec.TrustedProxies {
		u.w.charge(1)
		if _, err := clientaddr.ParsePrefix(e); err != nil {
			msg := "trustedProxies entry " + quote(e) + " is neither a CIDR nor an IP address"
			if strings.Contains(e, "%") {
				msg = "trustedProxies entry " + quote(e) + " has a zone, which a CIDR never has"
			}
			u.report(CodeSchema, specPath(diag.Field("trustedProxies"), diag.Item(e)), msg, "", nil)
		}
	}
	if t := g.Spec.Telemetry; t != nil && t.OTLP != nil {
		u.otlp(t.OTLP)
	}
	if s := g.Spec.StateStore; s != nil {
		// stateStore.tls and stateStore.cache.tls take the client pair rule
		// of the other client TLS blocks; their cleartext-URL conflict
		// needs the resolved URL, so the Node checks it (RZ-CFG-042).
		if s.TLS != nil {
			u.clientPair(specPath(diag.Field("stateStore"), diag.Field("tls")),
				&v1alpha1.UpstreamTLS{ClientCertificate: s.TLS.ClientCertificate, ClientKey: s.TLS.ClientKey})
		}
		if c := s.Cache; c != nil && c.TLS != nil {
			u.clientPair(specPath(diag.Field("stateStore"), diag.Field("cache"), diag.Field("tls")),
				&v1alpha1.UpstreamTLS{ClientCertificate: c.TLS.ClientCertificate, ClientKey: c.TLS.ClientKey})
		}
	}
}

// otlp checks the OTLP exporter: an endpoint the Node's telemetry
// settings dial (netaddr.ParseOTLPEndpoint), and a tls block only beside
// an https endpoint, since an http one ignores it, with clientCertificate
// and clientKey together.
func (u *unit) otlp(o *v1alpha1.OTLP) {
	base := specPath(diag.Field("telemetry"), diag.Field("otlp"))
	if o.Endpoint != "" {
		ep, err := netaddr.ParseOTLPEndpoint(o.Endpoint)
		switch {
		case err != nil:
			u.report(CodeSchema, base.Append(diag.Field("endpoint")), err.Error(), "", nil)
		case ep.Scheme == "http" && o.TLS != nil:
			u.report(CodeSchema, base.Append(diag.Field("tls")),
				"telemetry.otlp.tls applies to an https endpoint only, and this endpoint is http", "remove tls or use an https endpoint", nil)
		}
	}
	if o.TLS != nil {
		u.clientPair(base.Append(diag.Field("tls")), o.TLS)
	}
}

// listeners checks each listener of g: its own port, not the admin port
// (admin.port, else its default); tls with at least one certificate on an
// https listener and no tls on an http one, which ignores it; hostnames in
// the match.hosts forms.
func (u *unit) listeners(g *v1alpha1.Gateway) {
	admin := setting{value: u.v.defaults.adminPort}
	if a := g.Spec.Admin; a != nil && a.Port != nil {
		admin = gatewaySetting(u.f, int64(*a.Port), "admin", "port")
	}
	type firstUse struct {
		name string
		loc  diag.Location
	}
	ports := map[int32]firstUse{}
	for _, li := range g.Spec.Listeners {
		u.w.charge(1)
		lp := specPath(diag.Field("listeners"), diag.Keyed("name", li.Name))
		pp := lp.Append(diag.Field("port"))
		loc := u.f.path(pp)
		port := strconv.FormatInt(int64(li.Port), 10)
		switch first, dup := ports[li.Port]; {
		case int64(li.Port) == admin.value && admin.authored:
			u.reportAt(CodeSchema, pp, loc, "listener "+quote(li.Name)+" uses port "+port+", the admin port (spec.admin.port)",
				"give each listener its own port, apart from the admin port", admin.related("admin port set in"))
		case int64(li.Port) == admin.value:
			u.reportAt(CodeSchema, pp, loc, "listener "+quote(li.Name)+" uses port "+port+", the default admin port (spec.admin.port)",
				"give each listener its own port, apart from the admin port", nil)
		case dup:
			u.reportAt(CodeSchema, pp, loc, "listener "+quote(li.Name)+" uses port "+port+", already used by listener "+quote(first.name),
				"give each listener its own port", []diag.Related{{Location: first.loc, Message: "first used in"}})
		default:
			ports[li.Port] = firstUse{name: li.Name, loc: loc}
		}
		switch {
		case li.Protocol == v1alpha1.ListenerProtocolHTTPS && li.TLS == nil:
			u.report(CodeSchema, lp.Append(diag.Field("tls")), "an https listener needs tls with at least one certificate", "", nil)
		case li.Protocol == v1alpha1.ListenerProtocolHTTPS && len(li.TLS.Certificates) == 0:
			u.report(CodeSchema, lp.Append(diag.Field("tls"), diag.Field("certificates")), "an https listener needs at least one tls.certificates entry", "", nil)
		case li.Protocol == v1alpha1.ListenerProtocolHTTP && li.TLS != nil:
			u.report(CodeSchema, lp.Append(diag.Field("tls")),
				"listener "+quote(li.Name)+" has protocol http, which ignores tls", "remove tls or use protocol https", nil)
		}
		for _, h := range li.Hostnames {
			u.w.charge(1)
			if err := routematch.CheckHost(h); err != nil {
				u.report(CodeSchema, lp.Append(diag.Field("hostnames"), diag.Item(h)), causeText(err, h), "", nil)
			}
		}
	}
}

// clientPair checks a client TLS block (Upstream.spec.tls,
// telemetry.otlp.tls): clientCertificate and clientKey together or
// neither, reported at the missing one.
func (u *unit) clientPair(base diag.Path, t *v1alpha1.UpstreamTLS) {
	if (t.ClientCertificate == nil) == (t.ClientKey == nil) {
		return
	}
	missing, present := "clientKey", "clientCertificate"
	if t.ClientCertificate == nil {
		missing, present = present, missing
	}
	u.report(CodeSchema, base.Append(diag.Field(missing)),
		present+" is set without "+missing+": a client certificate needs its key, and the key its certificate", "", nil)
}
