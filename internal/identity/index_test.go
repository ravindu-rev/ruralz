// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Tests for spec 06 section 2.2: requirement 12 (one immutable index per
// snapshot, read without locks), 14 (API key binding by digest, double
// match 401 RZ-AUTH-002), 16 (JWT issuer plus subject or string claims), 17
// (OAuth client_id, else azp, through the Consumer's jwt issuer,
// OQ-security-and-identity-24 (a)), 18 (de-duplicated union; two or more
// is ambiguous), 19 (basic usernames byte-exact; certificate subject and
// uriSan exact; a leaf matching two Consumers is ambiguous), and the
// section 6.3 property test (JWT matches equal a brute-force evaluation of
// rules 16 to 18).

const iss = "https://login.acme.example"

func mustIndex(t testing.TB, cs ...*v1alpha1.Consumer) *Index {
	t.Helper()
	ix, err := NewIndex(cs, nil)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	return ix
}

func TestReq14APIKeyBinding(t *testing.T) {
	ix := mustIndex(t,
		consumer("acme", withHash("primary", testKeyA), withHash("old", testKeyC)),
		consumer("beta", withHash("primary", testKeyB), withHash("shared", testKeyC)),
		consumer("gamma", withHash("one", testKeyB+"x"), withHash("two", testKeyB+"x")),
	)
	tests := []struct {
		name      string
		key       string
		want      string
		ambiguous bool
	}{
		{"literal hash", testKeyA, "acme", false},
		{"second Consumer", testKeyB, "beta", false},
		{"unknown digest", "no-such-key-0123456789abc", "", false},
		{"digest held by two Consumers", testKeyC, "", true},
		{"same Consumer twice is one binding", testKeyB + "x", "gamma", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, amb := ix.APIKey(DigestString(tt.key))
			if nameOf(c) != tt.want || amb != tt.ambiguous {
				t.Fatalf("APIKey = %q, %v; want %q, %v", nameOf(c), amb, tt.want, tt.ambiguous)
			}
		})
	}
}

func TestReq16JWTSubjectAndClaims(t *testing.T) {
	ix := mustIndex(t,
		consumer("by-subject", withSubject(iss, "acme-integration")),
		consumer("by-claims", withClaims(iss, map[string]string{"client_id": "acme", "tenant": "t1"})),
		consumer("by-one-claim", withClaims(iss, map[string]string{"role": "batch"})),
		consumer("other-issuer", withSubject("https://other.example", "acme-integration")),
		consumer("empty-claim", withClaims(iss, map[string]string{"team": ""})),
		consumer("other-claims", withClaims("https://other.example", map[string]string{"role": "admin"})),
	)
	tests := []struct {
		name      string
		iss       string
		claims    MapClaims
		want      string
		ambiguous bool
	}{
		{"subject", iss, MapClaims{"sub": "acme-integration"}, "by-subject", false},
		{"subject is byte-exact", iss, MapClaims{"sub": "Acme-integration"}, "", false},
		{"subject of another issuer", "https://other.example", MapClaims{"sub": "acme-integration"}, "other-issuer", false},
		{"claims of another issuer", "https://other.example", MapClaims{"role": "admin"}, "other-claims", false},
		{"claims are per issuer", iss, MapClaims{"role": "admin"}, "", false},
		{"issuer is byte-exact", iss + "/", MapClaims{"sub": "acme-integration"}, "", false},
		{"non-string sub never matches", iss, MapClaims{"sub": 42.0}, "", false},
		{"every claim matches", iss, MapClaims{"client_id": "acme", "tenant": "t1"}, "by-claims", false},
		{"one claim missing", iss, MapClaims{"client_id": "acme"}, "", false},
		{"one claim differs", iss, MapClaims{"client_id": "acme", "tenant": "t2"}, "", false},
		{"numeric claim never matches", iss, MapClaims{"role": 1.0}, "", false},
		{"array claim never matches", iss, MapClaims{"role": []any{"batch"}}, "", false},
		{"single claim", iss, MapClaims{"role": "batch", "extra": "x"}, "by-one-claim", false},
		{"empty expected value", iss, MapClaims{"team": ""}, "empty-claim", false},
		{"subject and claims bind two Consumers", iss, MapClaims{"sub": "acme-integration", "role": "batch"}, "", true},
		{"nothing", iss, MapClaims{}, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, amb := ix.JWT(tt.iss, tt.claims)
			if nameOf(c) != tt.want || amb != tt.ambiguous {
				t.Fatalf("JWT = %q, %v; want %q, %v", nameOf(c), amb, tt.want, tt.ambiguous)
			}
		})
	}
}

