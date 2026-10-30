// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package dev

import (
	"bytes"
	"errors"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/cli/command"
	"github.com/ravindu-rev/ruralz/internal/cli/launch"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
)

// Events of ruralz dev run --output json.
const (
	eventReady    = "ready"
	eventActive   = "active"
	eventRejected = "rejected"
)

// runEvent is one NDJSON object of ruralz dev run --output json
// (docs/reference/01-cli-and-api-surface.md "Output formats and exit
// codes"): the event and the test render ruralzd serves after it, the one
// it activated for ready and active, the one it still serves for
// rejected.
type runEvent struct {
	Event          string          `json:"event"`
	Revision       string          `json:"revision"`
	Digest         string          `json:"digest"`
	TestDigest     string          `json:"testDigest"`
	Admin          string          `json:"admin"`
	AdminTokenFile string          `json:"adminTokenFile"`
	Listeners      []eventListener `json:"listeners"`
	Ports          []eventPort     `json:"ports"`
}

// eventListener is one listener of a runEvent.
type eventListener struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// eventPort is one rewritten port of a runEvent.
type eventPort struct {
	Name     string `json:"name"`
	Port     int    `json:"port"`
	TestPort int    `json:"testPort"`
}

// event returns the NDJSON object of name for rend.
func (r *runner) event(name string, rend *launch.Render) runEvent {
	ev := runEvent{
		Event: name, Revision: rend.Revision.Short(), Digest: rend.Revision.String(),
		TestDigest: rend.TestDigest.String(), Admin: rend.AdminURL(), AdminTokenFile: r.ws.TokenFile(),
		Listeners: []eventListener{}, Ports: []eventPort{},
	}
	for _, l := range rend.Listeners {
		ev.Listeners = append(ev.Listeners, eventListener{Name: l.Name, URL: l.URL()})
	}
	for _, p := range rend.Ports() {
		ev.Ports = append(ev.Ports, eventPort{Name: p.Name, Port: p.Port, TestPort: p.TestPort})
	}
	return ev
}

// printReady prints the readiness block, or the ready event: the Revision
// as rev-<12 hex> and its full digest, the test digest, the admin URL, the
// token file, one listener line per listener, the port mapping and a
// ruralz dev tap command line to paste.
func (r *runner) printReady(rend *launch.Render) error {
	if r.json {
		return r.writeEvent(eventReady, rend)
	}
	lines := []string{
		"revision: " + rend.Revision.Short() + " " + rend.Revision.String(),
		"testDigest: " + rend.TestDigest.String(),
		"admin: " + rend.AdminURL(),
		"adminTokenFile: " + r.ws.TokenFile(),
	}
	for _, l := range rend.Listeners {
		lines = append(lines, "listener "+l.Name+": "+l.URL())
	}
	lines = append(lines, rend.Mapping()...)
	lines = append(lines, "ruralz dev tap --admin "+shellWord(rend.AdminURL())+" --admin-token-file "+shellWord(r.ws.TokenFile()))
	return r.writeLines(lines)
}

// printActive reports an activated swap: "active rev-<12 hex>", then the
// port mapping when it changed; or the active event.
func (r *runner) printActive(rend, prev *launch.Render) error {
	if r.json {
		return r.writeEvent(eventActive, rend)
	}
	lines := []string{"active " + rend.Revision.Short()}
	if !rend.SameMapping(prev) {
		lines = append(lines, rend.Mapping()...)
	}
	return r.writeLines(lines)
}

// printRejected reports a swap /readyz never named within the ready
// timeout: "rejected, still serving rev-<12 hex>", or the rejected event
// with the test render ruralzd still serves.
func (r *runner) printRejected() error {
	if r.json {
		return r.writeEvent(eventRejected, r.active)
	}
	return r.writeLines([]string{"rejected, still serving " + r.active.Revision.Short()})
}

// writeEvent writes one NDJSON object.
func (r *runner) writeEvent(name string, rend *launch.Render) error {
	if err := r.out.Object(r.event(name, rend)); err != nil {
		return command.NoResultf("write standard output: %w", err)
	}
	return nil
}

// writeLines writes text lines to standard output.
func (r *runner) writeLines(lines []string) error {
	for _, l := range lines {
		if err := r.out.Line(l); err != nil {
			return command.NoResultf("write standard output: %w", err)
		}
	}
	return nil
}

// printWarnings prints the warnings of a valid render on standard error:
// its diagnostics, then lines such as unused secret overrides.
func (r *runner) printWarnings(rend *launch.Render) {
	r.printDiagnostics(rend.Diagnostics)
	for _, w := range rend.Warnings {
		_ = r.errw.Line(w)
	}
}

// printFailure prints why a render failed on standard error: the
// diagnostics of a *launch.Failure, else the error.
func (r *runner) printFailure(err error) {
	var f *launch.Failure
	if errors.As(err, &f) {
		r.printDiagnostics(f.Diagnostics)
		return
	}
	_ = r.errw.Line("ruralz dev run: " + strings.TrimRight(err.Error(), "\n"))
}

// printDiagnostics writes one text line per diagnostic on standard error,
// its severity colored on a terminal that takes color: error red, warning
// yellow.
func (r *runner) printDiagnostics(ds diag.List) {
	for _, d := range ds {
		line := d.AppendText(nil)
		if r.colorErr {
			line = colorSeverity(line, d)
		}
		_ = r.errw.Raw(line)
	}
}

// colorSeverity returns line, the text form of d, with its severity word
// wrapped in its color; the word follows the location and one space, and a
// line of another shape is returned as it is.
func colorSeverity(line []byte, d diag.Diagnostic) []byte {
	sev := d.Severity.String()
	color := ansiRed
	if d.Severity == diag.SeverityWarning {
		color = ansiYellow
	}
	i := bytes.Index(line, []byte(" "+sev+" "))
	if i < 0 {
		return line
	}
	out := make([]byte, 0, len(line)+len(color)+len(ansiReset))
	out = append(out, line[:i+1]...)
	out = append(out, color...)
	out = append(out, sev...)
	out = append(out, ansiReset...)
	return append(out, line[i+1+len(sev):]...)
}

// shellWord returns s as one shell word: bare when it holds only
// characters no shell treats specially, else in single quotes.
func shellWord(s string) string {
	safe := s != ""
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-_./:@%+=,", c)) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
