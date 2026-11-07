// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package loadgen

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"

	"github.com/ravindu-rev/ruralz/internal/problem"
)

// DefaultConnections bounds an HTTP target's keep-alive connections
// (target).
const DefaultConnections = 256

// maxProblemBytes bounds the problem document read for its code
// (target); Ruralz problem documents are a few hundred bytes
// (hypothesis).
const maxProblemBytes = 64 << 10

// HTTPConfig configures an HTTP target.
type HTTPConfig struct {
	// URL is the request URL, http or https; https verifies the server
	// against the system roots.
	URL string
	// Connections bounds the keep-alive connections; default
	// DefaultConnections.
	Connections int
}

// HTTPTarget sends GET requests over HTTP/1.1, on at most Connections
// keep-alive connections, the most recently used idle one first. It never
// follows redirects and never asks for compression, so each response is
// the one the server sent.
type HTTPTarget struct {
	url    string
	tr     *http.Transport
	client *http.Client
}

// NewHTTP returns an HTTP target.
func NewHTTP(c HTTPConfig) (*HTTPTarget, error) {
	u, err := url.Parse(c.URL)
	if err != nil {
		return nil, fmt.Errorf("loadgen: URL: %w", err)
	}
	if u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("loadgen: URL %q is not an absolute http or https URL", c.URL)
	}
	switch {
	case c.Connections < 0:
		return nil, errors.New("loadgen: negative connection bound")
	case c.Connections == 0:
		c.Connections = DefaultConnections
	}
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	tr := &http.Transport{
		DisableCompression:  true,
		MaxConnsPerHost:     c.Connections,
		MaxIdleConns:        c.Connections,
		MaxIdleConnsPerHost: c.Connections,
		Protocols:           &protocols,
	}
	return &HTTPTarget{
		url: c.URL,
		tr:  tr,
		client: &http.Client{
			Transport:     tr,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// Close closes the target's idle connections.
func (t *HTTPTarget) Close() error {
	t.tr.CloseIdleConnections()
	return nil
}

// Do sends one GET request and reads the whole response body.
func (t *HTTPTarget) Do(ctx context.Context) Response {
	var resp Response
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.url, nil)
	if err != nil {
		resp.Err, resp.Outcome = fmt.Errorf("loadgen: %w", err), Other
		return resp
	}
	res, err := t.client.Do(req)
	if err != nil {
		resp.Err = err
		resp.Outcome = Classify(err)
		return resp
	}
	defer func() { _ = res.Body.Close() }()
	resp.Status = res.StatusCode
	resp.Outcome = OutcomeForStatus(res.StatusCode)
	if res.StatusCode >= 400 && isProblem(res.Header.Get("Content-Type")) {
		head, err := io.ReadAll(io.LimitReader(res.Body, maxProblemBytes))
		if err != nil {
			return failBody(resp, err)
		}
		if doc, err := problem.Decode(head); err == nil {
			resp.Code = doc.Code
		}
	}
	if _, err := io.Copy(io.Discard, res.Body); err != nil {
		return failBody(resp, err)
	}
	return resp
}

// failBody marks a response whose body broke off.
func failBody(resp Response, err error) Response {
	resp.Err = err
	if resp.Outcome = Classify(err); resp.Outcome == Success {
		resp.Outcome = Other
	}
	return resp
}

func isProblem(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)
	return err == nil && mt == problem.ContentType
}
