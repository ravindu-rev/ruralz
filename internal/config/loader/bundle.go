// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package loader

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/overlay"
	"github.com/ravindu-rev/ruralz/internal/config/profile"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/jsonval"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Bundle is a source Bundle after stages A to C: every configuration file
// read and the base union parsed and checked. Render applies an overlay
// and substitution to a copy of the base union, so one Bundle renders for
// any number of Environments, concurrently too. A Bundle is immutable.
type Bundle struct {
	l *Loader
	// oneFile reports a one-file Bundle; root is the path of its root
	// file: ruralz.yaml, or the one file as given.
	oneFile bool
	root    string
	files   *tree.FileTable
	// sources holds every configuration file read, in path order.
	sources []*source
	// base is the base union: the root file's documents, then the other
	// base files' in path order, each file's in document order.
	base []*tree.Resource
	// baseIDs holds the identities of the base union.
	baseIDs map[tree.ID]bool
	// tainted reports a base document lost before its identity was read:
	// in a file stage A refused or stage B failed, or with an envelope
	// without kind or name. lost holds the identities of base documents
	// stage C dropped. An overlay document with such an identity, or any
	// overlay addition when tainted, would be a cascade: Render withholds
	// it.
	tainted bool
	lost    map[tree.ID]bool
	// gateway is the Gateway of the root file, when it has one.
	gateway    tree.ID
	hasGateway bool
	// overlays holds the files and stage A findings of each overlay.
	overlays     map[string]*overlaySet
	overlayNames []string
	// keep holds the diagnostics of stages A to C.
	keep *keeper
	// stopped reports a limit that stopped the load.
	stopped bool
	// found numbers the stage A findings, which ties of their sort key
	// keep in discovery order.
	found int
	// nodes counts the YAML and JSON nodes of the base files, of the
	// load's node budget; a render's overlay has what they leave.
	nodes int
}

// bundleFiles names a Bundle's files in the RZ-CFG-001 of the node budget.
const bundleFiles = "the Bundle's files hold"

// source is one configuration file read.
type source struct {
	// path is the root-relative path, or the one file as given.
	path    string
	overlay string
	id      tree.FileID
	format  profile.Format
	data    []byte
}

// overlaySet is one overlay directory: its files and stage A findings.
type overlaySet struct {
	sources  []*source
	findings diag.List
}

// File is one configuration file of a Bundle.
type File struct {
	// Path is slash-separated and relative to the Bundle root; for a
	// one-file Bundle, the file as given.
	Path string
	// Overlay names the overlay that holds the file; "" for a base file.
	Overlay string
}

// Files returns every configuration file the Bundle read, base files and
// the files of every overlay, in path order.
func (b *Bundle) Files() []File {
	out := make([]File, len(b.sources))
	for i, s := range b.sources {
		out[i] = File{Path: s.path, Overlay: s.overlay}
	}
	return out
}

// Overlays returns the names of the Bundle's overlay directories, sorted.
func (b *Bundle) Overlays() []string { return slices.Clone(b.overlayNames) }

// Diagnostics returns the findings of stages A to C, sorted and capped.
func (b *Bundle) Diagnostics() diag.List { return b.keep.clone().result() }

// FileTable returns the file table every position of the Bundle's trees
// refers to.
func (b *Bundle) FileTable() *tree.FileTable { return b.files }

// Parse runs stages A to C on src: discovery and reading, parsing, and the
// base union with its identity and Gateway checks. Findings are in the
// Bundle's diagnostics; the error is ErrSource for a source that cannot be
// read, or ctx's.
func (l *Loader) Parse(ctx context.Context, src Source) (*Bundle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	o, err := open(src)
	if err != nil {
		return nil, err
	}
	defer o.close()
	b := &Bundle{
		l: l, files: &tree.FileTable{}, keep: newKeeper(l.limits.MaxDiagnostics),
		baseIDs: map[tree.ID]bool{}, lost: map[tree.ID]bool{}, overlays: map[string]*overlaySet{},
	}
	if o.root == nil {
		b.oneFile, b.root = true, o.name
		err = b.readOne(o)
	} else {
		b.root = RootFile
		err = b.readDir(ctx, o.root)
	}
	if err != nil {
		return nil, err
	}
	if b.stopped {
		return b, nil
	}
	if err := b.union(ctx); err != nil {
		return nil, err
	}
	return b, nil
}

