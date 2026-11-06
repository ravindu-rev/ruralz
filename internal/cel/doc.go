// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package cel compiles, cost-checks and evaluates the CEL expressions of a
// Revision on cel.dev/cel-go (ADR-0019,
// docs/architecture/02-configuration-model.md "CEL expressions and allowed
// places"). It implements the internal/expr contracts: [Compiler] is the
// expr.Compiler, and its Builder, ProgramSet, Program and Value are the
// expr types every other package codes against. It is the only package that
// imports cel-go besides internal/cel/celtypes, whose views and type provider
// it uses.
//
// # Environments
//
// A site (expr.Site) is one occurrence of a place, refined by its
// attachment. The environment of a site declares only the variables
// available there, so any other variable is an "undeclared reference":
// Policy.spec.when declares response only when the Policy's first Phase is
// a response Phase, and the transform places declare upstream only at
// Upstream scope. The language is the standard library plus the strings
// (version 5) and encoders (version 1) extensions and the Ruralz functions
// url.pathEscape and url.queryEscape (urlLibrary) and net.prefix
// (netLibrary), with the duration, timestamp and regular-expression
// literal validators. net.prefix(address, v4, v6) masks an IPv4 address,
// an IPv4-mapped one included, to v4 bits and an IPv6 address to v6 bits
// and returns the network in CIDR form; an address that does not parse or
// a length out of range is a runtime error. It costs 1 plus 0.1 unit per
// input byte, and the estimators size its result at its input plus 4
// bytes.
//
// # Validation
//
// A finding is RZ-CFG-014 for a syntax, type or unavailable-variable error
// and RZ-CFG-015 for a static cost above 10,000 units at nominal sizes
// (target). Beyond cel-go's own checks, a checked expression is rejected
// when it:
//
//   - selects request.body outside the body sites (the composition places,
//     the transform.request places, the authz.cel rule, and the when of a
//     Policy that gates the request body at its attachment), or
//     response.body outside the transform.response places and the when of a
//     transform.response Policy;
//   - constructs a Ruralz object type, such as ruralz.Request{};
//   - passes a time zone that is not the literal "UTC" or a literal UTC
//     offset such as "+02:00" to a timestamp accessor, because a named zone
//     reads zone data on every evaluation;
//   - reads steps outside a sequential composition, or names a step that is
//     not an earlier step of the composition;
//   - has a result type the site cannot take: a bool site takes bool or
//     dyn, a string site string or dyn, an int site (config.cost) int or
//     dyn, a dyn site anything. A site's result is its place's, or the
//     narrower one it names (expr.Site.Result): a transform set[]
//     valueExpression is a string site for targets header, query and path
//     and a dyn site for target body. Programs of two sites that differ
//     only in their result are kept apart, so each keeps its own check.
//
// References reports what a source reads at a site without planning a
// program, for decisions taken before Filters are built, such as whether
// a transform's CEL field reads the body it acts on.
//
// # Cost
//
// The static estimate sizes every string reached from a variable, typed or
// dyn, at 256 bytes and every list or map at 32 entries (target); a dyn
// value used as a comprehension range, an in container or the receiver of
// join is sized as a container, and the elements of a dyn container as
// containers when every one the expression reads is used as one, else as
// strings. A second estimate, under the snapshot's input caps
// (expr.InputCaps, passed to NewBuilder), decides runtime tracking: the
// client request's fields at the header cap, which counts the request
// target, plus Policy edits, the claims and the Route and Consumer fields
// at their caps, and every other value, such as a body or a field of a call
// result, unbounded. A program whose bound under the caps stays within
// 1,000,000 units cannot reach the cost limit and evaluates without cost
// tracking; any other one evaluates under that limit (target). A program
// is reused across snapshots only under equal caps.
//
// Both estimates are cel-go's, under revision 1 of its cost model, pinned
// in every environment, which charges every unit its runtime charges: a
// unit per field selection, on a dyn value too, and a unit per selection,
// presence test or index whose operand is not an identifier, a selection,
// an index or a conditional, such as a list or map literal, a call or a
// comprehension, which cel-go evaluates as a relative attribute; and it
// counts a split result at one entry more than its string's size, as n
// separators split into n + 1 entries. So each is an upper bound of the
// runtime cost for inputs within its sizes.
//
// The cost limit alone bounds an evaluation (ADR-0019): cel-go charges a
// tracked program as it runs, in time linear in the units it charges, and
// aborts it at the limit with a runtime error, KindCostLimit, which no
// logical operator absorbs, as in `list.exists(...) || b`. An untracked
// program cannot reach the limit, since its bound caps its work.
//
// What remains of a tracked evaluation's allocations is cel-go's: the cost
// tracker clones itself and copies the arguments and the charge of every
// call it prices, and functions such as split build their results. An
// untracked program allocates only for the values it builds and the
// strings it reads from the request.
//
// # Evaluation
//
// Programs are immutable and safe for concurrent evaluation; they read the
// expr.Vars they are given and never write it. A runtime failure is an
// *expr.EvalError whose Kind classifies it and whose Error text never holds
// request data. A Builder reuses the Program of an identical site, source
// and input caps from the previous snapshot's ProgramSet, so a Hot Reload
// compiles only changed expressions.
//
// # Concurrency
//
// A Compiler is safe for concurrent use; it creates environments lazily
// under one mutex. A Builder takes concurrent Compile calls and one Build
// call after them. The package starts no goroutine.
package cel
