// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package openapi

import (
	"bytes"
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/profile"
	"github.com/ravindu-rev/ruralz/internal/config/render"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/routematch"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// The Gateway of an imported Bundle: one cleartext listener on the
// client port 8080 (docs/_meta/foundation-pack.md), named edge as in the
// example Bundles.
const (
	gatewayName  = "edge"
	listenerName = "http"
	listenerPort = 8080
)

// maxName is the longest metadata.name, an RFC 1123 label.
const maxName = 63

// Bundle is the Bundle Import built from a document.
type Bundle struct {
	// Resources are the Gateway, the Upstreams and the Routes, each kind
	// in name order: the canonical order of a rendered Bundle.
	Resources []Resource
	// Warnings say what the import left out and why, in document order.
	Warnings []Problem
	// Operations counts the operations of the document's paths.
	Operations int
	// Routes counts the operations imported, one Route each.
	Routes int
}

// Resource is one resource of an imported Bundle.
type Resource struct {
	// Kind is Gateway, Upstream or Route.
	Kind v1alpha1.Kind
	// Name is its metadata.name.
	Name string
	// Doc is the resource's document.
	Doc *tree.Node
}

// Import maps doc to a Bundle. Each server an operation uses, the first
// of its operation's, its path item's or the document's servers list,
// whichever is set first, that has an absolute http or https URL once its
// variables take their defaults, is one Upstream: its host and port as the
// one Endpoint, and an empty tls block for https. Each operation is one
// Route matching its method and its server's path joined with the
// operation's path: an exact path without parameters, a template when
// every parameter fills a whole segment, a regex when one shares a segment
// with text. An operation without such a server, whose path the Router
// cannot match, or whose criteria an earlier Route already has is left out
// with a warning, and so are the webhooks. Import never fails.
func Import(doc *Document) *Bundle {
	im := &importer{
		doc: doc, upstreams: map[string]string{}, criteria: map[routematch.Key]string{},
		names: map[v1alpha1.Kind]map[string]bool{}, warned: map[diag.Location]bool{},
	}
	gw := resourceDoc(v1alpha1.KindGateway, gatewayName, mapNode(
		"listeners", listNode(mapNode("name", strNode(listenerName), "protocol", strNode("http"), "port", intNode(listenerPort))),
	))
	im.b.Resources = append(im.b.Resources, Resource{Kind: v1alpha1.KindGateway, Name: gatewayName, Doc: gw})
	for _, p := range doc.Paths {
		for _, op := range p.Item.Operations {
			im.b.Operations++
			im.operation(p, op)
		}
	}
	if len(doc.Webhooks) > 0 {
		im.warn(doc.Webhooks[0].At, "webhooks are not imported: they are requests the API sends, not requests a Route receives")
	}
	slices.SortStableFunc(im.b.Resources, func(a, b Resource) int {
		return cmp.Or(cmp.Compare(kindOrder(a.Kind), kindOrder(b.Kind)), strings.Compare(a.Name, b.Name))
	})
	return &im.b
}

// kindOrder orders the kinds of an imported Bundle canonically.
func kindOrder(k v1alpha1.Kind) int {
	switch k {
	case v1alpha1.KindGateway:
		return 0
	case v1alpha1.KindUpstream:
		return 1
	default:
		return 2
	}
}

// importer is the state of one Import.
type importer struct {
	doc *Document
	b   Bundle
	// upstreams maps an endpoint key to its Upstream's name.
	upstreams map[string]string
	// criteria maps the match criteria of each Route to its name.
	criteria map[routematch.Key]string
	// names holds the names taken, per kind.
	names map[v1alpha1.Kind]map[string]bool
	// warned holds the servers lists warned about.
	warned map[diag.Location]bool
}

// warn records a warning at at.
func (im *importer) warn(at diag.Location, format string, args ...any) {
	im.b.Warnings = append(im.b.Warnings, Problem{Location: at, Message: fmt.Sprintf(format, args...)})
}

// operation maps one operation to a Route.
func (im *importer) operation(p Path, op *Operation) {
	method := strings.ToUpper(op.Method)
	what := method + " " + p.Key
	servers := op.Servers
	if len(servers) == 0 {
		servers = p.Item.Servers
	}
	if len(servers) == 0 {
		servers = im.doc.Servers
	}
	ep, why := im.server(servers)
	if why != "" {
		im.warn(op.At, "%s is not imported: %s", what, why)
		return
	}
	match, why := routePath(ep.base + p.Key)
	if why != "" {
		im.warn(op.At, "%s is not imported: %s", what, why)
		return
	}
	key := routematch.MatchKey(&routematch.Criteria{Path: &routematch.PathCriteria{
		Exact: match.Exact, Template: match.Template, Regex: match.Regex,
	}, Methods: []string{method}})
	if prev, dup := im.criteria[key]; dup {
		im.warn(op.At, "%s is not imported: Route %s already matches the same requests", what, quote(prev))
		return
	}
	upstream := im.upstream(ep)
	name := im.routeName(op, p.Key)
	im.criteria[key] = name
	var path *tree.Node
	switch {
	case match.Exact != "":
		path = mapNode("exact", strNode(match.Exact))
	case match.Template != "":
		path = mapNode("template", strNode(match.Template))
	default:
		path = mapNode("regex", strNode(match.Regex))
	}
	spec := mapNode(
		"match", mapNode("path", path, "methods", listNode(strNode(method))),
		"upstreams", listNode(mapNode("name", strNode(upstream))),
	)
	im.b.Resources = append(im.b.Resources, Resource{Kind: v1alpha1.KindRoute, Name: name, Doc: resourceDoc(v1alpha1.KindRoute, name, spec)})
	im.b.Routes++
}

// server returns the endpoint of the first server of list with an
// absolute http or https URL, warning once per list that holds other
// servers, or the reason none is usable: the first server's.
func (im *importer) server(list []Server) (endpoint, string) {
	if len(list) == 0 {
		return endpoint{}, "the document names no server, so no Upstream address"
	}
	first := ""
	for _, s := range list {
		ep, why := resolveServer(s)
		if why != "" {
			if first == "" {
				first = why
			}
			continue
		}
		if len(list) > 1 && !im.warned[list[0].At] {
			im.warned[list[0].At] = true
			im.warn(list[0].At, "only server %s of this servers list is imported; %d more are not", quote(s.URL), len(list)-1)
		}
		return ep, ""
	}
	return endpoint{}, first
}

// upstream returns the name of the Upstream of ep, adding it on first
// use: its host as an RFC 1123 label, then its port unless it is the
// scheme's default.
func (im *importer) upstream(ep endpoint) string {
	if name, ok := im.upstreams[ep.key()]; ok {
		return name
	}
	base := ep.host
	if ep.port != defaultPort(ep.scheme) {
		base += "-" + ep.port
	}
	name := im.unique(v1alpha1.KindUpstream, label(base), "upstream")
	im.upstreams[ep.key()] = name
	spec := mapNode("protocol", strNode("http"), "endpoints", listNode(mapNode("address", strNode(ep.address()))))
	if ep.scheme == "https" {
		spec.Set("tls", tree.Pos{}, mapNode())
	}
	im.b.Resources = append(im.b.Resources, Resource{Kind: v1alpha1.KindUpstream, Name: name, Doc: resourceDoc(v1alpha1.KindUpstream, name, spec)})
	return name
}

// routeName returns the name of an operation's Route: its operationId as
// an RFC 1123 label, else its method and path, made unique.
func (im *importer) routeName(op *Operation, path string) string {
	base := label(op.OperationID)
	if base == "" {
		base = label(op.Method + " " + path)
	}
	return im.unique(v1alpha1.KindRoute, base, "route")
}

// unique returns base, or base with -2, -3 and so on when a resource of
// kind already holds it, within maxName characters; an empty base is
// fallback.
func (im *importer) unique(kind v1alpha1.Kind, base, fallback string) string {
	if base == "" {
		base = fallback
	}
	taken := im.names[kind]
	if taken == nil {
		taken = map[string]bool{}
		im.names[kind] = taken
	}
	name := base
	for n := 2; taken[name]; n++ {
		suffix := "-" + strconv.Itoa(n)
		name = strings.TrimRight(base[:min(len(base), maxName-len(suffix))], "-") + suffix
	}
	taken[name] = true
	return name
}

// label returns s as an RFC 1123 label of at most maxName characters:
// lower-case letters and digits, a hyphen where s has other characters or
// a lower-case letter or digit meets an upper-case one (getPetById is
// get-pet-by-id, HTTPServer http-server), no hyphen at either end.
func label(s string) string {
	rs := []rune(s)
	var b strings.Builder
	dash := false
	for i, r := range rs {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r >= 'A' && r <= 'Z':
			if i > 0 && b.Len() > 0 {
				p := rs[i-1]
				lowerOrDigit := p >= 'a' && p <= 'z' || p >= '0' && p <= '9'
				acronymEnd := p >= 'A' && p <= 'Z' && i+1 < len(rs) && rs[i+1] >= 'a' && rs[i+1] <= 'z'
				if lowerOrDigit || acronymEnd {
					dash = true
				}
			}
			r += 'a' - 'A'
		default:
			dash = b.Len() > 0
			continue
		}
		if dash {
			b.WriteByte('-')
			dash = false
		}
		// r is an ASCII letter or digit, so the label holds one byte per
		// rune.
		b.WriteRune(r)
	}
	out := b.String()
	if len(out) > maxName {
		out = out[:maxName]
	}
	return strings.TrimRight(out, "-")
}

