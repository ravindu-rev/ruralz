// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package compile

// doc returns one resource of kind and name whose spec is the YAML block
// spec, indented by two spaces.
func doc(kind, name, spec string) string {
	return "apiVersion: ruralz/v1alpha1\nkind: " + kind + "\nmetadata:\n  name: " + name + "\nspec:\n" + spec
}

// gatewaySpec is the spec of a Gateway with one http listener on 8080.
const gatewaySpec = "  listeners:\n    - name: http\n      protocol: http\n      port: 8080\n"

// gateway returns the Gateway edge with one http listener and the spec
// lines extra.
func gateway(extra string) string { return doc("Gateway", "edge", gatewaySpec+extra) }

// upstreamDoc returns the http Upstream name with one Endpoint at addr and
// the spec lines extra.
func upstreamDoc(name, addr, extra string) string {
	return doc("Upstream", name, "  protocol: http\n  endpoints:\n    - address: \""+addr+"\"\n"+extra)
}

// routeDoc returns the Route name matching path prefix prefix, forwarding
// to Upstream up, with the spec lines extra.
func routeDoc(name, prefix, up, extra string) string {
	return doc("Route", name, "  match:\n    path:\n      prefix: "+prefix+"\n  upstreams:\n    - name: "+up+"\n"+extra)
}

// policyDoc returns the Policy name of type typ whose config is the JSON
// text config, with the spec lines extra.
func policyDoc(name, typ, config, extra string) string {
	return doc("Policy", name, "  type: "+typ+"\n  config: "+config+"\n"+extra)
}

// attach returns the spec.policies lines attaching names.
func attach(names ...string) string {
	out := "  policies:\n"
	for _, n := range names {
		out += "    - name: " + n + "\n"
	}
	return out
}

// Policy configs the pipeline validates.
const (
	headersConfig   = `{"request":{"set":[{"name":"x-a","value":"1"}]}}`
	rateLimitConfig = `{"limits":[{"requests":100,"window":"1s"}]}`
	cacheConfig     = `{}`
	corsConfig      = `{"allowOrigins":["https://shop.example"],"allowMethods":["GET"]}`
)
