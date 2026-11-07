// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package export

import (
	"strings"

	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// OpenAPIVersion is the openapi member of every exported document.
const OpenAPIVersion = "3.1.0"

// OpenAPI returns the OpenAPI 3.1.0 document of ops, the operations
// Operations returned for v, as a value tree (package documentation,
// "OpenAPI"). Write it with internal/config/profile's encoders and
// EncodeOptions.Verbatim, so its strings stay as they are. The body
// schemas are copies, so the document shares no node with v.
func OpenAPI(v *hub.Validated, ops []Operation) *tree.Node {
	title, version := "", ""
	if v != nil && v.Bundle != nil {
		title, version = gatewayName(v.Bundle), v.Revision.Digest.Short()
	}
	paths := mapping()
	var item *tree.Node
	last := ""
	for _, op := range documentOrder(ops) {
		if item == nil || op.Path != last {
			item = mapping()
			paths.Members = append(paths.Members, member(op.Path, item))
			last = op.Path
		}
		item.Members = append(item.Members, member(strings.ToLower(op.Method), operation(op)))
	}
	return mapping(
		member("openapi", str(OpenAPIVersion)),
		member("info", mapping(member("title", str(title)), member("version", str(version)))),
		member("paths", paths),
	)
}

// operation returns the Operation Object of op: its operationId, one path
// parameter per capture and, with a body schema, a required JSON request
// body.
func operation(op Operation) *tree.Node {
	n := mapping(member("operationId", str(op.OperationID)))
	if len(op.Params) > 0 {
		params := list()
		for _, p := range op.Params {
			params.Items = append(params.Items, mapping(
				member("name", str(p)),
				member("in", str("path")),
				member("required", boolean(true)),
				member("schema", mapping(member("type", str("string")))),
			))
		}
		n.Members = append(n.Members, member("parameters", params))
	}
	if op.Body != nil {
		n.Members = append(n.Members, member("requestBody", mapping(
			member("required", boolean(true)),
			member("content", mapping(member("application/json", mapping(member("schema", op.Body.Clone()))))),
		)))
	}
	return n
}

// mapping returns a map node of members.
func mapping(members ...tree.Member) *tree.Node {
	return &tree.Node{Kind: tree.KindMap, Members: members}
}

// list returns a list node of items.
func list(items ...*tree.Node) *tree.Node {
	return &tree.Node{Kind: tree.KindList, Items: items}
}

// member returns the member key of a map node.
func member(key string, v *tree.Node) tree.Member { return tree.Member{Key: key, Value: v} }

// str returns a string node.
func str(s string) *tree.Node { return &tree.Node{Kind: tree.KindString, Text: s} }

// boolean returns a bool node.
func boolean(b bool) *tree.Node { return &tree.Node{Kind: tree.KindBool, Bool: b} }
