// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package builtin

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// The tests of this file hold every Build refusal under an offline code
// to a validation finding on the same config
// (docs/architecture/02-configuration-model.md "Semantic validation"): a
// Bundle that ruralz bundle validate passes is never refused at
// activation, under an offline code, by a Factory of the table. A config
// rule a Factory refuses is a finding of stage F or of the type's check
// (checks.Table()); a CEL field that does not compile is RZ-CFG-014 or
// RZ-CFG-015, which stage J reports from the same compiler.

// configCase is one Policy config of one type, as JSON text.
type configCase struct {
	typ    v1alpha1.PolicyType
	name   string
	config string
	// code is the code Build refuses the config with, also the code of a
	// validation finding about it; "" for a config both accept.
	code string
}

// Secret references and endpoints of the cases.
const (
	caRef         = `{"secretRef":{"provider":"env","name":"CLIENT_CA"}}`
	crlRef        = `{"secretRef":{"provider":"env","name":"CLIENT_CRL"}}`
	oauthRef      = `{"secretRef":{"provider":"env","name":"OAUTH_SECRET"}}`
	jwksURL       = `https://127.0.0.1:1/jwks` // refused by the egress guard at once: no network
	oauthEndpoint = `https://idp.example/token`
	codeCFG5      = "RZ-CFG-005"
	codeCEL       = "RZ-CFG-014"
	codeHTTPS     = "RZ-CFG-037"
)

