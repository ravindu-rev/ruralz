// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"encoding/binary"
	"fmt"
	"hash/maphash"
	"slices"
	"strconv"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Claims is the read access the index needs to a verified JWT payload
// (spec 06 requirements 16 and 17): top-level members only.
type Claims interface {
	// StringClaim returns the value of the top-level claim name when it is
	// present and a JSON string.
	StringClaim(name string) (string, bool)
	// HasClaim reports whether the top-level claim name is present,
	// whatever its JSON type (null included).
	HasClaim(name string) bool
}

// MapClaims adapts a decoded payload whose string members are Go strings,
// such as the result of expr.Value.Native.
type MapClaims map[string]any

// StringClaim implements Claims.
func (m MapClaims) StringClaim(name string) (string, bool) {
	s, ok := m[name].(string)
	return s, ok
}

// HasClaim implements Claims.
func (m MapClaims) HasClaim(name string) bool {
	_, ok := m[name]
	return ok
}

// match accumulates the Consumers bound by one or more credential rules and
// de-duplicates them by name: none, one, or ambiguous (two or more distinct
// Consumers, 401 RZ-AUTH-002; spec 06 requirement 18).
type match struct {
	c         *expr.Consumer
	ambiguous bool
}

// add records c.
func (m *match) add(c *expr.Consumer) {
	switch {
	case c == nil:
	case m.c == nil:
		m.c = c
	case m.c.Name != c.Name:
		m.ambiguous = true
	}
}

// merge records every Consumer of o.
func (m *match) merge(o match) {
	if o.ambiguous {
		m.ambiguous = true
	}
	m.add(o.c)
}

// result returns the bound Consumer, or nil and true when ambiguous.
func (m match) result() (*expr.Consumer, bool) {
	if m.ambiguous {
		return nil, true
	}
	return m.c, false
}

type issSub struct{ iss, sub string }

type issClient struct{ iss, client string }

// claimShape holds the claims rules of one issuer that expect the same set
// of claim names (the shape). A lookup reads those claims once, hashes
// their values and probes one map, so its cost grows with the number of
// distinct shapes the Bundle configures for the issuer, never with the
// number of Consumers that share a shape or a claim value.
type claimShape struct {
	// names are the claim names, sorted.
	names []string
	// rules maps the seeded hash of the expected values (in names order)
	// to the rules with that hash. Rules expecting the same values are
	// merged at build time, so a slot holds more than one rule only on a
	// 64-bit collision; every hit is confirmed against the values.
	rules map[uint64][]claimRule
}

// claimRule is the Consumers whose credentials.jwt entry for the issuer
// expects exactly values (in the shape's names order).
type claimRule struct {
	values []string
	m      match
}

// shapeKey identifies a shape while the index is built: the issuer and the
// length-prefixed concatenation of the sorted claim names.
type shapeKey struct{ iss, names string }

// probe returns the rules the string claims of cl select: nil when one of
// the shape's claims is absent or not a string (non-string claims never
// match, spec 06 requirement 16).
func (s *claimShape) probe(seed maphash.Seed, cl Claims) []claimRule {
	var h maphash.Hash
	h.SetSeed(seed)
	for _, n := range s.names {
		v, ok := cl.StringClaim(n)
		if !ok {
			return nil
		}
		writeValue(&h, v)
	}
	return s.rules[h.Sum64()]
}

// matches reports whether every claim of the shape is a string claim equal
// to r's expected value.
func (r *claimRule) matches(names []string, cl Claims) bool {
	for i, n := range names {
		if v, ok := cl.StringClaim(n); !ok || v != r.values[i] {
			return false
		}
	}
	return true
}

// hashValues returns the seeded hash of values, as probe computes it from
// a token's claims.
func hashValues(seed maphash.Seed, values []string) uint64 {
	var h maphash.Hash
	h.SetSeed(seed)
	for _, v := range values {
		writeValue(&h, v)
	}
	return h.Sum64()
}

// writeValue adds v to h prefixed by its length, so the hashed bytes of
// distinct value tuples differ.
func writeValue(h *maphash.Hash, v string) {
	var n [binary.MaxVarintLen64]byte
	_, _ = h.Write(binary.AppendUvarint(n[:0], uint64(len(v))))
	_, _ = h.WriteString(v)
}

