// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package loader

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/schemaview"
	"github.com/ravindu-rev/ruralz/internal/config/subst"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// maxCycleShown is the most Environment names a promotion cycle message
// lists before "...", so a message stays one short line.
const maxCycleShown = 4

// Environment is one Environment of an environments file, as a CLI render
// reads it: the overlay it selects and its variables. Treat it as
// read-only.
type Environment struct {
	// Name is metadata.name.
	Name string
	// Overlay is the overlay directory the Environment selects:
	// spec.overlay, else Name.
	Overlay string
	// OverlayExplicit reports that spec.overlay is written, so a missing
	// directory is RZ-CFG-009 rather than an empty overlay.
	OverlayExplicit bool
	// OverlayLocation is where spec.overlay is written.
	OverlayLocation diag.Location
	// Variables are spec.variables: the only values a render for the
	// Environment substitutes.
	Variables subst.Map
	// From is spec.promotion.from; "" without a promotion. Only Ruralz
	// Control acts on promotion; the CLI checks that chains are acyclic.
	From string
	// Location is where metadata.name is written.
	Location diag.Location

	fromLocation diag.Location
}

// variableNames returns the names an RZ-CFG-010 hint may suggest.
func (e *Environment) variableNames() []string { return slices.Collect(maps.Keys(e.Variables)) }

// Environments is a checked --environments file. It is immutable.
type Environments struct {
	path   string
	byName map[string]*Environment
	names  []string
	// order holds the same names in the order the file declares them.
	order []string
	// declared holds every Environment name a document of the file
	// carries, checked or not; tainted reports a document lost before its
	// name was read. Select reports a missing name only when neither
	// explains it.
	declared map[string]bool
	tainted  bool
	keep     *keeper
}

// Diagnostics returns the findings of the file, sorted and capped.
func (e *Environments) Diagnostics() diag.List { return e.keep.clone().result() }

// Names returns the names of the Environments that passed every check,
// sorted.
func (e *Environments) Names() []string { return slices.Clone(e.names) }

// FileOrder returns the names Names returns in the order the file
// declares them: ruralz bundle validate with --environments and no --env
// runs once per Environment in that order
// (docs/reference/01-cli-and-api-surface.md "Global flags").
func (e *Environments) FileOrder() []string { return slices.Clone(e.order) }

// Path returns the file as given, slash-separated.
func (e *Environments) Path() string { return e.path }

// Select returns the Environment name. A name the file does not declare
// is RZ-CFG-009 with the nearest declared name as the hint. A declared
// Environment that failed a check, or a file that lost documents to a
// parse error, returns nil and no diagnostic: the file's diagnostics
// explain it.
func (e *Environments) Select(name string) (*Environment, diag.List) {
	if env, ok := e.byName[name]; ok {
		return env, nil
	}
	if e.declared[name] || e.tainted {
		return nil, nil
	}
	d := diag.Diagnostic{
		Code: CodeMissing, Severity: diag.SeverityError, Location: diag.Location{File: e.path},
		Resource: &diag.ResourceID{Kind: string(v1alpha1.KindEnvironment), Name: shown(name)},
		Message:  "Environment " + quote(name) + " is not declared in " + e.path,
	}
	if near, ok := diag.Nearest(name, slices.Sorted(maps.Keys(e.declared))); ok {
		d.Hint = "did you mean " + strconv.Quote(near) + "?"
	}
	return nil, diag.List{d}
}

