// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package openapi

import (
	"context"
	"errors"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// Load reads the OpenAPI 3.0 or 3.1 document at path, an operating system
// path, with every file its references name, and builds its model. A root
// document that cannot be read is a *ReadError; a document that is not a
// readable OpenAPI 3.0 or 3.1 document is an *Error holding every problem
// found, in file and position order; ctx's end is ctx.Err().
func Load(ctx context.Context, path string, o Options) (*Document, error) {
	l := newLoad(ctx, o)
	doc, err := l.document(filepath.Clean(path))
	switch {
	case errors.Is(err, errStop):
		return nil, &Error{Problems: l.sortedProblems()}
	case err != nil:
		return nil, err
	case len(l.problems) > 0:
		return nil, &Error{Problems: l.sortedProblems()}
	}
	for _, f := range l.byID {
		doc.Files = append(doc.Files, displayName(f.path))
	}
	return doc, nil
}

// versionPattern matches the openapi member of a 3.0 or 3.1 document; the
// first group is the minor version.
func versionPattern() *regexp.Regexp {
	return regexp.MustCompile(`^3\.([01])\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$`)
}

// document reads the root file at path and decodes it.
func (l *load) document(path string) (*Document, error) {
	f, err := l.open(path, diag.Location{}, true)
	if err != nil {
		return nil, err
	}
	if f.root == nil {
		return nil, errStop
	}
	root := f.root
	doc := &Document{At: l.at(root)}
	if !l.version(root, doc) {
		return nil, errStop
	}
	if n, ok := root.Get("info"); ok && l.isMapping(n, "info") {
		doc.Info = Info{Title: scalarText(member(n, "title")), Version: scalarText(member(n, "version")), At: l.at(n)}
	}
	doc.Servers = l.servers(root)
	paths, hasPaths := root.Get("paths")
	webhooks, hasWebhooks := root.Get("webhooks")
	components, hasComponents := root.Get("components")
	switch {
	case doc.Minor == 0 && !hasPaths:
		l.add(l.at(root), "an OpenAPI 3.0 document needs paths")
	case doc.Minor == 1 && !hasPaths && !hasWebhooks && !hasComponents:
		l.add(l.at(root), "an OpenAPI 3.1 document needs paths, webhooks or components")
	}
	if hasComponents && l.isMapping(components, "components") {
		doc.Components = components
	}
	if hasPaths && l.isMapping(paths, "paths") {
		if doc.Paths, err = l.paths(paths, true); err != nil {
			return nil, err
		}
	}
	if doc.Minor == 1 && hasWebhooks && l.isMapping(webhooks, "webhooks") {
		if doc.Webhooks, err = l.paths(webhooks, false); err != nil {
			return nil, err
		}
	}
	return doc, nil
}

// version checks the openapi member and records the version; false means
// the document is no OpenAPI 3.0 or 3.1 document, whose other members
// mean nothing here.
func (l *load) version(root *tree.Node, doc *Document) bool {
	v, ok := root.Get("openapi")
	if !ok {
		if _, swagger := root.Get("swagger"); swagger {
			l.add(l.at(root), "a Swagger 2.0 document; ruralz bundle import openapi reads OpenAPI 3.0 and 3.1")
		} else {
			l.add(l.at(root), "the document has no openapi member: it is no OpenAPI 3.0 or 3.1 document")
		}
		return false
	}
	if v.Kind != tree.KindString {
		l.add(l.at(v), "openapi is a %s, not a version string such as \"3.1.0\"", kindName(v))
		return false
	}
	m := versionPattern().FindStringSubmatch(v.Text)
	if m == nil {
		l.add(l.at(v), "openapi %s is not OpenAPI 3.0 or 3.1", quote(v.Text))
		return false
	}
	doc.Version, doc.Minor = v.Text, int(m[1][0]-'0')
	return true
}

// paths decodes a paths object, or with pathKeys false a webhooks object:
// each member's Path Item Object, its $ref resolved. Extensions (x-) are
// left out; a paths member must start with /.
func (l *load) paths(n *tree.Node, pathKeys bool) ([]Path, error) {
	var out []Path
	for _, m := range n.Members {
		if strings.HasPrefix(m.Key, "x-") {
			continue
		}
		at := l.files.Location(m.KeyPos)
		if pathKeys && !strings.HasPrefix(m.Key, "/") {
			l.add(at, "paths member %s does not begin with /", quote(m.Key))
			continue
		}
		v, err := l.deref(m.Value)
		if err != nil {
			return nil, err
		}
		if v == nil || !l.isMapping(v, "path item "+quote(m.Key)) {
			continue
		}
		item, err := l.pathItem(v)
		if err != nil {
			return nil, err
		}
		out = append(out, Path{Key: m.Key, Item: item, At: at})
		if l.full() {
			return nil, errStop
		}
	}
	return out, nil
}

// pathItem decodes a Path Item Object.
func (l *load) pathItem(n *tree.Node) (*PathItem, error) {
	item := &PathItem{
		At: l.at(n), Summary: l.str(n, "summary"), Description: l.str(n, "description"), Servers: l.servers(n),
	}
	var err error
	if item.Parameters, err = l.parameters(n); err != nil {
		return nil, err
	}
	for _, m := range n.Members {
		if !slices.Contains(Methods(), m.Key) {
			continue
		}
		if !l.isMapping(m.Value, m.Key+" operation") {
			continue
		}
		op, err := l.operation(m.Key, m.Value, l.files.Location(m.KeyPos))
		if err != nil {
			return nil, err
		}
		item.Operations = append(item.Operations, op)
	}
	return item, nil
}

// operation decodes the Operation Object n written under method.
func (l *load) operation(method string, n *tree.Node, at diag.Location) (*Operation, error) {
	op := &Operation{
		Method: method, At: at, OperationID: l.str(n, "operationId"),
		Summary: l.str(n, "summary"), Description: l.str(n, "description"),
		Deprecated: l.boolean(n, "deprecated"),
	}
	if t, ok := n.Get("tags"); ok && l.isList(t, "tags") {
		for _, it := range t.Items {
			if it == nil || it.Kind != tree.KindString {
				l.add(l.at(it), "a tags entry is a %s, not a string", kindName(it))
				continue
			}
			op.Tags = append(op.Tags, it.Text)
		}
	}
	var err error
	if op.Parameters, err = l.parameters(n); err != nil {
		return nil, err
	}
	op.Servers = l.servers(n)
	if b, ok := n.Get("requestBody"); ok {
		if op.RequestBody, err = l.requestBody(b); err != nil {
			return nil, err
		}
	}
	if r, ok := n.Get("responses"); ok && l.isMapping(r, "responses") {
		op.Responses = r
	}
	if s, ok := n.Get("security"); ok && l.isList(s, "security") {
		op.Security = s
	}
	if c, ok := n.Get("callbacks"); ok && l.isMapping(c, "callbacks") {
		op.Callbacks = c
	}
	return op, nil
}

// parameters decodes the parameters member of n, each entry a Parameter
// Object or a Reference Object that leads to one.
func (l *load) parameters(n *tree.Node) ([]*Parameter, error) {
	list, ok := n.Get("parameters")
	if !ok || !l.isList(list, "parameters") {
		return nil, nil
	}
	var out []*Parameter
	for _, it := range list.Items {
		p, err := l.deref(it)
		if err != nil {
			return nil, err
		}
		if p == nil || !l.isMapping(p, "a parameter") {
			continue
		}
		par := &Parameter{At: l.at(p), Required: l.boolean(p, "required"), Deprecated: l.boolean(p, "deprecated")}
		par.Name, par.In = l.requiredStr(p, "name", "a parameter"), l.requiredStr(p, "in", "a parameter")
		switch par.In {
		case "", "query", "header", "path", "cookie":
		default:
			l.add(l.at(member(p, "in")), "parameter in %s is not query, header, path or cookie", quote(par.In))
		}
		par.Schema, par.Content = l.schema(p), l.content(p)
		out = append(out, par)
	}
	return out, nil
}

// requestBody decodes a Request Body Object or a Reference Object that
// leads to one.
func (l *load) requestBody(n *tree.Node) (*RequestBody, error) {
	b, err := l.deref(n)
	if err != nil || b == nil || !l.isMapping(b, "requestBody") {
		return nil, err
	}
	return &RequestBody{
		At: l.at(b), Description: l.str(b, "description"), Required: l.boolean(b, "required"), Content: l.content(b),
	}, nil
}

// content decodes the content member of n: media types in authored order.
func (l *load) content(n *tree.Node) []MediaType {
	c, ok := n.Get("content")
	if !ok || !l.isMapping(c, "content") {
		return nil
	}
	var out []MediaType
	for _, m := range c.Members {
		if !l.isMapping(m.Value, "media type "+quote(m.Key)) {
			continue
		}
		out = append(out, MediaType{Type: m.Key, Schema: l.schema(m.Value), At: l.files.Location(m.KeyPos)})
	}
	return out
}

// schema returns the schema member of n as written: a mapping, or in 3.1
// also a boolean schema.
func (l *load) schema(n *tree.Node) *Schema {
	s, ok := n.Get("schema")
	if !ok {
		return nil
	}
	if s == nil || s.Kind != tree.KindMap && s.Kind != tree.KindBool {
		l.add(l.at(s), "schema is a %s, not a Schema Object", kindName(s))
		return nil
	}
	return &Schema{Node: s, At: l.at(s)}
}

// servers decodes the servers member of n.
func (l *load) servers(n *tree.Node) []Server {
	list, ok := n.Get("servers")
	if !ok || !l.isList(list, "servers") {
		return nil
	}
	var out []Server
	for _, it := range list.Items {
		if !l.isMapping(it, "a servers entry") {
			continue
		}
		url, ok := it.Get("url")
		if !ok {
			l.add(l.at(it), "a servers entry has no url")
			continue
		}
		if url.Kind != tree.KindString {
			l.add(l.at(url), "url is a %s, not a string", kindName(url))
			continue
		}
		s := Server{URL: url.Text, Description: l.str(it, "description"), At: l.at(url)}
		if vars, ok := it.Get("variables"); ok && l.isMapping(vars, "variables") {
			for _, m := range vars.Members {
				if !l.isMapping(m.Value, "server variable "+quote(m.Key)) {
					continue
				}
				v := ServerVariable{Name: m.Key, At: l.files.Location(m.KeyPos)}
				def, ok := m.Value.Get("default")
				if !ok || !isScalar(def) {
					l.add(v.At, "server variable %s has no default", quote(m.Key))
					continue
				}
				v.Default = scalarText(def)
				if e, ok := m.Value.Get("enum"); ok && l.isList(e, "enum") {
					for _, it := range e.Items {
						v.Enum = append(v.Enum, scalarText(it))
					}
				}
				s.Variables = append(s.Variables, v)
			}
		}
		out = append(out, s)
	}
	return out
}

// member returns the value of n's member key, or nil.
func member(n *tree.Node, key string) *tree.Node {
	v, _ := n.Get(key)
	return v
}

// isScalar reports whether n is a string, number or boolean.
func isScalar(n *tree.Node) bool {
	if n == nil {
		return false
	}
	switch n.Kind {
	case tree.KindString, tree.KindInt, tree.KindFloat, tree.KindBool:
		return true
	default:
		return false
	}
}

// scalarText returns the text of a scalar: a string as it is, a number as
// written, a boolean as true or false; "" for anything else.
func scalarText(n *tree.Node) string {
	if !isScalar(n) {
		return ""
	}
	if n.Kind == tree.KindBool {
		return strconv.FormatBool(n.Bool)
	}
	return n.Text
}

// isMapping reports whether n is a mapping, recording a problem naming
// what when it is not.
func (l *load) isMapping(n *tree.Node, what string) bool {
	if n != nil && n.Kind == tree.KindMap {
		return true
	}
	l.add(l.at(n), "%s is a %s, not a mapping", what, kindName(n))
	return false
}

// isList reports whether n is a list, recording a problem naming what
// when it is not.
func (l *load) isList(n *tree.Node, what string) bool {
	if n != nil && n.Kind == tree.KindList {
		return true
	}
	l.add(l.at(n), "%s is a %s, not a list", what, kindName(n))
	return false
}

// str returns n's string member key, "" when absent; another type is a
// problem.
func (l *load) str(n *tree.Node, key string) string {
	v, ok := n.Get(key)
	if !ok {
		return ""
	}
	if v == nil || v.Kind != tree.KindString {
		l.add(l.at(v), "%s is a %s, not a string", key, kindName(v))
		return ""
	}
	return v.Text
}

// requiredStr is str for a member the object what needs.
func (l *load) requiredStr(n *tree.Node, key, what string) string {
	if _, ok := n.Get(key); !ok {
		l.add(l.at(n), "%s has no %s", what, key)
		return ""
	}
	return l.str(n, key)
}

// boolean returns n's boolean member key, false when absent; another type
// is a problem.
func (l *load) boolean(n *tree.Node, key string) bool {
	v, ok := n.Get(key)
	if !ok {
		return false
	}
	if v == nil || v.Kind != tree.KindBool {
		l.add(l.at(v), "%s is a %s, not a boolean", key, kindName(v))
		return false
	}
	return v.Bool
}
