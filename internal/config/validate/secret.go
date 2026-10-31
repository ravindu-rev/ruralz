// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package validate

import (
	"net/url"
	"path"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/egress"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// destRule says where a secret field's value goes
// (docs/architecture/02-configuration-model.md "Secret-to-destination
// binding").
type destRule uint8

const (
	// destLocal is verification material and keys that never leave the
	// Node.
	destLocal destRule = iota
	// destStateStore is a State Store URL, sent only to the State Store it
	// names.
	destStateStore
	// destTokenURL is an auth.upstream-oauth2 clientSecret, sent only to
	// the origin of its Policy's tokenUrl.
	destTokenURL
)

// secretClass is what one x-ruralz-secret field holds and where it goes.
type secretClass struct {
	kind secret.Kind
	dest destRule
}

// classify returns the class of the secret field whose key is
// "<kind>|<Policy type or empty>|<schema path>", as secretKey builds it.
// Every x-ruralz-secret field of the hub schema has a row;
// TestEverySecretFieldClassified fails when one is added without. An
// AIProvider credentials.apiKey is local here: its baseUrl destination is
// Planned (M3), with the kind, which no M1 Node serves (RZ-CFG-040).
func classify(key string) (secretClass, bool) {
	switch key {
	case "Gateway||spec.listeners[].tls.certificates[].certificate",
		"Gateway||spec.telemetry.otlp.tls.clientCertificate",
		"Gateway||spec.stateStore.tls.clientCertificate",
		"Gateway||spec.stateStore.cache.tls.clientCertificate",
		"Upstream||spec.tls.clientCertificate":
		return secretClass{kind: secret.KindPEMCertificate, dest: destLocal}, true
	case "Gateway||spec.listeners[].tls.certificates[].privateKey",
		"Gateway||spec.telemetry.otlp.tls.clientKey",
		"Gateway||spec.stateStore.tls.clientKey",
		"Gateway||spec.stateStore.cache.tls.clientKey",
		"Upstream||spec.tls.clientKey":
		return secretClass{kind: secret.KindPEMPrivateKey, dest: destLocal}, true
	case "Gateway||spec.telemetry.otlp.tls.caCertificate",
		"Gateway||spec.stateStore.tls.caCertificate",
		"Gateway||spec.stateStore.cache.tls.caCertificate",
		"Upstream||spec.tls.caCertificate",
		"Policy|auth.mtls|spec.config.caCertificate":
		return secretClass{kind: secret.KindPEMCertPool, dest: destLocal}, true
	case "Policy|auth.mtls|spec.config.crl":
		return secretClass{kind: secret.KindPEMCRL, dest: destLocal}, true
	case "Gateway||spec.stateStore.url", "Gateway||spec.stateStore.cache.url":
		return secretClass{kind: secret.KindStateStoreURL, dest: destStateStore}, true
	case "Policy|auth.upstream-oauth2|spec.config.clientSecret":
		return secretClass{kind: secret.KindOpaque, dest: destTokenURL}, true
	case "Consumer||spec.credentials.apiKeys[].secretRef":
		return secretClass{kind: secret.KindAPIKey, dest: destLocal}, true
	case "AIProvider||spec.credentials.apiKey":
		return secretClass{kind: secret.KindOpaque, dest: destLocal}, true
	default:
		return secretClass{kind: secret.KindOpaque, dest: destLocal}, false
	}
}

// secretKey returns the classify key of the field at the instance path p
// of a resource of kind k: list elements become "[]", as in schema paths.
func secretKey(k v1alpha1.Kind, policyType string, p diag.Path) string {
	var b strings.Builder
	b.WriteString(string(k))
	b.WriteByte('|')
	if k == v1alpha1.KindPolicy {
		b.WriteString(policyType)
	}
	b.WriteByte('|')
	for i, e := range p {
		if e.Kind != diag.ElemField {
			b.WriteString("[]")
			continue
		}
		if i > 0 {
			b.WriteByte('.')
		}
		b.WriteString(e.Name)
	}
	return b.String()
}

// secretUse records the secretRef of the x-ruralz-secret field at c: a
// SecretValue holding secretRef, or a SecretRef itself (a Consumer API
// key), with the kind of value it holds and its destination.
func (u *unit) secretUse(c *schemaidx.Cursor, policyType string) {
	node := c.Node
	if node == nil || node.Kind != tree.KindMap {
		return
	}
	refNode := node
	if inner, ok := node.Get("secretRef"); ok && inner != nil && inner.Kind == tree.KindMap {
		refNode = inner
	}
	ref := v1alpha1.SecretRef{
		Provider: v1alpha1.SecretProvider(text(refNode, "provider")),
		Name:     text(refNode, "name"),
		Key:      text(refNode, "key"),
	}
	p := c.Path()
	cls, _ := classify(secretKey(u.r.Kind, policyType, p))
	u.secrets = append(u.secrets, hub.SecretUse{
		Ref: ref, Kind: cls.kind, Resource: *u.r.ResourceID(), Path: p,
		Loc: u.l.position(u.r, refNode.Pos), Destination: u.destination(cls),
	})
}

// text returns the string member name of the map n, or "".
func text(n *tree.Node, name string) string {
	if v, ok := n.Get(name); ok && v != nil && v.Kind == tree.KindString {
		return v.Text
	}
	return ""
}

// destination returns the destination of a secret of class cls in the
// unit's resource: hub.DestinationLocal, hub.DestinationStateStore, or the
// origin of the Policy's tokenUrl ("https://host:port", egress.Origin).
func (u *unit) destination(cls secretClass) string {
	switch cls.dest {
	case destStateStore:
		return hub.DestinationStateStore
	case destTokenURL:
		raw := ""
		if cfg, ok := u.r.Config.(*v1alpha1.AuthUpstreamOAuth2Config); ok && cfg != nil {
			raw = cfg.TokenURL
		} else if n := u.f.node(specPath(diag.Field("config"), diag.Field("tokenUrl"))); n != nil {
			raw = n.Text
		}
		return tokenOrigin(raw)
	default:
		return hub.DestinationLocal
	}
}

// tokenOrigin returns the origin of a tokenUrl in the form the Node binds
// a secret to (egress.Origin); a tokenUrl without a host, which the
// auth.upstream-oauth2 check reports as RZ-CFG-037, is its own text, so it
// still differs from every other destination.
func tokenOrigin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	return egress.Origin(u)
}