// ReadEnvironments reads and checks an --environments file: each document
// an Environment (a Cluster is skipped, any other kind is RZ-CFG-007),
// validated with the rendered schema (RZ-CFG-005, RZ-CFG-006), unique by
// name (RZ-CFG-008), with acyclic promotion.from chains (RZ-CFG-022). The
// file has its own source budget of Limits.MaxSourceBytes. The error is
// ErrSource for a file that cannot be read, or ctx's.
func (l *Loader) ReadEnvironments(ctx context.Context, src Source) (*Environments, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	o, err := open(src)
	if err != nil {
		return nil, err
	}
	defer o.close()
	if o.root != nil {
		return nil, fmt.Errorf("%w: %s is a directory, not an environments file", ErrSource, o.name)
	}
	keep := newKeeper(l.limits.MaxDiagnostics)
	e := &Environments{path: o.name, byName: map[string]*Environment{}, declared: map[string]bool{}, keep: keep}
	data, over, err := readFile(o.fsys, o.file, l.limits.MaxSourceBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSource, err)
	}
	files := &tree.FileTable{}
	s := &source{path: o.name, format: formatOf(o.name), data: data}
	s.id = files.Add(tree.File{Path: s.path, Role: tree.RoleEnvironments})
	switch {
	case over:
		keep.add(stepEnvironments, 0, diag.List{sizeLimit(s.path, data, l.limits.MaxSourceBytes, l.limits.MaxSourceBytes)})
		e.tainted = true
		return e, nil
	case lfsPointer(data):
		keep.add(stepEnvironments, 0, diag.List{lfsFinding(s.path)})
		e.tainted = true
		return e, nil
	}
	// The file has a node budget of its own, as it has a source budget.
	res, _, over, err := l.parse(ctx, []*source{s}, keep, stepEnvironmentsParse, l.limits.MaxNodes(), l.nodeMessage("the environments file holds"))
	if err != nil {
		return nil, err
	}
	if over {
		e.tainted = true
		return e, nil
	}
	e.tainted = res[0].failed
	envs, ok := l.environmentDocs(res[0], files, keep, e)
	if !ok {
		e.tainted = true
		return e, nil
	}
	valid := make([]bool, len(envs))
	err = l.parallel(ctx, len(envs), func(w *worker, i int) {
		ds, _ := l.views.ValidateWith(envs[i], files, schemaview.Options{MaxDiagnostics: l.limits.MaxDiagnostics, Yield: w.yield})
		keep.add(stepEnvironments, len(res[0].docs)+i, ds)
		valid[i] = !ds.HasErrors()
	})
	if err != nil {
		return nil, err
	}
	for i, r := range envs {
		if valid[i] {
			env := environmentOf(r, files)
			e.byName[env.Name] = env
			e.order = append(e.order, env.Name)
		}
	}
	e.names = slices.Sorted(maps.Keys(e.byName))
	e.cycles(keep)
	return e, nil
}

// environmentDocs runs the envelope checks of stage C on the documents of
// an environments file and returns its Environments in file order. ok is
// false when the file passes the resource limit, which stops the read.
func (l *Loader) environmentDocs(p parsed, files *tree.FileTable, keep *keeper, e *Environments) (envs []*tree.Resource, ok bool) {
	first := map[string]*tree.Resource{}
	for i, doc := range p.docs {
		r, known, ds := l.envelope(doc, files)
		if known && r.ID.Kind == v1alpha1.KindEnvironment {
			e.declared[r.ID.Name] = true
		}
		if len(ds) > 0 {
			keep.add(stepEnvironments, i, ds)
			if !known {
				e.tainted = true
			}
			continue
		}
		switch r.ID.Kind {
		case v1alpha1.KindEnvironment:
		case v1alpha1.KindCluster:
			// A control/ file may hold Clusters beside Environments; only
			// Ruralz Control reads them.
			continue
		default:
			keep.add(stepEnvironments, i, diag.List{{
				Code: CodeUnserved, Severity: diag.SeverityError, Location: files.Location(memberPos(r, "kind")),
				Resource: resourceID(r.ID), Path: diag.Path{diag.Field("kind")},
				Message: "kind " + quote(string(r.ID.Kind)) + " does not belong in an environments file, which holds Environment resources",
			}})
			continue
		}
		if len(first) == l.limits.MaxResources {
			keep.add(stepEnvironments, i, diag.List{{
				Code: CodeLimit, Severity: diag.SeverityError, Location: files.Location(r.Start),
				Message: "the environments file holds more than " + strconv.Itoa(l.limits.MaxResources) + " Environments",
			}})
			return nil, false
		}
		if prev, dup := first[r.ID.Name]; dup {
			keep.add(stepEnvironments, i, diag.List{{
				Code: CodeDuplicate, Severity: diag.SeverityError, Location: files.Location(namePos(r)),
				Resource: resourceID(r.ID), Path: diag.Path{diag.Field("metadata"), diag.Field("name")},
				Message: idText(r.ID) + " is declared twice",
				Related: []diag.Related{{Location: files.Location(namePos(prev)), Message: "first declared in"}},
			}})
			continue
		}
		first[r.ID.Name] = r
		envs = append(envs, r)
	}
	return envs, true
}

