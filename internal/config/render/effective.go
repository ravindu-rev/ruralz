// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package render

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"text/tabwriter"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/precedence"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// ErrUnknownRoute marks an --effective --route that names no Route of the
// Bundle.
var ErrUnknownRoute = errors.New("render: unknown Route")

// Layout of the --effective text table: text/tabwriter with these
// settings and space padding, so each column is as wide as its widest
// cell plus two spaces and the last column is not padded.
const (
	tableMinWidth = 0
	tableTabWidth = 8
	tablePadding  = 2
)

// tableHeader is the first line of the --effective text table: the
// columns of the configuration model's worked example.
const tableHeader = "PHASE\tLEG\tPOLICY\tFROM\tREASON\n"

// Effective returns the rows of the effective Filter Chain of route in v,
// as Resolver.Rows writes them (r supplies the Policy type registry; nil
// takes precedence's default registry): the Phases in execution order,
// within a Phase the client leg and then each upstream leg in execution
// order, then the Gateway Policies the Route removed with Phase and Leg
// "none". The rows are the ones the data plane builds its per-Phase arrays
// from, so the table and runtime order cannot diverge. A route that names
// no Route of v is ErrUnknownRoute, with the nearest Route name as a hint;
// a v without a Bundle, or a Route without a chain, is ErrInput.
func Effective(v *hub.Validated, route string, r *precedence.Resolver) ([]precedence.Row, error) {
	if v == nil || v.Bundle == nil {
		return nil, fmt.Errorf("%w: no validated Bundle", ErrInput)
	}
	if _, ok := v.Bundle.Route(route); !ok {
		return nil, unknownRoute(v.Bundle, route)
	}
	c, ok := v.Chains[route]
	if !ok || c == nil {
		return nil, fmt.Errorf("%w: Route %q has no effective Filter Chain", ErrInput, route)
	}
	rows := r.Rows(c, v.Bundle)
	if rows == nil {
		rows = []precedence.Row{}
	}
	return rows, nil
}

// unknownRoute returns the ErrUnknownRoute error for route, hinting the
// nearest Route name of b. The names are sorted first, since diag.Nearest
// compares a bounded number of candidates, so the hint never depends on
// the order of b.
func unknownRoute(b *hub.Bundle, route string) error {
	var names []string
	for _, res := range b.Resources() {
		if res.Kind == v1alpha1.KindRoute {
			names = append(names, res.Name)
		}
	}
	slices.Sort(names)
	err := fmt.Errorf("%w %s", ErrUnknownRoute, strconv.Quote(clip(route)))
	if near, ok := diag.Nearest(route, names); ok {
		err = fmt.Errorf("%w (did you mean %s?)", err, strconv.Quote(near))
	}
	return err
}

// WriteEffective writes rows, as Effective returns them, to w in format f.
// FormatText is the table of the configuration model's worked example: a
// header line PHASE, LEG, POLICY, FROM, REASON and one line per row,
// aligned by text/tabwriter (minimum width 0, tab width 8, padding 2,
// spaces). FormatJSON is the rows as one JSON array, indented by two
// spaces, with HTML characters unescaped and each row's members in the
// order of precedence.Row (phase, leg, policy, type, filterClass, slot,
// from, reason); no rows is []. Both end with a newline. The output is
// built in memory and written with one Write; any other format is
// ErrFormat.
func WriteEffective(w io.Writer, rows []precedence.Row, f Format) error {
	var buf bytes.Buffer
	switch f {
	case FormatText:
		tw := tabwriter.NewWriter(&buf, tableMinWidth, tableTabWidth, tablePadding, ' ', 0)
		_, _ = io.WriteString(tw, tableHeader)
		for _, r := range rows {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.Phase, r.Leg, r.Policy, r.From, r.Reason)
		}
		if err := tw.Flush(); err != nil {
			return fmt.Errorf("render: %w", err)
		}
	case FormatJSON:
		if rows == nil {
			rows = []precedence.Row{}
		}
		enc := json.NewEncoder(&buf)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		if err := enc.Encode(rows); err != nil {
			return fmt.Errorf("render: %w", err)
		}
	default:
		return fmt.Errorf("%w: %s for an effective table", ErrFormat, f)
	}
	if _, err := w.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("render: write: %w", err)
	}
	return nil
}
