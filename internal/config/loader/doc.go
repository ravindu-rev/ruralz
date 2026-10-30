// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package loader is stages A to E of the configuration pipeline
// (docs/architecture/02-configuration-model.md, Figure 3): it reads a
// Bundle and an --environments file and returns the merged, substituted
// resources that stage F validates.
//
// # Sources
//
// A Source names a Bundle directory rooted at ruralz.yaml, or one file
// holding a one-file Bundle such as a rendered Bundle. It is read from the
// operating system, a directory through os.Root so that no read leaves it,
// or from an fs.FS. A missing or unreadable source is an ErrSource error,
// never a diagnostic: the CLI exits 2 for it.
//
// # Stage A: discovery
//
// The loader reads every .yaml, .yml and .json file under the root, by
// case-sensitive extension, except paths with a segment that starts with a
// dot, the root's overlays/ directory and paths a .ruralzignore excludes.
// A .ruralzignore in any directory uses gitignore syntax (ignore.go) and
// Git's precedence: its patterns apply below its own directory, a deeper
// file's override a shallower file's, and an excluded directory is never
// read, its .ruralzignore included. Files are read in lexical byte order
// of their slash-separated root-relative paths, the same on every OS. A
// directory without ruralz.yaml is not a Bundle: RZ-CFG-016, and nothing
// else is read.
//
// Ruralz Control reads Git objects, so the loader refuses what a Git tree
// holds without file content: a symbolic link, a Git submodule (a
// directory below the root that holds .git), a Git LFS pointer and any
// entry that is neither a regular file nor a directory, each RZ-CFG-001.
//
// Every overlays/<name>/ directory with a valid overlay name is read too,
// so the limits cover the whole Bundle; its files are parsed, and its
// findings reported, only by a render that selects it.
//
// # Stages B and C: parse, union and identity
//
// Each file is parsed by internal/config/profile. Every document then
// needs a served apiVersion and kind (RZ-CFG-007) and a string
// metadata.name (RZ-CFG-005); Environment and Cluster are RZ-CFG-017 in a
// Bundle. Base files form a union, ruralz.yaml first, then the others in
// path order: a second document with one identity is RZ-CFG-008 and the
// first stays. The Bundle has exactly one Gateway, in ruralz.yaml
// (RZ-CFG-016); a one-file Bundle's file plays that role. A $patch in a
// base document is RZ-CFG-006 (overlay.CheckBase), whether or not a render
// selects an overlay.
//
// # Environments
//
// ReadEnvironments reads an --environments file: Environment resources,
// validated with the rendered schema (RZ-CFG-005, RZ-CFG-006), unique by
// name (RZ-CFG-008), with acyclic promotion.from chains (RZ-CFG-022). A
// Cluster is skipped and any other kind is RZ-CFG-007.
// Environments.Select returns the Environment a render uses; a name the
// file does not declare is RZ-CFG-009. Environments.FileOrder lists the
// Environments that passed every check in the order the file declares
// them, the order a run per Environment takes.
//
// # Stages D and E: overlay and substitution
//
// Bundle.Render applies at most one overlay, overlays/<spec.overlay>/
// (default: the Environment's name), through internal/config/overlay, then
// substitutes ${VAR} through internal/config/subst with the Environment's
// variables, or without an Environment the caller's (the process
// environment). Render works on a copy of the base union, so one Bundle
// renders for any number of Environments. An overlay can neither add nor
// remove the Gateway (RZ-CFG-016). When stages A to C lost a base
// document, an overlay document that may patch it is withheld rather than
// added, since its findings would only repeat the loss. Result.Text
// returns the bytes of each file the load read, in which the resources'
// positions count, so the configuration pipeline can place a CEL finding
// at a character inside a scalar.
//
// # Limits and diagnostics
//
// Limits bound a load: source bytes, files, resources, nesting, YAML nodes
// per document, .ruralzignore patterns and diagnostics. The node budget,
// NodesPerResource for each resource the limit allows, bounds the YAML and
// JSON nodes of every file the load parses together, so the trees a load
// holds stay bounded whatever the number of files. Passing a limit is
// RZ-CFG-001 and stops the load; the node past the budget is found in path
// order of the files, the same for every worker count. The diagnostics are
// the first Limits.MaxDiagnostics in diag.List.Sort order, the same for
// every worker count, then one RZ-CFG-001 "too many diagnostics" when
// there were more, as a diag.Collector of that cap holds them.
//
// # Work
//
// Files and resources are processed on at most Options.Workers goroutines,
// each yielding the processor after Options.YieldAfter of work when
// Options.Now is set. A panic in a worker is raised again on the calling
// goroutine once every worker has stopped.
package loader
