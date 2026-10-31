// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package render

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/profile"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// CodeLiteral is RZ-CFG-012: an x-ruralz-secret position whose value is not
// a well-formed reference, which render refuses to write.
const CodeLiteral = "RZ-CFG-012"

// DefaultMaxDiagnostics is the cap on the RZ-CFG-012 findings of one
// render when Options.MaxDiagnostics is zero: the per-run cap of the
// pipeline (10,000, target).
const DefaultMaxDiagnostics = 10_000

// Errors of the rendered Bundle.
var (
	// ErrOptions marks options a writer cannot work with, such as a
	// missing schema index.
	ErrOptions = errors.New("render: invalid options")
	// ErrInput marks input that cannot have come from a validated Bundle:
	// a nil resource, a root that is not a mapping, an apiVersion or kind
	// without a schema, or a Route chain the pipeline did not compute.
	ErrInput = errors.New("render: input is not a validated Bundle")
)

// envelopeOrder returns the members a rendered resource starts with, in
// this order; every other member follows in authored order.
func envelopeOrder() []string { return []string{"apiVersion", "kind", "metadata", "spec"} }

// metadataOrder returns the members a rendered metadata object starts
// with, in this order, as envelopeOrder does for the resource.
func metadataOrder() []string { return []string{"name", "labels", "annotations"} }

// Options configure Documents and WriteBundle.
type Options struct {
	// Schemas indexes the rendered schema of every served apiVersion
	// (schemaidx.Embedded). The secret check reads the x-ruralz-secret
	// positions of each resource from the index of its own apiVersion.
	// Required.
	Schemas *schemaidx.Set
	// Files converts tree positions to diagnostic locations; nil leaves
	// the file names empty.
	Files *tree.FileTable
	// MaxDiagnostics caps the RZ-CFG-012 findings of one call
	// (DefaultMaxDiagnostics when zero or less); one more finding stops
	// the check with the RZ-CFG-001 "too many diagnostics" error of
	// diag.Collector.
	MaxDiagnostics int
}

// Documents returns the documents of the rendered Bundle of rs, the
// resources of one Environment as stage E leaves them: overlay merged,
// ${VAR} substituted and validated by the pipeline, but neither defaulted
// nor normalized. The documents are in canonical order, by kind
// (hub.KindOrder) and then name bytewise, whatever the order of rs. Each
// is a copy of its resource's envelope whose members start with
// apiVersion, kind, metadata and spec, and whose metadata starts with
// name, labels and annotations; every other member keeps its authored
// order. Only those two objects are copied; the values below them are
// shared with rs, which Documents never changes, so a caller that changes
// a document, as ruralz dev run --ephemeral-ports changes ports, changes a
// copy (tree.Node.Clone) and writes it with Encode.
//
// Before it returns a document, Documents checks every x-ruralz-secret
// position of every resource: a value there that is not a well-formed
// reference (a position the schema does not declare, a value of a type or
// an enumerated value the schema does not admit) is reported as
// RZ-CFG-012 at the secret field, naming neither the value nor any member
// below the field, and then no document is returned. A resource whose
// apiVersion or kind has no schema in o.Schemas, a nil resource and a root
// that is not a mapping are ErrInput. ctx is checked between resources.
func Documents(ctx context.Context, rs []*tree.Resource, o Options) ([]*tree.Node, diag.List, error) {
	if o.Schemas == nil {
		return nil, nil, fmt.Errorf("%w: no schema index", ErrOptions)
	}
	sorted, err := canonicalOrder(rs)
	if err != nil {
		return nil, nil, err
	}
	found := diag.NewCollector(cmp.Or(max(o.MaxDiagnostics, 0), DefaultMaxDiagnostics))
	for _, r := range sorted {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if err := checkSecrets(o.Schemas, r, o.Files, found); err != nil {
			return nil, nil, err
		}
		if found.Full() {
			break
		}
	}
	if ds := found.List(); len(ds) > 0 {
		return nil, ds, nil
	}
	docs := make([]*tree.Node, len(sorted))
	for i, r := range sorted {
		docs[i] = envelope(r.Root)
	}
	return docs, nil, nil
}

// WriteBundle writes the rendered Bundle of rs (Documents) to w in format
// f with Encode. RZ-CFG-012 findings are returned as from Documents, with
// nothing written; a format other than FormatYAML and FormatJSON is
// ErrFormat.
func WriteBundle(ctx context.Context, w io.Writer, rs []*tree.Resource, f Format, o Options) (diag.List, error) {
	if f != FormatYAML && f != FormatJSON {
		return nil, fmt.Errorf("%w: %s for a rendered Bundle", ErrFormat, f)
	}
	docs, ds, err := Documents(ctx, rs, o)
	if err != nil || len(ds) > 0 {
		return ds, err
	}
	return nil, Encode(w, docs, f)
}

