// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package errcode is the registry of RZ-<AREA>-<NNN> error codes. Each area's
// registry is owned by one design document (foundation pack section 8.6);
// a code never changes meaning and is never reused. Client-facing errors
// carry a code registered here, and repocheck fails on any RZ code literal
// that is not.
package errcode

import (
	"slices"
	"strings"
)

// Area is an error code area.
type Area string

// Error code areas.
const (
	// AreaCFG is owned by 02-configuration-model.md.
	AreaCFG Area = "CFG"
	// AreaRT is owned by 03-data-plane.md.
	AreaRT Area = "RT"
	// AreaUP is owned by 09-traffic-management-and-resilience.md.
	AreaUP Area = "UP"
	// AreaAUTH is owned by 08-security-and-identity.md.
	AreaAUTH Area = "AUTH"
	// AreaRL is owned by 09-traffic-management-and-resilience.md.
	AreaRL Area = "RL"
	// AreaPLG is owned by 05-wasm-plugin-system.md.
	AreaPLG Area = "PLG"
	// AreaAI is owned by 06-ai-llm-gateway.md.
	AreaAI Area = "AI"
	// AreaCP is owned by 04-control-plane-and-gitops.md.
	AreaCP Area = "CP"
	// AreaSTS is owned by 11-scalability-and-distributed-state.md.
	AreaSTS Area = "STS"
)

// Owner returns the slug of the document that owns the area's registry.
func (a Area) Owner() string {
	switch a {
	case AreaCFG:
		return "configuration-model"
	case AreaRT:
		return "data-plane"
	case AreaUP:
		return "traffic-management-and-resilience"
	case AreaAUTH:
		return "security-and-identity"
	case AreaRL:
		return "traffic-management-and-resilience"
	case AreaPLG:
		return "wasm-plugin-system"
	case AreaAI:
		return "ai-llm-gateway"
	case AreaCP:
		return "control-plane-and-gitops"
	case AreaSTS:
		return "scalability-and-distributed-state"
	default:
		return ""
	}
}

// Code is one registered error code.
type Code struct {
	// ID is the code, RZ-<AREA>-<NNN>.
	ID string
	// Area is the code's area.
	Area Area
	// Status is the HTTP status when the owning document fixes a single
	// one, else 0.
	Status int
	// StatusNote describes the status or signal when it is not a single
	// HTTP status, as the owning document states it.
	StatusNote string
	// Meaning is the registered meaning.
	Meaning string
}

// Pattern matches the syntax of an error code.
const Pattern = `RZ-(CFG|RT|UP|AUTH|RL|PLG|AI|CP|STS)-[0-9]{3}`