func TestReq17OAuthClientBinding(t *testing.T) {
	ix := mustIndex(t,
		consumer("portal", withSubject(iss, "portal-sub"), withClient("acme-portal")),
		consumer("no-jwt-issuer", withClient("orphan")),
		consumer("elsewhere", withSubject("https://other.example", "x"), withClient("elsewhere-client")),
	)
	tests := []struct {
		name   string
		claims MapClaims
		want   string
	}{
		{"client_id", MapClaims{"client_id": "acme-portal"}, "portal"},
		{"azp when client_id is absent", MapClaims{"azp": "acme-portal"}, "portal"},
		{"client_id wins over azp", MapClaims{"client_id": "other", "azp": "acme-portal"}, ""},
		{"non-string client_id is present: no azp fallback", MapClaims{"client_id": 7.0, "azp": "acme-portal"}, ""},
		{"null client_id is present", MapClaims{"client_id": nil, "azp": "acme-portal"}, ""},
		{"non-string azp", MapClaims{"azp": true}, ""},
		{"client without a jwt entry never binds", MapClaims{"client_id": "orphan"}, ""},
		{"client whose jwt issuer differs", MapClaims{"client_id": "elsewhere-client"}, ""},
		{"subject and client of one Consumer bind once", MapClaims{"sub": "portal-sub", "client_id": "acme-portal"}, "portal"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, amb := ix.JWT(iss, tt.claims)
			if nameOf(c) != tt.want || amb {
				t.Fatalf("JWT = %q, %v; want %q, false", nameOf(c), amb, tt.want)
			}
		})
	}
	// The other issuer binds its own client.
	if c, _ := ix.JWT("https://other.example", MapClaims{"client_id": "elsewhere-client"}); nameOf(c) != "elsewhere" {
		t.Fatalf("JWT(other) = %q, want elsewhere", nameOf(c))
	}
}

func TestReq18DoubleMatchIsAmbiguous(t *testing.T) {
	ix := mustIndex(t,
		consumer("a", withSubject(iss, "s")),
		consumer("b", withSubject(iss, "s")),
		consumer("c", withSubject(iss, "t")),
		consumer("d", withSubject(iss, "portal"), withClient("cli")),
		consumer("e", withSubject(iss, "e"), withClient("cli")),
	)
	for _, claims := range []MapClaims{{"sub": "s"}, {"client_id": "cli"}, {"sub": "t", "client_id": "cli"}} {
		if c, amb := ix.JWT(iss, claims); c != nil || !amb {
			t.Errorf("JWT(%v) = %q, %v; want ambiguous", claims, nameOf(c), amb)
		}
	}
	if c, amb := ix.JWT(iss, MapClaims{"sub": "t"}); nameOf(c) != "c" || amb {
		t.Errorf("JWT(t) = %q, %v", nameOf(c), amb)
	}
}

func TestReq19BasicBinding(t *testing.T) {
	ix := mustIndex(t,
		consumer("acme", withBasic("acme-batch", cfgBasicHash, nil)),
		consumer("dup1", withBasic("shared", cfgBasicHash, i32(700000))),
		consumer("dup2", withBasic("shared", cfgBasicHash, nil)),
	)
	cred, amb := ix.Basic("acme-batch")
	if cred == nil || amb || nameOf(cred.Consumer) != "acme" {
		t.Fatalf("Basic(acme-batch) = %+v, %v", cred, amb)
	}
	salt, key, _ := ParseBasicHash(cfgBasicHash)
	if cred.Username != "acme-batch" || cred.Iterations != DefaultIterations || cred.Salt != salt || cred.Key != key ||
		cred.Digest != CredentialDigest(cfgBasicHash, DefaultIterations) {
		t.Fatalf("credential decoded wrong: %+v", cred)
	}
	for _, u := range []string{"Acme-batch", "acme-batch ", ""} {
		if c, amb := ix.Basic(u); c != nil || amb {
			t.Errorf("Basic(%q) matched; usernames are byte-exact", u)
		}
	}
	if c, amb := ix.Basic("shared"); c != nil || !amb {
		t.Errorf("Basic(shared) = %+v, %v; want ambiguous", c, amb)
	}
}