// routePath returns the Route path criterion of the request path p, or why
// the Router cannot match it: p as an exact path when it holds no
// parameter; a template when every parameter fills a whole segment, each
// name made [A-Za-z_][A-Za-z0-9_]* (a character outside it becomes _, a
// leading digit gets _ before it, and a repeated name takes _2, _3); else
// an anchored regex in which each parameter matches one or more
// characters other than /.
func routePath(p string) (v1alpha1.PathMatch, string) {
	segs := strings.Split(strings.TrimPrefix(p, "/"), "/")
	parts := make([][]segPart, len(segs))
	params, partial := false, false
	for i, s := range segs {
		ps, ok := segmentParts(s)
		if !ok {
			return v1alpha1.PathMatch{}, "the path " + quote(p) + " has an unbalanced { or } or an empty {}"
		}
		parts[i] = ps
		for _, x := range ps {
			params = params || x.param
		}
		partial = partial || len(ps) > 1 && slices.ContainsFunc(ps, func(x segPart) bool { return x.param })
	}
	switch {
	case !params:
		if err := routematch.CheckPath(p); err != nil {
			return v1alpha1.PathMatch{}, "the Router cannot match the path " + quote(p) + ": " + err.Error()
		}
		return v1alpha1.PathMatch{Exact: p}, ""
	case !partial:
		if t, ok := templateOf(parts); ok {
			return v1alpha1.PathMatch{Template: t}, ""
		}
	}
	re, why := regexOf(parts)
	if why != "" {
		return v1alpha1.PathMatch{}, "the Router cannot match the path " + quote(p) + ": " + why
	}
	return v1alpha1.PathMatch{Regex: re}, ""
}

