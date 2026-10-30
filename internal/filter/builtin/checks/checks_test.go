// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package checks

import (
	"bytes"
	"encoding/json"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/registry"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// checked lists the Policy types whose config rules the configuration
// model registers beyond the schema (docs/architecture/02-configuration-
// model.md "Policy", "Registered from feature documents"), served or not;
// every other type is validated by the schema and the other stages alone.
func checked() []v1alpha1.PolicyType {
	return []v1alpha1.PolicyType{
		v1alpha1.PolicyTypeAuthJWT,
		v1alpha1.PolicyTypeAuthAPIKey,
		v1alpha1.PolicyTypeAuthzCEL,
		v1alpha1.PolicyTypeAuthzIP,
		v1alpha1.PolicyTypeAuthzGeoIP,
		v1alpha1.PolicyTypeRateLimit,
		v1alpha1.PolicyTypeValidationJSONSchema,
		v1alpha1.PolicyTypeCORS,
		v1alpha1.PolicyTypeHeaders,
		v1alpha1.PolicyTypeTransformRequest,
		v1alpha1.PolicyTypeTransformResponse,
		v1alpha1.PolicyTypeAuthUpstreamOAuth2,
	}
}

// TestTableTypes: the table holds a non-nil check for exactly the types
// with registered config rules, authz.geoip's although its Filter is not
// served in M1 (configuration model, "Semantic validation"), and none for
// a type whose rules are the schema's or another stage's.
func TestTableTypes(t *testing.T) {
	tab := Table()
	got := slices.Sorted(maps.Keys(tab))
	want := slices.Sorted(slices.Values(checked()))
	if !slices.Equal(got, want) {
		t.Fatalf("Table() types = %v\nwant %v", got, want)
	}
	reg := registry.New()
	for typ, c := range tab {
		if c == nil {
			t.Errorf("Table()[%s] is nil", typ)
		}
		if _, ok := reg.Lookup(typ); !ok {
			t.Errorf("Table()[%s]: not a registered Policy type", typ)
		}
	}
	if reg.Served(v1alpha1.PolicyTypeAuthzGeoIP) || tab[v1alpha1.PolicyTypeAuthzGeoIP] == nil {
		t.Error("authz.geoip: its check must run from M1, before its Filter is served")
	}
	for _, typ := range []v1alpha1.PolicyType{
		v1alpha1.PolicyTypeAuthBasic, v1alpha1.PolicyTypeAuthMTLS, v1alpha1.PolicyTypeAuthUpstreamSigV4,
		v1alpha1.PolicyTypeQuota, v1alpha1.PolicyTypeCache, v1alpha1.PolicyTypePlugin,
	} {
		if _, ok := tab[typ]; ok {
			t.Errorf("Table()[%s]: its rules are the schema's or another stage's, so it has no check", typ)
		}
	}
}

// TestTableIsFresh: every call returns a new map, so a caller that changes
// its copy changes no other caller's.
func TestTableIsFresh(t *testing.T) {
	a, b := Table(), Table()
	delete(a, v1alpha1.PolicyTypeCORS)
	a[v1alpha1.PolicyTypePlugin] = func(*hub.Resource, func(diag.Path) diag.Location, func(diag.Diagnostic)) {}
	if _, ok := b[v1alpha1.PolicyTypeCORS]; !ok {
		t.Error("deleting from one table removed the entry from another")
	}
	if _, ok := Table()[v1alpha1.PolicyTypePlugin]; ok {
		t.Error("adding to one table added the entry to a later one")
	}
}

// policy returns a Policy resource named p of type typ holding cfg.
func policy(typ v1alpha1.PolicyType, cfg any) *hub.Resource {
	return &hub.Resource{
		ID:     hub.ID{Kind: v1alpha1.KindPolicy, Name: "p"},
		Object: &v1alpha1.Policy{Spec: v1alpha1.PolicySpec{Type: typ}},
		Config: cfg,
	}
}

// TestTableWiresEachType: each type's entry is its own check: a config of
// that type breaking one rule gets that rule's code under spec.config,
// and the check of every other type ignores it.
func TestTableWiresEachType(t *testing.T) {
	str := func(s string) *string { return &s }
	yes := true
	zero := v1alpha1.Duration(0)
	bad := map[v1alpha1.PolicyType]struct {
		cfg  any
		code string
	}{
		v1alpha1.PolicyTypeAuthJWT: {&v1alpha1.AuthJWTConfig{Issuers: []v1alpha1.JWTIssuer{
			{Issuer: "https://idp.example", JWKSURL: "http://idp.example/jwks", Audiences: []string{"api"}},
		}}, "RZ-CFG-037"},
		v1alpha1.PolicyTypeAuthAPIKey: {&v1alpha1.AuthAPIKeyConfig{Header: str("host")}, "RZ-CFG-005"},
		v1alpha1.PolicyTypeAuthzCEL:   {&v1alpha1.AuthzCELConfig{}, "RZ-CFG-005"},
		v1alpha1.PolicyTypeAuthzIP:    {&v1alpha1.AuthzIPConfig{Allow: []string{"office"}}, "RZ-CFG-005"},
		v1alpha1.PolicyTypeAuthzGeoIP: {&v1alpha1.AuthzGeoIPConfig{Allow: []v1alpha1.CountryCode{}}, "RZ-CFG-005"},
		v1alpha1.PolicyTypeRateLimit: {&v1alpha1.RateLimitConfig{Limits: []v1alpha1.RateLimit{
			{Requests: 10, Window: v1alpha1.Duration(1500 * time.Microsecond)},
		}}, "RZ-CFG-005"},
		v1alpha1.PolicyTypeValidationJSONSchema: {&v1alpha1.ValidationJSONSchemaConfig{Schema: v1alpha1.JSONSchemaDocument(`{"type":"nope"}`)}, "RZ-CFG-005"},
		v1alpha1.PolicyTypeCORS:                 {&v1alpha1.CORSConfig{AllowOrigins: []string{"*"}, AllowCredentials: &yes}, "RZ-CFG-005"},
		v1alpha1.PolicyTypeHeaders: {&v1alpha1.HeadersConfig{Request: &v1alpha1.HeaderRequestOps{
			Remove: []string{"bad name"},
		}}, "RZ-CFG-005"},
		v1alpha1.PolicyTypeTransformRequest: {&v1alpha1.TransformRequestConfig{Replace: []v1alpha1.Replace{
			{Pattern: "(", Replacement: "x"},
		}}, "RZ-CFG-005"},
		v1alpha1.PolicyTypeTransformResponse: {&v1alpha1.TransformResponseConfig{ContentType: str("not a media type")}, "RZ-CFG-005"},
		v1alpha1.PolicyTypeAuthUpstreamOAuth2: {&v1alpha1.AuthUpstreamOAuth2Config{
			TokenURL: "https://idp.example/token", ClientID: "c", Timeout: &zero,
		}, "RZ-CFG-005"},
	}
	tab := Table()
	for typ, c := range tab {
		b, ok := bad[typ]
		if !ok {
			t.Errorf("%s: no case", typ)
			continue
		}
		var got diag.List
		c(policy(typ, b.cfg), nil, func(d diag.Diagnostic) { got = append(got, d) })
		if len(got) == 0 || got[0].Code != b.code || !strings.HasPrefix(got[0].Path.String(), "spec.config") {
			t.Errorf("%s: check = %v, want %s under spec.config", typ, got, b.code)
		}
		for other, oc := range tab {
			if other == typ {
				continue
			}
			oc(policy(typ, b.cfg), nil, func(d diag.Diagnostic) {
				t.Errorf("the %s check reports on a %s config: %v", other, typ, d)
			})
		}
	}
}

// deniedModules are the module path prefixes Figure 1 or Figure 2 of
// docs/engineering/01-tech-stack-and-libraries.md ("Dependency graph")
// link into ruralzd or ruralz-control but not into cmd/ruralz: an offline
// check, which every binary links, never imports one.
func deniedModules() []string {
	return []string{
		"github.com/lestrrat-go/", // jwx and the modules it links
		"github.com/valyala/fastjson",
		"github.com/redis/rueidis",
		"go.opentelemetry.io/",
		"github.com/prometheus/",
		"google.golang.org/grpc",
		"github.com/quic-go/quic-go",
		"connectrpc.com/connect",
		"github.com/coder/websocket",
		"github.com/twmb/franz-go",
		"github.com/nats-io/nats.go",
		"github.com/eclipse/paho.golang",
		"github.com/mochi-mqtt/server",
		"github.com/tiktoken-go/tokenizer",
		"k8s.io/client-go",
		"golang.org/x/sys",
		"github.com/hashicorp/raft",
		"github.com/coreos/go-oidc",
	}
}

// TestTableLinksOnlyCLIModules: the table links into all three binaries,
// so the packages it links, on every platform the CLI ships for, link no
// module that cmd/ruralz must not link; jwx above all, which the auth.jwt
// Filter links and its check package does not (docs/engineering/
// 02-repository-layout-and-conventions.md "Package layers").
func TestTableLinksOnlyCLIModules(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("no go command to list the linked packages: %v", err)
	}
	for _, goos := range []string{"linux", "darwin", "windows"} {
		t.Run(goos, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), "go", "list", "-deps", "-test=false", "-json=ImportPath,Module", ".")
			cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH=amd64", "CGO_ENABLED=0")
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("go list: %v: %s", err, stderr.String())
			}
			dec := json.NewDecoder(bytes.NewReader(out))
			n := 0
			denied := map[string]string{} // module path: the first package linked from it
			for dec.More() {
				var p struct {
					ImportPath string
					Module     *struct {
						Path string
						Main bool
					}
				}
				if err := dec.Decode(&p); err != nil {
					t.Fatal(err)
				}
				n++
				if p.Module == nil || p.Module.Main {
					continue
				}
				for _, d := range deniedModules() {
					if strings.HasPrefix(p.Module.Path, d) || p.Module.Path == strings.TrimSuffix(d, "/") {
						if _, seen := denied[p.Module.Path]; !seen {
							denied[p.Module.Path] = p.ImportPath
						}
					}
				}
			}
			if n == 0 {
				t.Fatal("go list listed no package")
			}
			for _, m := range slices.Sorted(maps.Keys(denied)) {
				t.Errorf("links module %s (package %s), which cmd/ruralz must not link", m, denied[m])
			}
		})
	}
}