func TestReq19CertificateBinding(t *testing.T) {
	ix := mustIndex(t,
		consumer("batch", withCert("batch-client", "", "spiffe://acme.example/batch")),
		consumer("by-subject", withCert("s", "CN=client,O=Acme", "")),
		consumer("by-uri", withCert("u", "", "spiffe://acme.example/other")),
	)
	tests := []struct {
		name      string
		subject   string
		uris      []string
		want      string
		ambiguous bool
	}{
		{"uriSan", "CN=x", []string{"spiffe://acme.example/batch"}, "batch", false},
		{"one of several SANs", "", []string{"https://a", "spiffe://acme.example/batch"}, "batch", false},
		{"subject exact", "CN=client,O=Acme", nil, "by-subject", false},
		{"subject is exact, not normalized", "CN=client, O=Acme", nil, "", false},
		{"no match", "CN=nobody", []string{"spiffe://x"}, "", false},
		{"subject to one Consumer, uriSan to another", "CN=client,O=Acme", []string{"spiffe://acme.example/other"}, "", true},
		{"two SANs of two Consumers", "", []string{"spiffe://acme.example/batch", "spiffe://acme.example/other"}, "", true},
		{"empty inputs", "", []string{""}, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, amb := ix.Certificate(tt.subject, tt.uris)
			if nameOf(c) != tt.want || amb != tt.ambiguous {
				t.Fatalf("Certificate = %q, %v; want %q, %v", nameOf(c), amb, tt.want, tt.ambiguous)
			}
		})
	}
}

func TestNewIndexRejectsWhatValidationRejects(t *testing.T) {
	tests := []struct {
		name string
		c    *v1alpha1.Consumer
		code string
	}{
		{"malformed literal hash", consumer("a", func(c *v1alpha1.Consumer) {
			c.Spec.Credentials.APIKeys = []v1alpha1.APIKey{{Name: "k", Hash: "sha256:DEADBEEF"}}
		}), CodeSchema},
		{"malformed basic hash", consumer("a", withBasic("u", "pbkdf2-sha256:secretish", nil)), CodeSchema},
		{"iterations below range", consumer("a", withBasic("u", cfgBasicHash, i32(599999))), CodeIterations},
		{"iterations above range", consumer("a", withBasic("u", cfgBasicHash, i32(1000001))), CodeIterations},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewIndex([]*v1alpha1.Consumer{tt.c}, nil)
			if code, _ := errcode.CodeOf(err); code != tt.code {
				t.Fatalf("NewIndex error %v, want %s", err, tt.code)
			}
			if strings.Contains(err.Error(), "secretish") || strings.Contains(err.Error(), "DEADBEEF") {
				t.Fatalf("error echoes credential material: %v", err)
			}
		})
	}
}

func TestReq12IndexSharesCompiledViews(t *testing.T) {
	src := []*v1alpha1.Consumer{consumer("acme", withHash("k", testKeyA)), nil, consumer("acme")}
	compiled := CompileConsumers(src)
	ix, err := NewIndex(src, compiled.Map())
	if err != nil {
		t.Fatal(err)
	}
	want, _ := compiled.Get("acme")
	if c, _ := ix.APIKey(DigestString(testKeyA)); c != want {
		t.Fatal("the index returned a view other than the snapshot's")
	}
	if c, ok := ix.Consumer("acme"); !ok || c != want || len(ix.Consumers()) != 1 {
		t.Fatal("Consumer lookup wrong")
	}
	if !ix.Retain() {
		t.Fatal("Retain without a KeyIndex must succeed")
	}
	ix.Release()
	if ix.KeyIndex() != nil || len(ix.SecretKeys()) != 0 {
		t.Fatal("literal-only index has secret keys")
	}
}

// Requirement 12: lookups run on the request path; they allocate nothing.
func TestReq12LookupsAllocateNothing(t *testing.T) {
	ix := mustIndex(t,
		consumer("a", withHash("k", testKeyA), withSubject(iss, "s"), withClient("cli"),
			withClaims(iss, map[string]string{"role": "r", "team": "t"})),
		consumer("b", withBasic("u", cfgBasicHash, nil), withCert("c", "CN=b", "spiffe://b")),
	)
	d := DigestString(testKeyA)
	claims := MapClaims{"sub": "s", "role": "r", "team": "t", "azp": "cli"}
	uris := []string{"spiffe://b"}
	checks := map[string]func(){
		"APIKey":      func() { ix.APIKey(d) },
		"JWT":         func() { ix.JWT(iss, claims) },
		"Basic":       func() { ix.Basic("u") },
		"Certificate": func() { ix.Certificate("CN=b", uris) },
	}
	for name, fn := range checks {
		if n := testing.AllocsPerRun(100, fn); n != 0 {
			t.Errorf("%s allocates %v times", name, n)
		}
	}
}

