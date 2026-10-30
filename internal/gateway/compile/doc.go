// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package compile is the snapshot compiler of a Node
// (docs/architecture/03-data-plane.md "Configuration snapshots and hot
// reload"; docs/operations/02-zero-downtime-upgrades-and-hot-reload.md
// "Validation gate"): it turns the pipeline's output for one Revision
// (hub.Validated) into an unpublished *snapshot.Snapshot, off the request
// path, running steps 7, 9 and 10 of the validation gate. Each step
// reports every finding it makes, sorted as diagnostics, and a step with
// an error ends the compile there, so its first error's code is the
// rejection's.
//
// # Serve check (step 7)
//
// Every resource of the Revision, whether or not a Route reaches it, is
// checked for features this release does not serve (RZ-CFG-040): the
// kinds, fields and enum values of the served-feature table
// (registry.Registry.CheckFeatures, generated from the +ruralz:planned
// markers) present with a value other than their default, and the Policy
// types the Policy type registry does not serve (CheckServed).
//
// # Secrets and the State Store (step 9)
//
// Every secretRef use of the Revision (hub.Validated.Secrets) resolves
// through the Node's secret resolver, each failing use reported
// (RZ-CFG-026, RZ-CFG-042 for a State Store URL without credentials); a
// Gateway without stateStore also resolves RURALZ_STATE_STORE_URL when the
// variable is set, so a value that does not resolve is a finding; unset or
// blank, the memory driver serves. The snapshot's State Store
// handles are then acquired, one new handle per role
// (statestore/manager.Manager.Acquire), whose opener refuses a URL the
// transport rules refuse (RZ-CFG-042) or that does not fit its topology
// (RZ-CFG-026), reported at the Gateway's stateStore or stateStore.cache.
//
// # Compile (step 10)
//
// Filters, Upstream runtimes and metric label sets are built only for
// what some Route reaches: a Policy that some Route's effective Filter
// Chain runs, built once per attachment, its (Policy, scope, Phase set),
// never per Route; an Upstream that some Route names in upstreams or a
// composition step. The Filters come from the injected Filter tables
// (filter.Registry), each with the attachment's filter.BuildEnv: its
// CEL sites (pipeline.Pipeline.PolicySite, the sites stage J checked),
// the resolved secrets, the State Store handles, its metric handles and
// the Filter of the previous snapshot built for the same Policy, type,
// scope, Phase set and canonical config (BuildEnv.Previous), so its
// pools, watches and token buckets carry over. Builds run on a bounded
// pool of goroutines that yield every 100 µs.
//
// Each Route compiles to its per-Phase Filter arrays, built from its
// effective chain (hub.Chain) so render --effective and the runtime order
// cannot diverge, with one shared LegChain per Upstream; its forwarder
// (upstream/forward.New for upstreams, the composition engine for
// composition); its Route timeout (15 s); the largest stateStoreTimeout
// of its State Store Policies; and its body needs: the request body is
// gated when a client Policy gates it (an onRequestBody Policy, a
// transform.request whose config needs the body), when an Upstream-scope
// transform.request needs it, or when its composition does; the client
// response is gated for a client transform.response whose config or when
// reads the body; it is teed for the Response Cache on a Route whose
// chain holds a cache Policy. One Router compiles per listener from the
// Routes bound to it, with their compiled match.when.
//
// Every refusal of the compile is a finding: each Filter a Factory
// refuses, each Upstream the Upstream layer refuses (upstream.Check),
// each Router, forwarder and composition, and each compiled CEL field
// (RZ-CFG-014, RZ-CFG-015).
//
// # Invalidating Routes
//
// A Route without a cache Policy invalidates the Response Cache entries of
// every cache Route it may share a URI with
// (docs/architecture/09-traffic-management-and-resilience.md "Response
// caching", Invalidating Routes): it counts as such unless its listeners,
// its hosts, or an exact, prefix or template path provably share no URI
// with each cache Route; a regex path or a match.when proves nothing. Such
// a Route gets, after its Policies in Chain.Policies, the cache Filter of
// the overlapping cache Policy with the largest stateStoreTimeout, which
// subscribes to no Phase, so the executor only calls its Invalidate:
// the bump goes to the cache Route's State Store connection with that
// timeout.
//
// # Results
//
// A compile that succeeds returns a Compiled: the snapshot, its secret
// table (which the listener Set prepares with), its Upstream set, the
// activation warnings (a Revision that attaches cache and a limit Policy
// type without stateStore.cache; the balancer budget's fallbacks) and the
// summed circuitBreaker.maxConnections of its Upstreams. Activate, at the
// snapshot swap, switches the Node's Upstream runtimes and secret
// resolver to it; Discard releases one that is never published. A
// rejected compile, a canceled one and one that panics close every
// Filter, Upstream set and State Store handle they opened, keeping every
// Filter the active snapshot shares.
//
// A finding's error carries a registered RZ-CFG code; an error without
// one, or a panic, is a fault of the release, which the caller rejects
// with RZ-CFG-045, and ctx's error is the compile deadline (RZ-CFG-044).
// Findings that read the same, such as one Policy refused alike at two
// attachments, are reported once.
package compile