// Encode writes docs, as Documents returns them, to w in format f:
// FormatYAML writes one YAML stream that is itself a one-file Bundle,
// FormatJSON the same documents, in the same order, as one JSON array
// indented by two spaces. Both forms write every literal ${ of a string
// value as $${, the form substitution reads back as ${, so loading the
// output yields the same values, and end with a newline (profile.Encode
// and profile.EncodeJSON give the scalar forms). The output is built in
// memory and written with one Write, so a failure writes nothing. A
// format other than FormatYAML and FormatJSON is ErrFormat.
func Encode(w io.Writer, docs []*tree.Node, f Format) error {
	var buf bytes.Buffer
	var err error
	switch f {
	case FormatYAML:
		err = profile.Encode(&buf, docs)
	case FormatJSON:
		err = profile.EncodeJSON(&buf, &tree.Node{Kind: tree.KindList, Items: docs}, profile.EncodeOptions{})
	default:
		return fmt.Errorf("%w: %s for a rendered Bundle", ErrFormat, f)
	}
	if err != nil {
		return fmt.Errorf("render: %w", err)
	}
	if _, err := w.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("render: write: %w", err)
	}
	return nil
}

// canonicalOrder returns a copy of rs sorted by kind (hub.KindOrder) and
// then name bytewise, the order of the Revision; a nil resource or a root
// that is not a mapping is ErrInput.
func canonicalOrder(rs []*tree.Resource) ([]*tree.Resource, error) {
	for i, r := range rs {
		if r == nil || r.Root == nil || r.Root.Kind != tree.KindMap {
			return nil, fmt.Errorf("%w: resource %d has no envelope mapping", ErrInput, i)
		}
	}
	out := slices.Clone(rs)
	slices.SortStableFunc(out, func(a, b *tree.Resource) int {
		return cmp.Or(cmp.Compare(hub.KindOrder(a.ID.Kind), hub.KindOrder(b.ID.Kind)), cmp.Compare(a.ID.Name, b.ID.Name))
	})
	return out, nil
}

// envelope returns a copy of root with its members in envelope order and
// a copy of its metadata object, when it is a mapping, with its members in
// metadata order; everything below is shared with root.
func envelope(root *tree.Node) *tree.Node {
	out := *root
	out.Members = ordered(root.Members, envelopeOrder())
	for i, m := range out.Members {
		if m.Key == "metadata" && m.Value != nil && m.Value.Kind == tree.KindMap {
			meta := *m.Value
			meta.Members = ordered(m.Value.Members, metadataOrder())
			out.Members[i].Value = &meta
		}
	}
	return &out
}

// ordered returns a new slice of ms: the members named in first, in that
// order, then every other member in authored order. Keys are unique (stage
// B keeps the first member of a key), so no member is dropped.
func ordered(ms []tree.Member, first []string) []tree.Member {
	out := make([]tree.Member, 0, len(ms))
	for _, k := range first {
		if i := slices.IndexFunc(ms, func(m tree.Member) bool { return m.Key == k }); i >= 0 {
			out = append(out, ms[i])
		}
	}
	for _, m := range ms {
		if !slices.Contains(first, m.Key) {
			out = append(out, m)
		}
	}
	return out
}

// checkSecrets adds to found one RZ-CFG-012 finding per x-ruralz-secret
// field of r whose value is not a well-formed reference. It returns
// ErrInput when r's apiVersion or kind has no schema in schemas, since its
// secret positions are then unknown.
func checkSecrets(schemas *schemaidx.Set, r *tree.Resource, files *tree.FileTable, found *diag.Collector) error {
	idx, ok := schemas.Index(r.APIVersion)
	if !ok {
		return fmt.Errorf("%w: %s has apiVersion %q, which no schema serves", ErrInput, r.ID, clip(r.APIVersion))
	}
	if _, ok := idx.Resource(string(r.ID.Kind)); !ok {
		return fmt.Errorf("%w: %s has kind %q, which apiVersion %s does not serve", ErrInput, r.ID, clip(string(r.ID.Kind)), r.APIVersion)
	}
	s := &secretCheck{r: r, files: files, found: found, depth: -1}
	err := idx.Walk(string(r.ID.Kind), r.Root, s.visit)
	s.close()
	if err != nil && !errors.Is(err, errFull) {
		return fmt.Errorf("%w: %s: %w", ErrInput, r.ID, err)
	}
	return nil
}