// countingClaims counts claim reads, the work a JWT lookup does.
type countingClaims struct {
	MapClaims
	reads int
}

func (c *countingClaims) StringClaim(name string) (string, bool) {
	c.reads++
	return c.MapClaims.StringClaim(name)
}

func (c *countingClaims) HasClaim(name string) bool {
	c.reads++
	return c.MapClaims.HasClaim(name)
}

// "Index lookups constant time": the work of a JWT lookup, the only lookup
// that is more than one hash probe, does not grow with the number of
// Consumers: 10 and 10,000 Consumers with claims bindings cost the same
// number of claim reads. The fixtures include Consumers that all share the
// alphabetically first claim pair ("env": "prod"), where an index on one
// claim would scan every Consumer's rule.
func TestReq12JWTLookupWorkIndependentOfConsumerCount(t *testing.T) {
	tests := []struct {
		name   string
		claims func(i int) map[string]string
		token  MapClaims
	}{
		{
			name: "unique first claim",
			claims: func(i int) map[string]string {
				return map[string]string{"client_id": fmt.Sprintf("app-%d", i), "tenant": "t"}
			},
			token: MapClaims{"sub": "sub-7", "client_id": "app-7", "tenant": "t"},
		},
		{
			name: "shared first claim",
			claims: func(i int) map[string]string {
				return map[string]string{"env": "prod", "tenant": fmt.Sprintf("t-%d", i)}
			},
			token: MapClaims{"sub": "sub-7", "env": "prod", "tenant": "t-7"},
		},
		{
			name: "shared first claim, three shapes",
			claims: func(i int) map[string]string {
				switch i % 3 {
				case 0:
					return map[string]string{"env": "prod", "tenant": fmt.Sprintf("t-%d", i)}
				case 1:
					return map[string]string{"env": "prod", "region": "eu", "tenant": fmt.Sprintf("t-%d", i)}
				default:
					return map[string]string{"env": "prod", "team": fmt.Sprintf("t-%d", i)}
				}
			},
			token: MapClaims{"sub": "sub-7", "env": "prod", "region": "eu", "tenant": "t-7", "team": "t-7"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reads := func(n int) int {
				cs := make([]*v1alpha1.Consumer, n)
				for i := range cs {
					cs[i] = consumer(fmt.Sprintf("c%d", i), withClaims(iss, tt.claims(i)),
						withSubject(iss, fmt.Sprintf("sub-%d", i)))
				}
				ix := mustIndex(t, cs...)
				cl := &countingClaims{MapClaims: tt.token}
				c, amb := ix.JWT(iss, cl)
				if nameOf(c) != "c7" || amb {
					t.Fatalf("JWT with %d Consumers = %q, %v", n, nameOf(c), amb)
				}
				return cl.reads
			}
			if small, large := reads(10), reads(10_000); small != large {
				t.Fatalf("claim reads grow with Consumers: %d for 10, %d for 10,000", small, large)
			}
		})
	}
}

// Requirement 16 on a hash collision: a slot holding a rule whose values
// differ from the token's (a 64-bit collision, forced here) never binds;
// only the confirmed rule does.
func TestReq16ClaimsHashHitIsConfirmed(t *testing.T) {
	ix := mustIndex(t,
		consumer("a", withClaims(iss, map[string]string{"env": "prod", "tenant": "t1"})),
		consumer("b", withClaims(iss, map[string]string{"env": "prod", "tenant": "t2"})),
	)
	shapes := ix.jwtShapes[iss]
	if len(shapes) != 1 || len(shapes[0].rules) != 2 {
		t.Fatalf("shapes = %+v, want one shape with two slots", shapes)
	}
	s := shapes[0]
	// Move b's rule into a's slot, as a collision would place it.
	ha := hashValues(ix.seed, []string{"prod", "t1"})
	hb := hashValues(ix.seed, []string{"prod", "t2"})
	s.rules[ha] = append(s.rules[ha], s.rules[hb]...)
	delete(s.rules, hb)
	tests := []struct {
		token MapClaims
		want  string
	}{
		{MapClaims{"env": "prod", "tenant": "t1"}, "a"},
		{MapClaims{"env": "prod", "tenant": "t2"}, ""},
		{MapClaims{"env": "prod", "tenant": "t3"}, ""},
		{MapClaims{"env": "prod"}, ""},
		{MapClaims{"env": "prod", "tenant": 1.0}, ""},
	}
	for _, tt := range tests {
		if c, amb := ix.JWT(iss, tt.token); nameOf(c) != tt.want || amb {
			t.Errorf("JWT(%v) = %q, %v; want %q", tt.token, nameOf(c), amb, tt.want)
		}
	}
}

