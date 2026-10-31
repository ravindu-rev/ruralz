// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cors

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/httpfield"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Type is the Policy type this package implements.
const Type = v1alpha1.PolicyTypeCORS

// Codes, each registered in internal/errcode.
const (
	// CodeConfig is a config the Policy refuses (package documentation,
	// "Configuration").
	CodeConfig = "RZ-CFG-005"
	// CodeRejected is a preflight the Policy does not allow (403).
	CodeRejected = "RZ-RT-008"
)

// wildcard is the allowOrigins, allowHeaders or exposeHeaders entry that
// stands for every origin or field name; the configuration refuses it
// with allowCredentials true.
const wildcard = "*"

// authorization is the request header a wildcard never covers: browsers
// require Access-Control-Allow-Headers to name it (Fetch, CORS-preflight
// fetch).
const authorization = "authorization"

// Field names, in canonical form.
const (
	fieldOrigin           = "Origin"
	fieldRequestMethod    = "Access-Control-Request-Method"
	fieldRequestHeaders   = "Access-Control-Request-Headers"
	fieldAllowOrigin      = "Access-Control-Allow-Origin"
	fieldAllowMethods     = "Access-Control-Allow-Methods"
	fieldAllowHeaders     = "Access-Control-Allow-Headers"
	fieldAllowCredentials = "Access-Control-Allow-Credentials"
	fieldExposeHeaders    = "Access-Control-Expose-Headers"
	fieldMaxAge           = "Access-Control-Max-Age"
	fieldVary             = "Vary"
)

// varyPreflight is the Vary of an allowed preflight's answer, which
// depends on all three request fields.
const varyPreflight = fieldOrigin + ", " + fieldRequestMethod + ", " + fieldRequestHeaders

// errNoMessage is the cause of cannot decide when onResponse has no
// response message.
var errNoMessage = errors.New("cors: no response message")

// Factory builds cors Filters (filter.Factory).
type Factory struct{}

var _ filter.Factory = Factory{}

// Build compiles a cors config. A config of the wrong type, or one Check
// refuses (package documentation, "Configuration"), is an error carrying
// RZ-CFG-005 naming the first finding. Nothing is carried over from
// env.Previous: the tables are rebuilt in microseconds.
func (Factory) Build(ctx context.Context, env filter.BuildEnv) (filter.Filter, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cfg, ok := env.Config.(*v1alpha1.CORSConfig)
	if !ok || cfg == nil {
		return nil, errcode.Errorf(CodeConfig, "cors: config is %T, want *v1alpha1.CORSConfig", env.Config)
	}
	if fs := findings(cfg); len(fs) > 0 {
		return nil, errcode.Errorf(CodeConfig, "cors: config.%s: %s", fs[0].path, fs[0].message)
	}
	status := errcode.Status(CodeRejected)
	if status == 0 {
		return nil, errcode.Errorf(CodeConfig, "cors: %s has no registered status", CodeRejected)
	}
	return newFilter(cfg, status), nil
}

// origin is the Access-Control-Allow-Origin answer for an allowed origin,
// built once.
type origin struct {
	acao []string
}

// Filter is a compiled cors Policy. It is immutable and safe for
// concurrent use; the field value slices it hands out are shared and
// read-only (package documentation, "Response side").
type Filter struct {
	// origins holds the exact allowOrigins entries; anyOrigin, set when
	// allowOrigins holds "*", answers every origin with "*".
	origins   map[string]*origin
	anyOrigin *origin
	methods   map[string]struct{}
	// headers holds allowHeaders in lower case; anyHeader is set when
	// allowHeaders holds "*", so every requested field name passes.
	headers   map[string]struct{}
	anyHeader bool
	status    int
	// Shared field values: Access-Control-Allow-Methods, -Allow-Headers,
	// -Allow-Credentials, -Expose-Headers and -Max-Age (nil when not
	// sent), and Vary on actual and preflight answers.
	acam, acah, acac, aceh, maxAge []string
	varyOrigin, varyPre            []string
}

