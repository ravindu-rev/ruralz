// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package problem writes and reads RFC 9457 problem documents, the error
// body of every Node-generated response and admin error. A document never
// echoes request content: title, status and code come from the RZ registry,
// requestId is the 32-hex trace ID, and detail is fixed operator-authored
// text (RZ-RT-009 names a schema keyword location only).
package problem

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"unicode/utf8"

	"github.com/ravindu-rev/ruralz/internal/errcode"
)

// ContentType is the media type of a problem document.
const ContentType = "application/problem+json"

// Problem is one Node-generated error body. Members are written in the
// order title, status, code, requestId, detail; there is no type member
// (about:blank).
type Problem struct {
	// Title is fixed per code; see Title.
	Title string
	// Status is the HTTP status.
	Status int
	// Code is a registered RZ code.
	Code string
	// RequestID is the request's 32 lowercase hex trace ID.
	RequestID string
	// Detail is optional operator-authored text, never request content.
	Detail string
}

// New returns the problem for code with its registered status and title.
// A code without a single registered status gets status 500 and must be
// given an explicit status by the caller instead.
func New(code, requestID string) Problem {
	status := errcode.Status(code)
	if status == 0 {
		status = http.StatusInternalServerError
	}
	return Problem{Title: Title(code, status), Status: status, Code: code, RequestID: requestID}
}

// Title returns the fixed title of code. RZ-RT codes have their own titles;
// RZ-AUTH titles are generic by status (Security and identity forbids
// revealing the reason); every other code uses the HTTP status text.
func Title(code string, status int) string {
	switch code {
	case "RZ-RT-001":
		return "No matching Route"
	case "RZ-RT-002":
		return "Request header block too large"
	case "RZ-RT-003":
		return "Request body too large"
	case "RZ-RT-004":
		return "Buffer budget exhausted"
	case "RZ-RT-005":
		return "Node at capacity"
	case "RZ-RT-006":
		return "Route match failed"
	case "RZ-RT-007":
		return "Route timeout before any Upstream attempt"
	case "RZ-RT-008":
		return "CORS preflight rejected"
	case "RZ-RT-009":
		return "Request body failed validation"
	case "RZ-RT-010":
		return "No composition step matched"
	case "RZ-RT-011":
		return "Policy could not decide"
	case "RZ-RT-012":
		return "Response Policy failed"
	case "RZ-RT-014":
		return "Configuration snapshot retired"
	case "RZ-RT-015":
		return "Composition step failed"
	case "RZ-RT-016":
		return "Node draining"
	case "RZ-RT-017":
		return "Request rejected"
	case "RZ-RT-018":
		return "Not in cache"
	case "RZ-RT-019":
		return "Tap subscriber limit reached"
	}
	if t := http.StatusText(status); t != "" {
		return t
	}
	return "Error"
}

// Append appends the JSON document to dst.
func Append(dst []byte, p Problem) []byte {
	dst = append(dst, `{"title":`...)
	dst = appendString(dst, p.Title)
	dst = append(dst, `,"status":`...)
	dst = strconv.AppendInt(dst, int64(p.Status), 10)
	dst = append(dst, `,"code":`...)
	dst = appendString(dst, p.Code)
	dst = append(dst, `,"requestId":`...)
	dst = appendString(dst, p.RequestID)
	if p.Detail != "" {
		dst = append(dst, `,"detail":`...)
		dst = appendString(dst, p.Detail)
	}
	return append(dst, '}')
}

// Write sets Content-Type, Cache-Control: no-store and an exact
// Content-Length, copies extra (such as WWW-Authenticate or Retry-After),
// writes the status and the body. It never reads the request.
func Write(w http.ResponseWriter, p Problem, extra http.Header) {
	var buf [256]byte
	body := Append(buf[:0], p)
	h := w.Header()
	for k, vs := range extra {
		h[k] = vs
	}
	h.Set("Content-Type", ContentType)
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(p.Status)
	_, _ = w.Write(body)
}

// Document is a decoded problem document from any Ruralz server, for the
// CLI and tests. Unknown members are ignored.
type Document struct {
	Type      string `json:"type,omitempty"`
	Title     string `json:"title"`
	Status    int    `json:"status"`
	Detail    string `json:"detail,omitempty"`
	Code      string `json:"code,omitempty"`
	RequestID string `json:"requestId,omitempty"`
}

// Decode parses a problem document.
func Decode(b []byte) (Document, error) {
	var d Document
	if err := json.Unmarshal(b, &d); err != nil {
		return Document{}, fmt.Errorf("problem document: %w", err)
	}
	return d, nil
}

// appendString appends s as a JSON string: minimal RFC 8259 escaping, no
// HTML escaping, invalid UTF-8 replaced by U+FFFD.
func appendString(dst []byte, s string) []byte {
	const hex = "0123456789abcdef"
	dst = append(dst, '"')
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			switch {
			case c == '"' || c == '\\':
				dst = append(dst, '\\', c)
			case c < 0x20:
				dst = append(dst, '\\', 'u', '0', '0', hex[c>>4], hex[c&0xf])
			default:
				dst = append(dst, c)
			}
			i++
			continue
		}
		r, n := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && n == 1 {
			dst = append(dst, "�"...)
		} else {
			dst = append(dst, s[i:i+n]...)
		}
		i += n
	}
	return append(dst, '"')
}
