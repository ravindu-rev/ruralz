// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package admin

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/problem"
)

// Admin paths of docs/architecture/03-data-plane.md "Admin endpoints".
// PathPprof is a prefix: every profile path is under it.
const (
	PathHealthz    = "/healthz"
	PathReadyz     = "/readyz"
	PathMetrics    = "/metrics"
	PathPprof      = "/debug/pprof/"
	PathSnapshots  = "/debug/snapshots"
	PathUpstreams  = "/debug/upstreams"
	PathConfigDump = "/config/dump"
	PathTap        = "/tap"
)

// Codes of the router's problem documents.
const (
	// CodeNotFound is a path with no admin endpoint: 404.
	CodeNotFound = "RZ-RT-020"
	// CodeMethodNotAllowed is a method other than GET on an endpoint: 405.
	CodeMethodNotAllowed = "RZ-RT-021"
)

// healthzBody is the /healthz body.
const healthzBody = `{"status":"ok"}`

// buildEndpoints maps each exact path to its handler; an endpoint without
// a handler is left out, so it answers 404.
func (s *Server) buildEndpoints(cfg Config) map[string]http.Handler {
	m := map[string]http.Handler{
		PathHealthz: http.HandlerFunc(serveHealthz),
		PathReadyz:  cfg.Readiness,
	}
	if cfg.Tap != nil {
		m[PathTap] = cfg.Tap
	}
	if cfg.Snapshots != nil {
		snapshots := cfg.Snapshots
		m[PathSnapshots] = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			body, err := json.Marshal(snapshots())
			if err != nil {
				// adminapi.Snapshots holds strings, integers and string
				// pointers; encoding cannot fail.
				panic(err)
			}
			writeJSON(w, body)
		})
	}
	for path, h := range map[string]http.Handler{
		PathMetrics:    cfg.Metrics,
		PathUpstreams:  cfg.Upstreams,
		PathConfigDump: cfg.ConfigDump,
	} {
		if h != nil {
			m[path] = h
		}
	}
	return m
}

// route dispatches an authenticated request by its exact path: 404
// RZ-RT-020 for a path with no endpoint, then 405 RZ-RT-021 for a method
// other than GET.
func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	h := s.endpoint(r.URL.Path)
	if h == nil {
		problem.Write(w, problem.New(CodeNotFound, s.requestID(r)), nil)
		return
	}
	if r.Method != http.MethodGet {
		problem.Write(w, problem.New(CodeMethodNotAllowed, s.requestID(r)), http.Header{"Allow": {http.MethodGet}})
		return
	}
	h.ServeHTTP(w, r)
}

// endpoint returns the handler of path, nil when no endpoint serves it.
func (s *Server) endpoint(path string) http.Handler {
	if h, ok := s.endpoints[path]; ok {
		return h
	}
	if name, ok := strings.CutPrefix(path, PathPprof); ok {
		return s.profile(name)
	}
	return nil
}

// serveHealthz answers 200 {"status":"ok"} while the process responds.
func serveHealthz(w http.ResponseWriter, _ *http.Request) {
	setJSONHeaders(w, len(healthzBody))
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, healthzBody)
}

// writeJSON writes a 200 JSON body.
func writeJSON(w http.ResponseWriter, body []byte) {
	setJSONHeaders(w, len(body))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// setJSONHeaders sets the headers of a JSON body of n bytes; admin bodies
// describe the moment, so none is cached.
func setJSONHeaders(w http.ResponseWriter, n int) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Length", strconv.Itoa(n))
}