var _ filter.Filter = (*Filter)(nil)

// newFilter builds the tables of a checked cfg.
func newFilter(cfg *v1alpha1.CORSConfig, status int) *Filter {
	f := &Filter{
		origins:    make(map[string]*origin, len(cfg.AllowOrigins)),
		methods:    make(map[string]struct{}, len(cfg.AllowMethods)),
		headers:    make(map[string]struct{}, len(cfg.AllowHeaders)),
		status:     status,
		acam:       []string{strings.Join(cfg.AllowMethods, ", ")},
		varyOrigin: []string{fieldOrigin},
		varyPre:    []string{varyPreflight},
	}
	for _, o := range cfg.AllowOrigins {
		if o == wildcard {
			f.anyOrigin = &origin{acao: []string{wildcard}}
			continue
		}
		f.origins[o] = &origin{acao: []string{o}}
	}
	for _, m := range cfg.AllowMethods {
		f.methods[m] = struct{}{}
	}
	for _, h := range cfg.AllowHeaders {
		if h == wildcard {
			f.anyHeader = true
			continue
		}
		f.headers[strings.ToLower(h)] = struct{}{}
	}
	switch {
	case f.anyHeader:
		f.acah = []string{wildcard}
	case len(cfg.AllowHeaders) > 0:
		f.acah = []string{strings.Join(cfg.AllowHeaders, ", ")}
	}
	switch {
	case slices.Contains(cfg.ExposeHeaders, wildcard):
		f.aceh = []string{wildcard}
	case len(cfg.ExposeHeaders) > 0:
		f.aceh = []string{strings.Join(cfg.ExposeHeaders, ", ")}
	}
	if cfg.AllowCredentials != nil && *cfg.AllowCredentials {
		f.acac = []string{"true"}
	}
	if cfg.MaxAge != nil {
		f.maxAge = []string{strconv.FormatInt(int64(time.Duration(*cfg.MaxAge)/time.Second), 10)}
	}
	return f
}

// Handle runs the request side in onRequestHeaders and the response side
// in onResponse, and continues in every other Phase.
func (f *Filter) Handle(_ context.Context, p phase.Phase, x filter.Exchange) filter.Result {
	switch p {
	case phase.OnRequestHeaders:
		return f.request(x)
	case phase.OnResponse:
		return f.response(x)
	default:
		return filter.Next()
	}
}

// allowed returns the answer for the Origin field lines of a request: the
// wildcard's when allowOrigins holds "*", else the exact entry's; nil when
// the origin is not allowed or the request carries more than one Origin
// line.
func (f *Filter) allowed(origins []string) *origin {
	if len(origins) != 1 {
		return nil
	}
	if f.anyOrigin != nil {
		return f.anyOrigin
	}
	return f.origins[origins[0]]
}

// request answers a preflight, and records an allowed actual request in
// the Policy's slot for the response side. It allocates nothing except to
// answer a preflight.
func (f *Filter) request(x filter.Exchange) filter.Result {
	h := x.Header()
	origins := h[fieldOrigin]
	if len(origins) == 0 {
		return filter.Next()
	}
	o := f.allowed(origins)
	method := x.Method()
	if _, preflight := h[fieldRequestMethod]; preflight && method == http.MethodOptions {
		return f.preflight(h, o)
	}
	if o == nil {
		return filter.Next()
	}
	if _, ok := f.methods[method]; ok {
		if slot := x.PolicyState(); slot != nil {
			*slot = o
		}
	}
	return filter.Next()
}

