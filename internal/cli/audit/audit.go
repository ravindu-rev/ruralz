// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package audit holds the rules and the finding model of ruralz bundle
// audit (docs/reference/01-cli-and-api-surface.md "Command table",
// "Global flags" and "Output formats and exit codes"): security and
// best-practice findings on the rendered Bundle of one run, beside the
// diagnostics the configuration pipeline reports for it.
//
// # Findings
//
// A Finding is one line of the audit: a rule finding of Check, or a
// diagnostic of the run, whose rule is its RZ code (FromDiagnostic). The
// text form is the diagnostic text form of the configuration model
// ("Diagnostics and source map") with the rule in the code position,
// "<file>:<line>:<column> <severity> <rule> <Kind>/<name> <path>:
// <message>", then the hint, the related locations and
// "[environment=<name>]" of a multi-Environment run. WriteJSON writes one
// array of objects whose members are, in order, severity, rule, resource
// and message, then, when set, file, line, column, path, hint, related and
// environment.
//
// # Rules
//
// Check runs every rule on the hub.Validated of one run without error, in
// canonical resource order, and returns the findings sorted. A finding
// sits at its field's source location, or at the nearest authored ancestor
// when the field is missing or a default, as diagnostics do; a
// resource-level finding sits at the resource's start.
//
//   - route-without-auth (warning): a Route whose effective Filter Chain
//     runs no Policy of Filter class auth in its client leg
//     (docs/features/01-feature-catalog.md "Configuration audit", foundation
//     pack section 9). Gateway Policies a Route inherits count; ones it
//     excludes do not.
//   - cleartext-hop (warning): a cleartext hop, which
//     docs/architecture/08-security-and-identity.md "Transport security"
//     makes a degraded state and audit finding: a Gateway listener with
//     protocol http, an Upstream that is not protocol ai and has no
//     spec.tls, a Gateway telemetry.otlp.endpoint with scheme http, and an
//     AIProvider baseUrl with scheme http.
//   - The Capability findings of docs/architecture/05-wasm-plugin-system.md
//     "Deny-by-default", warnings at each granted Capability of a Plugin:
//     plugin-credential-exposure for credentials.read, always flagged, and
//     request.body.read; plugin-payload-exposure for response.body.read and
//     stream.read; plugin-can-deny-traffic for response.send;
//     plugin-state-round-trip for state.read and state.write; and
//     plugin-exfiltration-path for log.write and metrics.write when the
//     Plugin also grants credentials.read, request.body.read or
//     state.write.
//   - plugin-enforcing-open (error): a plugin Policy with failureMode open
//     whose Plugin enforces, granting response.send, request.body.write,
//     response.body.write or stream.write, which a fault breaker trip
//     bypasses.
//   - Pool sizing, by the same document's "Default limits": the plugin
//     Policies some Route's effective chain runs share a pool when their
//     Plugin's image digest, canonical config (RFC 8785) and limits are
//     equal. A pool is a state.* pool when its Plugin grants state.read or
//     state.write; it holds N = floor(share / limits.memoryBytes)
//     instances, the share being half of the Gateway's
//     limits.maxPluginMemoryBytes, the Node cap, for a state.* pool and a
//     quarter otherwise. Each Plugin of a state.* pool whose N is below 4
//     is plugin-pool-small (error); one whose guaranteed parked bound,
//     N - ceil(N/4), is below 16 is plugin-parked-bound-low (warning). The
//     warm reservations, one instance of limits.memoryBytes per pool,
//     above half of the Node cap are plugin-warm-reservation (warning), at
//     the Gateway.
//
// Key-like values in plaintext Plugin config are the run's own RZ-CFG-013
// warnings, which the audit prints with the rest of its findings.
//
// # Effects
//
// The package reads no file, environment or clock; every function is safe
// for concurrent use and leaves its inputs unchanged.
package audit

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"slices"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
)