// errFull stops a secret walk once the collector is full.
var errFull = errors.New("render: diagnostic cap reached")

// secretCheck is the state of one resource's secret walk: the secret
// field whose subtree the walk is in, and whether a value in it is not a
// reference.
type secretCheck struct {
	r     *tree.Resource
	files *tree.FileTable
	found *diag.Collector
	// depth is the depth of the current secret field, -1 outside one.
	depth int
	// field, at and schema describe the current secret field.
	field  diag.Path
	at     tree.Pos
	schema *schemaidx.Node
	// bad records a value in the current field that is not a reference.
	bad bool
}

// visit is the schemaidx.WalkFunc of the check. A secret field starts at
// the first position whose Info.InSecret is set (the field itself, which
// carries x-ruralz-secret) and ends when the walk returns to its depth or
// above. Every position inside one must be declared by the schema, hold a
// type the schema admits, and hold an enumerated value when the schema
// enumerates them.
func (s *secretCheck) visit(c *schemaidx.Cursor) error {
	if s.depth >= 0 && c.Depth() <= s.depth {
		s.close()
		if s.found.Full() {
			return errFull
		}
	}
	if !c.Info.InSecret {
		return nil
	}
	if s.depth < 0 {
		s.depth, s.field, s.at, s.schema = c.Depth(), c.Path(), c.Node.Pos, c.Info.Node
	}
	if s.bad {
		return schemaidx.ErrSkipChildren
	}
	if !reference(c.Info.Node, c.Node) {
		s.bad = true
		return schemaidx.ErrSkipChildren
	}
	return nil
}

// close ends the current secret field, reporting it when a value in it is
// not a reference.
func (s *secretCheck) close() {
	if s.depth >= 0 && s.bad {
		at := s.at
		if !at.Known() {
			at = s.r.Start
		}
		var loc diag.Location
		if s.files != nil {
			loc = s.files.Location(at)
		} else if at.Known() {
			loc = diag.Location{Line: int(at.Line), Column: int(at.Column)}
		}
		s.found.Add(diag.Diagnostic{
			Code: CodeLiteral, Severity: diag.SeverityError, Location: loc,
			Resource: s.r.ID.ResourceID(), Path: s.field,
			Message: "the secret field holds a value that is not a secret reference, and render writes only references",
			Hint:    "reference the secret as " + shape(s.schema, 0),
		})
	}
	s.depth, s.field, s.schema, s.bad = -1, nil, nil, false
}

// reference reports whether n may stand at the secret position whose
// schema is sch: the schema declares the position, admits n's type and,
// when it enumerates string values, holds n's value.
func reference(sch *schemaidx.Node, n *tree.Node) bool {
	if sch == nil || !sch.Types().Has(typeOf(n)) {
		return false
	}
	if enum := sch.Enum(); len(enum) > 0 {
		return n.Kind == tree.KindString && slices.Contains(enum, n.Text)
	}
	return true
}

// typeOf returns the instance type of n.
func typeOf(n *tree.Node) schemaidx.TypeSet {
	switch n.Kind {
	case tree.KindNull:
		return schemaidx.TypeNull
	case tree.KindBool:
		return schemaidx.TypeBoolean
	case tree.KindInt:
		return schemaidx.TypeInteger
	case tree.KindFloat:
		return schemaidx.TypeNumber
	case tree.KindString:
		return schemaidx.TypeString
	case tree.KindMap:
		return schemaidx.TypeObject
	case tree.KindList:
		return schemaidx.TypeArray
	default:
		return 0
	}
}

// shape describes the required members of a secret field from its schema,
// such as "{secretRef: {name, provider}}", for the RZ-CFG-012 hint.
func shape(n *schemaidx.Node, depth int) string {
	req := n.Required()
	if len(req) == 0 || depth > 2 {
		return "a secretRef"
	}
	parts := make([]string, 0, len(req))
	for _, name := range req {
		child, _ := n.Property(name)
		if len(child.Required()) > 0 && depth < 2 {
			parts = append(parts, name+": "+shape(child, depth+1))
			continue
		}
		parts = append(parts, name)
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// maxShown bounds the code points of an input value quoted in an error.
const maxShown = 64

// clip returns s cut to maxShown code points and "...", or s when it is
// shorter, so an error never carries a long input value.
func clip(s string) string {
	n := 0
	for i := range s {
		if n == maxShown {
			return s[:i] + "..."
		}
		n++
	}
	return s
}