// validCases are configs validation accepts and every Factory builds.
func validCases() []configCase {
	return []configCase{
		{typ: v1alpha1.PolicyTypeAuthJWT, name: "one issuer", config: `{"issuers":[{"issuer":"https://idp.example","jwksUrl":"` + jwksURL + `","audiences":["shop-api"]}]}`},
		{typ: v1alpha1.PolicyTypeAuthJWT, name: "two issuers", config: `{"issuers":[{"issuer":"https://a.example","jwksUrl":"` + jwksURL + `","audiences":["a","b"]},{"issuer":"tenant b","jwksUrl":"HTTPS://127.0.0.1:2/keys","audiences":["b"]}]}`},
		{typ: v1alpha1.PolicyTypeAuthAPIKey, name: "default header", config: `{}`},
		{typ: v1alpha1.PolicyTypeAuthAPIKey, name: "own header", config: `{"header":"x-shop-key"}`},
		{typ: v1alpha1.PolicyTypeAuthAPIKey, name: "empty header reads as the default", config: `{"header":""}`},
		{typ: v1alpha1.PolicyTypeAuthBasic, name: "no fields", config: `{}`},
		{typ: v1alpha1.PolicyTypeAuthMTLS, name: "subjects", config: `{"caCertificate":` + caRef + `,"subjects":[{"subject":"CN=shop,O=Example"},{"uriSan":"spiffe://shop.example/web"}]}`},
		{typ: v1alpha1.PolicyTypeAuthMTLS, name: "any verified leaf with a CRL", config: `{"caCertificate":` + caRef + `,"crl":` + crlRef + `}`},
		{typ: v1alpha1.PolicyTypeAuthzCEL, name: "header rule", config: `{"rule":"consumer != null"}`},
		{typ: v1alpha1.PolicyTypeAuthzCEL, name: "body rule", config: `{"rule":"request.body.amount < 100"}`},
		{typ: v1alpha1.PolicyTypeAuthzIP, name: "both lists", config: `{"allow":["10.0.0.0/8","2001:db8::1"],"deny":["10.1.0.0/16"]}`},
		{typ: v1alpha1.PolicyTypeAuthzIP, name: "deny beside an empty allow", config: `{"allow":[],"deny":["192.0.2.1"]}`},
		{typ: v1alpha1.PolicyTypeRateLimit, name: "one limit", config: `{"limits":[{"requests":100,"window":"1s"}]}`},
		{typ: v1alpha1.PolicyTypeRateLimit, name: "every field", config: `{"key":"consumer != null ? consumer.name : \"anonymous\"","limits":[{"requests":100,"window":"1s","burst":50,"perNodeCeiling":20},{"requests":1000,"window":"1m","burst":0,"when":"request.method == \"POST\""}],"cost":"2","responseHeaders":"none","localOnly":true}`},
		{typ: v1alpha1.PolicyTypeQuota, name: "default key", config: `{"consumerQuota":"daily"}`},
		{typ: v1alpha1.PolicyTypeQuota, name: "key and cost", config: `{"consumerQuota":"daily","key":"consumer.name + \":\" + request.method","cost":"request.method == \"POST\" ? 2 : 1"}`},
		{typ: v1alpha1.PolicyTypeValidationJSONSchema, name: "object schema", config: `{"schema":{"type":"object","properties":{"id":{"type":"integer"}},"required":["id"]}}`},
		{typ: v1alpha1.PolicyTypeValidationJSONSchema, name: "boolean schema", config: `{"schema":true}`},
		{typ: v1alpha1.PolicyTypeCORS, name: "credentials", config: `{"allowOrigins":["https://shop.example","null"],"allowMethods":["GET","POST"],"allowHeaders":["x-shop"],"maxAge":"10m","exposeHeaders":["x-total"]}`},
		{typ: v1alpha1.PolicyTypeCORS, name: "wildcards", config: `{"allowOrigins":["*"],"allowMethods":["GET"],"allowHeaders":["*"],"exposeHeaders":["*"]}`},
		{typ: v1alpha1.PolicyTypeCORS, name: "credentialed origin", config: `{"allowOrigins":["https://shop.example:8443"],"allowMethods":["GET"],"allowCredentials":true}`},
		{typ: v1alpha1.PolicyTypeCache, name: "no key", config: `{}`},
		{typ: v1alpha1.PolicyTypeCache, name: "per consumer", config: `{"key":"consumer != null ? consumer.name : \"anonymous\""}`},
		{typ: v1alpha1.PolicyTypeHeaders, name: "both sides", config: `{"request":{"set":[{"name":"x-a","value":"1"}],"add":[{"name":"x-b","valueExpression":"request.method"}],"remove":["x-c"]},"response":{"set":[{"name":"x-d","value":""}]}}`},
		{typ: v1alpha1.PolicyTypeTransformRequest, name: "headers, query and path", config: `{"set":[{"target":"header","name":"x-tenant","valueExpression":"\"t1\""},{"target":"query","name":"page","valueExpression":"\"1\""},{"target":"path","valueExpression":"\"/v2\" + request.path"}],"remove":[{"target":"header","name":"x-debug"}]}`},
		{typ: v1alpha1.PolicyTypeTransformRequest, name: "body", config: `{"body":"request.body","contentType":"application/json","set":[{"target":"body","name":"meta.source","valueExpression":"\"gateway\""}],"arrayOps":[{"op":"move","from":"items","to":"data.items"}],"replace":[{"pattern":"foo","replacement":"bar","literal":true}]}`},
		{typ: v1alpha1.PolicyTypeTransformResponse, name: "header and body", config: `{"set":[{"target":"header","name":"x-r","valueExpression":"\"1\""}],"remove":[{"target":"body","name":"secret"}],"arrayOps":[{"op":"delete","from":"items.*.internal"}]}`},
		{typ: v1alpha1.PolicyTypeAuthUpstreamOAuth2, name: "client credentials", config: `{"tokenUrl":"` + oauthEndpoint + `","clientId":"shop","clientSecret":` + oauthRef + `,"scopes":["read"],"timeout":"3s"}`},
		{typ: v1alpha1.PolicyTypeAuthUpstreamOAuth2, name: "default timeout", config: `{"tokenUrl":"` + oauthEndpoint + `","clientId":"shop","clientSecret":` + oauthRef + `}`},
	}
}