// stop records a limit that stops the load.
func (b *Bundle) stop(s int, d diag.Diagnostic) {
	b.keep.add(s, 0, diag.List{d})
	b.stopped = true
}

// sourceError wraps an error reading the source; ctx's errors pass as
// they are.
func sourceError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(err, ctxErr) {
		return ctxErr
	}
	return fmt.Errorf("%w: %w", ErrSource, err)
}

// readOne reads a one-file Bundle.
func (b *Bundle) readOne(o *opened) error {
	limit := b.l.limits.MaxSourceBytes
	data, over, err := readFile(o.fsys, o.file, limit)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrSource, err)
	}
	s := &source{path: o.name, format: formatOf(o.name)}
	s.id = b.files.Add(tree.File{Path: s.path, Role: tree.RoleBase})
	if over {
		b.stop(stepDiscover, sizeLimit(s.path, data, limit, limit))
		return nil
	}
	if lfsPointer(data) {
		b.finding("", lfsFinding(s.path), true)
		return nil
	}
	s.data = data
	b.sources = append(b.sources, s)
	return nil
}

// formatOf returns the format of a file name; a name without a
// configuration extension, possible only for a one-file Bundle or an
// environments file named as given, is read as YAML.
func formatOf(name string) profile.Format {
	if f, ok := profile.FormatOf(name); ok {
		return f
	}
	return profile.FormatYAML
}

// lfsFinding is the RZ-CFG-001 of a Git LFS pointer.
func lfsFinding(path string) diag.Diagnostic {
	return diag.Diagnostic{
		Code: CodeLimit, Severity: diag.SeverityError, Location: diag.Location{File: path},
		Message: "Git LFS pointer, not the file's content: a Bundle holds the content itself",
		Hint:    "fetch the content with git lfs pull, or commit the file without LFS",
	}
}

// sizeLimit is the RZ-CFG-001 of source text past the limit, at the first
// byte past it: offset off of data, the file at path.
func sizeLimit(path string, data []byte, off, limit int64) diag.Diagnostic {
	at := jsonval.PositionOf(data, int(off))
	return diag.Diagnostic{
		Code: CodeLimit, Severity: diag.SeverityError,
		Location: diag.Location{File: path, Line: at.Line, Column: at.Column},
		Message:  "the source text passes the limit of " + strconv.FormatInt(limit, 10) + " bytes",
	}
}

// readDir runs stage A on a Bundle directory: it checks the root file,
// walks the tree, reading each directory's .ruralzignore as it enters it,
// and reads every configuration file in path order against the source
// budget, which the .ruralzignore files read first.
func (b *Bundle) readDir(ctx context.Context, root fs.FS) error {
	limits := b.l.limits
	info, err := fs.Lstat(root, RootFile)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		b.stop(stepDiscover, diag.Diagnostic{
			Code: CodeGateway, Severity: diag.SeverityError, Location: diag.Location{File: RootFile},
			Message: "the directory holds no " + RootFile + ", so it is not a Bundle: " + RootFile + " is the Bundle root and declares its Gateway",
		})
		return nil
	case err != nil:
		return sourceError(ctx, err)
	case info.IsDir():
		b.stop(stepDiscover, diag.Diagnostic{
			Code: CodeGateway, Severity: diag.SeverityError, Location: diag.Location{File: RootFile},
			Message: RootFile + " is a directory: it must be the file that declares the Bundle's Gateway",
		})
		return nil
	}

	w := &walker{
		ctx: ctx, fsys: root, maxFiles: limits.MaxFiles, overlays: map[string]bool{},
		maxBytes: limits.MaxSourceBytes, maxPatterns: limits.MaxIgnorePatterns,
	}
	if err := w.walk("", ""); err != nil {
		return sourceError(ctx, err)
	}
	for _, f := range w.findings {
		b.finding(f.overlay, f.d, f.lost)
	}
	if w.stop != nil {
		b.stop(stepDiscover, *w.stop)
		return nil
	}
	for name := range w.overlays {
		b.overlaySet(name)
	}
	b.overlayNames = slices.Sorted(maps.Keys(b.overlays))
	if w.rootIgnored {
		b.stop(stepDiscover, diag.Diagnostic{
			Code: CodeGateway, Severity: diag.SeverityError, Location: diag.Location{File: IgnoreFile},
			Message: IgnoreFile + " excludes " + RootFile + ", the Bundle root that declares the Gateway",
		})
		return nil
	}

	used := w.used
	slices.SortFunc(w.files, func(a, c candidate) int { return strings.Compare(a.path, c.path) })
	for i, c := range w.files {
		if i%readDirBatch == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		left := limits.MaxSourceBytes - used
		data, over, err := readFile(root, c.path, left)
		switch {
		case errors.Is(err, errNotRegular):
			b.notRegular(c)
			continue
		case err != nil:
			return sourceError(ctx, err)
		case over:
			b.stop(stepDiscover, sizeLimit(c.path, data, left, limits.MaxSourceBytes))
			return nil
		}
		used += int64(len(data))
		s := &source{path: c.path, overlay: c.overlay, format: formatOf(c.path)}
		role := tree.RoleBase
		if c.overlay != "" {
			role = tree.RoleOverlay
		}
		s.id = b.files.Add(tree.File{Path: c.path, Role: role})
		if lfsPointer(data) {
			b.finding(c.overlay, lfsFinding(c.path), true)
			continue
		}
		s.data = data
		b.sources = append(b.sources, s)
		if c.overlay != "" {
			set := b.overlaySet(c.overlay)
			set.sources = append(set.sources, s)
		}
	}
	return nil
}