// Rule names of Check.
const (
	// RuleRouteWithoutAuth is a Route that runs no authentication Policy.
	RuleRouteWithoutAuth = "route-without-auth"
	// RuleCleartextHop is a listener, Upstream, OTLP endpoint or
	// AIProvider without TLS.
	RuleCleartextHop = "cleartext-hop"
	// RulePluginCredentialExposure is a credentials.read or
	// request.body.read grant.
	RulePluginCredentialExposure = "plugin-credential-exposure" //nolint:gosec // G101: a rule name, not a credential.
	// RulePluginPayloadExposure is a response.body.read or stream.read
	// grant.
	RulePluginPayloadExposure = "plugin-payload-exposure"
	// RulePluginCanDenyTraffic is a response.send grant.
	RulePluginCanDenyTraffic = "plugin-can-deny-traffic"
	// RulePluginStateRoundTrip is a state.read or state.write grant.
	RulePluginStateRoundTrip = "plugin-state-round-trip"
	// RulePluginExfiltrationPath is a log.write or metrics.write grant
	// beside a High-risk read or state.write.
	RulePluginExfiltrationPath = "plugin-exfiltration-path"
	// RulePluginEnforcingOpen is an enforcing plugin Policy with
	// failureMode open.
	RulePluginEnforcingOpen = "plugin-enforcing-open"
	// RulePluginPoolSmall is a state.* pool of fewer than 4 instances.
	RulePluginPoolSmall = "plugin-pool-small"
	// RulePluginParkedBoundLow is a state.* pool whose guaranteed parked
	// bound is below 16.
	RulePluginParkedBoundLow = "plugin-parked-bound-low"
	// RulePluginWarmReservation is warm Plugin reservations above half of
	// the Node cap.
	RulePluginWarmReservation = "plugin-warm-reservation"
)

// RuleInfo describes one rule of Check: its name and the severity of
// every finding it reports.
type RuleInfo struct {
	// Name is the rule name, the rule member of a finding.
	Name string
	// Severity is the severity of the rule's findings.
	Severity diag.Severity
}

// Rules returns every rule of Check in documentation order.
func Rules() []RuleInfo {
	return []RuleInfo{
		{RuleRouteWithoutAuth, diag.SeverityWarning},
		{RuleCleartextHop, diag.SeverityWarning},
		{RulePluginCredentialExposure, diag.SeverityWarning},
		{RulePluginPayloadExposure, diag.SeverityWarning},
		{RulePluginCanDenyTraffic, diag.SeverityWarning},
		{RulePluginStateRoundTrip, diag.SeverityWarning},
		{RulePluginExfiltrationPath, diag.SeverityWarning},
		{RulePluginEnforcingOpen, diag.SeverityError},
		{RulePluginPoolSmall, diag.SeverityError},
		{RulePluginParkedBoundLow, diag.SeverityWarning},
		{RulePluginWarmReservation, diag.SeverityWarning},
	}
}

// Finding is one entry of an audit: a rule finding, or a diagnostic of the
// run whose rule is its RZ code.
type Finding struct {
	// Rule is the rule name, or the RZ code of a diagnostic.
	Rule string
	// Severity is error or warning.
	Severity diag.Severity
	diag.Location
	// Resource is the resource the finding is about, when known.
	Resource *diag.ResourceID
	// Path is the key-aware path inside the resource.
	Path diag.Path
	// Message is one line; it never contains a secret value.
	Message string
	// Hint is optional, such as a nearest match.
	Hint string
	// Related lists secondary locations.
	Related []diag.Related
	// Environment names the Environment of a multi-Environment run.
	Environment string
}

// FromDiagnostic returns the finding of the diagnostic d: its code is the
// rule.
func FromDiagnostic(d diag.Diagnostic) Finding {
	return Finding{
		Rule: d.Code, Severity: d.Severity, Location: d.Location, Resource: d.Resource, Path: d.Path,
		Message: d.Message, Hint: d.Hint, Related: d.Related, Environment: d.Environment,
	}
}

// FromDiagnostics returns the finding of each diagnostic of ds, in order.
func FromDiagnostics(ds diag.List) []Finding {
	out := make([]Finding, 0, len(ds))
	for _, d := range ds {
		out = append(out, FromDiagnostic(d))
	}
	return out
}