// segPart is a piece of a path segment: literal text or a parameter.
type segPart struct {
	text  string
	param bool
}

// segmentParts splits a path segment into literal text and {name}
// parameters; false for an unbalanced brace or an empty name.
func segmentParts(s string) ([]segPart, bool) {
	var out []segPart
	for s != "" {
		i := strings.IndexAny(s, "{}")
		switch {
		case i < 0:
			return append(out, segPart{text: s}), true
		case s[i] == '}':
			return nil, false
		case i > 0:
			out = append(out, segPart{text: s[:i]})
		}
		end := strings.IndexAny(s[i+1:], "{}")
		if end <= 0 || s[i+1+end] == '{' {
			return nil, false
		}
		out = append(out, segPart{text: s[i+1 : i+1+end], param: true})
		s = s[i+1+end+1:]
	}
	if len(out) == 0 {
		out = append(out, segPart{})
	}
	return out, true
}

// templateOf returns the Route template of segments whose parameters
// each fill a segment, or false when the Router would refuse it, as for
// an empty or dot segment.
func templateOf(parts [][]segPart) (string, bool) {
	var b strings.Builder
	used := map[string]bool{}
	for _, ps := range parts {
		b.WriteByte('/')
		if ps[0].param {
			b.WriteString("{" + paramName(ps[0].text, used) + "}")
		} else {
			b.WriteString(ps[0].text)
		}
	}
	t := b.String()
	if routematch.CheckTemplate(t) != nil {
		return "", false
	}
	return t, true
}

