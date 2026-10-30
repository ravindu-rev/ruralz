// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package validate is stage H of the configuration pipeline
// (docs/architecture/02-configuration-model.md "Validation and diff
// semantics"): references, cross-resource rules and secret destinations,
// checked on the hub.Bundle stage G built, with the same results in
// ruralz, ruralz-control and ruralzd.
//
// # Rules
//
// Per resource, on up to workers goroutines:
//
//   - RZ-CFG-009: every x-ruralz-ref field names a resource of the kind
//     it implies; a name another kind holds is a wrong-kind target. A
//     Route's spec.listeners name listeners of the Gateway, and a quota or
//     ai.token-budget config.consumerQuota names a quota some Consumer
//     defines with unit requests or tokens.
//   - RZ-CFG-005, rules the schema cannot state that a Node would
//     otherwise refuse or ignore at activation: listener ports distinct
//     and apart from the admin port; an https listener with tls and a
//     certificate, and no tls on an http listener; listener hostnames and
//     Route match.hosts in the exact or one leading "*." form;
//     trustedProxies entries that are CIDRs or addresses; an OTLP
//     endpoint the Node can dial, with tls only beside an https one;
//     clientCertificate and clientKey together; Route path and header
//     criteria the Router compiles and a non-empty grpc service; no when
//     on an aggregate step; a literal step path that passes the hardening
//     of the client path with no dot segment, a step target that is a dot
//     path, and a non-empty group beside collection: true (step); an
//     Upstream Endpoint source, and Endpoint
//     addresses and dns discovery as internal/netaddr parses them for the
//     Upstream layer; a Policy filterClass the registry accepts; a
//     Consumer quota window of at least 1s in whole seconds.
//   - RZ-CFG-021: a Plugin image pinned by its full @sha256 digest.
//   - RZ-CFG-031 and RZ-CFG-032: composition steps within
//     limits.maxCompositionSteps, and explicit maxBodyBytes values within
//     limits.maxResponseBodyBytes, leaving each other step a share.
//   - The injected offline check of each Policy type (Options.Checks),
//     such as RZ-CFG-037 for a jwksUrl or tokenUrl that is not https.
//
// Then across resources, in the calling goroutine:
//
//   - RZ-CFG-035 and RZ-CFG-036, and the Consumer binding rules, through
//     internal/identity.
//   - RZ-CFG-023: two Routes with equal routematch.MatchKey on a common
//     listener.
//   - RZ-CFG-041: one secret used with two destinations.
//
// The schema states, and stage F reports, ring-hash without hashKey,
// a plugin Policy without plugin, a step maxBodyBytes below 1 and a step
// with collection: true and no group. The
// config of a plugin Policy against its Plugin's configSchema
// (RZ-CFG-021) ships with the plugin Policy, Planned (M2); RZ-CFG-034
// belongs to stage K and RZ-CFG-040 to the Node's serve check.
//
// # Secrets
//
// Run lists every secretRef use (Result.Secrets, hub.Validated.Secrets)
// with the kind of value it holds, which selects the Node's check, and its
// destination: the origin of the tokenUrl for an auth.upstream-oauth2
// clientSecret, hub.DestinationStateStore for stateStore.url and
// stateStore.cache.url, hub.DestinationLocal for everything else.
//
// # Output
//
// Diagnostics carry the source location of the field, or of its nearest
// authored ancestor for a default or a missing field. A run returns at
// most Options.MaxDiagnostics+1 of them, the first in diag.List.Sort
// order with ties in report order, and holds no more than that at once;
// the result never depends on the worker count. Quoted authored values are
// clipped to 64 code points. Workers yield the processor every
// Options.YieldAfter of work, so activation shares it with live traffic.
package validate
