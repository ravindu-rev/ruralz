// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

// ConsumerSpec is the spec of a Consumer.
type ConsumerSpec struct {
	// Tier is a free-form Tier name read by Policies.
	Tier string `json:"tier,omitempty"`
	// Credentials identify the Consumer; at least one credential.
	// +ruralz:required
	Credentials Credentials `json:"credentials"`
	// Quotas are named quotas used by quota and ai.token-budget Policies.
	// +ruralz:list=map,key=name
	Quotas []Quota `json:"quotas,omitempty"`
	// Tags are free-form tags.
	// +ruralz:list=set
	Tags []string `json:"tags,omitempty"`
}

// Credentials identify a Consumer.
// +ruralz:minProperties=1
type Credentials struct {
	// APIKeys are API keys, stored hashed.
	// +ruralz:list=map,key=name
	APIKeys []APIKey `json:"apiKeys,omitempty"`
	// JWT binds token subjects or claims per issuer.
	// +ruralz:list=map,key=issuer
	JWT []JWTBinding `json:"jwt,omitempty"`
	// OAuthClients are OAuth client identifiers.
	// +ruralz:list=map,key=clientId
	OAuthClients []OAuthClient `json:"oauthClients,omitempty"`
	// Basic are auth.basic credentials; a username in two Consumers is RZ-CFG-035.
	// +ruralz:list=map,key=username
	Basic []BasicCredential `json:"basic,omitempty"`
	// Certificates are auth.mtls identities; one in two Consumers is RZ-CFG-035.
	// +ruralz:list=map,key=name
	Certificates []CertificateCredential `json:"certificates,omitempty"`
}

// APIKey is one API key, given by exactly one of hash or secretRef.
// +ruralz:exactlyOneOf=hash,secretRef
type APIKey struct {
	// Name identifies the key.
	// +ruralz:required
	Name string `json:"name"`
	// Hash is the SHA-256 digest of the key.
	// +ruralz:pattern=^sha256:[0-9a-f]{64}$
	Hash string `json:"hash,omitempty"`
	// SecretRef holds a retrievable key; the Node hashes it at load and keeps only the hash.
	// +ruralz:secret
	SecretRef *SecretRef `json:"secretRef,omitempty"`
}

// JWTBinding binds a Consumer to tokens of one issuer by exactly one of
// subject or claims.
// +ruralz:exactlyOneOf=subject,claims
type JWTBinding struct {
	// Issuer is the token issuer.
	// +ruralz:required
	Issuer string `json:"issuer"`
	// Subject is the expected sub claim.
	Subject string `json:"subject,omitempty"`
	// Claims are expected claim values.
	Claims map[string]string `json:"claims,omitempty"`
}

// OAuthClient is one OAuth client identifier.
type OAuthClient struct {
	// ClientID is the OAuth client identifier.
	// +ruralz:required
	ClientID string `json:"clientId"`
}

// BasicCredential is one auth.basic user.
type BasicCredential struct {
	// Username is the user name.
	// +ruralz:required
	Username string `json:"username"`
	// Hash is pbkdf2-sha256:<salt>:<key> in base64url, with a 16-byte salt and a 32-byte key.
	// +ruralz:required
	// +ruralz:pattern=^pbkdf2-sha256:[A-Za-z0-9_-]{22}:[A-Za-z0-9_-]{43}$
	Hash string `json:"hash"`
	// Iterations is the PBKDF2 iteration count; outside 600000 to 1000000 is RZ-CFG-036.
	// +ruralz:default=600000
	Iterations *int32 `json:"iterations,omitempty"`
}

// CertificateCredential is one auth.mtls identity, given by exactly one of
// subject or uriSan.
// +ruralz:exactlyOneOf=subject,uriSan
type CertificateCredential struct {
	// Name identifies the credential.
	// +ruralz:required
	Name string `json:"name"`
	// Subject is the certificate subject distinguished name.
	Subject string `json:"subject,omitempty"`
	// URISAN is a URI subject alternative name, such as a SPIFFE ID.
	URISAN string `json:"uriSan,omitempty"`
}

// QuotaUnit is what a quota counts.
type QuotaUnit string

// Quota units.
const (
	// QuotaUnitRequests counts requests.
	QuotaUnitRequests QuotaUnit = "requests"
	// QuotaUnitTokens counts LLM input plus output tokens.
	QuotaUnitTokens QuotaUnit = "tokens"
)

// Quota is one named Consumer quota.
type Quota struct {
	// Name identifies the quota.
	// +ruralz:required
	Name string `json:"name"`
	// Unit is requests or tokens.
	// +ruralz:required
	Unit QuotaUnit `json:"unit"`
	// Limit is the amount allowed per window.
	// +ruralz:required
	// +ruralz:minimum=0
	Limit int64 `json:"limit"`
	// Window is the window length.
	// +ruralz:required
	Window Duration `json:"window"`
}