// preflight answers a CORS-preflight request: 204 with the CORS fields
// when o (the answer for the request's origin, nil when not allowed), the
// requested method and every requested header name are allowed, else 403
// RZ-RT-008.
func (f *Filter) preflight(h http.Header, o *origin) filter.Result {
	requested := h[fieldRequestMethod]
	if o == nil || len(requested) != 1 {
		return f.reject()
	}
	if _, ok := f.methods[requested[0]]; !ok {
		return f.reject()
	}
	auth, ok := f.requestedHeaders(h[fieldRequestHeaders])
	if !ok {
		return f.reject()
	}
	hdr := http.Header{
		fieldAllowOrigin:  o.acao,
		fieldAllowMethods: f.acam,
		fieldVary:         f.varyPre,
	}
	switch {
	case auth != "":
		hdr[fieldAllowHeaders] = []string{wildcard + ", " + auth}
	case f.acah != nil:
		hdr[fieldAllowHeaders] = f.acah
	}
	if f.acac != nil {
		hdr[fieldAllowCredentials] = f.acac
	}
	if f.maxAge != nil {
		hdr[fieldMaxAge] = f.maxAge
	}
	return filter.Reply(&filter.Response{Status: http.StatusNoContent, Header: hdr})
}

// requestedHeaders reports whether every element of the
// Access-Control-Request-Headers field lines is an allowed field name:
// any field name when allowHeaders holds "*", else one allowHeaders lists,
// ignoring case; empty elements are skipped (RFC 9110 section 5.6.1) and
// an element that is no field name is refused. A browser lists only the
// CORS-unsafe request headers there, so without allowHeaders only
// safelisted ones pass. With "*", it also returns the Authorization
// element as requested, which the answer must name beside "*" because a
// browser never lets "*" cover Authorization.
func (f *Filter) requestedHeaders(lines []string) (auth string, ok bool) {
	for _, v := range lines {
		for v != "" {
			var elem string
			elem, v = nextElement(v)
			if elem == "" {
				continue
			}
			if !httpfield.IsToken(elem) {
				return "", false
			}
			if f.anyHeader {
				if auth == "" && strings.EqualFold(elem, authorization) {
					auth = elem
				}
				continue
			}
			if _, ok := f.headers[strings.ToLower(elem)]; !ok {
				return "", false
			}
		}
	}
	return auth, true
}

// reject refuses a preflight with 403 RZ-RT-008 and no CORS field.
func (f *Filter) reject() filter.Result {
	return filter.Deny(f.status, CodeRejected, nil)
}

// response sets Access-Control-Allow-Origin, and the credentials and
// expose fields when configured, on the response to an allowed actual
// request, and adds Origin to Vary on every response.
func (f *Filter) response(x filter.Exchange) filter.Result {
	m := x.Message()
	if m == nil {
		return filter.Undecided("", errNoMessage)
	}
	h := m.Header()
	if slot := x.PolicyState(); slot != nil {
		if o, ok := (*slot).(*origin); ok && o != nil {
			h[fieldAllowOrigin] = o.acao
			if f.acac != nil {
				h[fieldAllowCredentials] = f.acac
			}
			if f.aceh != nil {
				h[fieldExposeHeaders] = f.aceh
			}
		}
	}
	if vs := h[fieldVary]; len(vs) == 0 {
		h[fieldVary] = f.varyOrigin
	} else if !varies(vs) {
		h[fieldVary] = append(vs, fieldOrigin)
	}
	return filter.Next()
}

// varies reports whether the Vary field lines vs already list Origin
// (in any case) or "*" (RFC 9110 section 12.5.5).
func varies(vs []string) bool {
	for _, v := range vs {
		for v != "" {
			var elem string
			elem, v = nextElement(v)
			if elem == "*" || strings.EqualFold(elem, fieldOrigin) {
				return true
			}
		}
	}
	return false
}

// nextElement splits the first element off the comma-separated list v and
// trims the SP and HTAB around it.
func nextElement(v string) (elem, rest string) {
	i := strings.IndexByte(v, ',')
	if i < 0 {
		return httpfield.TrimOWS(v), ""
	}
	return httpfield.TrimOWS(v[:i]), v[i+1:]
}
