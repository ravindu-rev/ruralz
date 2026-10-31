// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package validate

import (
	"strconv"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/netaddr"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// upstream checks the Upstream rules the Upstream layer would otherwise
// refuse at activation (Traffic management "Service discovery"), each
// RZ-CFG-005: an Endpoint source, Endpoint addresses, dns discovery and
// the client TLS pair. The address, name and port rules are
// internal/netaddr's, which the Upstream layer applies too. ring-hash
// without hashKey is the schema's (stage F).
func (u *unit) upstream() {
	up, ok := hub.Object[v1alpha1.Upstream](u.r)
	if !ok || up == nil {
		return
	}
	sp := &up.Spec
	if sp.Discovery == nil && len(sp.Endpoints) == 0 && sp.Protocol != v1alpha1.UpstreamProtocolAI {
		u.report(CodeSchema, specPath(diag.Field("endpoints")),
			"an Upstream needs endpoints unless it sets discovery or has protocol ai", "", nil)
	}
	for _, e := range sp.Endpoints {
		u.w.charge(1)
		if _, err := netaddr.ParseHostPort(e.Address); err != nil {
			u.report(CodeSchema, specPath(diag.Field("endpoints"), diag.Keyed("address", e.Address), diag.Field("address")),
				"endpoint address "+quote(e.Address)+": "+err.Error(), "", nil)
		}
	}
	if d := sp.Discovery; d != nil && d.Type == v1alpha1.DiscoveryTypeDNS {
		u.dns(d)
	}
	if sp.TLS != nil {
		u.clientPair(specPath(diag.Field("tls")), sp.TLS.ClientCertificate, sp.TLS.ClientKey)
	}
}

// dns checks dns discovery: a service DNS name and a port that is a
// number from 1 to 65535 or an SRV service name.
func (u *unit) dns(d *v1alpha1.Discovery) {
	base := specPath(diag.Field("discovery"))
	if err := netaddr.CheckName(d.Service); err != nil {
		u.report(CodeSchema, base.Append(diag.Field("service")), "dns discovery needs a service DNS name: "+err.Error(), "", nil)
	}
	at := base.Append(diag.Field("port"))
	switch p := d.Port; {
	case p == nil:
		u.report(CodeSchema, at, "dns discovery needs a port number or name", "", nil)
	case p.IsString:
		if err := netaddr.CheckPortName(p.Str); err != nil {
			u.report(CodeSchema, at, "discovery.port "+quote(p.Str)+": "+err.Error(), "", nil)
		}
	default:
		if err := netaddr.CheckPort(int64(p.Int)); err != nil {
			u.report(CodeSchema, at, "discovery.port "+strconv.FormatInt(int64(p.Int), 10)+": "+err.Error(), "", nil)
		}
	}
}