// paramName returns the template name of an OpenAPI parameter name, not
// yet in used, and records it.
func paramName(s string, used map[string]bool) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			if b.Len() == 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	base := b.String()
	name := base
	for n := 2; used[name]; n++ {
		name = base + "_" + strconv.Itoa(n)
	}
	used[name] = true
	return name
}

// regexOf returns the anchored regex of segments: each literal piece in
// its normalized form, quoted, and each parameter one or more characters
// other than /. The Router anchors a path regex itself.
func regexOf(parts [][]segPart) (string, string) {
	var b strings.Builder
	for _, ps := range parts {
		b.WriteByte('/')
		for _, x := range ps {
			if x.param {
				b.WriteString("[^/]+")
				continue
			}
			if x.text == "" {
				continue
			}
			// The Router matches the normalized path, so a piece is
			// matched in its normalized form, which must stay one piece of
			// one segment: no dot segment, no escape it refuses.
			n, err := routematch.NormalizePath("/" + x.text)
			if err != nil || n == "/" || strings.Contains(n[1:], "/") {
				return "", "the piece " + quote(x.text) + " is not a valid path segment"
			}
			b.WriteString(regexp.QuoteMeta(n[1:]))
		}
	}
	re := b.String()
	if _, err := routematch.CompileRegex(re); err != nil {
		return "", err.Error()
	}
	return re, ""
}

// Files returns the Bundle as a directory: ruralz.yaml holding the
// Gateway, and one file per Upstream under upstreams/ and per Route under
// routes/, each named after the resource.
func (b *Bundle) Files() ([]render.File, error) {
	out := make([]render.File, 0, len(b.Resources))
	for _, r := range b.Resources {
		var buf bytes.Buffer
		if err := profile.Encode(&buf, []*tree.Node{r.Doc}); err != nil {
			return nil, fmt.Errorf("openapi: encode %s/%s: %w", r.Kind, r.Name, err)
		}
		var p string
		switch r.Kind {
		case v1alpha1.KindGateway:
			p = "ruralz.yaml"
		case v1alpha1.KindUpstream:
			p = "upstreams/" + r.Name + ".yaml"
		case v1alpha1.KindRoute:
			p = "routes/" + r.Name + ".yaml"
		default:
			return nil, fmt.Errorf("openapi: an imported Bundle holds no %s", r.Kind)
		}
		out = append(out, render.File{Path: p, Content: buf.Bytes()})
	}
	return out, nil
}

// Stream returns the Bundle as a one-file Bundle: one YAML stream of its
// resources in order.
func (b *Bundle) Stream() ([]byte, error) {
	docs := make([]*tree.Node, 0, len(b.Resources))
	for _, r := range b.Resources {
		docs = append(docs, r.Doc)
	}
	var buf bytes.Buffer
	if err := profile.Encode(&buf, docs); err != nil {
		return nil, fmt.Errorf("openapi: encode the Bundle: %w", err)
	}
	return buf.Bytes(), nil
}

// resourceDoc returns the document of a resource: apiVersion, kind,
// metadata.name and spec.
func resourceDoc(kind v1alpha1.Kind, name string, spec *tree.Node) *tree.Node {
	return mapNode(
		"apiVersion", strNode(v1alpha1.APIVersion),
		"kind", strNode(string(kind)),
		"metadata", mapNode("name", strNode(name)),
		"spec", spec,
	)
}

// mapNode returns a mapping of the key and value pairs kv.
func mapNode(kv ...any) *tree.Node {
	n := &tree.Node{Kind: tree.KindMap, Members: []tree.Member{}}
	for i := 0; i+1 < len(kv); i += 2 {
		k, _ := kv[i].(string)
		v, _ := kv[i+1].(*tree.Node)
		n.Members = append(n.Members, tree.Member{Key: k, Value: v})
	}
	return n
}

// strNode returns a string node.
func strNode(s string) *tree.Node { return &tree.Node{Kind: tree.KindString, Text: s} }

// intNode returns an integer node.
func intNode(i int) *tree.Node { return &tree.Node{Kind: tree.KindInt, Text: strconv.Itoa(i)} }

// listNode returns a list of items.
func listNode(items ...*tree.Node) *tree.Node {
	return &tree.Node{Kind: tree.KindList, Items: items}
}
