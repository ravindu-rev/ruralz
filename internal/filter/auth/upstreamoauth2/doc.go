// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package upstreamoauth2 is the auth.upstream-oauth2 Policy type
// (docs/architecture/08-security-and-identity.md "Upstream
// authentication"): it obtains an access token from config.tokenUrl with
// the OAuth2 client credentials grant (RFC 6749 section 4.4) and sends it
// to the Upstream as "Authorization: Bearer <token>" (RFC 6750). It is
// Filter class upstream-auth, Upstream scope only, Phase
// onUpstreamRequest, failureMode closed only
// (docs/architecture/02-configuration-model.md "Policy").
//
// # Token registry
//
// Registry is the Node-wide filter.Component that holds the tokens: one
// entry per Policy name and fetch parameters (tokenUrl, clientId,
// clientSecret reference, scopes, timeout), shared by the Filters every
// live snapshot builds for that Policy, so a Hot Reload that keeps the
// config keeps the token. The last Filter's Close removes the entry.
//
//   - A request whose entry holds a token valid at the Registry clock's
//     now reads it with one atomic load: no lock, no wait. Every attempt
//     of a leg, retries included, sets the header anew.
//   - A miss (no token yet, or the token expired) runs one single-flight
//     fetch per Policy and Node: the requests that miss together wait for
//     the same fetch, each within its own request deadline; the fetch is
//     bounded by config.timeout (default DefaultTimeout). A failed fetch,
//     or a deadline reached while waiting, is CannotDecide with
//     RZ-AUTH-020, which the closed-only class answers with 401.
//   - A token is refreshed at two thirds of its lifetime (target) in the
//     background while requests keep using it. A failed refresh keeps the
//     token serving and retries at two thirds of the remaining lifetime;
//     once it expires, the next request misses. The lifetime is
//     expires_in, or DefaultLifetime when the response has none, counted
//     from the moment the request was sent.
//   - A new fetch of one entry starts at least MinFetchInterval after the
//     previous one started; a miss within MinFetchInterval of a failed
//     fetch fails at once with RZ-AUTH-020 instead of starting another.
//
// Fetches never run on the request goroutine: Registry.Run, which the
// ruralzd supervisor runs before the first activation, starts each one on
// a goroutine it owns, at most one per entry, and at shutdown cancels them
// and returns after they exit. A miss before Run starts waits for it; a
// miss after Run returned fails at once.
//
// # The token endpoint hop
//
// The hop is TB-10 of "Threat model and trust boundaries": tokenUrl MUST
// be an absolute https URL (RZ-CFG-037, reported offline by Check and
// again by the Factory), the server is verified with TLS 1.2 or newer
// against the system roots (tlsconf.IdentityProvider), and the connection
// passes the egress guard ("Secrets" rule 6). clientSecret is
// destination-bound to the origin of tokenUrl
// (docs/architecture/02-configuration-model.md "Secret-to-destination
// binding"): the client refuses any other origin and follows no redirect,
// so the secret reaches only that origin. The client authenticates with
// client_secret_basic, the method RFC 6749 section 2.3.1 makes mandatory
// for servers, with clientId and the secret form-encoded before Basic
// encoding; the secret is read from the Revision's secret table at each
// fetch, so a rotated value is used from the next fetch on. The body is
// grant_type=client_credentials plus scope, the configured scopes in order
// without duplicates or empty ones, joined by spaces, when there are any.
//
// A 200 response is a JSON object of at most MaxResponseBytes with a
// non-empty access_token of visible ASCII characters, a token_type of
// Bearer in any letter case (an absent token_type is taken as Bearer), and
// an expires_in that is a positive integer, a JSON number or a decimal
// string, in seconds. Anything else, another status, a redirect, or a
// token that expires before its response arrives fails the fetch.
//
// # Telemetry and secrets
//
// Each request sets ruralz_auth_upstream_token_age_seconds of its Policy
// to the age of the token it sent, and each fetch to the age of the token
// held; each failed fetch counts once in
// ruralz_auth_upstream_refresh_failures_total and logs one warning, except
// a fetch canceled by the last Close or by the end of Run. The client
// secret and tokens never reach a log, an error, a metric or a span:
// errors carry fixed text, a status and at most an RFC 6749 error code
// from a fixed list, never response content.
package upstreamoauth2
