// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package upstreamoauth2 is the auth.upstream-oauth2 Policy type
// (docs/architecture/08-security-and-identity.md "Upstream
// authentication"): it obtains a token and sends it to the Upstream as
// "Authorization: Bearer <token>" (RFC 6750). It is Filter class
// upstream-auth, Upstream scope only, Phase onUpstreamRequest,
// failureMode closed only (docs/architecture/02-configuration-model.md
// "Policy"). config.grantType says how the token is obtained:
//
//   - client-credentials, the default: the OAuth2 client credentials grant
//     (RFC 6749 section 4.4) at config.tokenUrl with config.clientId and
//     config.clientSecret;
//   - jwt-bearer: the JWT bearer grant (RFC 7523) at config.tokenUrl, its
//     assertion signed with the service-account JSON key of
//     config.serviceAccountKey, which never leaves the Node;
//   - metadata-server: the token of the workload's own service account
//     from the Compute Engine or GKE metadata server, at its fixed address
//     (MetadataServerURL), without a tokenUrl or a credential;
//   - token-exchange: the token exchange (RFC 8693) of config.subjectToken,
//     such as a projected Kubernetes service account token, at a Security
//     Token Service at config.tokenUrl, for a federated access token whose
//     audience is config.workloadIdentityProvider; with
//     config.serviceAccount, the federated token impersonates that account
//     through the IAM Service Account Credentials API (IAMCredentialsURL).
//
// The token is an access token for config.scopes, or with config.audience
// an ID token for that audience (jwt-bearer, metadata-server, and
// token-exchange with config.serviceAccount). Check holds each grant type
// to the fields it takes and requires, RZ-CFG-005, and the Factory refuses
// the same configs with the same codes.
//
// # Token registry
//
// Registry is the Node-wide filter.Component that holds the tokens: one
// entry per Policy name and fetch parameters (grant type, tokenUrl,
// clientId, credential reference, scopes, audience, the token-exchange
// fields, timeout), shared by the Filters every
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
//     expires_in, or DefaultLifetime when the response has none, as ID
//     tokens and impersonated access tokens have, counted from the moment
//     the request was sent.
//   - A new fetch of one entry starts at least MinFetchInterval after the
//     previous one started; a miss within MinFetchInterval of a failed
//     fetch fails at once with RZ-AUTH-020 instead of starting another.
//
// Fetches never run on the request goroutine: Registry.Run, which the
// ruralzd supervisor runs before the first activation, starts each one on
// a goroutine it owns, at most one per entry, and at shutdown cancels them
// and returns after they exit. A miss before Run starts waits for it; a
// miss after Run returned fails at once. A fetch that fails once it was
// canceled, by the end of Run or the last Close, or timed out fails with
// that cause, whatever the token endpoint answered meanwhile: an answer
// that races the cancellation may reply to the connection teardown rather
// than to the grant.
//
// # The token endpoint hop
//
// The hop is TB-10 of "Threat model and trust boundaries": tokenUrl MUST
// be an absolute https URL (RZ-CFG-037, reported offline by Check and
// again by the Factory), the server is verified with TLS 1.2 or newer
// against the system roots (tlsconf.IdentityProvider), and the connection
// passes the egress guard ("Secrets" rule 6). clientSecret and
// subjectToken are destination-bound to the origin of tokenUrl
// (docs/architecture/02-configuration-model.md "Secret-to-destination
// binding"): the client refuses any other origin and follows no redirect,
// so the secret reaches only that origin. serviceAccountKey is local: it
// signs the assertion, which alone is sent. Every secret is read from the
// Revision's secret table at each fetch, so a rotated value is used from
// the next fetch on.
//
//   - client-credentials authenticates with client_secret_basic, the
//     method RFC 6749 section 2.3.1 makes mandatory for servers, with
//     clientId and the secret form-encoded before Basic encoding. The body
//     is grant_type=client_credentials plus scope, the configured scopes
//     in order without duplicates or empty ones, joined by spaces, when
//     there are any.
//   - jwt-bearer posts grant_type and assertion, without client
//     authentication: an RS256 JWS naming the key's private_key_id as kid,
//     whose claims are the key's client_email as iss, tokenUrl as aud, the
//     Registry clock's time of the fetch as iat and an hour later as exp,
//     and the scopes as scope, or with audience the email as sub and the
//     audience as target_audience. Each fetch signs a new assertion.
//   - metadata-server sends a GET with Metadata-Flavor: Google to the
//     token endpoint of the default service account, the scopes
//     comma-separated, or with audience to its identity endpoint, which
//     answers with the ID token as the whole body. The metadata server
//     serves cleartext HTTP on a link-local address: its client is bound
//     to that one origin and may fetch it over http
//     (egress.ClientOptions.Cleartext), and the egress guard refuses the
//     address unless RURALZ_FETCH_ALLOW holds it.
//   - token-exchange posts the RFC 8693 parameters without client
//     authentication: the subject token trimmed of surrounding white
//     space, as a token file ends with a newline, config.subjectTokenType,
//     the provider as audience, an access token as requested_token_type,
//     and the scopes as scope, or the cloud-platform scope when it
//     impersonates. An issued_token_type other than an access token fails
//     the fetch. Impersonation posts the federated token as Bearer to the
//     account's generateAccessToken, for the scopes, or with audience to
//     generateIdToken, with includeEmail, on a client bound to the IAM
//     Service Account Credentials API's origin.
//
// A 200 response is a JSON object of at most MaxResponseBytes with a
// non-empty token of visible ASCII characters: access_token with a
// token_type of Bearer in any letter case (an absent token_type is taken
// as Bearer) and an expires_in that is a positive integer, a JSON number
// or a decimal string, in seconds; id_token for a jwt-bearer ID token;
// accessToken or token for an impersonated one. The metadata server's
// identity endpoint answers with the ID token alone. Anything else,
// another status, a redirect, or a token that expires before its response
// arrives fails the fetch.
//
// # Telemetry and secrets
//
// Each request sets ruralz_auth_upstream_token_age_seconds of its Policy
// to the age of the token it sent, and each fetch to the age of the token
// held; each failed fetch counts once in
// ruralz_auth_upstream_refresh_failures_total and logs one warning, except
// a fetch canceled by the last Close or by the end of Run. Secrets,
// assertions and tokens never reach a log, an error, a metric or a span:
// errors carry fixed text, a status and at most an RFC 6749 error code
// from a fixed list, never response content.
package upstreamoauth2