type basicEntry struct {
	cred      *BasicCredential
	ambiguous bool
}

// Index is the immutable credential index of one snapshot (spec 06
// requirement 12): every lookup is a hash-map probe on the presented
// credential, independent of the number of Consumers (a JWT lookup probes
// once per distinct claim-name set configured for the token's issuer), and
// safe for concurrent use without locks. Digests of secretRef-held API keys
// live in a KeyIndex beside it (WithSecretKeys), replaced copy-on-write on
// rotation.
type Index struct {
	consumers map[string]*expr.Consumer

	apiKeys map[[32]byte]match

	jwtSubject map[issSub]match
	// seed keys the claim value hashes of jwtShapes; it is chosen per
	// Index, so a token cannot be shaped to collide.
	seed maphash.Seed
	// jwtShapes lists, per issuer, the shapes of its claims rules, sorted
	// by claim names.
	jwtShapes map[string][]*claimShape
	oauth     map[issClient]match

	basic map[string]basicEntry

	certSubject map[string]match
	certURI     map[string]match

	secretKeys []SecretKey
	keys       *KeyIndex
}

// NewIndex compiles the credential index of consumers. compiled supplies
// the snapshot's expr.Consumer views by name (identity.Consumers.Map), so
// lookups return the same pointers the snapshot holds; a Consumer missing
// from it is compiled here. secretRef-held API keys are listed by
// SecretKeys for NewKeyIndex; a malformed literal hash or basic hash is
// RZ-CFG-005, basic iterations out of range RZ-CFG-036.
func NewIndex(consumers []*v1alpha1.Consumer, compiled map[string]*expr.Consumer) (*Index, error) {
	ix := &Index{
		consumers:   make(map[string]*expr.Consumer, len(consumers)),
		apiKeys:     map[[32]byte]match{},
		jwtSubject:  map[issSub]match{},
		seed:        maphash.MakeSeed(),
		jwtShapes:   map[string][]*claimShape{},
		oauth:       map[issClient]match{},
		basic:       map[string]basicEntry{},
		certSubject: map[string]match{},
		certURI:     map[string]match{},
	}
	shapes := map[shapeKey]*claimShape{}
	for _, src := range consumers {
		if src == nil {
			continue
		}
		name := src.Metadata.Name
		if _, dup := ix.consumers[name]; dup {
			continue
		}
		c := compiled[name]
		if c == nil {
			c = CompileConsumer(src)
		}
		ix.consumers[name] = c
		if err := ix.add(src, c, shapes); err != nil {
			return nil, err
		}
	}
	for k, s := range shapes {
		ix.jwtShapes[k.iss] = append(ix.jwtShapes[k.iss], s)
	}
	for _, ss := range ix.jwtShapes {
		slices.SortFunc(ss, func(a, b *claimShape) int { return slices.Compare(a.names, b.names) })
	}
	slices.SortFunc(ix.secretKeys, compareSecretKeys)
	return ix, nil
}