// refusedCases are configs a Factory refuses under an offline code, at
// least one per rule a Factory enforces on a config.
func refusedCases() []configCase {
	long := strings.Repeat("x", 300)
	var many []string
	for i := range 33 {
		many = append(many, `"x-h`+strconv.Itoa(i)+`"`)
	}
	return []configCase{
		{v1alpha1.PolicyTypeAuthJWT, "no issuers", `{"issuers":[]}`, codeCFG5},
		{v1alpha1.PolicyTypeAuthJWT, "empty issuer", `{"issuers":[{"issuer":"","jwksUrl":"` + jwksURL + `","audiences":["a"]}]}`, codeCFG5},
		{v1alpha1.PolicyTypeAuthJWT, "issuer twice", `{"issuers":[{"issuer":"i","jwksUrl":"` + jwksURL + `","audiences":["a"]},{"issuer":"i","jwksUrl":"` + jwksURL + `","audiences":["b"]}]}`, codeCFG5},
		{v1alpha1.PolicyTypeAuthJWT, "no audiences", `{"issuers":[{"issuer":"i","jwksUrl":"` + jwksURL + `","audiences":[]}]}`, codeCFG5},
		{v1alpha1.PolicyTypeAuthJWT, "http jwksUrl", `{"issuers":[{"issuer":"i","jwksUrl":"http://idp.example/jwks","audiences":["a"]}]}`, codeHTTPS},
		{v1alpha1.PolicyTypeAuthJWT, "jwksUrl without host", `{"issuers":[{"issuer":"i","jwksUrl":"https:///jwks","audiences":["a"]}]}`, codeHTTPS},
		{v1alpha1.PolicyTypeAuthJWT, "jwksUrl not a URL", `{"issuers":[{"issuer":"i","jwksUrl":"https://[::1","audiences":["a"]}]}`, codeHTTPS},
		{v1alpha1.PolicyTypeAuthAPIKey, "managed header", `{"header":"host"}`, codeCFG5},
		{v1alpha1.PolicyTypeAuthAPIKey, "not a token", `{"header":"x api key"}`, codeCFG5},
		{v1alpha1.PolicyTypeAuthAPIKey, "header too long", `{"header":"` + long + `"}`, codeCFG5},
		{v1alpha1.PolicyTypeAuthMTLS, "empty subject", `{"caCertificate":` + caRef + `,"subjects":[{"subject":""}]}`, codeCFG5},
		{v1alpha1.PolicyTypeAuthMTLS, "empty uriSan", `{"caCertificate":` + caRef + `,"subjects":[{"uriSan":""}]}`, codeCFG5},
		{v1alpha1.PolicyTypeAuthMTLS, "both", `{"caCertificate":` + caRef + `,"subjects":[{"subject":"CN=a","uriSan":"spiffe://a"}]}`, codeCFG5},
		{v1alpha1.PolicyTypeAuthMTLS, "neither", `{"caCertificate":` + caRef + `,"subjects":[{}]}`, codeCFG5},
		{v1alpha1.PolicyTypeAuthzCEL, "empty rule", `{"rule":""}`, codeCFG5},
		{v1alpha1.PolicyTypeAuthzCEL, "syntax error", `{"rule":"consumer !="}`, codeCEL},
		{v1alpha1.PolicyTypeAuthzCEL, "not a bool", `{"rule":"request.method"}`, codeCEL},
		{v1alpha1.PolicyTypeAuthzIP, "both written empty", `{"allow":[],"deny":[]}`, codeCFG5},
		{v1alpha1.PolicyTypeAuthzIP, "allow written empty", `{"allow":[]}`, codeCFG5},
		{v1alpha1.PolicyTypeAuthzIP, "not an address", `{"allow":["office-net"]}`, codeCFG5},
		{v1alpha1.PolicyTypeAuthzIP, "prefix too long", `{"deny":["10.0.0.0/33"]}`, codeCFG5},
		{v1alpha1.PolicyTypeRateLimit, "no limits", `{"limits":[]}`, codeCFG5},
		{v1alpha1.PolicyTypeRateLimit, "nine limits", `{"limits":[` + strings.Join(limits(9), ",") + `]}`, codeCFG5},
		{v1alpha1.PolicyTypeRateLimit, "window below 1ms", `{"limits":[{"requests":1,"window":"500us"}]}`, codeCFG5},
		{v1alpha1.PolicyTypeRateLimit, "window not whole milliseconds", `{"limits":[{"requests":1,"window":"1500us"}]}`, codeCFG5},
		{v1alpha1.PolicyTypeRateLimit, "interval below 1us", `{"limits":[{"requests":2000,"window":"1ms"}]}`, codeCFG5},
		{v1alpha1.PolicyTypeRateLimit, "ceiling above requests", `{"limits":[{"requests":10,"window":"1s","perNodeCeiling":11}]}`, codeCFG5},
		{v1alpha1.PolicyTypeRateLimit, "burst above requests", `{"limits":[{"requests":10,"window":"1s","burst":11}]}`, codeCFG5},
		{v1alpha1.PolicyTypeRateLimit, "two equal limits", `{"limits":[{"requests":10,"window":"1s"},{"requests":10,"window":"1000ms","burst":5}]}`, codeCFG5},
		{v1alpha1.PolicyTypeRateLimit, "requests below 1", `{"limits":[{"requests":0,"window":"1s"}]}`, codeCFG5},
		{v1alpha1.PolicyTypeRateLimit, "when does not compile", `{"limits":[{"requests":1,"window":"1s","when":"request.method =="}]}`, codeCEL},
		{v1alpha1.PolicyTypeRateLimit, "cost not an int", `{"limits":[{"requests":1,"window":"1s"}],"cost":"\"two\""}`, codeCEL},
		{v1alpha1.PolicyTypeQuota, "key does not compile", `{"consumerQuota":"daily","key":"consumer.name +"}`, codeCEL},
		{v1alpha1.PolicyTypeQuota, "cost not an int", `{"consumerQuota":"daily","cost":"request.method"}`, codeCEL},
		{v1alpha1.PolicyTypeValidationJSONSchema, "unknown type", `{"schema":{"type":"nope"}}`, codeCFG5},
		{v1alpha1.PolicyTypeValidationJSONSchema, "remote ref", `{"schema":{"$ref":"https://schemas.example/order.json"}}`, codeCFG5},
		{v1alpha1.PolicyTypeValidationJSONSchema, "not RE2", `{"schema":{"pattern":"(?<=a)b"}}`, codeCFG5},
		{v1alpha1.PolicyTypeCORS, "origin not serialized", `{"allowOrigins":["https://Shop.example"]}`, codeCFG5},
		{v1alpha1.PolicyTypeCORS, "origin with a path", `{"allowOrigins":["https://shop.example/"]}`, codeCFG5},
		{v1alpha1.PolicyTypeCORS, "method not a token", `{"allowMethods":["GE T"]}`, codeCFG5},
		{v1alpha1.PolicyTypeCORS, "credentials with any origin", `{"allowOrigins":["*"],"allowCredentials":true}`, codeCFG5},
		{v1alpha1.PolicyTypeCORS, "credentials with the null origin", `{"allowOrigins":["null"],"allowCredentials":true}`, codeCFG5},
		{v1alpha1.PolicyTypeCORS, "credentials with any header", `{"allowOrigins":["https://shop.example"],"allowHeaders":["*"],"allowCredentials":true}`, codeCFG5},
		{v1alpha1.PolicyTypeCache, "key does not compile", `{"key":"consumer."}`, codeCEL},
		{v1alpha1.PolicyTypeHeaders, "managed name", `{"request":{"remove":["host"]}}`, codeCFG5},
		{v1alpha1.PolicyTypeHeaders, "names equal ignoring case", `{"request":{"set":[{"name":"X-A","value":"1"},{"name":"x-a","value":"2"}]}}`, codeCFG5},
		{v1alpha1.PolicyTypeHeaders, "value with a leading space", `{"response":{"set":[{"name":"x-a","value":" padded"}]}}`, codeCFG5},
		{v1alpha1.PolicyTypeHeaders, "value and valueExpression", `{"response":{"add":[{"name":"x-a","value":"1","valueExpression":"\"2\""}]}}`, codeCFG5},
		{v1alpha1.PolicyTypeHeaders, "neither value nor valueExpression", `{"response":{"add":[{"name":"x-a"}]}}`, codeCFG5},
		{v1alpha1.PolicyTypeHeaders, "too many entries", `{"request":{"remove":[` + strings.Join(many, ",") + `]}}`, codeCFG5},
		{v1alpha1.PolicyTypeHeaders, "valueExpression does not compile", `{"request":{"add":[{"name":"x-a","valueExpression":"1 +"}]}}`, codeCEL},
		{v1alpha1.PolicyTypeTransformRequest, "pattern not RE2", `{"replace":[{"pattern":"(","replacement":"x"}]}`, codeCFG5},
		{v1alpha1.PolicyTypeTransformRequest, "not a media type", `{"body":"request.body","contentType":"not a media type"}`, codeCFG5},
		{v1alpha1.PolicyTypeTransformRequest, "path with a name", `{"set":[{"target":"path","name":"x","valueExpression":"\"/a\""}]}`, codeCFG5},
		{v1alpha1.PolicyTypeTransformRequest, "two paths", `{"set":[{"target":"path","valueExpression":"\"/a\""},{"target":"path","valueExpression":"\"/b\""}]}`, codeCFG5},
		{v1alpha1.PolicyTypeTransformRequest, "path removed", `{"remove":[{"target":"path","name":"x"}]}`, codeCFG5},
		{v1alpha1.PolicyTypeTransformRequest, "empty body path segment", `{"set":[{"target":"body","name":"a..b","valueExpression":"1"}]}`, codeCFG5},
		{v1alpha1.PolicyTypeTransformRequest, "move without to", `{"arrayOps":[{"op":"move","from":"a"}]}`, codeCFG5},
		{v1alpha1.PolicyTypeTransformRequest, "query name not a token", `{"set":[{"target":"query","name":"a b","valueExpression":"\"1\""}]}`, codeCFG5},
		{v1alpha1.PolicyTypeTransformRequest, "body does not compile", `{"body":"request.body +"}`, codeCEL},
		{v1alpha1.PolicyTypeTransformResponse, "managed header", `{"set":[{"target":"header","name":"content-length","valueExpression":"\"1\""}]}`, codeCFG5},
		{v1alpha1.PolicyTypeTransformResponse, "to holds a star", `{"arrayOps":[{"op":"append","from":"a","to":"b.*"}]}`, codeCFG5},
		{v1alpha1.PolicyTypeTransformResponse, "content-encoding in a body rewrite", `{"body":"response.body","set":[{"target":"header","name":"content-encoding","valueExpression":"\"gzip\""}]}`, codeCFG5},
		{v1alpha1.PolicyTypeTransformResponse, "delete with to", `{"arrayOps":[{"op":"delete","from":"a","to":"b"}]}`, codeCFG5},
		{v1alpha1.PolicyTypeAuthUpstreamOAuth2, "http tokenUrl", `{"tokenUrl":"http://idp.example/token","clientId":"shop","clientSecret":` + oauthRef + `}`, codeHTTPS},
		{v1alpha1.PolicyTypeAuthUpstreamOAuth2, "tokenUrl without host", `{"tokenUrl":"https://","clientId":"shop","clientSecret":` + oauthRef + `}`, codeHTTPS},
		{v1alpha1.PolicyTypeAuthUpstreamOAuth2, "zero timeout", `{"tokenUrl":"` + oauthEndpoint + `","clientId":"shop","clientSecret":` + oauthRef + `,"timeout":"0s"}`, codeCFG5},
	}
}

