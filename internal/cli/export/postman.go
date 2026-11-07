// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package export

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// PostmanSchema is the schema URL of a Postman Collection v2.1.0.
const PostmanSchema = "https://schema.getpostman.com/json/collection/v2.1.0/collection.json"

// baseURLVariable is the collection variable every request URL starts
// with.
const baseURLVariable = "baseUrl"

// collection is a Postman Collection v2.1.0, its members in the order
// they are written.
type collection struct {
	Info     collectionInfo `json:"info"`
	Item     []item         `json:"item"`
	Variable []variable     `json:"variable"`
}

// collectionInfo is the info member of a collection.
type collectionInfo struct {
	Name   string `json:"name"`
	Schema string `json:"schema"`
}

// item is one request of a collection.
type item struct {
	Name    string  `json:"name"`
	Request request `json:"request"`
}

// request is an item's request.
type request struct {
	Method string     `json:"method"`
	Header []header   `json:"header,omitempty"`
	Body   *rawBody   `json:"body,omitempty"`
	URL    requestURL `json:"url"`
}

// header is one request header.
type header struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// rawBody is a raw request body.
type rawBody struct {
	Mode    string      `json:"mode"`
	Raw     string      `json:"raw"`
	Options bodyOptions `json:"options"`
}

// bodyOptions are the options of a raw body.
type bodyOptions struct {
	Raw rawOptions `json:"raw"`
}

// rawOptions name the language of a raw body.
type rawOptions struct {
	Language string `json:"language"`
}

// requestURL is a request's URL: its raw form, host and path segments,
// and the path variables it names.
type requestURL struct {
	Raw      string     `json:"raw"`
	Host     []string   `json:"host"`
	Path     []string   `json:"path"`
	Variable []variable `json:"variable,omitempty"`
}

// variable is a collection or path variable.
type variable struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Postman returns the Postman Collection v2.1.0 of ops, the operations
// Operations returned for v, as JSON indented by two spaces, without HTML
// escaping and with a final newline (package documentation, "Postman").
func Postman(v *hub.Validated, ops []Operation) ([]byte, error) {
	c := collection{Item: []item{}, Variable: []variable{{Key: baseURLVariable, Value: baseURL(v)}}}
	if v != nil && v.Bundle != nil {
		c.Info = collectionInfo{Name: gatewayName(v.Bundle), Schema: PostmanSchema}
	} else {
		c.Info = collectionInfo{Schema: PostmanSchema}
	}
	for _, op := range documentOrder(ops) {
		c.Item = append(c.Item, item{Name: op.OperationID, Request: postmanRequest(op)})
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(c); err != nil {
		return nil, fmt.Errorf("export: encode the Postman collection: %w", err)
	}
	return buf.Bytes(), nil
}

// postmanRequest returns the request of op: its method and URL, and with a
// body schema a JSON content-type and an empty raw JSON body.
func postmanRequest(op Operation) request {
	segs := strings.Split(strings.TrimPrefix(op.Path, "/"), "/")
	for i, s := range segs {
		if isCapture(s) {
			segs[i] = ":" + s[1:len(s)-1]
		}
	}
	host := "{{" + baseURLVariable + "}}"
	r := request{Method: op.Method, URL: requestURL{
		Raw: host + "/" + strings.Join(segs, "/"), Host: []string{host}, Path: segs,
	}}
	for _, p := range op.Params {
		r.URL.Variable = append(r.URL.Variable, variable{Key: p})
	}
	if op.Body != nil {
		r.Header = []header{{Key: "content-type", Value: "application/json"}}
		r.Body = &rawBody{Mode: "raw", Options: bodyOptions{Raw: rawOptions{Language: "json"}}}
	}
	return r
}

// baseURL returns <protocol>://localhost:<port> of the Gateway's first
// listener by name, or "" without one.
func baseURL(v *hub.Validated) string {
	if v == nil || v.Bundle == nil {
		return ""
	}
	g := v.Bundle.Gateway()
	if g == nil || len(g.Spec.Listeners) == 0 {
		return ""
	}
	l := slices.MinFunc(g.Spec.Listeners, func(a, b v1alpha1.Listener) int { return cmp.Compare(a.Name, b.Name) })
	return string(l.Protocol) + "://localhost:" + strconv.Itoa(int(l.Port))
}
