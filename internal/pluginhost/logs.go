// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pluginhost

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"unicode/utf8"

	"github.com/ravindu-rev/ruralz/internal/redact"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
)

// guestLog writes one guest log line at level (docs/architecture/05-wasm-
// plugin-system.md "Default limits", Guest logs): a message is truncated
// at 4 KiB, a call drops what passes 16 KiB, and a Policy drops what passes
// 1 MiB per second; known credentials are redacted. msg holds at most
// logMessageBytes+1 bytes of a message of total bytes.
func (c *call) guestLog(level int32, msg []byte, total uint64) {
	truncated := total > logMessageBytes
	if len(msg) > logMessageBytes {
		msg = msg[:logMessageBytes]
	}
	if c.logBytes+len(msg) > logCallBytes {
		c.b.tel.LogDropped()
		return
	}
	if !c.b.logs.admit(len(msg)) {
		c.b.tel.LogDropped()
		return
	}
	c.logBytes += len(msg)
	if truncated {
		c.b.tel.LogTruncated()
	}
	text := c.redact(validUTF8(msg))
	attrs := []slog.Attr{
		slog.String(catalog.KeyPolicy, c.b.policy),
		slog.String(catalog.KeyPlugin, c.b.plugin),
		slog.Bool(catalog.KeyTruncated, truncated),
	}
	if !c.configuring {
		attrs = append(attrs, slog.String(catalog.KeyPhase, c.ph.String()))
		if c.x != nil {
			attrs = append(attrs, slog.String(catalog.KeyTraceID, c.x.RequestID()))
		}
	}
	attrs = append(attrs, slog.String(catalog.KeyGuestText, text))
	c.b.logger.LogAttrs(c.logContext(), slogLevel(level), "plugin log", attrs...)
}

// slogLevel maps a guest level to a slog level.
func slogLevel(level int32) slog.Level {
	switch level {
	case levelDebug:
		return slog.LevelDebug
	case levelWarn:
		return slog.LevelWarn
	case levelError:
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// validUTF8 returns msg with invalid UTF-8 replaced.
func validUTF8(msg []byte) string {
	if utf8.Valid(msg) {
		return string(msg)
	}
	return strings.ToValidUTF8(string(msg), "�")
}

// redact replaces the request's credential header values the call may not
// read, and the Policy's resolved secrets, in a guest log line.
func (c *call) redact(s string) string {
	var known [][]byte
	if p := c.inst; p != nil && p.p != nil {
		known = append(known, p.p.secrets...)
	}
	if c.x != nil {
		for k, vs := range c.x.Header() {
			if !c.b.creds.Credential(k) {
				continue
			}
			for _, v := range vs {
				known = append(known, []byte(v))
			}
		}
	}
	if len(known) == 0 {
		return s
	}
	b := []byte(s)
	for _, k := range known {
		if len(k) < 4 {
			// Short values would redact ordinary text; credentials and
			// secrets are longer.
			continue
		}
		b = bytes.ReplaceAll(b, k, []byte(redact.Marker))
	}
	return string(b)
}

// logContext is the context guest logs carry: the request's.
func (c *call) logContext() context.Context {
	if c.reqCtx != nil {
		return c.reqCtx
	}
	return context.Background()
}