// environmentOf reads an Environment that passed the schema: its overlay,
// variables and promotion source.
func environmentOf(r *tree.Resource, files *tree.FileTable) *Environment {
	env := &Environment{Name: r.ID.Name, Overlay: r.ID.Name, Location: files.Location(namePos(r)), Variables: subst.Map{}}
	spec, _ := r.Root.Get("spec")
	if ov, ok := spec.Get("overlay"); ok && ov != nil && ov.Kind == tree.KindString {
		env.Overlay, env.OverlayExplicit, env.OverlayLocation = ov.Text, true, files.Location(ov.Pos)
	}
	if vars, ok := spec.Get("variables"); ok && vars != nil {
		for _, m := range vars.Members {
			if m.Value != nil && m.Value.Kind == tree.KindString {
				env.Variables[m.Key] = m.Value.Text
			}
		}
	}
	if p, ok := spec.Get("promotion"); ok {
		if from, ok := p.Get("from"); ok && from != nil && from.Kind == tree.KindString {
			env.From, env.fromLocation = from.Text, files.Location(from.Pos)
		}
	}
	return env
}

// cycles reports every Environment on a promotion cycle as RZ-CFG-022 at
// its spec.promotion.from. A chain that only leads into a cycle, or names
// an Environment the file does not hold, is not a cycle.
func (e *Environments) cycles(keep *keeper) {
	const (
		unseen = iota
		walking
		done
	)
	state := map[string]int{}
	for _, start := range e.names {
		var path []string
		for cur := start; ; {
			if state[cur] == done {
				break
			}
			if state[cur] == walking {
				cycle := path[slices.Index(path, cur):]
				for i, name := range cycle {
					env := e.byName[name]
					keep.add(stepCycles, i, diag.List{{
						Code: CodeCycle, Severity: diag.SeverityError, Location: env.fromLocation,
						Resource: &diag.ResourceID{Kind: string(v1alpha1.KindEnvironment), Name: shown(name)},
						Path:     diag.Path{diag.Field("spec"), diag.Field("promotion"), diag.Field("from")},
						Message:  "promotion cycle: " + cycleText(cycle, i),
						Hint:     "promotion.from chains must end at an Environment without a promotion",
					}})
				}
				break
			}
			env, ok := e.byName[cur]
			if !ok || env.From == "" {
				break
			}
			state[cur] = walking
			path = append(path, cur)
			cur = env.From
		}
		for _, name := range path {
			state[name] = done
		}
	}
}

// cycleText writes a promotion cycle from member i back to it, listing at
// most maxCycleShown names.
func cycleText(cycle []string, i int) string {
	var b strings.Builder
	for k := range min(len(cycle), maxCycleShown) {
		b.WriteString(shown(cycle[(i+k)%len(cycle)]))
		b.WriteString(" -> ")
	}
	if len(cycle) > maxCycleShown {
		b.WriteString("... -> ")
	}
	b.WriteString(shown(cycle[i]))
	if len(cycle) > maxCycleShown {
		b.WriteString(" (" + strconv.Itoa(len(cycle)) + " Environments)")
	}
	return b.String()
}