// Diagnostic returns f as a diagnostic whose code is the rule, the form
// the text line and its colors are written in.
func (f Finding) Diagnostic() diag.Diagnostic {
	return diag.Diagnostic{
		Code: f.Rule, Severity: f.Severity, Location: f.Location, Resource: f.Resource, Path: f.Path,
		Message: f.Message, Hint: f.Hint, Related: f.Related, Environment: f.Environment,
	}
}

// AppendText appends the one-line text form of f: the diagnostic text
// form with the rule in the code position.
func (f Finding) AppendText(dst []byte) []byte { return f.Diagnostic().AppendText(dst) }

// IsError reports whether f has error severity: any Severity but
// SeverityWarning, as diag.List.HasErrors counts it.
func (f Finding) IsError() bool { return f.Severity != diag.SeverityWarning }

// HasErrors reports whether any finding of fs is an error.
func HasErrors(fs []Finding) bool { return slices.ContainsFunc(fs, Finding.IsError) }

// Sort orders fs as diag.List.Sort orders diagnostics, the rule standing
// for the code: by environment, file, line, column, rule, path and
// message.
func Sort(fs []Finding) {
	slices.SortStableFunc(fs, func(a, b Finding) int {
		return cmp.Or(
			cmp.Compare(a.Environment, b.Environment),
			cmp.Compare(a.File, b.File),
			cmp.Compare(a.Line, b.Line),
			cmp.Compare(a.Column, b.Column),
			cmp.Compare(a.Rule, b.Rule),
			cmp.Compare(a.Path.String(), b.Path.String()),
			cmp.Compare(a.Message, b.Message),
		)
	})
}

// WriteText writes one text line per finding of fs, in order.
func WriteText(w io.Writer, fs []Finding) error {
	var buf []byte
	for _, f := range fs {
		buf = append(f.AppendText(buf), '\n')
	}
	if _, err := w.Write(buf); err != nil {
		return fmt.Errorf("audit: write: %w", err)
	}
	return nil
}

// jsonRelated is a related location of the JSON form, as diagnostics
// write it.
type jsonRelated struct {
	File    string `json:"file,omitempty"`
	Line    int    `json:"line,omitempty"`
	Column  int    `json:"column,omitempty"`
	Message string `json:"message,omitempty"`
}

// jsonFinding is one object of the JSON form: severity, rule, resource and
// message, then the members that are set.
type jsonFinding struct {
	Severity    diag.Severity    `json:"severity"`
	Rule        string           `json:"rule"`
	Resource    *diag.ResourceID `json:"resource,omitempty"`
	Message     string           `json:"message"`
	File        string           `json:"file,omitempty"`
	Line        int              `json:"line,omitempty"`
	Column      int              `json:"column,omitempty"`
	Path        diag.Path        `json:"path,omitempty"`
	Hint        string           `json:"hint,omitempty"`
	Related     []jsonRelated    `json:"related,omitempty"`
	Environment string           `json:"environment,omitempty"`
}

// WriteJSON writes fs as one JSON array, "[]" when empty: two-space
// indentation, no HTML escaping and a final newline, each finding an
// object of the members of jsonFinding. The array is written with one
// Write call, so nothing reaches w when encoding fails.
func WriteJSON(w io.Writer, fs []Finding) error {
	out := make([]jsonFinding, 0, len(fs))
	for _, f := range fs {
		jf := jsonFinding{
			Severity: f.Severity, Rule: f.Rule, Resource: f.Resource, Message: f.Message,
			File: f.File, Line: f.Line, Column: f.Column, Path: f.Path, Hint: f.Hint, Environment: f.Environment,
		}
		for _, r := range f.Related {
			jf.Related = append(jf.Related, jsonRelated{File: r.File, Line: r.Line, Column: r.Column, Message: r.Message})
		}
		out = append(out, jf)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		return fmt.Errorf("audit: encode findings: %w", err)
	}
	if _, err := w.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("audit: write: %w", err)
	}
	return nil
}
