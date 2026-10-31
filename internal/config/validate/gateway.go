// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package validate

import (
	"cmp"
	"slices"
	"strconv"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/clientaddr"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/netaddr"
	"github.com/ravindu-rev/ruralz/internal/routematch"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// gateway checks the Gateway rules a Node would otherwise refuse or
// ignore at activation: listener ports, listener TLS, listener hostnames,
// trustedProxies, the OTLP endpoint and client TLS, the client pairs of
// stateStore.tls and stateStore.cache.tls, and the zone maps
// stateStore.zones and stateStore.cache.zones.
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
		base := specPath(diag.Field("stateStore"))
		// stateStore.tls and stateStore.cache.tls take the client pair rule
		// of the other client TLS blocks; their cleartext-URL conflict
		// needs the resolved URL, so the Node checks it (RZ-CFG-042).
		if s.TLS != nil {
			u.clientPair(base.Append(diag.Field("tls")), s.TLS.ClientCertificate, s.TLS.ClientKey)
		}
		u.zones("stateStore.zones", base, s.Zones)
		u.zonesDriver(base, s)
		if c := s.Cache; c != nil {
			cache := base.Append(diag.Field("cache"))
			if c.TLS != nil {
				u.clientPair(cache.Append(diag.Field("tls")), c.TLS.ClientCertificate, c.TLS.ClientKey)
			}
			// The cache connection is always redis, whatever the main
			// driver: its zone map needs no driver check.
			u.zones("stateStore.cache.zones", cache, c.Zones)
		}
	}
}

// zones checks one connection's zone map (stateStore.zones or
// stateStore.cache.zones, named by field) under base: each host a
// hostname or an IP literal, in the form the State client looks hosts up
// in (netaddr.CanonicalHost), and named once in the map after case
// folding and IP canonicalization. Two hosts equal as written are
// duplicate keyed-list entries, which stage F reports first; a host in
// both maps is each connection's own and no finding. An entry for a host
// the connection never dials is no finding either: URLs are secrets, and
// cluster and sentinel hosts appear only at run time. Of two entries
// naming one host, the later in the source is the finding, whatever the
// key order of the map list.
func (u *unit) zones(field string, base diag.Path, zones []v1alpha1.StateStoreZone) {
	type entry struct {
		written, host string
		path          diag.Path
		loc           diag.Location
	}
	valid := make([]entry, 0, len(zones))
	for _, z := range zones {
		u.w.charge(1)
		hp := base.Append(diag.Field("zones"), diag.Keyed("host", z.Host), diag.Field("host"))
		host, err := netaddr.CanonicalHost(z.Host)
		if err != nil {
			u.report(CodeSchema, hp, field+" host "+quote(z.Host)+" is neither a hostname nor an IP literal",
				"write the host alone, as the URL names it or the topology announces it, without a port or brackets", nil)
			continue
		}
		valid = append(valid, entry{written: z.Host, host: host, path: hp, loc: u.f.path(hp)})
	}
	slices.SortStableFunc(valid, func(a, b entry) int {
		return cmp.Or(strings.Compare(a.loc.File, b.loc.File), cmp.Compare(a.loc.Line, b.loc.Line), cmp.Compare(a.loc.Column, b.loc.Column))
	})
	first := make(map[string]entry, len(valid))
	for _, e := range valid {
		if f, dup := first[e.host]; dup {
			u.reportAt(CodeSchema, e.path, e.loc, field+" names host "+quote(e.written)+" twice, first as "+quote(f.written),
				"name each host once: hosts compare case-insensitively and IP literals in canonical form",
				[]diag.Related{{Location: f.loc, Message: "first named in"}})
			continue
		}
		first[e.host] = e
	}
}

// zonesDriver checks that stateStore.zones has entries only beside driver
// redis: the memory driver has no servers, so its zone map could never
// apply. The driver is the materialized default memory when unset.
func (u *unit) zonesDriver(base diag.Path, s *v1alpha1.StateStore) {
	if len(s.Zones) == 0 {
		return
	}
	driver := v1alpha1.StateStoreDriverMemory
	if s.Driver != nil {
		driver = *s.Driver
	}
	if driver == v1alpha1.StateStoreDriverRedis {
		return
	}
	dp := base.Append(diag.Field("driver"))
	msg := "stateStore.zones applies to driver redis only, and this State Store uses the default driver " + string(driver)
	var related []diag.Related
	if n := u.f.node(dp); n != nil && n.Style != tree.StyleDefaulted {
		msg = "stateStore.zones applies to driver redis only, and this State Store uses driver " + string(driver)
		related = []diag.Related{{Location: u.f.path(dp), Message: "driver set in"}}
	}
	u.report(CodeSchema, base.Append(diag.Field("zones")), msg, "remove zones or use driver redis", related)
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
		u.clientPair(base.Append(diag.Field("tls")), o.TLS.ClientCertificate, o.TLS.ClientKey)
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

// clientPair checks the clientCertificate and clientKey of a client TLS
// block (Upstream.spec.tls, telemetry.otlp.tls, stateStore.tls,
// stateStore.cache.tls): together or neither, reported at the missing one.
func (u *unit) clientPair(base diag.Path, cert, key *v1alpha1.SecretValue) {
	if (cert == nil) == (key == nil) {
		return
	}
	missing, present := "clientKey", "clientCertificate"
	if cert == nil {
		missing, present = present, missing
	}
	u.report(CodeSchema, base.Append(diag.Field(missing)),
		present+" is set without "+missing+": a client certificate needs its key, and the key its certificate", "", nil)
}
