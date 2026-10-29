// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package diff is the engine of ruralz bundle diff: it compares two
// rendered states, FROM and TO, and reports what changed in the human form
// and in the ruralz.diff.v1 JSON form
// (docs/architecture/02-configuration-model.md "Diff semantics";
// docs/reference/01-cli-and-api-surface.md owns the JSON compatibility).
//
// # Sides
//
// Each side is a hub.Validated: a Bundle rendered for an Environment, or
// canonical content (a live Node's /config/dump, a saved dump) run again
// through the pipeline. The CLI resolves the source forms; this package
// reads no file and opens no connection. Side.Origin and Side.Location
// say what a side was read from (JSON "bundle", "dump" or "admin") and
// Side.Environment which Environment it was rendered for. Naming two
// Environments reviews a promotion: each side then carries its own.
//
// # Rules
//
//  1. Resources match by identity (kind, metadata.name); a rename is a
//     removal plus an addition.
//  2. Comparison uses the canonical form, so formatting, file moves,
//     member order and apiVersion never appear: the trees compare as
//     ruralz.canonical.v1 bytes would, without apiVersion and the
//     ruralz.io/conversion-data annotation, numbers by their RFC 8785
//     text, and, at a schema level above 0, without the x-ruralz-since
//     members at their defaults. Equal digests are equal states.
//  3. Lists compare by their x-ruralz-list type. A map list compares by
//     key and a set list by element, so reordering them is never a change.
//     An orderedMap list compares by key and moves every shared entry whose
//     position changed. An atomic list, and a list in schemaless content,
//     is replaced whole.
//  4. A value at a secret position is shown only as its secretRef. A value
//     the diff would show there that is not a well-formed reference (a
//     literal, an undeclared member, a type or enum the schema does not
//     admit, the rule render applies) fails with RZ-CFG-012, naming its
//     path but not the value.
//  5. Each change carries the x-ruralz-impact classes on its path and, for
//     an added, removed or replaced value, inside the value. A Policy's type
//     class is the class of its Filter class in the registry (classImpact),
//     plus plugin for a plugin Policy and ai for an ai.* Policy. A change
//     to an attachment (a field naming a Policy: spec.policies,
//     spec.excludePolicies) carries the attached Policy's type class, and a
//     change to a Policy's own spec its own: TO's for an addition, FROM's
//     for a removal, both otherwise. An added or removed resource carries
//     those of every field it holds, its attachments included, and a Policy
//     its type class. Every change to a Plugin's spec, and an added or
//     removed Plugin, carries plugin. A change to Plugin spec.capabilities
//     or spec.image, and an added Plugin, is flagged as a Capability grant.
//  6. The effective layer compares every Route's resolved Filter Chain
//     (hub.Chain) Phase by Phase, client leg and upstream legs.
//
// # Field operations
//
// A modified resource lists replace, add, remove and move operations, in
// that order, each in document order (members in RFC 8785 order, list
// entries in list order). Paths are key-aware diag.Paths: [name=x] for map
// and orderedMap entries, [item=x] for set elements. Values are in
// canonical form. Applied to FROM's canonical form of the resource, the
// operations yield TO's: a removed, added or replaced path is resolved by
// key; map entries and set elements take their canonical (sorted) place;
// in an orderedMap a moved entry takes position ToIndex, an entry neither
// moved nor added keeps its position, and added entries fill the free
// positions in operation order. A move carries zero-based positions in
// JSON and one-based ones in the human form.
//
// # Effective changes
//
// Per Route, Phase and leg, a Policy only TO runs is added, one only FROM
// runs is removed, and of the Policies both run the fewest whose moving
// restores TO's order (those outside one longest run kept in order) are
// moved. Each change names its Filter class and the Policy before it in
// TO's chain, FROM's for a removal. A Route only one side holds has its
// whole chain added or removed.
//
// # Output
//
// Diff.AppendText writes the human form: a header naming both Revisions
// and the Environment, one aligned line per resource (modified, added,
// removed, each in kind catalog order, then by name) with its operations
// indented below, the effective changes and a summary. Authored text is
// escaped where it is not printable, so no value can forge or hide a line.
// Diff.AppendJSON writes ruralz.diff.v1, indented by two spaces, with the
// full sha256 digest beside each rev-<12 hex>. Both are deterministic:
// they depend only on the inputs, never on Options.Workers or scheduling.
//
// # Concurrency
//
// An Engine is immutable and safe for concurrent use. Compare runs its
// resources and Routes on at most Options.Workers goroutines that it owns
// and waits for; a panic in one is raised again on the caller's goroutine.
// It checks ctx between resources and Routes.
package diff