// Valid reports whether id has the syntax of an error code; it does not
// check registration.
func Valid(id string) bool {
	rest, ok := strings.CutPrefix(id, "RZ-")
	if !ok {
		return false
	}
	area, num, ok := strings.Cut(rest, "-")
	if !ok || Area(area).Owner() == "" || len(num) != 3 {
		return false
	}
	for _, r := range num {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// Lookup returns the registered code with the given ID.
func Lookup(id string) (Code, bool) {
	all := All()
	i := slices.IndexFunc(all, func(c Code) bool { return c.ID == id })
	if i < 0 {
		return Code{}, false
	}
	return all[i], true
}

// All returns every registered code, ordered by area and number.
func All() []Code {
	return []Code{
		{ID: "RZ-CFG-001", Area: AreaCFG, Meaning: "Parse error, non-UTF-8, byte order mark, or size or depth limit"},
		{ID: "RZ-CFG-002", Area: AreaCFG, Meaning: "Duplicate key"},
		{ID: "RZ-CFG-003", Area: AreaCFG, Meaning: "Anchor, alias or merge key"},
		{ID: "RZ-CFG-004", Area: AreaCFG, Meaning: "Custom tag"},
		{ID: "RZ-CFG-005", Area: AreaCFG, Meaning: "JSON Schema violation"},
		{ID: "RZ-CFG-006", Area: AreaCFG, Meaning: "Unknown field"},
		{ID: "RZ-CFG-007", Area: AreaCFG, Meaning: "Unknown `kind`, or unknown or unserved `apiVersion`"},
		{ID: "RZ-CFG-008", Area: AreaCFG, Meaning: "Duplicate identity in base files or within one overlay"},
		{ID: "RZ-CFG-009", Area: AreaCFG, Meaning: "Missing or wrong-kind reference target"},
		{ID: "RZ-CFG-010", Area: AreaCFG, Meaning: "Undefined variable without default"},
		{ID: "RZ-CFG-011", Area: AreaCFG, Meaning: "`${VAR}` in a forbidden position"},
		{ID: "RZ-CFG-012", Area: AreaCFG, Meaning: "Literal in a `SecretValue` field"},
		{ID: "RZ-CFG-013", Area: AreaCFG, Meaning: "Warning: credential-shaped literal or secret-like variable name"},
		{ID: "RZ-CFG-014", Area: AreaCFG, Meaning: "CEL syntax, type or unavailable-variable error"},
		{ID: "RZ-CFG-015", Area: AreaCFG, Meaning: "CEL cost ceiling exceeded"},
		{ID: "RZ-CFG-016", Area: AreaCFG, Meaning: "Not exactly one `Gateway`, or `Gateway` outside `ruralz.yaml`"},
		{ID: "RZ-CFG-017", Area: AreaCFG, Meaning: "`Environment` or `Cluster` inside a Bundle"},
		{ID: "RZ-CFG-018", Area: AreaCFG, Meaning: "Two Policies in one slot at one scope"},
		{ID: "RZ-CFG-019", Area: AreaCFG, Meaning: "Excluding or replacing an `overridable: false` Policy"},
		{ID: "RZ-CFG-020", Area: AreaCFG, Meaning: "Policy type or Plugin Phases not allowed at the scope"},
		{ID: "RZ-CFG-021", Area: AreaCFG, Meaning: "Plugin without digest, or `config` violates its schema"},
		{ID: "RZ-CFG-022", Area: AreaCFG, Meaning: "Promotion cycle"},
		{ID: "RZ-CFG-023", Area: AreaCFG, Meaning: "Two Routes with identical match criteria"},
		{ID: "RZ-CFG-024", Area: AreaCFG, Meaning: "Field newer than the oldest target Node serves"},
		{ID: "RZ-CFG-025", Area: AreaCFG, Meaning: "Warning: deprecated field or apiVersion"},
		{ID: "RZ-CFG-026", Area: AreaCFG, Meaning: "`secretRef` unresolvable on a Node, or a resolved `stateStore.url` that does not fit `topology`"},
		{ID: "RZ-CFG-027", Area: AreaCFG, Meaning: "Digest mismatch: content does not hash to its digest, or a pushed Bundle re-renders differently"},
		{ID: "RZ-CFG-028", Area: AreaCFG, Meaning: "Plugin artifact unavailable, digest mismatch, or artifact disagrees with its `Plugin.spec`"},
		{ID: "RZ-CFG-029", Area: AreaCFG, Meaning: "`failureMode` not allowed for the Policy type"},
		{ID: "RZ-CFG-030", Area: AreaCFG, Meaning: "Overlay `apiVersion` differs from its base resource"},
		{ID: "RZ-CFG-031", Area: AreaCFG, Meaning: "More composition steps than `maxCompositionSteps`"},
		{ID: "RZ-CFG-032", Area: AreaCFG, Meaning: "Step `maxBodyBytes` values exceed `maxResponseBodyBytes` or leave no default share"},
		{ID: "RZ-CFG-033", Area: AreaCFG, Meaning: "Signature verification failed for a Revision or Plugin artifact"},
		{ID: "RZ-CFG-034", Area: AreaCFG, Meaning: "`AIModel` reachable from a Route with an `ai.token-budget` Policy sets no `limits.maxOutputTokens`"},
		{ID: "RZ-CFG-035", Area: AreaCFG, Meaning: "A `credentials.basic` `username`, or a `credentials.certificates` `subject` or `uriSan`, declared by two Consumers"},
		{ID: "RZ-CFG-036", Area: AreaCFG, Meaning: "`credentials.basic[].iterations` outside 600,000 to 1,000,000"},
		{ID: "RZ-CFG-037", Area: AreaCFG, Meaning: "`auth.jwt` `jwksUrl` or `auth.upstream-oauth2` `tokenUrl` is not an `https` URL"},
		{ID: "RZ-CFG-038", Area: AreaCFG, Meaning: "Effective Filter Chain combines a `cache` Policy with an `onRequestBody` authorization, validation or Plugin auth or authz Policy"},
		{ID: "RZ-CFG-039", Area: AreaCFG, Meaning: "A listener or admin port could not be bound during activation; the active Revision keeps serving"},
		{ID: "RZ-CFG-040", Area: AreaCFG, Meaning: "A resource uses a feature this Node release does not serve"},
		{ID: "RZ-CFG-041", Area: AreaCFG, Meaning: "A `secretRef` is used for two destinations (secret-to-destination binding)"},
		{ID: "RZ-RT-001", Area: AreaRT, Status: 404, Meaning: "No Route matched on this listener"},
		{ID: "RZ-RT-002", Area: AreaRT, Status: 431, Meaning: "Request headers exceed `limits.maxRequestHeaderBytes`"},
		{ID: "RZ-RT-003", Area: AreaRT, Status: 413, Meaning: "Request body exceeds `limits.maxRequestBodyBytes`"},
		{ID: "RZ-RT-004", Area: AreaRT, Status: 503, Meaning: "The buffer budget cannot cover a gate, step body or stream reservation"},
		{ID: "RZ-RT-005", Area: AreaRT, Status: 503, Meaning: "The Node in-flight ceiling is full"},
		{ID: "RZ-RT-006", Area: AreaRT, Status: 500, Meaning: "`match.when` runtime error; no fallthrough"},
		{ID: "RZ-RT-007", Area: AreaRT, Status: 504, Meaning: "Route `timeout` expired before any Upstream attempt"},
		{ID: "RZ-RT-008", Area: AreaRT, Status: 403, Meaning: "Rejected by a `cors` Policy"},
		{ID: "RZ-RT-009", Area: AreaRT, Status: 400, Meaning: "Rejected by a `validation.json-schema` Policy"},
		{ID: "RZ-RT-010", Area: AreaRT, Status: 404, Meaning: "`conditional` composition: no step's `when` is true"},
		{ID: "RZ-RT-011", Area: AreaRT, Status: 503, Meaning: "A `cors`, `validation.json-schema`, `headers` or `transform.*` Policy could not decide under `closed`"},
		{ID: "RZ-RT-012", Area: AreaRT, Status: 502, Meaning: "A response-Phase Policy failed under `closed`"},
		{ID: "RZ-RT-013", Area: AreaRT, StatusNote: "None; stream ended", Meaning: "A streamed chunk exceeded its cap"},
		{ID: "RZ-RT-014", Area: AreaRT, StatusNote: "503 before commit; stream ended after", Meaning: "The pinned snapshot's grace period ended"},
		{ID: "RZ-RT-015", Area: AreaRT, Status: 502, Meaning: "A composition step failed on a CEL runtime error or a body over `maxBodyBytes`"},
		{ID: "RZ-RT-016", Area: AreaRT, StatusNote: "503 before commit; stream ended after", Meaning: "The Drain deadline was reached"},
		{ID: "RZ-RT-017", Area: AreaRT, Status: 400, Meaning: "Request target or framing rejected by request hardening"},
		{ID: "RZ-RT-018", Area: AreaRT, Status: 504, Meaning: "An `only-if-cached` request missed the Response Cache"},
		{ID: "RZ-RT-019", Area: AreaRT, Status: 503, Meaning: "The admin `/tap` subscriber limit is reached"},
		{ID: "RZ-UP-001", Area: AreaUP, Status: 502, Meaning: "Connect failed or dial timed out, no retry"},
		{ID: "RZ-UP-002", Area: AreaUP, Status: 502, Meaning: "TLS handshake or verification failed or timed out, no retry"},
		{ID: "RZ-UP-003", Area: AreaUP, Status: 504, Meaning: "Deadline expired, or the final attempt timed out"},
		{ID: "RZ-UP-004", Area: AreaUP, Status: 502, Meaning: "Reset or protocol error before a response, no retry"},
		{ID: "RZ-UP-005", Area: AreaUP, Status: 503, Meaning: "Circuit breaker open, or half-open with its probe in flight"},
		{ID: "RZ-UP-006", Area: AreaUP, Status: 503, Meaning: "In-flight ceiling and pending queue full"},
		{ID: "RZ-UP-007", Area: AreaUP, Status: 502, Meaning: "Retries ran and the last attempt got no response"},
		{ID: "RZ-UP-008", Area: AreaUP, Status: 503, Meaning: "Endpoint set empty"},
		{ID: "RZ-UP-009", Area: AreaUP, StatusNote: "Not applicable", Meaning: "Upstream failed after commit; the stream ended with an error"},
		{ID: "RZ-UP-010", Area: AreaUP, Status: 502, Meaning: "Buffered plain `upstreams` response over its cap (area per OQ-data-plane-9)"},
		{ID: "RZ-UP-011", Area: AreaUP, Status: 502, Meaning: "A merged or non-final composition step got a non-2xx response"},
		{ID: "RZ-AUTH-001", Area: AreaAUTH, Status: 401, Meaning: "Credential missing, or every auth-class Policy skipped"},
		{ID: "RZ-AUTH-002", Area: AreaAUTH, Status: 401, Meaning: "Credential invalid, no matching Consumer, or a second binding"},
		{ID: "RZ-AUTH-003", Area: AreaAUTH, Status: 401, Meaning: "Token expired, not yet valid, without or over-long `exp`, or wrong issuer or audience"},
		{ID: "RZ-AUTH-004", Area: AreaAUTH, Status: 401, Meaning: "Credential revoked"},
		{ID: "RZ-AUTH-005", Area: AreaAUTH, Status: 401, Meaning: "Unknown `kid`"},
		{ID: "RZ-AUTH-006", Area: AreaAUTH, Status: 401, Meaning: "No usable keys for the issuer"},
		{ID: "RZ-AUTH-007", Area: AreaAUTH, Status: 429, Meaning: "Authentication throttled"},
		{ID: "RZ-AUTH-008", Area: AreaAUTH, Status: 421, Meaning: "An `auth.mtls` Route was reached over a connection that requested no client certificate"},
		{ID: "RZ-AUTH-010", Area: AreaAUTH, Status: 403, Meaning: "Denied by `authz.cel`"},
		{ID: "RZ-AUTH-011", Area: AreaAUTH, Status: 403, Meaning: "Denied by `authz.opa`"},
		{ID: "RZ-AUTH-012", Area: AreaAUTH, Status: 403, Meaning: "Denied by `authz.cedar`"},
		{ID: "RZ-AUTH-013", Area: AreaAUTH, Status: 403, Meaning: "Denied by `authz.ip`"},
		{ID: "RZ-AUTH-014", Area: AreaAUTH, Status: 403, Meaning: "Denied by `authz.geoip`"},
		{ID: "RZ-AUTH-015", Area: AreaAUTH, Status: 403, Meaning: "Authorization could not decide"},
		{ID: "RZ-AUTH-016", Area: AreaAUTH, StatusNote: "401 or 403", Meaning: "Rejected by a `plugin` Policy of class `auth` (401) or `authz` (403)"},
		{ID: "RZ-AUTH-020", Area: AreaAUTH, Status: 401, Meaning: "Upstream credential not obtained or signed (status: OQ-security-and-identity-9)"},
		{ID: "RZ-RL-001", Area: AreaRL, Status: 429, Meaning: "Denied by a local token bucket"},
		{ID: "RZ-RL-002", Area: AreaRL, Status: 429, Meaning: "Denied by GCRA or the over-limit cache"},
		{ID: "RZ-RL-003", Area: AreaRL, Status: 429, Meaning: "Consumer quota exhausted"},
		{ID: "RZ-RL-004", Area: AreaRL, Status: 403, Meaning: "Consumer missing or lacks the named quota"},
		{ID: "RZ-RL-005", Area: AreaRL, Status: 503, Meaning: "`config.key` CEL error under `failureMode: closed`"},
		{ID: "RZ-PLG-001", Area: AreaPLG, Meaning: "Guest trap, or a host panic recovered at the boundary"},
		{ID: "RZ-PLG-002", Area: AreaPLG, Meaning: "Call exceeded its wall-clock deadline"},
		{ID: "RZ-PLG-003", Area: AreaPLG, Meaning: "Linear memory limit reached"},
		{ID: "RZ-PLG-004", Area: AreaPLG, Meaning: "No idle instance within the wait; the pool manager was refused at the Node cap or pool share"},
		{ID: "RZ-PLG-005", Area: AreaPLG, Meaning: "No idle instance within the wait"},
		{ID: "RZ-PLG-006", Area: AreaPLG, Meaning: "Host Function misuse, a sticky error whatever the guest returns: wrong Phase, exhausted budget, second blocking State Store call, `ERR_DENIED`"},
		{ID: "RZ-PLG-007", Area: AreaPLG, Meaning: "Invalid guest result, such as `RESPOND` without an accepted `response_send`"},
		{ID: "RZ-PLG-008", Area: AreaPLG, Meaning: "Background instantiation or `rz_configure` failure; pool marked degraded, never returned to a request"},
		{ID: "RZ-PLG-009", Area: AreaPLG, Meaning: "Fault breaker open; the guest was not called"},
		{ID: "RZ-PLG-010", Area: AreaPLG, Meaning: "Parked bound reached; State Store call not attempted, and the guest returned cannot-decide"},
		{ID: "RZ-PLG-011", Area: AreaPLG, Meaning: "Guest returned cannot-decide without a preceding `ERR_UNAVAILABLE` or parked-bound refusal"},
		{ID: "RZ-AI-001", Area: AreaAI, Status: 404, Meaning: "`model` is not an `AIModel` in the Upstream's `ai.models`"},
		{ID: "RZ-AI-002", Area: AreaAI, Status: 400, Meaning: "Invalid, ambiguous or untranslatable body; disallowed operation, parameter, field, part or server tool"},
		{ID: "RZ-AI-003", Area: AreaAI, Status: 400, Meaning: "Estimated input exceeds `limits.maxInputTokens`"},
		{ID: "RZ-AI-004", Area: AreaAI, Status: 503, Meaning: "No eligible candidate before any attempt"},
		{ID: "RZ-AI-005", Area: AreaAI, StatusNote: "503 when every failure was a rate limit, else 502", Meaning: "Provider Fallback exhausted"},
		{ID: "RZ-AI-006", Area: AreaAI, StatusNote: "429 with `retry-after`", Meaning: "Token Budget cannot cover the reservation"},
		{ID: "RZ-AI-007", Area: AreaAI, Status: 403, Meaning: "Consumer missing or lacks the named token quota"},
		{ID: "RZ-AI-008", Area: AreaAI, StatusNote: "Error event, stream ends", Meaning: "Streaming guard: local output count exceeded C"},
		{ID: "RZ-AI-009", Area: AreaAI, StatusNote: "Error event, stream ends", Meaning: "Provider failure after commit"},
		{ID: "RZ-AI-010", Area: AreaAI, StatusNote: "400 before commit; error event after", Meaning: "Guardrail block"},
		{ID: "RZ-AI-011", Area: AreaAI, Status: 400, Meaning: "Provider rejected the request as invalid"},
		{ID: "RZ-AI-012", Area: AreaAI, Status: 400, Meaning: "Provider content-policy refusal"},
		{ID: "RZ-AI-013", Area: AreaAI, Status: 502, Meaning: "Non-streamed provider response exceeds `maxResponseBodyBytes`"},
		{ID: "RZ-AI-014", Area: AreaAI, StatusNote: "503 before commit; 502 in `onResponse`; error event in `onChunk`", Meaning: "An AI Policy could not decide (CEL error, failed declared remote call) under `failureMode: closed`"},
		{ID: "RZ-CP-001", Area: AreaCP, Meaning: "Enrollment token rejected, for any reason"},
		{ID: "RZ-CP-002", Area: AreaCP, Meaning: "`node.id` taken, or `Hello` mismatches the certificate"},
		{ID: "RZ-CP-003", Area: AreaCP, Meaning: "Node identity revoked or expired"},
		{ID: "RZ-CP-004", Area: AreaCP, Meaning: "No quorum, leader or lease"},
		{ID: "RZ-CP-005", Area: AreaCP, Meaning: "Promotion gate not satisfied"},
		{ID: "RZ-CP-006", Area: AreaCP, Meaning: "Awaiting approval"},
		{ID: "RZ-CP-007", Area: AreaCP, Meaning: "RBAC or separation-of-duties denial"},
		{ID: "RZ-CP-008", Area: AreaCP, Meaning: "Rollout transition not allowed"},
		{ID: "RZ-CP-009", Area: AreaCP, Meaning: "Another Rollout is active for the Cluster"},
		{ID: "RZ-CP-010", Area: AreaCP, Meaning: "Shed; retry later"},
		{ID: "RZ-CP-011", Area: AreaCP, Meaning: "Git fetch or authentication failed"},
		{ID: "RZ-CP-012", Area: AreaCP, Meaning: "Write-back conflict or push failed"},
		{ID: "RZ-CP-013", Area: AreaCP, Meaning: "Content quorum write failed"},
		{ID: "RZ-CP-014", Area: AreaCP, Meaning: "Signing key unavailable"},
		{ID: "RZ-CP-015", Area: AreaCP, Meaning: "Backup or restore failed"},
		{ID: "RZ-CP-016", Area: AreaCP, Meaning: "Revision is for another Environment"},
		{ID: "RZ-CP-017", Area: AreaCP, Meaning: "Revision never `complete` in this Cluster"},
		{ID: "RZ-CP-018", Area: AreaCP, Meaning: "Webhook signature invalid or rate-limited"},
		{ID: "RZ-CP-019", Area: AreaCP, Meaning: "Voter removal refused: leader or quorum"},
		{ID: "RZ-CP-020", Area: AreaCP, Meaning: "Revocation entries unavailable, or the entry cap is reached"},
		{ID: "RZ-CP-021", Area: AreaCP, Meaning: "Finalize refused, or a status report from a binary newer than the Control Store version allows is rejected"},
		{ID: "RZ-STS-001", Area: AreaSTS, StatusNote: "Per pack 8.10", Meaning: "The call exceeded `stateStoreTimeout` or the remaining per-request deadline"},
		{ID: "RZ-STS-002", Area: AreaSTS, StatusNote: "Per pack 8.10", Meaning: "Connection error, error reply (such as out of memory or `NOSCRIPT`) or unparsable reply"},
		{ID: "RZ-STS-003", Area: AreaSTS, StatusNote: "Per pack 8.10", Meaning: "Skipped: the shard's State client breaker was open"},
		{ID: "RZ-STS-004", Area: AreaSTS, StatusNote: "Per pack 8.10", Meaning: "Not attempted: the per-request deadline was spent or the in-flight ceiling full"},
		{ID: "RZ-STS-005", Area: AreaSTS, StatusNote: "Per pack 8.10", Meaning: "The deployment lacks a command set the Policy needs, such as vector search"},
	}
}