// secretSlot holds the first two uses, in canonical order, of one secret
// with distinct destinations: enough to find, for any later use, the
// earliest earlier use with another destination.
type secretSlot struct {
	first, second int
}

// newSlot returns an empty slot.
func newSlot() *secretSlot { return &secretSlot{first: -1, second: -1} }

// conflict returns the earliest recorded use whose destination is not
// dest, or -1.
func (s *secretSlot) conflict(uses []hub.SecretUse, dest string) int {
	switch {
	case s.first < 0:
		return -1
	case uses[s.first].Destination != dest:
		return s.first
	default:
		return s.second
	}
}

// record adds use i.
func (s *secretSlot) record(uses []hub.SecretUse, i int) {
	switch {
	case s.first < 0:
		s.first = i
	case s.second < 0 && uses[i].Destination != uses[s.first].Destination:
		s.second = i
	}
}

// secretSource is every use of one secret source: an env variable, a
// file, or a kubernetes or vault secret.
type secretSource struct {
	// whole holds the uses of a file without key, which read the whole
	// file; anyKeyed the uses with a key, whatever it is.
	whole, anyKeyed *secretSlot
	// keyed holds the uses per key.
	keyed map[string]*secretSlot
}

// sourceOf returns the source a reference reads and the key within it,
// and whether a use without key overlaps every key of the source. An env
// reference ignores its key; a file reference names its cleaned absolute
// path, and without key reads the whole file, so it overlaps each of its
// keys; kubernetes and vault references match by name and key.
func sourceOf(r v1alpha1.SecretRef) (src, key string, overlaps bool) {
	switch r.Provider {
	case v1alpha1.SecretProviderEnv:
		return "env\x00" + r.Name, "", false
	case v1alpha1.SecretProviderFile:
		name := r.Name
		if path.IsAbs(name) {
			name = path.Clean(name)
		}
		return "file\x00" + name, r.Key, true
	default:
		return string(r.Provider) + "\x00" + r.Name, r.Key, false
	}
}

