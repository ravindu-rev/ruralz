// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package adminclient

import (
	"encoding/hex"
	"io"
)

// traceparent draws a fresh W3C traceparent that is not sampled for one
// request: version 00, a random trace ID and parent ID, flags 00. A Node
// takes the trace ID of a valid traceparent as the requestId of its
// problem documents (docs/architecture/03-data-plane.md "Admin
// endpoints"), so that requestId names the CLI's request.
func (c *Client) traceparent() (string, error) {
	var b [24]byte
	c.randMu.Lock()
	_, err := io.ReadFull(c.rand, b[:])
	c.randMu.Unlock()
	if err != nil {
		return "", err
	}
	return formatTraceparent(b), nil
}

// formatTraceparent formats the trace ID b[:16] and parent ID b[16:] as
// "00-<32 hex>-<16 hex>-00". An all-zero ID, which W3C Trace Context makes
// invalid, gets its last bit set.
func formatTraceparent(b [24]byte) string {
	if [16]byte(b[:16]) == [16]byte{} {
		b[15] = 1
	}
	if [8]byte(b[16:]) == [8]byte{} {
		b[23] = 1
	}
	out := make([]byte, 0, 55)
	out = append(out, "00-"...)
	out = hex.AppendEncode(out, b[:16])
	out = append(out, '-')
	out = hex.AppendEncode(out, b[16:])
	out = append(out, "-00"...)
	return string(out)
}