// limits returns n distinct limits[] entries.
func limits(n int) []string {
	var out []string
	for i := range n {
		out = append(out, `{"requests":`+strconv.Itoa(i+1)+`,"window":"1s"}`)
	}
	return out
}

// TestValidConfigsBuild: a config validation accepts builds at every
// attachment the effective chains run.
func TestValidConfigsBuild(t *testing.T) {
	h := newHarness(t)
	for _, c := range validCases() {
		t.Run(string(c.typ)+"/"+c.name, func(t *testing.T) {
			o := h.run(t, c.typ, c.config)
			if !o.validated {
				t.Fatalf("validation refuses the config: %s", o.describe())
			}
			if len(o.builds) == 0 {
				t.Fatalf("no effective chain runs the Policy: %s", o.describe())
			}
			for _, b := range o.builds {
				if b.err != nil {
					t.Errorf("Build at %s refuses a validated config: %v", b.scope, b.err)
				}
			}
		})
	}
}

// TestBuildRefusalsAreFindings: every config a Factory refuses under an
// offline code is a validation finding with that code about the same
// Policy, so the Bundle has no Revision and never reaches a Node.
func TestBuildRefusalsAreFindings(t *testing.T) {
	h := newHarness(t)
	for _, c := range refusedCases() {
		t.Run(string(c.typ)+"/"+c.name, func(t *testing.T) {
			o := h.run(t, c.typ, c.config)
			if o.validated {
				t.Fatalf("validation accepts a config Build refuses: %s", o.describe())
			}
			err := o.firstBuild()
			if got := codeOf(err); got != c.code {
				t.Errorf("Build = %v (code %q), want a refusal with %s", err, got, c.code)
			}
			if !slices.ContainsFunc(o.findings, func(d diag.Diagnostic) bool { return d.Code == c.code }) {
				t.Errorf("no %s finding about the Policy: %s", c.code, o.describe())
			}
		})
	}
}

