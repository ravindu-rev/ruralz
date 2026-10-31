// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package loader

import (
	"strconv"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/profile"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// envelope reads the identity and apiVersion of one parsed document, the
// stage C checks every document of a Bundle or an environments file gets:
// a served apiVersion and kind (RZ-CFG-007) and a string metadata.name
// (RZ-CFG-005). known reports that the kind and name could be read, even
// when the document failed; ds lists its findings, and the document passes
// only without any.
func (l *Loader) envelope(doc profile.Document, files *tree.FileTable) (res *tree.Resource, known bool, ds diag.List) {
	root := doc.Root
	res = &tree.Resource{Root: root, Start: doc.Start}
	at := func(n *tree.Node) diag.Location {
		if n != nil && n.Pos.Known() {
			return files.Location(n.Pos)
		}
		return files.Location(doc.Start)
	}
	av, hasAV := root.Get("apiVersion")
	kind, hasKind := root.Get("kind")
	meta, hasMeta := root.Get("metadata")
	var name *tree.Node
	hasName := false
	if meta != nil && meta.Kind == tree.KindMap {
		name, hasName = meta.Get("name")
	}
	kindOK := kind != nil && kind.Kind == tree.KindString
	nameOK := name != nil && name.Kind == tree.KindString
	if kindOK {
		res.ID.Kind = v1alpha1.Kind(kind.Text)
	}
	if nameOK {
		res.ID.Name = name.Text
	}
	known = kindOK && nameOK
	var rid *diag.ResourceID
	if known {
		rid = resourceID(res.ID)
	}
	add := func(code string, n *tree.Node, path diag.Path, msg, hint string) {
		ds = append(ds, diag.Diagnostic{
			Code: code, Severity: diag.SeverityError, Location: at(n), Resource: rid,
			Path: path, Message: msg, Hint: hint,
		})
	}

	var idx *schemaidx.Index
	switch {
	case !hasAV || av == nil:
		add(CodeUnserved, nil, diag.Path{diag.Field("apiVersion")}, "apiVersion is required", "")
	case av.Kind != tree.KindString:
		add(CodeUnserved, av, diag.Path{diag.Field("apiVersion")}, "apiVersion must be a string", "")
	default:
		res.APIVersion = av.Text
		var ok bool
		if idx, ok = l.served(av.Text); !ok {
			d, _ := l.conv.CheckServed(av.Text)
			add(CodeUnserved, av, d.Path, d.Message, d.Hint)
		}
	}

	switch {
	case !hasKind || kind == nil:
		add(CodeUnserved, nil, diag.Path{diag.Field("kind")}, "kind is required", "")
	case !kindOK:
		add(CodeUnserved, kind, diag.Path{diag.Field("kind")}, "kind must be a string", "")
	case idx != nil:
		if _, ok := idx.Resource(kind.Text); !ok {
			hint := ""
			if near, ok := diag.Nearest(kind.Text, idx.Kinds()); ok {
				hint = "did you mean " + strconv.Quote(near) + "?"
			}
			add(CodeUnserved, kind, diag.Path{diag.Field("kind")},
				"kind "+quote(kind.Text)+" is not served by apiVersion "+quote(res.APIVersion), hint)
		}
	}

	switch {
	case !hasMeta || meta == nil:
		add(CodeEnvelope, nil, diag.Path{diag.Field("metadata")}, "metadata is required: it holds the name that identifies the resource", "")
	case meta.Kind != tree.KindMap:
		add(CodeEnvelope, meta, diag.Path{diag.Field("metadata")}, "metadata must be an object", "")
	case !hasName || name == nil:
		add(CodeEnvelope, meta, diag.Path{diag.Field("metadata"), diag.Field("name")}, "metadata.name is required: it identifies the resource", "")
	case !nameOK:
		add(CodeEnvelope, name, diag.Path{diag.Field("metadata"), diag.Field("name")}, "metadata.name must be a string", "")
	}
	return res, known, ds
}

// controlKind reports RZ-CFG-017 for an Environment or Cluster in a Bundle:
// they say where a Revision goes, not what it holds.
func controlKind(res *tree.Resource, files *tree.FileTable) (diag.Diagnostic, bool) {
	var msg string
	switch res.ID.Kind {
	case v1alpha1.KindEnvironment:
		msg = "an Environment lives outside the Bundle; the CLI reads it from an --environments file"
	case v1alpha1.KindCluster:
		msg = "a Cluster lives outside the Bundle; only Ruralz Control reads it"
	default:
		return diag.Diagnostic{}, false
	}
	at := res.Start
	if k, ok := res.Root.Get("kind"); ok && k != nil && k.Pos.Known() {
		at = k.Pos
	}
	return diag.Diagnostic{
		Code: CodeControlKind, Severity: diag.SeverityError, Location: files.Location(at),
		Resource: resourceID(res.ID), Path: diag.Path{diag.Field("kind")}, Message: msg,
	}, true
}

// memberPos returns the position of a root member's value of res, else
// the document start.
func memberPos(res *tree.Resource, member string) tree.Pos {
	if v, ok := res.Root.Get(member); ok && v != nil && v.Pos.Known() {
		return v.Pos
	}
	return res.Start
}

// namePos returns the position of res's metadata.name, else the document
// start.
func namePos(res *tree.Resource) tree.Pos {
	if md, ok := res.Root.Get("metadata"); ok && md != nil {
		if v, ok := md.Get("name"); ok && v != nil && v.Pos.Known() {
			return v.Pos
		}
	}
	return res.Start
}