// overlaySet returns the set of overlay name, creating it.
func (b *Bundle) overlaySet(name string) *overlaySet {
	set, ok := b.overlays[name]
	if !ok {
		set = &overlaySet{}
		b.overlays[name] = set
	}
	return set
}

// finding records a stage A finding of a base file, or of an overlay's
// file for the renders that select the overlay. lost reports an entry
// whose documents the load cannot read, which taints the base union.
func (b *Bundle) finding(overlay string, d diag.Diagnostic, lost bool) {
	if overlay == "" {
		b.found++
		b.keep.add(stepDiscover, b.found, diag.List{d})
		b.tainted = b.tainted || lost
		return
	}
	set := b.overlaySet(overlay)
	set.findings = append(set.findings, d)
}

// notRegular records a file that stopped being a regular file after the
// walk listed it.
func (b *Bundle) notRegular(c candidate) {
	b.finding(c.overlay, entryDiagnostic(c.path, fs.ModeIrregular), true)
}

// parsed is the stage B result of one file.
type parsed struct {
	docs []profile.Document
	// failed reports a stage B error: a document of the file was lost.
	failed bool
}

// parseOptions returns the parser options of one file of a load.
func (l *Loader) parseOptions(budget *profile.Budget, yield func()) profile.Options {
	return profile.Options{
		MaxBytes: maxInt(l.limits.MaxSourceBytes), MaxDepth: l.limits.MaxDepth,
		MaxDocumentTokens: l.limits.MaxDocumentNodes, MaxDiagnostics: l.limits.MaxDiagnostics,
		Budget: budget, Yield: yield,
	}
}