// add indexes one Consumer's credentials; shapes collects the claims
// shapes by issuer and claim names while NewIndex runs.
func (ix *Index) add(src *v1alpha1.Consumer, c *expr.Consumer, shapes map[shapeKey]*claimShape) error {
	cr := &src.Spec.Credentials
	for _, k := range cr.APIKeys {
		switch {
		case k.SecretRef != nil:
			ix.secretKeys = append(ix.secretKeys, SecretKey{Consumer: c.Name, Name: k.Name, Ref: secret.RefOf(*k.SecretRef)})
		default:
			d, err := ParseDigest(k.Hash)
			if err != nil {
				return errcode.Wrap(CodeSchema, fmt.Errorf("consumer %q apiKeys %q: %w", c.Name, k.Name, err))
			}
			m := ix.apiKeys[d]
			m.add(c)
			ix.apiKeys[d] = m
		}
	}
	// An entry with an empty subject and no claims binds nothing (Check
	// reports it as RZ-CFG-005): matching every token of the issuer would
	// be the unsafe reading.
	for _, j := range cr.JWT {
		if j.Subject != "" {
			key := issSub{j.Issuer, j.Subject}
			m := ix.jwtSubject[key]
			m.add(c)
			ix.jwtSubject[key] = m
		}
		if len(j.Claims) > 0 {
			ix.addClaims(j.Issuer, j.Claims, c, shapes)
		}
	}
	// OAuth clients bind through the Consumer's own jwt issuers
	// (OQ-security-and-identity-24 (a), spec 06 requirement 17).
	for _, o := range cr.OAuthClients {
		for _, j := range cr.JWT {
			key := issClient{j.Issuer, o.ClientID}
			m := ix.oauth[key]
			m.add(c)
			ix.oauth[key] = m
		}
	}
	for _, b := range cr.Basic {
		cred, err := basicCredential(c, b)
		if err != nil {
			return err
		}
		e, dup := ix.basic[b.Username]
		switch {
		case !dup:
			e.cred = cred
		case e.cred.Consumer.Name != c.Name:
			e.ambiguous = true
		}
		ix.basic[b.Username] = e
	}
	for _, cert := range cr.Certificates {
		if cert.Subject != "" {
			m := ix.certSubject[cert.Subject]
			m.add(c)
			ix.certSubject[cert.Subject] = m
		}
		if cert.URISAN != "" {
			m := ix.certURI[cert.URISAN]
			m.add(c)
			ix.certURI[cert.URISAN] = m
		}
	}
	return nil
}

// addClaims indexes one credentials.jwt claims entry of c under its shape.
func (ix *Index) addClaims(iss string, claims map[string]string, c *expr.Consumer, shapes map[shapeKey]*claimShape) {
	names := make([]string, 0, len(claims))
	for n := range claims {
		names = append(names, n)
	}
	slices.Sort(names)
	var sig strings.Builder
	values := make([]string, len(names))
	for i, n := range names {
		sig.WriteString(strconv.Itoa(len(n)))
		sig.WriteByte(':')
		sig.WriteString(n)
		values[i] = claims[n]
	}
	key := shapeKey{iss, sig.String()}
	s := shapes[key]
	if s == nil {
		s = &claimShape{names: names, rules: map[uint64][]claimRule{}}
		shapes[key] = s
	}
	h := hashValues(ix.seed, values)
	slot := s.rules[h]
	for i := range slot {
		if slices.Equal(slot[i].values, values) {
			slot[i].m.add(c)
			return
		}
	}
	r := claimRule{values: values}
	r.m.add(c)
	s.rules[h] = append(slot, r)
}

// basicCredential decodes one credentials.basic entry.
func basicCredential(c *expr.Consumer, b v1alpha1.BasicCredential) (*BasicCredential, error) {
	salt, key, err := ParseBasicHash(b.Hash)
	if err != nil {
		return nil, errcode.Wrap(CodeSchema, fmt.Errorf("consumer %q basic credential: %w", c.Name, err))
	}
	n := Iterations(b.Iterations)
	if !ValidIterations(n) {
		return nil, errcode.Errorf(CodeIterations, "consumer %q basic credential: iterations %d outside %d to %d",
			c.Name, n, MinIterations, MaxIterations)
	}
	return &BasicCredential{
		Consumer:   c,
		Username:   b.Username,
		Salt:       salt,
		Key:        key,
		Iterations: n,
		Digest:     CredentialDigest(b.Hash, n),
	}, nil
}

// WithSecretKeys returns a view of ix whose API key lookups also consult k
// (literal digests first, then k). ix is not modified; a nil k gives a
// view without secretRef-held keys.
func (ix *Index) WithSecretKeys(k *KeyIndex) *Index {
	v := *ix
	v.keys = k
	return &v
}

// SecretKeys returns the secretRef-held API keys, sorted by Consumer, key
// name and reference, for NewKeyIndex. The slice is shared: read it only.
func (ix *Index) SecretKeys() []SecretKey { return ix.secretKeys }

// KeyIndex returns the KeyIndex of this view, or nil.
func (ix *Index) KeyIndex() *KeyIndex { return ix.keys }

