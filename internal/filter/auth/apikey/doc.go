// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package apikey is the auth.api-key Policy type
// (docs/architecture/08-security-and-identity.md "Authentication" and
// "Consumers and tiers"). In onRequestHeaders it reads the API key from
// the single field line of config.header (default x-api-key, matched
// case-insensitively), takes the SHA-256 digest of its exact bytes and
// binds the Consumer that holds that digest in the snapshot's credential
// index (internal/filter/auth.AcquireIndex): a literal apiKeys[].hash, or
// a secretRef-held key the index hashed at load and keeps following across
// rotations (internal/identity.KeyIndex). API keys rely on their entropy:
// no throttle runs before the lookup ("Pre-authentication throttling").
// It is an auth-class Filter, failureMode closed only, and never calls the
// State Store.
//
// # Decisions
//
// Filter.Handle lists the RZ-AUTH code of every outcome ("RZ-AUTH
// decision codes"): a missing or empty key is RZ-AUTH-001; more than one
// field line, a key no Consumer holds, a key two Consumers hold and a
// second successful authentication (Security rule 2) are RZ-AUTH-002; a
// held key on the revocation list is RZ-AUTH-004. The list
// (identity.Revoker) is consulted after the lookup verified the key and
// before the Consumer binds; M1 wires identity.NopRevoker. Every
// rejection is a 401 with the challenge APIKey header="<config.header>"
// and never echoes the key or its digest.
//
// # Identity and forwarding
//
// A successful authentication sets the request identity: method api-key,
// the Policy's name, the bound Consumer's snapshot view, empty claims and
// the Consumer's name as principal. The Filter removes every field line of
// config.header, in any case, from the live request headers, so the key
// never reaches the Upstream ("Request hardening": auth.api-key strips its
// credential).
//
// # Configuration
//
// config.header follows the field-name rule of the headers Policy
// ("Policy" in the Configuration model): a name that is not an RFC 9110
// token of at most 256 bytes, or one the Node manages (pseudo-headers,
// host, the hop-by-hop fields, content-length, expect, traceparent and
// tracestate), is RZ-CFG-005. Check is that rule as the offline
// hub.PolicyCheck every binary runs, and Build applies it too. An absent
// or empty header is the default x-api-key.
//
// # Removal in file mode
//
// In file mode an API key is revoked by removing it in a Revision that
// Hot Reload activates ("Revocation"). Every Filter acquires the
// credential index of the snapshot it is built for, a Filter whose Policy
// is unchanged included, so requests that pin the new snapshot no longer
// bind the removed key, while requests already running on the previous
// snapshot finish on it. A secretRef-held key leaves service the same way
// when its Consumer entry is removed, and its old value stops binding as
// soon as the resolver publishes a rotation, without a new Revision.
package apikey