// parse runs stage B on srcs in the worker pool against a node budget of
// limit nodes, what the load's budget leaves them, and offers the findings
// to keep under step s. It returns the files' results and the nodes they
// built. One shared budget bounds the trees of every file together; which
// file a shared budget stops first depends on the schedule, so when the
// files pass it, parse counts them again in path order, each with what the
// files before it left, and reports the findings of the files up to the
// first that passes it, at its node past the budget. over is then true,
// and the load stops.
func (l *Loader) parse(ctx context.Context, srcs []*source, keep *keeper, s, limit int, msg string) (res []parsed, nodes int, over bool, err error) {
	budget := profile.NewBudget(limit, msg)
	stage := newKeeper(l.limits.MaxDiagnostics)
	out := make([]parsed, len(srcs))
	errs := make([]error, len(srcs))
	err = l.parallel(ctx, len(srcs), func(w *worker, i int) {
		src := srcs[i]
		docs, ds, err := profile.Parse(ctx, src.data, src.id, src.path, src.format, l.parseOptions(budget, w.yield))
		if err != nil {
			errs[i] = err
			return
		}
		stage.add(s, i, ds)
		out[i] = parsed{docs: docs, failed: ds.HasErrors()}
	})
	if err != nil {
		return nil, 0, false, err
	}
	if err := errors.Join(errs...); err != nil {
		return nil, 0, false, err
	}
	if !budget.Exceeded() {
		keep.merge(stage)
		return out, budget.Taken(), false, nil
	}
	ordered := slices.SortedFunc(slices.Values(srcs), func(a, b *source) int { return strings.Compare(a.path, b.path) })
	w := l.newWorker()
	left := limit
	for i, src := range ordered {
		if err := ctx.Err(); err != nil {
			return nil, 0, false, err
		}
		b := profile.NewBudget(left, msg)
		_, ds, err := profile.Parse(ctx, src.data, src.id, src.path, src.format, l.parseOptions(b, w.yield))
		if err != nil {
			return nil, 0, false, err
		}
		keep.add(s, i, ds)
		if b.Exceeded() {
			return nil, limit, true, nil
		}
		left -= b.Taken()
	}
	// Each file's nodes are a function of its bytes, so the count in path
	// order passes the budget whenever the shared one did.
	keep.add(s, len(ordered), diag.List{{Code: CodeLimit, Severity: diag.SeverityError, Message: msg}})
	return nil, limit, true, nil
}

// nodeMessage words the RZ-CFG-001 of a load's files past the node
// budget.
func (l *Loader) nodeMessage(what string) string {
	return what + " more than " + strconv.Itoa(l.limits.MaxNodes()) + " YAML and JSON nodes, " +
		strconv.Itoa(NodesPerResource) + " per resource of the resource limit"
}

// baseSources returns the base files: the root file first, so the union
// keeps its declarations over any other file's, then the others in path
// order.
func (b *Bundle) baseSources() []*source {
	var out []*source
	for _, s := range b.sources {
		if s.overlay == "" {
			out = append(out, s)
		}
	}
	if i := slices.IndexFunc(out, b.isRoot); i > 0 {
		root := out[i]
		copy(out[1:i+1], out[:i])
		out[0] = root
	}
	return out
}

// isRoot reports whether s is the root file, which declares the Gateway.
func (b *Bundle) isRoot(s *source) bool { return b.oneFile || s.path == RootFile }

// union runs stages B and C on the base files: parsing, then the envelope
// of every document, the base union and the Gateway checks.
func (b *Bundle) union(ctx context.Context) error {
	srcs := b.baseSources()
	res, nodes, over, err := b.l.parse(ctx, srcs, b.keep, stepParse, b.l.limits.MaxNodes(), b.l.nodeMessage(bundleFiles))
	if err != nil {
		return err
	}
	if over {
		b.stopped = true
		return nil
	}
	b.nodes = nodes
	w := b.l.newWorker()
	var (
		first        = map[tree.ID]*tree.Resource{}
		rootTainted  = !slices.ContainsFunc(srcs, b.isRoot)
		outsideGates int
		gateway      *tree.Resource
	)
	item := 0
	for i, s := range srcs {
		if res[i].failed {
			b.tainted = true
			rootTainted = rootTainted || b.isRoot(s)
		}
		for _, doc := range res[i].docs {
			w.yield()
			item++
			r, known, ds := b.l.envelope(doc, b.files)
			if len(ds) == 0 {
				if d, ok := controlKind(r, b.files); ok {
					ds = diag.List{d}
				}
			}
			if len(ds) > 0 {
				b.keep.add(stepUnion, item, ds)
				if known {
					b.lost[r.ID] = true
				} else {
					b.tainted = true
				}
				rootTainted = rootTainted || b.isRoot(s)
				continue
			}
			if len(first)+1 > b.l.limits.MaxResources {
				b.stop(stepUnion, diag.Diagnostic{
					Code: CodeLimit, Severity: diag.SeverityError, Location: b.files.Location(r.Start),
					Message: "the Bundle holds more than " + strconv.Itoa(b.l.limits.MaxResources) + " resources",
				})
				return nil
			}
			if prev, dup := first[r.ID]; dup {
				b.keep.add(stepUnion, item, diag.List{{
					Code: CodeDuplicate, Severity: diag.SeverityError, Location: b.files.Location(namePos(r)),
					Resource: resourceID(r.ID), Path: diag.Path{diag.Field("metadata"), diag.Field("name")},
					Message: idText(r.ID) + " is declared twice: a resource lives in exactly one place",
					Related: []diag.Related{{Location: b.files.Location(namePos(prev)), Message: "first declared in"}},
				}})
				continue
			}
			first[r.ID] = r
			if r.ID.Kind == v1alpha1.KindGateway {
				switch {
				case !b.isRoot(s):
					outsideGates++
					b.keep.add(stepUnion, item, diag.List{b.gatewayOutside(r, "a Gateway is declared only in "+RootFile+", the Bundle root")})
				case gateway != nil:
					d := b.gatewayOutside(r, "a Bundle has exactly one Gateway, and "+b.root+" already declares "+idText(gateway.ID))
					d.Related = []diag.Related{{Location: b.files.Location(namePos(gateway)), Message: "first declared in"}}
					b.keep.add(stepUnion, item, diag.List{d})
				default:
					gateway = r
				}
			}
			b.base = append(b.base, r)
			b.baseIDs[r.ID] = true
		}
	}
	switch {
	case gateway != nil:
		b.gateway, b.hasGateway = gateway.ID, true
	case outsideGates == 0 && !rootTainted:
		b.keep.add(stepUnion, item+1, diag.List{{
			Code: CodeGateway, Severity: diag.SeverityError, Location: diag.Location{File: b.root},
			Message: "the Bundle has no Gateway: " + b.root + " declares exactly one",
		}})
	}
	return b.checkBase(ctx)
}