// checkSecrets reports each secret use whose secret an earlier use, in
// canonical order, sends to another destination (RZ-CFG-041), naming the
// earliest such use: one secret has one destination, so no Bundle can
// send a Consumer API key or a TLS key to a tokenUrl. Two uses read one
// secret when sourceOf gives them one source and the same key, or when
// one reads a whole file and the other a key of it.
func checkSecrets(uses []hub.SecretUse, w *work, out *sink) {
	sources := map[string]*secretSource{}
	for i := range uses {
		w.charge(1)
		src, key, overlaps := sourceOf(uses[i].Ref)
		s := sources[src]
		if s == nil {
			s = &secretSource{whole: newSlot(), anyKeyed: newSlot(), keyed: map[string]*secretSlot{}}
			sources[src] = s
		}
		// against holds the slots of the uses that read the same secret;
		// own those this use joins.
		var against, own [2]*secretSlot
		switch {
		case overlaps && key == "":
			against, own = [2]*secretSlot{s.whole, s.anyKeyed}, [2]*secretSlot{s.whole}
		case overlaps:
			k := s.slot(key)
			against, own = [2]*secretSlot{k, s.whole}, [2]*secretSlot{k, s.anyKeyed}
		default:
			k := s.slot(key)
			against, own = [2]*secretSlot{k}, [2]*secretSlot{k}
		}
		j := -1
		for _, sl := range against {
			if sl == nil {
				continue
			}
			if c := sl.conflict(uses, uses[i].Destination); c >= 0 && (j < 0 || c < j) {
				j = c
			}
		}
		if j >= 0 {
			out.add(secretFinding(uses, i, j))
		}
		for _, sl := range own {
			if sl != nil {
				sl.record(uses, i)
			}
		}
	}
}

// slot returns the slot of the uses with key, created on first use.
func (s *secretSource) slot(key string) *secretSlot {
	sl := s.keyed[key]
	if sl == nil {
		sl = newSlot()
		s.keyed[key] = sl
	}
	return sl
}

// secretFinding returns the RZ-CFG-041 finding at use i, whose secret use
// j sends elsewhere.
func secretFinding(uses []hub.SecretUse, i, j int) diag.Diagnostic {
	this, other := uses[i], uses[j]
	thisRef, otherRef := secret.RefOf(this.Ref).String(), secret.RefOf(other.Ref).String()
	at := other.Resource.Kind + "/" + other.Resource.Name + " " + other.Path.String()
	var msg string
	if thisRef == otherRef {
		msg = "secretRef " + quote(thisRef) + " has destination " + quote(this.Destination) +
			" here and " + quote(other.Destination) + " at " + at
	} else {
		msg = "secretRef " + quote(thisRef) + " has destination " + quote(this.Destination) +
			" here, and " + quote(otherRef) + " reads the same secret with destination " + quote(other.Destination) + " at " + at
	}
	res := this.Resource
	return diag.Diagnostic{
		Code: CodeDestination, Severity: diag.SeverityError, Location: this.Loc,
		Resource: &res, Path: this.Path, Message: limitMessage(msg),
		Hint:    "one secret is sent to one destination; use a separate secret for each",
		Related: []diag.Related{{Location: other.Loc, Message: "first used in"}},
	}
}
