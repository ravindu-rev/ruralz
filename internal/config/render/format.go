// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package render

import "errors"

// Format is an output form of ruralz bundle render.
type Format uint8

// Output forms.
const (
	// FormatYAML is the rendered Bundle as one YAML stream, the default
	// without --effective (--output yaml).
	FormatYAML Format = iota + 1
	// FormatJSON is the same content as JSON (--output json): the
	// rendered Bundle's resources, or the --effective rows, as one array.
	FormatJSON
	// FormatText is the --effective table, its default (--output text).
	FormatText
)

// ErrFormat marks a Format the writer does not produce: WriteBundle writes
// FormatYAML and FormatJSON, WriteEffective FormatText and FormatJSON.
var ErrFormat = errors.New("render: unsupported output format")

// String returns the --output name: "yaml", "json" or "text".
func (f Format) String() string {
	switch f {
	case FormatYAML:
		return "yaml"
	case FormatJSON:
		return "json"
	case FormatText:
		return "text"
	default:
		return "unknown"
	}
}

// ParseFormat returns the Format an --output value names: "yaml", "json"
// or "text", matched exactly.
func ParseFormat(s string) (Format, bool) {
	for _, f := range []Format{FormatYAML, FormatJSON, FormatText} {
		if f.String() == s {
			return f, true
		}
	}
	return 0, false
}
