// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package mockup

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"
)

// Behavior programs one answer of the mock Upstream. The zero value
// answers 200 with an empty body.
type Behavior struct {
	// Status is the response status, 200 to 599; 0 means 200.
	Status int
	// Header is added to the response (after ContentType, so it can
	// replace it).
	Header http.Header
	// Body is the literal response body. When nil, the body is BodySize
	// bytes of GeneratedBody.
	Body []byte
	// BodySize is the generated body's length when Body is nil.
	BodySize int64
	// ContentType is the Content-Type of a body; default
	// "application/octet-stream" ("application/json" for Echo).
	ContentType string
	// Delay is waited before the response headers.
	Delay time.Duration
	// Echo answers with an Echo JSON document describing the request
	// (method, path, headers, body, ...), replacing Body and BodySize.
	Echo bool
}

// validate checks b; errors carry no package prefix (callers add it).
func (b Behavior) validate() error {
	if b.Status != 0 && (b.Status < 200 || b.Status > 599) {
		return fmt.Errorf("status %d outside 200..599", b.Status)
	}
	if b.BodySize < 0 {
		return errors.New("negative body size")
	}
	if b.Delay < 0 {
		return fmt.Errorf("negative delay %v", b.Delay)
	}
	return nil
}

// clone copies the mutable parts so later caller edits do not reach the
// server.
func (b Behavior) clone() Behavior {
	b.Header = b.Header.Clone()
	b.Body = slices.Clone(b.Body)
	return b
}
