// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package header

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/httpfield"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

var _ hub.PolicyCheck = Check

// Config members, as findings name them.
const (
	fieldRequest  = "request"
	fieldResponse = "response"
	fieldSet      = "set"
	fieldAdd      = "add"
	fieldRemove   = "remove"
	fieldName     = "name"
	fieldValue    = "value"
)

// op is one set or add entry with the request and response entry types
// unified.
type op struct {
	name       string
	value      *string
	expression string
}

// ops is one side of a config with its entries unified.
type ops struct {
	// side is the config member, request or response.
	side     string
	set, add []op
	remove   []string
}

// requestOps unifies config.request; nil is an empty side.
func requestOps(o *v1alpha1.HeaderRequestOps) ops {
	out := ops{side: fieldRequest}
	if o == nil {
		return out
	}
	for _, e := range o.Set {
		out.set = append(out.set, op{e.Name, e.Value, e.ValueExpression})
	}
	for _, e := range o.Add {
		out.add = append(out.add, op{e.Name, e.Value, e.ValueExpression})
	}
	out.remove = o.Remove
	return out
}

// responseOps unifies config.response; nil is an empty side.
func responseOps(o *v1alpha1.HeaderResponseOps) ops {
	out := ops{side: fieldResponse}
	if o == nil {
		return out
	}
	for _, e := range o.Set {
		out.set = append(out.set, op{e.Name, e.Value, e.ValueExpression})
	}
	for _, e := range o.Add {
		out.add = append(out.add, op{e.Name, e.Value, e.ValueExpression})
	}
	out.remove = o.Remove
	return out
}

// entries returns the number of entries of the side.
func (o ops) entries() int { return len(o.set) + len(o.add) + len(o.remove) }

// finding is one RZ-CFG-005 finding at a path under spec.config.
type finding struct {
	path    diag.Path
	message string
	// first is the earlier entry a duplicate name repeats; nil otherwise.
	first diag.Path
	// firstMessage labels first in a diagnostic's related location.
	firstMessage string
	// schema marks a finding the JSON Schema reports itself: Build
	// refuses it and Check leaves it out.
	schema bool
}

// findings returns what Build refuses in cfg, in field order: the entry
// count first, then each side's set, add and remove entries (package
// documentation, "Configuration").
func findings(cfg *v1alpha1.HeadersConfig) []finding {
	sides := [...]ops{requestOps(cfg.Request), responseOps(cfg.Response)}
	var out []finding
	if n := sides[0].entries() + sides[1].entries(); n > MaxEntries {
		out = append(out, finding{
			message: strconv.Itoa(n) + " entries across the set, add and remove lists, more than " + strconv.Itoa(MaxEntries),
		})
	}
	for _, o := range sides {
		out = o.findings(out)
	}
	return out
}

// findings appends the findings of one side.
func (o ops) findings(out []finding) []finding {
	for _, list := range [...]struct {
		field   string
		entries []op
	}{{fieldSet, o.set}, {fieldAdd, o.add}} {
		names := nameIndex{keyed: list.field == fieldSet}
		for i, e := range list.entries {
			at := diag.Path{diag.Field(o.side), diag.Field(list.field), diag.Index(i)}
			if names.keyed {
				at[2] = diag.Keyed(fieldName, e.name)
			}
			out = names.check(out, at.Append(diag.Field(fieldName)), e.name)
			switch {
			case e.value != nil && e.expression != "":
				out = append(out, finding{path: at, message: "both value and valueExpression are set", schema: true})
			case e.value == nil && e.expression == "":
				out = append(out, finding{path: at, message: "neither value nor a non-empty valueExpression is set"})
			case e.value != nil:
				if err := httpfield.CheckValue(*e.value); err != nil {
					out = append(out, finding{path: at.Append(diag.Field(fieldValue)), message: fieldError(err)})
				}
			}
		}
	}
	names := nameIndex{keyed: true}
	for _, name := range o.remove {
		out = names.check(out, diag.Path{diag.Field(o.side), diag.Field(fieldRemove), diag.Item(name)}, name)
	}
	return out
}

// nameIndex tracks the names of one list to find two that are equal
// ignoring case. keyed is set for a map or set list, whose byte-equal
// names the schema already reports as duplicate entries.
type nameIndex struct {
	keyed bool
	seen  map[string]named
}

// named is the first entry of one canonical key.
type named struct {
	name string
	at   diag.Path
}

// check appends the findings of name at path at: not a field name,
// managed by the Node, or equal ignoring case to an earlier name of the
// list.
func (x *nameIndex) check(out []finding, at diag.Path, name string) []finding {
	if err := httpfield.CheckName(name); err != nil {
		return append(out, finding{path: at, message: fieldError(err)})
	}
	if p := httpfield.Protect(name); p != httpfield.Unprotected {
		out = append(out, finding{path: at, message: p.Message(name)})
	}
	key := http.CanonicalHeaderKey(name)
	first, dup := x.seen[key]
	if !dup {
		if x.seen == nil {
			x.seen = map[string]named{}
		}
		x.seen[key] = named{name: name, at: at}
		return out
	}
	label := "first entry at"
	if at[len(at)-1].Kind == diag.ElemItem {
		label = "first element at"
	}
	return append(out, finding{
		path:         at,
		message:      strconv.Quote(name) + " names the same field as " + strconv.Quote(first.name) + ", ignoring case",
		first:        first.at,
		firstMessage: label,
		schema:       x.keyed && name == first.name,
	})
}

// fieldError is the diagnostic text of an httpfield name or value error:
// its text without the package prefix. It never holds a value's bytes.
func fieldError(err error) string { return strings.TrimPrefix(err.Error(), "httpfield: ") }

// Check is the offline check of a headers Policy (a hub.PolicyCheck run at
// stage H by every binary): each finding of the package documentation,
// "Configuration", is RZ-CFG-005 at its path under spec.config, a
// duplicate name with the first entry as its related location; findings
// the schema reports itself are left out. It compiles no CEL: stage J
// compiles every valueExpression. A resource whose Config is not a decoded
// HeadersConfig yields nothing.
func Check(r *hub.Resource, loc func(diag.Path) diag.Location, add func(diag.Diagnostic)) {
	if r == nil || add == nil {
		return
	}
	cfg, ok := r.Config.(*v1alpha1.HeadersConfig)
	if !ok || cfg == nil {
		return
	}
	root := diag.Path{diag.Field("spec"), diag.Field("config")}
	locate := func(p diag.Path) diag.Location {
		if loc == nil {
			return diag.Location{}
		}
		return loc(p)
	}
	for _, f := range findings(cfg) {
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
