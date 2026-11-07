// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"encoding/json"
	"math"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/jsonval"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Pool sizing bounds of docs/architecture/05-wasm-plugin-system.md
// "Default limits" and the rules after "Deny-by-default".
const (
	// minStatePool is the smallest state.* pool that is not an error.
	minStatePool = 4
	// minParkedBound is the smallest guaranteed parked bound that is not a
	// warning (target).
	minParkedBound = 16
)

// Check runs every rule on v, the validated Bundle of one run without
// error, and returns the findings sorted (Sort). files turns the tree
// positions of the run into locations; idx, the hub schema index the run
// used, supplies the schema defaults of the limits a resource leaves out,
// since stage G creates no absent object. Findings name no Environment.
func Check(v *hub.Validated, files *tree.FileTable, idx *schemaidx.Index) []Finding {
	if v == nil || v.Bundle == nil {
		return nil
	}
	c := &checker{v: v, files: files, idx: idx}
	for _, r := range v.Bundle.Resources() {
		switch r.Kind {
		case v1alpha1.KindRoute:
			c.route(r)
		case v1alpha1.KindGateway:
			c.gateway(r)
		case v1alpha1.KindUpstream:
			c.upstream(r)
		case v1alpha1.KindAIProvider:
			c.aiProvider(r)
		case v1alpha1.KindPlugin:
			c.plugin(r)
		case v1alpha1.KindPolicy:
			c.policy(r)
		default:
		}
	}
	c.pools()
	Sort(c.out)
	return c.out
}

// checker collects the findings of one Check.
type checker struct {
	v     *hub.Validated
	files *tree.FileTable
	idx   *schemaidx.Index
	out   []Finding
}

// add records a finding of rule about r at path, located at the path's
// node or, when the node is missing or a default, at its nearest authored
// ancestor; an empty path is the resource's start. Its severity is the
// rule's.
func (c *checker) add(rule string, r *hub.Resource, path diag.Path, msg string) {
	c.out = append(c.out, Finding{
		Rule: rule, Severity: severityOf(rule), Location: c.locate(r, path),
		Resource: r.ResourceID(), Path: path, Message: msg,
	})
}

// severityOf returns the severity of rule in Rules.
func severityOf(rule string) diag.Severity {
	for _, ri := range Rules() {
		if ri.Name == rule {
			return ri.Severity
		}
	}
	return diag.SeverityError
}

// locate returns the location of the node at path in r's tree: the node's
// own position, or for a missing node or one materialized from a default,
// the position of its nearest authored ancestor; without one, and for an
// empty path, the resource's start.
func (c *checker) locate(r *hub.Resource, path diag.Path) diag.Location {
	if len(path) == 0 || r.Tree == nil {
		return r.Source.Start
	}
	var best tree.Pos
	cur := r.Tree
	defaulted := cur.Style == tree.StyleDefaulted
	if !defaulted && cur.Pos.Known() {
		best = cur.Pos
	}
	for _, e := range path {
		next, ok := cur.At(diag.Path{e})
		if !ok {
			break
		}
		cur = next
		defaulted = defaulted || cur.Style == tree.StyleDefaulted
		if !defaulted && cur.Pos.Known() {
			best = cur.Pos
		}
	}
	if !best.Known() || c.files == nil {
		return r.Source.Start
	}
	if loc := c.files.Location(best); loc.File != "" {
		return loc
	}
	return r.Source.Start
}

// route reports a Route whose effective chain runs no Policy of Filter
// class auth in its client leg.
func (c *checker) route(r *hub.Resource) {
	chain := c.v.Chains[r.Name]
	if chain != nil {
		for _, entries := range chain.Client {
			for _, e := range entries {
				if e.Class == phase.ClassAuth {
					return
				}
			}
		}
	}
	c.add(RuleRouteWithoutAuth, r, nil, "Route "+strconv.Quote(r.Name)+" runs no authentication Policy")
}

// spec returns the path spec followed by elems.
func spec(elems ...diag.PathElem) diag.Path {
	return append(diag.Path{diag.Field("spec")}, elems...)
}

