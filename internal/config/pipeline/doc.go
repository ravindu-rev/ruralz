// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package pipeline is the configuration pipeline facade: the one entry
// through which ruralz, ruralz-control and ruralzd run stages A to M of
// docs/architecture/02-configuration-model.md (Figure 3) over the stage
// packages, so a Bundle that passes ruralz bundle validate passes the
// offline stages of Node activation with identical diagnostics. It also
// runs stages J and K, which have no package of their own, and the online
// stage.
//
// # Wiring
//
// New builds every stage once per process. The offline check of each
// Policy type (Options.Checks, internal/filter/builtin/checks.Table()) and
// the CEL compiler (Options.CEL, internal/cel) are injected, so
// internal/config imports neither internal/filter nor cel-go
// (docs/engineering/02-repository-layout-and-conventions.md "Package
// layers"); both are required.
//
// # Runs
//
// Run renders one Bundle for one Environment: the loader (stages A to E,
// internal/config/loader), the rendered schema views (F,
// internal/config/schemaview), defaults and hub conversion (G,
// internal/config/defaults), references and cross-resource rules (H,
// internal/config/validate), effective Filter Chains (I,
// internal/config/precedence), CEL (J), effective-chain rules (K) and the
// canonical form and digest (L and M, internal/config/canonical).
//
// Each stage runs on what passed the stages before it: F on the resources
// no error of stages A to E names (but for RZ-CFG-008 and RZ-CFG-016,
// which name a resource that stays sound), G on the resources F accepted,
// and H to K on the Bundle of the resources G built. A finding that only
// restates such a failure is left out: the stage F finding of a value a
// substitution error left in place, an RZ-CFG-009 for a reference to a
// resource that failed, an RZ-CFG-009 for a quota a failed Consumer
// declares, and an RZ-CFG-031 or RZ-CFG-032 judged against the default of
// a limit a failed Gateway authors. A limit, the diagnostics cap, the
// substitution output limit or an Environment that cannot be used ends a
// run; L and M run only when no stage found an error, so every error of a
// Bundle comes back in one run and an invalid Bundle has no Revision.
//
// The ${VAR} values are the Environment's spec.variables with
// Request.Environment, never the process environment, and
// Request.Variables, the process environment the binary read, without
// one (configuration model, "Environment substitution").
//
// RunEach runs a Bundle for every Environment of an environments file in
// file order: the file and stages A to C once, their findings shared and
// naming no Environment, then stages D to M per Environment, whose
// findings name it (docs/reference/01-cli-and-api-surface.md "Global
// flags").
//
// FromResources and FromCanonical re-enter ruralz.canonical.v1 content
// (Last-Known-Good, a handover candidate, a Control-mode Revision, a
// /config/dump as a diff source): stages F to M again, and content whose
// recomputed canonical bytes differ is RZ-CFG-027.
//
// Parse, ReadEnvironments, Converters and BuildFunc give ruralz bundle
// render --api-version the facade's own loader, conversion registry and
// builds, and Schemas, HubIndex, Registry and Resolver give the rendered
// Bundle, the diff engine and render --effective the components a run
// used.
//
// # Diagnostics
//
// A run's diagnostics are in diag.List.Sort order and capped at
// Limits.MaxDiagnostics, with one RZ-CFG-001 "too many diagnostics" when
// there were more, the same for every worker count.
//
// # Stage J
//
// Each x-ruralz-cel field is compiled at its sites (PlaceOf, RouteSite,
// PolicySite) and cost-checked by the injected compiler. A Policy field is
// compiled once per attachment of a Policy some Route's effective chain
// runs, with the scope, the first Phase it runs in there and whether it
// gates the request body there; a Policy no chain runs is compiled at
// every scope its type allows, with the first Phase it would run in there,
// and a finding is reported only when every one of those sites raises it.
// Composition fields carry the mode and the earlier steps.
// hub.Validated.CEL lists every expression that can run, with its site,
// for the snapshot compiler.
//
// One diagnostic reports each distinct issue of an expression, however
// many attachments compile it, with each attaching PolicyRef whose site
// raises it as a related location "attached at", in canonical order. An
// issue with a position sits at the file position of the character it
// names when the field is a plain scalar, over several lines too, or a
// quoted scalar on one line, escapes counted; otherwise at the field, its
// message ending with "at L:C", the line and column within the expression
// (configuration model, "Diagnostics and source map"). The loader keeps
// the bytes of each file it read (loader.Result.Text) for this.
//
// # Stage K
//
// RZ-CFG-034: an AIModel without limits.maxOutputTokens that a Route
// whose effective chain holds an ai.token-budget Policy reaches through
// the ai.models of an ai Upstream it names, reported once at its limits,
// or at the resource, with each such Route as a related location.
//
// # Online stage
//
// Online follows the digest and never changes it. Fetching Plugin
// artifacts is Planned (M2), so in M1 it fails each Plugin with
// RZ-CFG-028 and passes a Bundle without Plugins.
//
// # Rendered Bundle
//
// With Request.Render a run keeps the resources as stage E left them,
// before stage G rewrites them in place; WriteRendered writes them, and
// Effective returns a Route's effective Filter Chain rows, for ruralz
// bundle render.
//
// # Concurrency and effects
//
// A Pipeline is immutable and safe for concurrent use. Each stage of a run
// uses at most Options.Workers goroutines, which the run starts and waits
// for; each yields the processor every Options.YieldAfter of work when
// Options.Now is set. A panic in a stage reaches the caller on its
// goroutine once every goroutine of the run has stopped. The package reads
// files only through the loader's sources, opens no connection, logs
// nothing and reads the clock only through Options.Now.
package pipeline