// checkBase reports every $patch of the base union as RZ-CFG-006: valid
// only in overlays. It runs once, whether or not a render selects an
// overlay, since it only reads the base.
func (b *Bundle) checkBase(ctx context.Context) error {
	return b.l.parallel(ctx, len(b.base), func(w *worker, i int) {
		b.keep.add(stepCheckBase, i, overlay.CheckBase(b.base[i], overlay.Options{
			Schemas: b.l.schemas, Files: b.files, MaxDiagnostics: b.l.limits.MaxDiagnostics, Yield: w.yield,
		}))
	})
}

// gatewayOutside is the RZ-CFG-016 of Gateway r with message msg, at its
// kind.
func (b *Bundle) gatewayOutside(r *tree.Resource, msg string) diag.Diagnostic {
	return diag.Diagnostic{
		Code: CodeGateway, Severity: diag.SeverityError, Location: b.files.Location(memberPos(r, "kind")),
		Resource: resourceID(r.ID), Path: diag.Path{diag.Field("kind")}, Message: msg,
	}
}

// ParseFile parses one configuration file of the Bundle, by its path as
// Files lists it, and returns its documents that pass the envelope checks
// of stage C, as authored: unmerged and unsubstituted, in fresh trees the
// caller owns. The diagnostics are the file's own: stage B's and the
// envelope's. ruralz bundle render --api-version rewrites each file this
// way. A path that is not a file of the Bundle is ErrNoFile; so is a file
// stage A refused, which the Bundle's diagnostics report.
func (b *Bundle) ParseFile(ctx context.Context, path string) ([]*tree.Resource, diag.List, error) {
	i := slices.IndexFunc(b.sources, func(s *source) bool { return s.path == path })
	if i < 0 {
		return nil, nil, fmt.Errorf("%w: %s", ErrNoFile, path)
	}
	s := b.sources[i]
	keep := newKeeper(b.l.limits.MaxDiagnostics)
	res, _, over, err := b.l.parse(ctx, []*source{s}, keep, stepParse, b.l.limits.MaxNodes(), b.l.nodeMessage(bundleFiles))
	if err != nil {
		return nil, nil, err
	}
	if over {
		return nil, keep.result(), nil
	}
	var out []*tree.Resource
	for j, doc := range res[0].docs {
		r, _, ds := b.l.envelope(doc, b.files)
		if len(ds) == 0 {
			if d, ok := controlKind(r, b.files); ok {
				ds = diag.List{d}
			}
		}
		if len(ds) > 0 {
			keep.add(stepUnion, j, ds)
			continue
		}
		out = append(out, r)
	}
	return out, keep.result(), nil
}
