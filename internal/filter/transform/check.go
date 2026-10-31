// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package transform

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/filter/transform/dotpath"
	"github.com/ravindu-rev/ruralz/internal/httpfield"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Config members, as findings name them.
const (
	fieldContentType     = "contentType"
	fieldSet             = "set"
	fieldRemove          = "remove"
	fieldArrayOps        = "arrayOps"
	fieldReplace         = "replace"
	fieldTarget          = "target"
	fieldName            = "name"
	fieldValueExpression = "valueExpression"
	fieldOp              = "op"
	fieldFrom            = "from"
	fieldTo              = "to"
	fieldPath            = "path"
	fieldPattern         = "pattern"
)

// finding is one RZ-CFG-005 finding at a path under spec.config.
type finding struct {
	path    diag.Path
	message string
	// first is the earlier entry a second path entry repeats; nil
	// otherwise. firstMessage labels it in a related location.
	first        diag.Path
	firstMessage string
	// schema marks a finding the JSON Schema reports itself: Build refuses
	// it and Check leaves it out.
	schema bool
}

// findings returns what Build refuses in s, in field order: the entry
// count, contentType, then the set, remove, arrayOps and replace entries
// (package documentation, "Configuration").
func findings(s *Spec) []finding {
	var out []finding
	if n := s.entries(); n > MaxEntries {
		out = append(out, finding{
			message: strconv.Itoa(n) + " entries across the set, remove, arrayOps and replace lists, more than " + strconv.Itoa(MaxEntries),
		})
	}
	if s.ContentType != nil {
		if err := checkMediaType(*s.ContentType); err != nil {
			out = append(out, finding{path: diag.Path{diag.Field(fieldContentType)}, message: strconv.Quote(*s.ContentType) + " is " + err.Error()})
		}
	}
	rewrites := s.rewritesBody()
	firstPath := -1
	for i, e := range s.Set {
		at := diag.Path{diag.Field(fieldSet), diag.Index(i)}
		switch e.Target {
		case 0:
			out = append(out, finding{path: at.Append(diag.Field(fieldTarget)), message: unknownTarget(s.Side, e.RawTarget, true), schema: true})
		case TargetPath:
			if e.Name != "" {
				out = append(out, finding{path: at.Append(diag.Field(fieldName)), message: "target path takes no name"})
			}
			if firstPath >= 0 {
				out = append(out, finding{
					path:         at.Append(diag.Field(fieldTarget)),
					message:      "a second set entry with target path; a Policy rewrites the path once",
					first:        diag.Path{diag.Field(fieldSet), diag.Index(firstPath), diag.Field(fieldTarget)},
					firstMessage: "first entry at",
				})
			} else {
				firstPath = i
			}
		default:
			out = nameFindings(out, at.Append(diag.Field(fieldName)), e.Target, e.Name, rewrites)
		}
		if e.ValueExpression == "" {
			out = append(out, finding{path: at.Append(diag.Field(fieldValueExpression)), message: "valueExpression is empty"})
		}
	}
	for i, e := range s.Remove {
		at := diag.Path{diag.Field(fieldRemove), diag.Index(i)}
		switch e.Target {
		case 0:
			out = append(out, finding{path: at.Append(diag.Field(fieldTarget)), message: unknownTarget(s.Side, e.RawTarget, false), schema: true})
		case TargetPath:
			out = append(out, finding{path: at.Append(diag.Field(fieldTarget)), message: "target path cannot be removed; a set entry rewrites it"})
		default:
			out = nameFindings(out, at.Append(diag.Field(fieldName)), e.Target, e.Name, rewrites)
		}
	}
	for i, a := range s.ArrayOps {
		out = arrayOpFindings(out, diag.Path{diag.Field(fieldArrayOps), diag.Index(i)}, a)
	}
	for i, r := range s.Replace {
		at := diag.Path{diag.Field(fieldReplace), diag.Index(i)}
		if r.Path != "" {
			if _, err := dotpath.Parse(r.Path); err != nil {
				out = append(out, finding{path: at.Append(diag.Field(fieldPath)), message: pathMessage(err)})
			}
		}
		out = patternFindings(out, at.Append(diag.Field(fieldPattern)), r)
	}
	return out
}

// unknownTarget is the message of a target the side does not know, which
// the schema's enum refuses.
func unknownTarget(side Side, raw string, set bool) string {
	switch {
	case side == Response:
		return "target " + strconv.Quote(raw) + " is not header or body"
	case set:
		return "target " + strconv.Quote(raw) + " is not header, query, body or path"
	default:
		return "target " + strconv.Quote(raw) + " is not header, query or body"
	}
}

// nameFindings appends the findings of the name of a header, query or body
// entry at path at: a header name that is not a field name
// (httpfield.CheckName) or that the Node manages (httpfield.Protect), or,
// in a Policy that rewrites the body, Content-Encoding or a body digest
// field; a query name that is not a field name; a body dot path Parse
// refuses.
func nameFindings(out []finding, at diag.Path, t Target, name string, rewrites bool) []finding {
	switch t {
	case TargetHeader:
		if err := httpfield.CheckName(name); err != nil {
			return append(out, finding{path: at, message: fieldError(err)})
		}
		if p := httpfield.Protect(name); p != httpfield.Unprotected {
			out = append(out, finding{path: at, message: p.Message(name)})
		}
		if rewrites && bodyField(name) {
			out = append(out, finding{path: at, message: strconv.Quote(name) + " is a field the Node drops when a Policy rewrites the body, as this one does"})
		}
	case TargetQuery:
		if err := httpfield.CheckName(name); err != nil {
			out = append(out, finding{path: at, message: "query " + fieldError(err)})
		}
	case TargetBody:
		if _, err := dotpath.Parse(name); err != nil {
			out = append(out, finding{path: at, message: pathMessage(err)})
		}
	default:
	}
	return out
}