// gateway reports the Gateway's cleartext listeners and a cleartext OTLP
// endpoint.
func (c *checker) gateway(r *hub.Resource) {
	g, ok := hub.Object[v1alpha1.Gateway](r)
	if !ok {
		return
	}
	for _, l := range g.Spec.Listeners {
		if l.Protocol == v1alpha1.ListenerProtocolHTTP {
			c.add(RuleCleartextHop, r, spec(diag.Field("listeners"), diag.Keyed("name", l.Name), diag.Field("protocol")),
				"listener "+strconv.Quote(l.Name)+" serves cleartext HTTP")
		}
	}
	if t := g.Spec.Telemetry; t != nil && t.OTLP != nil && cleartextURL(t.OTLP.Endpoint) {
		c.add(RuleCleartextHop, r, spec(diag.Field("telemetry"), diag.Field("otlp"), diag.Field("endpoint")),
			"the OTLP endpoint is cleartext http")
	}
}

// cleartextURL reports whether s is a URL whose scheme is http.
func cleartextURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && strings.EqualFold(u.Scheme, "http")
}

// upstream reports an Upstream that sends its traffic without TLS. An ai
// Upstream has no Endpoints of its own: its AIProviders are the hops.
func (c *checker) upstream(r *hub.Resource) {
	u, ok := hub.Object[v1alpha1.Upstream](r)
	if !ok || u.Spec.Protocol == v1alpha1.UpstreamProtocolAI || u.Spec.TLS != nil {
		return
	}
	c.add(RuleCleartextHop, r, spec(diag.Field("protocol")),
		"Upstream "+strconv.Quote(r.Name)+" connects to its Endpoints without TLS")
}

// aiProvider reports an AIProvider whose baseUrl is cleartext http.
func (c *checker) aiProvider(r *hub.Resource) {
	p, ok := hub.Object[v1alpha1.AIProvider](r)
	if !ok || !cleartextURL(p.Spec.BaseURL) {
		return
	}
	c.add(RuleCleartextHop, r, spec(diag.Field("baseUrl")),
		"AIProvider "+strconv.Quote(r.Name)+" calls a cleartext http baseUrl")
}

// highRiskReads are the Capabilities that make log.write and metrics.write
// an exfiltration path, in the order a message names the first of them.
func highRiskReads() []v1alpha1.Capability {
	return []v1alpha1.Capability{
		v1alpha1.CapabilityCredentialsRead, v1alpha1.CapabilityRequestBodyRead, v1alpha1.CapabilityStateWrite,
	}
}

// enforcingCapabilities are the Capabilities that make a Plugin enforce:
// it can answer or rewrite traffic.
func enforcingCapabilities() []v1alpha1.Capability {
	return []v1alpha1.Capability{
		v1alpha1.CapabilityResponseSend, v1alpha1.CapabilityRequestBodyWrite,
		v1alpha1.CapabilityResponseBodyWrite, v1alpha1.CapabilityStreamWrite,
	}
}

// stateCapabilities are the Capabilities of a state.* pool.
func stateCapabilities() []v1alpha1.Capability {
	return []v1alpha1.Capability{v1alpha1.CapabilityStateRead, v1alpha1.CapabilityStateWrite}
}

// grantsAny reports whether p grants any of caps.
func grantsAny(p *v1alpha1.Plugin, caps []v1alpha1.Capability) bool {
	return slices.ContainsFunc(caps, func(c v1alpha1.Capability) bool { return slices.Contains(p.Spec.Capabilities, c) })
}

// plugin reports the Capability findings of a Plugin, one per granted
// Capability the Deny-by-default table flags.
func (c *checker) plugin(r *hub.Resource) {
	p, ok := hub.Object[v1alpha1.Plugin](r)
	if !ok {
		return
	}
	name := strconv.Quote(r.Name)
	for _, capability := range p.Spec.Capabilities {
		at := spec(diag.Field("capabilities"), diag.Item(string(capability)))
		grant := "Plugin " + name + " grants " + string(capability)
		switch capability {
		case v1alpha1.CapabilityCredentialsRead, v1alpha1.CapabilityRequestBodyRead:
			c.add(RulePluginCredentialExposure, r, at, grant+": credential exposure")
		case v1alpha1.CapabilityResponseBodyRead, v1alpha1.CapabilityStreamRead:
			c.add(RulePluginPayloadExposure, r, at, grant+": payload exposure")
		case v1alpha1.CapabilityResponseSend:
			c.add(RulePluginCanDenyTraffic, r, at, grant+": the Plugin can deny traffic")
		case v1alpha1.CapabilityStateRead, v1alpha1.CapabilityStateWrite:
			c.add(RulePluginStateRoundTrip, r, at, grant+": each call adds a State Store round trip")
		case v1alpha1.CapabilityLogWrite, v1alpha1.CapabilityMetricsWrite:
			for _, risk := range highRiskReads() {
				if slices.Contains(p.Spec.Capabilities, risk) {
					c.add(RulePluginExfiltrationPath, r, at, grant+" with "+string(risk)+": an exfiltration path")
					break
				}
			}
		default:
		}
	}
}

