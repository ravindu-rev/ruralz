// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package export writes what ruralz bundle export prints
// (docs/reference/01-cli-and-api-surface.md "Command table" and "Output
// formats and exit codes", docs/_meta/foundation-pack.md section 9): an
// OpenAPI 3.1 document and a Postman collection v2.1 of the HTTP
// operations of one Environment's rendered Bundle, and a Graphviz DOT
// graph of its Routes, Policies and Upstreams. It reads the validated
// Bundle the configuration pipeline produced (hub.Validated) and runs no
// stage itself.
//
// # Operations
//
// Operations lists the HTTP operations of the Bundle's Routes, the Routes
// in canonical order by name. A Route qualifies when its match sets no
// grpc, graphql or topic, its path is exact or template, and every
// Upstream it reaches, through spec.upstreams or a composition step, has
// protocol http; any other Route is a Skip naming the reason. The OpenAPI
// path is the exact path, or the template with its {name} captures as
// written; an exact path holding a brace has none. OpenAPI holds one path
// per hierarchy, so a template of the hierarchy of a path an earlier Route
// exported takes that path, whose capture names its operations then use.
//
// The methods are the Route's match.methods, or for a Route without them
// the eight methods OpenAPI describes, each in the order get, put, post,
// delete, options, head, patch, trace; a method outside those eight, and a
// path and method an earlier Route already exported, are a Skip for that
// method. An operation's parameters are the template captures in order,
// and its body schema is the config.schema of the first
// validation.json-schema Policy of the Route's effective client chain in
// execution order, in the normalized form of the validated Bundle, its
// numbers exact and its members sorted as the canonical form sorts them:
// the operation schema of docs/architecture/06-ai-llm-gateway.md
// (tools[].inputSchemaFrom). Its
// operationId is the Route name when the Route exports one method, else
// "<route>-<method in lower case>", and one an earlier operation already
// uses gains "-2", "-3" and so on.
//
// # OpenAPI
//
// OpenAPI builds the OpenAPI 3.1.0 document of the operations as a value
// tree, which internal/config/profile writes as YAML or JSON with strings
// verbatim: info.title is the Gateway's metadata.name and info.version the
// Revision's rev-<12 hex>; paths are sorted bytewise and each path item
// holds its operations in method order. An operation has its operationId,
// one path parameter per capture (in: path, required: true, schema type
// string), and a required application/json request body with the body
// schema when it has one. The document names no servers, responses,
// security or query parameters.
//
// # Postman
//
// Postman writes the Postman Collection v2.1.0 JSON of the same
// operations, in the document's order: the collection is named after the
// Gateway, each item after its operation's operationId, and its request
// carries the method and the URL {{baseUrl}} followed by the path, each
// capture written :name in the raw URL and the path segments and listed
// in url.variable with an empty value. An operation with a body schema
// also sends content-type application/json and an empty raw JSON body.
// The collection variable baseUrl is <protocol>://localhost:<port> of the
// Gateway's first listener by name.
//
// # DOT
//
// DOT writes a digraph named after the Gateway, laid out left to right,
// with one node per Route (a box), Policy (an ellipse, labeled with its
// type) and Upstream (a cylinder), each set in name order, then the edges:
// for each Route by name, one to each Policy of its effective client
// chain in execution order and one to each Upstream it reaches in spec
// order, each target once; then for each Upstream by name, one to each of
// its upstream-leg Policies in order. Without a Route the graph holds
// every Route, Policy and Upstream of the Bundle, unattached ones as lone
// nodes; with one it holds that Route, the Policies of its client chain
// and of every leg, the Upstreams it reaches, and those Upstreams' leg
// Policies from the Route's chain. A Route the Bundle lacks is
// ErrUnknownRoute with the nearest Route name as a hint.
//
// # Concurrency and effects
//
// Every function is safe for concurrent use and never changes its inputs;
// nothing in the package touches the file system, logs, reads the clock or
// opens a connection.
package export