// arrayOpFindings appends the findings of the arrayOps entry a at path at:
// an op the schema's enum refuses; a move or append without from or to,
// or a delete without from or with to, which the schema also reports when
// the member is absent; a dot path Parse refuses; and a to holding a *
// segment.
func arrayOpFindings(out []finding, at diag.Path, a v1alpha1.ArrayOp) []finding {
	switch a.Op {
	case v1alpha1.ArrayOpMove, v1alpha1.ArrayOpAppend:
		out = pathFinding(out, at.Append(diag.Field(fieldFrom)), a.From, string(a.Op)+" needs from and to")
		out = pathFinding(out, at.Append(diag.Field(fieldTo)), a.To, string(a.Op)+" needs from and to")
		if p, err := dotpath.Parse(a.To); err == nil && p.HasStar() {
			out = append(out, finding{path: at.Append(diag.Field(fieldTo)), message: "to holds a * segment; an operation writes one place"})
		}
	case v1alpha1.ArrayOpDelete:
		out = pathFinding(out, at.Append(diag.Field(fieldFrom)), a.From, "delete needs from")
		if a.To != "" {
			out = append(out, finding{path: at.Append(diag.Field(fieldTo)), message: "delete takes no to"})
		}
	default:
		out = append(out, finding{path: at.Append(diag.Field(fieldOp)), message: "op " + strconv.Quote(string(a.Op)) + " is not move, append or delete", schema: true})
	}
	return out
}

// pathFinding appends the finding of the dot path p at path at: missing
// when empty, else what Parse refuses.
func pathFinding(out []finding, at diag.Path, p, missing string) []finding {
	if p == "" {
		return append(out, finding{path: at, message: missing})
	}
	if _, err := dotpath.Parse(p); err != nil {
		out = append(out, finding{path: at, message: pathMessage(err)})
	}
	return out
}

// patternFindings appends the findings of a replace entry's pattern at
// path at: over MaxPatternBytes bytes, which the schema reports itself
// when it is also over that many characters, or not valid RE2 unless the
// entry is literal.
func patternFindings(out []finding, at diag.Path, r v1alpha1.Replace) []finding {
	if n := len(r.Pattern); n > MaxPatternBytes {
		return append(out, finding{
			path:    at,
			message: "pattern is " + strconv.Itoa(n) + " bytes, more than " + strconv.Itoa(MaxPatternBytes),
			schema:  utf8.RuneCountInString(r.Pattern) > MaxPatternBytes,
		})
	}
	if literal(r) {
		return out
	}
	if _, err := regexp.Compile(r.Pattern); err != nil {
		out = append(out, finding{path: at, message: "pattern is not valid RE2: " + strings.TrimPrefix(err.Error(), "error parsing regexp: ")})
	}
	return out
}

// bodyField reports whether name, a valid field name, is Content-Encoding
// or a body digest field (Content-Digest, Repr-Digest, Digest,
// Content-MD5), which the Node drops whenever the body is rewritten.
func bodyField(name string) bool {
	for _, f := range [...]string{"content-encoding", "content-digest", "repr-digest", "digest", "content-md5"} {
		if asciiEqualFold(name, f) {
			return true
		}
	}
	return false
}

// pathMessage is the finding text of a dot path Parse refuses: the rule
// and its byte offset.
func pathMessage(err error) string {
	return strings.TrimPrefix(err.Error(), "dotpath: ")
}

// fieldError is the diagnostic text of an httpfield name error: its text
// without the package prefix.
func fieldError(err error) string { return strings.TrimPrefix(err.Error(), "httpfield: ") }

// Report is the offline check of the transform config s, the Config of
// Policy r (a hub.PolicyCheck the type packages run at stage H in every
// binary): each finding of the package documentation, "Configuration", is
// RZ-CFG-005 at its path under spec.config, a second path entry with the
// first as its related location; findings the schema reports itself are
// left out. It compiles no CEL: stage J compiles every CEL field.
func Report(r *hub.Resource, s Spec, loc func(diag.Path) diag.Location, add func(diag.Diagnostic)) {
	if r == nil || add == nil {
		return
	}
	root := diag.Path{diag.Field("spec"), diag.Field("config")}
	locate := func(p diag.Path) diag.Location {
		if loc == nil {
			return diag.Location{}
		}
		return loc(p)
	}
	for _, f := range findings(&s) {
		if f.schema {
			continue
		}
		p := root.Append(f.path...)
		d := diag.Diagnostic{
			Code:     CodeConfig,
			Severity: diag.SeverityError,
			Location: locate(p),
			Resource: r.ResourceID(),
			Path:     p,
			Message:  f.message,
		}
		if f.first != nil && loc != nil {
			d.Related = []diag.Related{{Location: locate(root.Append(f.first...)), Message: f.firstMessage}}
		}
		add(d)
	}
}