// policy reports an enforcing plugin Policy whose failureMode is open.
func (c *checker) policy(r *hub.Resource) {
	pol, ok := hub.Object[v1alpha1.Policy](r)
	if !ok || pol.Spec.Type != v1alpha1.PolicyTypePlugin || pol.Spec.FailureMode == nil ||
		*pol.Spec.FailureMode != v1alpha1.FailureModeOpen {
		return
	}
	p, ok := c.v.Bundle.Plugin(pol.Spec.Plugin)
	if !ok || !grantsAny(p, enforcingCapabilities()) {
		return
	}
	c.add(RulePluginEnforcingOpen, r, spec(diag.Field("failureMode")),
		"Policy "+strconv.Quote(r.Name)+" runs enforcing Plugin "+strconv.Quote(pol.Spec.Plugin)+
			" with failureMode open, which a fault breaker trip bypasses")
}

// poolKey identifies an instance pool: Policies with an equal Plugin
// digest, canonical config and limits share one.
type poolKey struct {
	digest, config, timeout string
	memory                  int64
}

// pools reports the pool sizing findings: each Plugin of a state.* pool
// below 4 instances or below a guaranteed parked bound of 16, and warm
// reservations above half of the Node cap.
func (c *checker) pools() {
	gw := c.gatewayResource()
	if gw == nil {
		return
	}
	nodeCap := c.nodeCap(gw)
	pools := map[poolKey]bool{}
	var statePlugins []string
	for _, name := range c.attachedPluginPolicies() {
		pol, ok := c.v.Bundle.Policy(name)
		if !ok {
			continue
		}
		pr, ok := c.v.Bundle.Get(hub.ID{Kind: v1alpha1.KindPlugin, Name: pol.Spec.Plugin})
		if !ok {
			continue
		}
		p, ok := hub.Object[v1alpha1.Plugin](pr)
		if !ok {
			continue
		}
		memory, timeout := c.pluginLimits(p)
		pools[poolKey{digest: digestOf(p.Spec.Image), config: c.canonicalConfig(name), timeout: timeout, memory: memory}] = true
		if grantsAny(p, stateCapabilities()) && !slices.Contains(statePlugins, pr.Name) {
			statePlugins = append(statePlugins, pr.Name)
		}
	}
	slices.Sort(statePlugins)
	for _, name := range statePlugins {
		pr, _ := c.v.Bundle.Get(hub.ID{Kind: v1alpha1.KindPlugin, Name: name})
		p, _ := hub.Object[v1alpha1.Plugin](pr)
		memory, _ := c.pluginLimits(p)
		if memory <= 0 {
			continue
		}
		// floor(floor(cap/2)/memory) is floor(cap/(2 memory)) and cannot
		// overflow.
		n := nodeCap / 2 / memory
		at := spec(diag.Field("limits"), diag.Field("memoryBytes"))
		quoted := strconv.Quote(name)
		if n < minStatePool {
			c.add(RulePluginPoolSmall, pr, at, "Plugin "+quoted+" gets a state.* pool of "+
				strconv.FormatInt(n, 10)+" instances, below "+strconv.Itoa(minStatePool))
			continue
		}
		if parked := n - (n+3)/4; parked < minParkedBound {
			c.add(RulePluginParkedBoundLow, pr, at, "Plugin "+quoted+" gets a guaranteed parked bound of "+
				strconv.FormatInt(parked, 10)+", below "+strconv.Itoa(minParkedBound))
		}
	}
	var warm int64
	for k := range pools {
		warm = saturatingAdd(warm, k.memory)
	}
	// Strictly above half of the cap: for integers, warm > floor(cap/2)
	// exactly when warm > cap/2.
	if warm > nodeCap/2 {
		c.add(RulePluginWarmReservation, gw, spec(diag.Field("limits"), diag.Field("maxPluginMemoryBytes")),
			"warm Plugin reservations of "+strconv.FormatInt(warm, 10)+" bytes exceed half of the "+
				strconv.FormatInt(nodeCap, 10)+"-byte Node cap")
	}
}

// saturatingAdd returns a + b, or math.MaxInt64 when the sum overflows; a
// and b are not negative.
func saturatingAdd(a, b int64) int64 {
	if b > math.MaxInt64-a {
		return math.MaxInt64
	}
	return a + b
}