// Requirement 18 inside one shape: Consumers expecting the same values
// share one rule and are ambiguous; one Consumer repeating them is not.
func TestReq18SameClaimsInOneShape(t *testing.T) {
	same := map[string]string{"env": "prod", "tenant": "t"}
	tok := MapClaims{"env": "prod", "tenant": "t"}
	if c, amb := mustIndex(t, consumer("a", withClaims(iss, same)), consumer("b", withClaims(iss, same))).JWT(iss, tok); c != nil || !amb {
		t.Errorf("two Consumers with the same claims = %q, %v; want ambiguous", nameOf(c), amb)
	}
	if c, amb := mustIndex(t, consumer("a", withClaims(iss, same), withClaims("other", same))).JWT(iss, tok); nameOf(c) != "a" || amb {
		t.Errorf("one Consumer = %q, %v", nameOf(c), amb)
	}
}

// Requirement 16: an entry with an empty subject and no claims binds no
// token of its issuer (Check reports it as RZ-CFG-005).
func TestReq16EmptyBindingBindsNothing(t *testing.T) {
	c := consumer("a")
	c.Spec.Credentials.JWT = []v1alpha1.JWTBinding{{Issuer: iss, Claims: map[string]string{}}, {Issuer: "other"}}
	ix := mustIndex(t, c)
	for _, tok := range []MapClaims{{}, {"sub": ""}, {"sub": "x", "env": "prod"}} {
		for _, i := range []string{iss, "other"} {
			if got, amb := ix.JWT(i, tok); got != nil || amb {
				t.Errorf("JWT(%s, %v) = %q, %v; want unbound", i, tok, nameOf(got), amb)
			}
		}
	}
}

// bruteJWT evaluates rules 16 to 18 directly over the Consumer list.
func bruteJWT(cs []*v1alpha1.Consumer, iss string, claims MapClaims) (string, bool) {
	var matched []string
	for _, c := range cs {
		hit := false
		hasIss := false
		for _, j := range c.Spec.Credentials.JWT {
			if j.Issuer != iss {
				continue
			}
			hasIss = true
			if sub, ok := claims["sub"].(string); ok && j.Subject != "" && sub == j.Subject {
				hit = true
			}
			if len(j.Claims) > 0 {
				all := true
				for k, v := range j.Claims {
					if got, ok := claims[k].(string); !ok || got != v {
						all = false
					}
				}
				hit = hit || all
			}
		}
		if hasIss {
			client, ok := claims["client_id"].(string)
			if _, present := claims["client_id"]; !present {
				client, ok = claims["azp"].(string)
			}
			for _, o := range c.Spec.Credentials.OAuthClients {
				if ok && o.ClientID == client {
					hit = true
				}
			}
		}
		if hit && !slices.Contains(matched, c.Metadata.Name) {
			matched = append(matched, c.Metadata.Name)
		}
	}
	switch len(matched) {
	case 0:
		return "", false
	case 1:
		return matched[0], false
	default:
		return "", true
	}
}

// splitmix is a deterministic splitmix64 sequence: property rounds are
// reproducible from the seed.
type splitmix uint64

// IntN returns a value in [0, n).
func (s *splitmix) IntN(n int) int {
	*s += 0x9e3779b97f4a7c15
	z := uint64(*s)
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	z ^= z >> 31
	return int(z>>33) % n
}