// TestCasesCoverEveryFactory: every type the table builds has a valid
// case, so TestValidConfigsBuild and the fuzz seeds reach its Factory.
func TestCasesCoverEveryFactory(t *testing.T) {
	h := newHarness(t)
	covered := map[v1alpha1.PolicyType]bool{}
	for _, c := range validCases() {
		covered[c.typ] = true
	}
	for _, e := range h.reg.Entries() {
		if _, ok := h.filters.Factory(e.Type); ok && !covered[e.Type] {
			t.Errorf("%s has a Factory but no valid case", e.Type)
		}
	}
}

// fuzzTypes are the types FuzzBuildRefusalsAreFindings draws from, by
// index, and the index of each.
func fuzzTypes() ([]v1alpha1.PolicyType, map[v1alpha1.PolicyType]uint8) {
	var out []v1alpha1.PolicyType
	index := map[v1alpha1.PolicyType]uint8{}
	var next uint8
	for _, c := range append(validCases(), refusedCases()...) {
		if _, ok := index[c.typ]; !ok {
			out = append(out, c.typ)
			index[c.typ] = next
			next++
		}
	}
	return out, index
}

// FuzzBuildRefusalsAreFindings: for any config text of any type the table
// builds, a Bundle validation accepts is built by every Factory at every
// attachment without a refusal under an offline code; a refusal under a
// code that follows the digest, such as a secret the Node cannot resolve
// (RZ-CFG-026), is the Node's to report.
func FuzzBuildRefusalsAreFindings(f *testing.F) {
	types, index := fuzzTypes()
	for _, c := range append(validCases(), refusedCases()...) {
		f.Add(index[c.typ], c.config)
	}
	h := newHarness(f)
	f.Fuzz(func(t *testing.T, i uint8, config string) {
		typ := types[int(i)%len(types)]
		o := h.run(t, typ, config)
		if !o.validated {
			return
		}
		for _, b := range o.builds {
			if code := codeOf(b.err); b.err != nil && (code == "" || offline(code)) {
				t.Errorf("%s: Build at %s refuses a validated config with %v: %s", typ, b.scope, b.err, o.describe())
			}
		}
	})
}