// Retain takes a reference on the view's KeyIndex for a Filter that keeps
// the index (see KeyIndex.Retain); it returns false when the KeyIndex is
// already closed and always true without one.
func (ix *Index) Retain() bool { return ix.keys == nil || ix.keys.Retain() }

// Release drops a reference on the view's KeyIndex; a Filter calls it once
// from its Close for every successful acquisition.
func (ix *Index) Release() {
	if ix.keys != nil {
		ix.keys.Release()
	}
}

// Consumer returns the named Consumer's view.
func (ix *Index) Consumer(name string) (*expr.Consumer, bool) {
	c, ok := ix.consumers[name]
	return c, ok
}

// Consumers returns the views by name. The map is shared: read it only.
func (ix *Index) Consumers() map[string]*expr.Consumer { return ix.consumers }

// APIKey binds an API key by its SHA-256 digest (spec 06 requirement 14):
// the Consumer holding it, nil when unknown (401 RZ-AUTH-002), or
// ambiguous when two distinct Consumers hold it (401 RZ-AUTH-002).
func (ix *Index) APIKey(digest [32]byte) (c *expr.Consumer, ambiguous bool) {
	var m match
	if e, ok := ix.apiKeys[digest]; ok {
		m.merge(e)
	}
	if ix.keys != nil {
		if e, ok := ix.keys.lookup(digest); ok {
			if e.ambiguous {
				m.ambiguous = true
			}
			m.add(ix.consumers[e.consumer])
		}
	}
	return m.result()
}

// JWT binds a verified token of issuer iss (spec 06 requirements 16 to
// 18): Consumers with a credentials.jwt entry for iss whose subject equals
// the string claim sub, or whose claims all equal string claims; and
// Consumers with an oauthClients clientId equal to the string claim
// client_id (or, when client_id is absent, azp) that also list iss in
// credentials.jwt. The union is de-duplicated: one Consumer binds, none
// leaves the token unbound, two or more are ambiguous. Claims bindings cost
// one hash probe per distinct claim-name set configured for iss, whatever
// the number of Consumers sharing a set or a claim value; the lookup
// allocates nothing.
func (ix *Index) JWT(iss string, claims Claims) (c *expr.Consumer, ambiguous bool) {
	var m match
	if sub, ok := claims.StringClaim("sub"); ok {
		if e, ok := ix.jwtSubject[issSub{iss, sub}]; ok {
			m.merge(e)
		}
	}
	for _, s := range ix.jwtShapes[iss] {
		rules := s.probe(ix.seed, claims)
		for i := range rules {
			if rules[i].matches(s.names, claims) {
				m.merge(rules[i].m)
			}
		}
	}
	client, ok := claims.StringClaim("client_id")
	if !ok && !claims.HasClaim("client_id") {
		client, ok = claims.StringClaim("azp")
	}
	if ok {
		if e, found := ix.oauth[issClient{iss, client}]; found {
			m.merge(e)
		}
	}
	return m.result()
}

// Basic returns the credential of a byte-exact username (spec 06
// requirement 19): nil when undeclared, ambiguous when two Consumers
// declare it (RZ-CFG-035 prevents that statically).
func (ix *Index) Basic(username string) (cred *BasicCredential, ambiguous bool) {
	e, ok := ix.basic[username]
	if !ok {
		return nil, false
	}
	if e.ambiguous {
		return nil, true
	}
	return e.cred, false
}

// Certificate binds a leaf auth.mtls verified (spec 06 requirement 19):
// credentials.certificates entries match subject (the leaf's RFC 4514
// string) or one of its URI SANs, exactly; a leaf matching two Consumers
// (for example subject to A and uriSan to B) is ambiguous.
func (ix *Index) Certificate(subject string, uriSANs []string) (c *expr.Consumer, ambiguous bool) {
	var m match
	if subject != "" {
		if e, ok := ix.certSubject[subject]; ok {
			m.merge(e)
		}
	}
	for _, u := range uriSANs {
		if m.ambiguous {
			break
		}
		if e, ok := ix.certURI[u]; ok && u != "" {
			m.merge(e)
		}
	}
	return m.result()
}