// Section 6.3 property: for random Consumer sets, JWT()'s result equals a
// brute-force evaluation of rules 16 to 18.
func TestReq16to18JWTMatchesBruteForce(t *testing.T) {
	seed := splitmix(0x0616_0018)
	rng := &seed
	issuers := []string{"i1", "i2"}
	vals := []string{"x", "y", "z"}
	names := []string{"sub", "client_id", "azp", "role", "tenant"}
	pick := func(s []string) string { return s[rng.IntN(len(s))] }
	for round := range 2000 {
		n := 1 + rng.IntN(6)
		cs := make([]*v1alpha1.Consumer, n)
		for i := range cs {
			c := consumer(fmt.Sprintf("c%d", i))
			for range rng.IntN(3) {
				j := v1alpha1.JWTBinding{Issuer: pick(issuers)}
				if rng.IntN(2) == 0 {
					j.Subject = pick(vals)
				} else {
					j.Claims = map[string]string{}
					for range 1 + rng.IntN(2) {
						j.Claims[pick(names[1:])] = pick(vals)
					}
				}
				c.Spec.Credentials.JWT = append(c.Spec.Credentials.JWT, j)
			}
			for range rng.IntN(2) {
				c.Spec.Credentials.OAuthClients = append(c.Spec.Credentials.OAuthClients, v1alpha1.OAuthClient{ClientID: pick(vals)})
			}
			cs[i] = c
		}
		claims := MapClaims{}
		for _, name := range names {
			switch rng.IntN(4) {
			case 0: // absent
			case 1:
				claims[name] = 1.0
			default:
				claims[name] = pick(vals)
			}
		}
		i := pick(issuers)
		ix := mustIndex(t, cs...)
		c, amb := ix.JWT(i, claims)
		want, wantAmb := bruteJWT(cs, i, claims)
		if nameOf(c) != want || amb != wantAmb {
			t.Fatalf("round %d: JWT(%s, %v) = %q, %v; brute force %q, %v", round, i, claims, nameOf(c), amb, want, wantAmb)
		}
	}
}

// consumersOfSize builds n Consumers with every credential kind.
func consumersOfSize(n int) []*v1alpha1.Consumer {
	cs := make([]*v1alpha1.Consumer, n)
	for i := range cs {
		cs[i] = consumer(fmt.Sprintf("c%d", i),
			withHash("k", fmt.Sprintf("%s-%d", testKeyA, i)),
			withSubject(iss, fmt.Sprintf("sub-%d", i)),
			withClaims(iss, map[string]string{"app": fmt.Sprintf("app-%d", i)}),
			withBasic(fmt.Sprintf("user-%d", i), cfgBasicHash, nil),
			withCert("c", "", fmt.Sprintf("spiffe://c/%d", i)))
	}
	return cs
}

// Benchmarks for "Index lookups constant time": per-lookup cost at 10 and
// 100,000 Consumers.
func BenchmarkIndex(b *testing.B) {
	for _, n := range []int{10, 100_000} {
		ix, err := NewIndex(consumersOfSize(n), nil)
		if err != nil {
			b.Fatal(err)
		}
		d := DigestString(fmt.Sprintf("%s-%d", testKeyA, 7))
		claims := MapClaims{"sub": "sub-7", "app": "app-7"}
		uris := []string{"spiffe://c/7"}
		user := "user-7"
		b.Run(fmt.Sprintf("APIKey/consumers=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				ix.APIKey(d)
			}
		})
		b.Run(fmt.Sprintf("JWT/consumers=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				ix.JWT(iss, claims)
			}
		})
		b.Run(fmt.Sprintf("Basic/consumers=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				ix.Basic(user)
			}
		})
		b.Run(fmt.Sprintf("Certificate/consumers=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				ix.Certificate("", uris)
			}
		})
	}
}

// BenchmarkJWTSharedFirstClaim: claims bindings whose alphabetically first
// claim pair ("env": "prod") every Consumer shares cost the same per lookup
// at 10 and 10,000 Consumers ("Index lookups constant time"; spec 06
// requirement 32's auth.jwt budget).
func BenchmarkJWTSharedFirstClaim(b *testing.B) {
	for _, n := range []int{10, 10_000} {
		cs := make([]*v1alpha1.Consumer, n)
		for i := range cs {
			cs[i] = consumer(fmt.Sprintf("c%d", i),
				withClaims(iss, map[string]string{"env": "prod", "tenant": fmt.Sprintf("t-%d", i)}))
		}
		ix, err := NewIndex(cs, nil)
		if err != nil {
			b.Fatal(err)
		}
		claims := MapClaims{"sub": "s", "env": "prod", "tenant": "t-7"}
		b.Run(fmt.Sprintf("consumers=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				ix.JWT(iss, claims)
			}
		})
	}
}

// BenchmarkNewIndex measures the per-snapshot compile of 10,000 Consumers.
func BenchmarkNewIndex(b *testing.B) {
	cs := consumersOfSize(10_000)
	compiled := CompileConsumers(cs).Map()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := NewIndex(cs, compiled); err != nil {
			b.Fatal(err)
		}
	}
}

var _ Claims = MapClaims(nil)
