// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package openapi

import (
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// Document is an OpenAPI 3.0 or 3.1 document, its Reference Objects
// resolved wherever the model reads an object. Schemas stay as written:
// JSON Schema documents whose references are the schema's own.
type Document struct {
	// Version is the openapi member, such as 3.1.0.
	Version string
	// Minor is the minor version, 0 or 1.
	Minor int
	// Info is the info object.
	Info Info
	// Servers are the root servers, in order.
	Servers []Server
	// Paths are the members of paths in authored order, extensions left
	// out.
	Paths []Path
	// Webhooks are the members of the 3.1 webhooks object in authored
	// order.
	Webhooks []Path
	// Components is the components object as written, nil without one.
	Components *tree.Node
	// At is where the document starts.
	At diag.Location
	// Files names every file the load read, the root first, in the order
	// it read them.
	Files []string
}

// Info is the info object. A scalar of another type than a string, such
// as version: 1.0, reads as its text.
type Info struct {
	// Title is info.title.
	Title string
	// Version is info.version.
	Version string
	// At is where the object is.
	At diag.Location
}

// Server is a Server Object.
type Server struct {
	// URL is the url member as written, variables unsubstituted.
	URL string
	// Description is the description member.
	Description string
	// Variables are the server variables in authored order.
	Variables []ServerVariable
	// At is where the url member's value is.
	At diag.Location
}

// ServerVariable is one member of a Server Object's variables.
type ServerVariable struct {
	// Name is the variable name.
	Name string
	// Default is the default value.
	Default string
	// Enum lists the allowed values; empty means any.
	Enum []string
	// At is where the variable is.
	At diag.Location
}

// Path is one member of the paths or webhooks object.
type Path struct {
	// Key is the member name: a path template such as /pets/{petId}, or a
	// webhook's name.
	Key string
	// Item is the Path Item Object, its own $ref resolved.
	Item *PathItem
	// At is where the member's key is.
	At diag.Location
}

// PathItem is a Path Item Object.
type PathItem struct {
	// Summary is the summary member.
	Summary string
	// Description is the description member.
	Description string
	// Servers override the root servers for the item's operations.
	Servers []Server
	// Parameters apply to every operation of the item.
	Parameters []*Parameter
	// Operations are the item's operations in authored order.
	Operations []*Operation
	// At is where the object is.
	At diag.Location
}

// Methods are the operation members of a Path Item Object in the order
// the OpenAPI Specification lists them.
func Methods() []string {
	return []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"}
}

// Operation is an Operation Object.
type Operation struct {
	// Method is the member name it is written under, such as get.
	Method string
	// OperationID is the operationId member.
	OperationID string
	// Summary is the summary member.
	Summary string
	// Description is the description member.
	Description string
	// Tags is the tags member.
	Tags []string
	// Parameters are the operation's own parameters.
	Parameters []*Parameter
	// RequestBody is the request body, nil without one.
	RequestBody *RequestBody
	// Deprecated is the deprecated member.
	Deprecated bool
	// Servers override the path item's and the root servers.
	Servers []Server
	// Responses is the responses object as written, nil without one.
	Responses *tree.Node
	// Security is the security member as written; nil inherits the
	// document's.
	Security *tree.Node
	// Callbacks is the callbacks member as written.
	Callbacks *tree.Node
	// At is where the member's key is.
	At diag.Location
}

// Parameter is a Parameter Object.
type Parameter struct {
	// Name is the name member.
	Name string
	// In is the in member: query, header, path or cookie.
	In string
	// Required is the required member.
	Required bool
	// Deprecated is the deprecated member.
	Deprecated bool
	// Schema is the schema member, nil without one.
	Schema *Schema
	// Content holds the content member's media types.
	Content []MediaType
	// At is where the object is.
	At diag.Location
}

// RequestBody is a Request Body Object.
type RequestBody struct {
	// Description is the description member.
	Description string
	// Required is the required member.
	Required bool
	// Content holds the media types in authored order.
	Content []MediaType
	// At is where the object is.
	At diag.Location
}

// MediaType is one member of a content object.
type MediaType struct {
	// Type is the media type or range, such as application/json.
	Type string
	// Schema is the schema member, nil without one.
	Schema *Schema
	// At is where the member's key is.
	At diag.Location
}

// Schema is a Schema Object as written: in 3.1 a JSON Schema draft
// 2020-12 document, in 3.0 the specification's extended subset.
type Schema struct {
	// Node is the schema.
	Node *tree.Node
	// At is where the schema is.
	At diag.Location
}