// gatewayResource returns the Bundle's Gateway resource, or nil.
func (c *checker) gatewayResource() *hub.Resource {
	for _, r := range c.v.Bundle.Resources() {
		if r.Kind == v1alpha1.KindGateway {
			return r
		}
	}
	return nil
}

// attachedPluginPolicies returns, sorted, the plugin Policies some Route's
// effective chain runs, in its client leg or any upstream leg.
func (c *checker) attachedPluginPolicies() []string {
	var out []string
	add := func(entries []hub.Entry) {
		for _, e := range entries {
			if e.Type == v1alpha1.PolicyTypePlugin && !slices.Contains(out, e.Policy) {
				out = append(out, e.Policy)
			}
		}
	}
	for _, chain := range c.v.Chains {
		if chain == nil {
			continue
		}
		for _, entries := range chain.Client {
			add(entries)
		}
		for _, leg := range chain.Legs {
			for _, entries := range leg.Phases {
				add(entries)
			}
		}
	}
	slices.Sort(out)
	return out
}

// digestOf returns the digest of an image reference, the part after its
// "@"; an image without one is the whole reference.
func digestOf(image string) string {
	if _, d, ok := strings.Cut(image, "@"); ok {
		return d
	}
	return image
}

// canonicalConfig returns the RFC 8785 form of the config of the Policy
// named name, or "" when it has none or it cannot be encoded.
func (c *checker) canonicalConfig(name string) string {
	r, ok := c.v.Bundle.Get(hub.ID{Kind: v1alpha1.KindPolicy, Name: name})
	if !ok || r.Tree == nil {
		return ""
	}
	n, ok := r.Tree.At(spec(diag.Field("config")))
	if !ok {
		return ""
	}
	b, err := jsonval.AppendCanonical(nil, n.JSONValue())
	if err != nil {
		return ""
	}
	return string(b)
}

// nodeCap returns the Gateway's limits.maxPluginMemoryBytes, or its schema
// default when the Gateway sets no limits.
func (c *checker) nodeCap(gw *hub.Resource) int64 {
	if g, ok := hub.Object[v1alpha1.Gateway](gw); ok && g.Spec.Limits != nil && g.Spec.Limits.MaxPluginMemoryBytes != nil {
		return int64(*g.Spec.Limits.MaxPluginMemoryBytes)
	}
	return c.defaultBytes(v1alpha1.KindGateway, spec(diag.Field("limits"), diag.Field("maxPluginMemoryBytes")))
}

// pluginLimits returns p's limits.memoryBytes and limits.timeout, each its
// schema default when p leaves it out; the timeout is in
// time.Duration.String form.
func (c *checker) pluginLimits(p *v1alpha1.Plugin) (memory int64, timeout string) {
	memory = -1
	if l := p.Spec.Limits; l != nil {
		if l.MemoryBytes != nil {
			memory = int64(*l.MemoryBytes)
		}
		if l.Timeout != nil {
			timeout = time.Duration(*l.Timeout).String()
		}
	}
	if memory < 0 {
		memory = c.defaultBytes(v1alpha1.KindPlugin, spec(diag.Field("limits"), diag.Field("memoryBytes")))
	}
	if timeout == "" {
		timeout = c.defaultDuration(v1alpha1.KindPlugin, spec(diag.Field("limits"), diag.Field("timeout")))
	}
	return memory, timeout
}

// defaultValue returns the schema default at path of kind.
func (c *checker) defaultValue(kind v1alpha1.Kind, path diag.Path) (any, bool) {
	if c.idx == nil {
		return nil, false
	}
	info, ok := c.idx.Lookup(string(kind), nil, path)
	if !ok || !info.Known() {
		return nil, false
	}
	return info.Node.Default()
}

// defaultBytes returns the byte size default at path of kind, an integer
// or a quantity; 0 without one.
func (c *checker) defaultBytes(kind v1alpha1.Kind, path diag.Path) int64 {
	v, ok := c.defaultValue(kind, path)
	if !ok {
		return 0
	}
	switch t := v.(type) {
	case json.Number:
		if n, err := strconv.ParseInt(string(t), 10, 64); err == nil {
			return n
		}
	case string:
		if b, err := v1alpha1.ParseByteSize(t); err == nil {
			return int64(b)
		}
	default:
	}
	return 0
}

// defaultDuration returns the duration default at path of kind in
// time.Duration.String form; "" without one.
func (c *checker) defaultDuration(kind v1alpha1.Kind, path diag.Path) string {
	v, ok := c.defaultValue(kind, path)
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return s
	}
	return d.String()
}
