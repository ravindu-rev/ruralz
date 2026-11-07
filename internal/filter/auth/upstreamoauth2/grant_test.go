// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package upstreamoauth2

import (
	"slices"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// The fields of each grant type (docs/architecture/02-configuration-model.md
// "Registered from feature documents"): what each takes and requires,
// checked offline by Check and refused by the Factory with the same code.

// Test references of the Google sources' secrets.
func keyRef() v1alpha1.SecretRef {
	return v1alpha1.SecretRef{Provider: v1alpha1.SecretProviderFile, Name: "/etc/ruralz/gcp/key.json"}
}

func subjectRef() v1alpha1.SecretRef {
	return v1alpha1.SecretRef{Provider: v1alpha1.SecretProviderFile, Name: "/etc/ruralz/gcp/token"}
}

// testProvider is a workload identity provider's full resource name.
const testProvider = "//iam.googleapis.com/projects/123/locations/global/workloadIdentityPools/shop/providers/gke"

// grantOfType returns a grantType value.
func grantOfType(g v1alpha1.OAuth2GrantType) *v1alpha1.OAuth2GrantType { return &g }

// jwtConfig returns a valid jwt-bearer config for tokenURL.
func jwtConfig(tokenURL string) *v1alpha1.AuthUpstreamOAuth2Config {
	return &v1alpha1.AuthUpstreamOAuth2Config{
		GrantType:         grantOfType(v1alpha1.OAuth2GrantJWTBearer),
		TokenURL:          strPtr(tokenURL),
		ServiceAccountKey: &v1alpha1.SecretValue{SecretRef: keyRef()},
		Scopes:            []string{"https://www.googleapis.com/auth/cloud-platform"},
	}
}

// metadataConfig returns a valid metadata-server config.
func metadataConfig() *v1alpha1.AuthUpstreamOAuth2Config {
	return &v1alpha1.AuthUpstreamOAuth2Config{GrantType: grantOfType(v1alpha1.OAuth2GrantMetadataServer)}
}

// exchangeConfig returns a valid token-exchange config for tokenURL.
func exchangeConfig(tokenURL string) *v1alpha1.AuthUpstreamOAuth2Config {
	return &v1alpha1.AuthUpstreamOAuth2Config{
		GrantType:                grantOfType(v1alpha1.OAuth2GrantTokenExchange),
		TokenURL:                 strPtr(tokenURL),
		SubjectToken:             &v1alpha1.SecretValue{SecretRef: subjectRef()},
		WorkloadIdentityProvider: testProvider,
	}
}

// finding is one expected diagnostic: code at spec.config.<field>.
type finding struct{ code, field string }

// TestCheckGrantFields checks the fields each grant type takes and
// requires, reported at their spec.config paths, and that the Factory's
// settings refuse the first of them with the same code and message.
func TestCheckGrantFields(t *testing.T) {
	const token = "https://oauth2.googleapis.com/token" //nolint:gosec // G101: a token endpoint address, not a credential
	sts := "https://sts.googleapis.com/v1/token"
	with := func(cfg *v1alpha1.AuthUpstreamOAuth2Config, mod func(*v1alpha1.AuthUpstreamOAuth2Config)) *v1alpha1.AuthUpstreamOAuth2Config {
		mod(cfg)
		return cfg
	}
	audience := "https://orders.shop.example"
	defaultType := DefaultSubjectTokenType
	otherType := "urn:ietf:params:oauth:token-type:id_token"
	for _, tc := range []struct {
		name string
		cfg  *v1alpha1.AuthUpstreamOAuth2Config
		want []finding
	}{
		{"client-credentials by default", validConfig(token), nil},
		{"client-credentials named", with(validConfig(token), func(c *v1alpha1.AuthUpstreamOAuth2Config) {
			c.GrantType = grantOfType(v1alpha1.OAuth2GrantClientCredentials)
		}), nil},
		{"client-credentials without its fields", &v1alpha1.AuthUpstreamOAuth2Config{}, []finding{
			{CodeConfig, "tokenUrl"}, {CodeConfig, "clientId"}, {CodeConfig, "clientSecret"},
		}},
		{"client-credentials with a Google field", with(validConfig(token), func(c *v1alpha1.AuthUpstreamOAuth2Config) {
			c.ServiceAccountKey = &v1alpha1.SecretValue{SecretRef: keyRef()}
			c.SubjectToken = &v1alpha1.SecretValue{SecretRef: subjectRef()}
			c.WorkloadIdentityProvider, c.ServiceAccount = testProvider, "sa@shop.iam.gserviceaccount.com"
		}), []finding{
			{CodeConfig, "serviceAccountKey"}, {CodeConfig, "subjectToken"}, {CodeConfig, "workloadIdentityProvider"}, {CodeConfig, "serviceAccount"},
		}},
		{"client-credentials with audience", with(validConfig(token), func(c *v1alpha1.AuthUpstreamOAuth2Config) { c.Audience = audience }), []finding{
			{CodeConfig, "audience"},
		}},
		{"client-credentials keeps the default subjectTokenType", with(validConfig(token), func(c *v1alpha1.AuthUpstreamOAuth2Config) { c.SubjectTokenType = &defaultType }), nil},
		{"client-credentials with another subjectTokenType", with(validConfig(token), func(c *v1alpha1.AuthUpstreamOAuth2Config) { c.SubjectTokenType = &otherType }), []finding{
			{CodeConfig, "subjectTokenType"},
		}},
		{"jwt-bearer", jwtConfig(token), nil},
		{"jwt-bearer for an ID token", with(jwtConfig(token), func(c *v1alpha1.AuthUpstreamOAuth2Config) { c.Scopes, c.Audience = nil, audience }), nil},
		{"jwt-bearer with client credentials", with(jwtConfig(token), func(c *v1alpha1.AuthUpstreamOAuth2Config) {
			c.ClientID, c.ClientSecret = strPtr(testClientID), &v1alpha1.SecretValue{SecretRef: testRef()}
		}), []finding{{CodeConfig, "clientId"}, {CodeConfig, "clientSecret"}}},
		{"jwt-bearer without its key", with(jwtConfig(token), func(c *v1alpha1.AuthUpstreamOAuth2Config) { c.ServiceAccountKey = nil }), []finding{
			{CodeConfig, "serviceAccountKey"},
		}},
		{"jwt-bearer over http", jwtConfig("http://oauth2.googleapis.com/token"), []finding{{CodeNotHTTPS, "tokenUrl"}}},
		{"jwt-bearer with audience and scopes", with(jwtConfig(token), func(c *v1alpha1.AuthUpstreamOAuth2Config) { c.Audience = audience }), []finding{
			{CodeConfig, "scopes"},
		}},
		{"metadata-server", metadataConfig(), nil},
		{"metadata-server for an ID token", with(metadataConfig(), func(c *v1alpha1.AuthUpstreamOAuth2Config) { c.Audience = audience }), nil},
		{"metadata-server with scopes", with(metadataConfig(), func(c *v1alpha1.AuthUpstreamOAuth2Config) { c.Scopes = []string{"a"} }), nil},
		{"metadata-server with a tokenUrl and a credential", with(metadataConfig(), func(c *v1alpha1.AuthUpstreamOAuth2Config) {
			c.TokenURL, c.ClientID = strPtr("http://169.254.169.254/token"), strPtr(testClientID)
			c.ClientSecret = &v1alpha1.SecretValue{SecretRef: testRef()}
		}), []finding{{CodeConfig, "tokenUrl"}, {CodeConfig, "clientId"}, {CodeConfig, "clientSecret"}}},
		{"token-exchange", exchangeConfig(sts), nil},
		{"token-exchange impersonating", with(exchangeConfig(sts), func(c *v1alpha1.AuthUpstreamOAuth2Config) {
			c.ServiceAccount, c.SubjectTokenType = "orders@shop.iam.gserviceaccount.com", &otherType
		}), nil},
		{"token-exchange ID token impersonating", with(exchangeConfig(sts), func(c *v1alpha1.AuthUpstreamOAuth2Config) {
			c.ServiceAccount, c.Audience = "orders@shop.iam.gserviceaccount.com", audience
		}), nil},
		{"token-exchange ID token without impersonation", with(exchangeConfig(sts), func(c *v1alpha1.AuthUpstreamOAuth2Config) { c.Audience = audience }), []finding{
			{CodeConfig, "audience"},
		}},
		{"token-exchange without provider or token", with(exchangeConfig(sts), func(c *v1alpha1.AuthUpstreamOAuth2Config) {
			c.SubjectToken, c.WorkloadIdentityProvider = nil, ""
		}), []finding{{CodeConfig, "subjectToken"}, {CodeConfig, "workloadIdentityProvider"}}},
		{"token-exchange with a key", with(exchangeConfig(sts), func(c *v1alpha1.AuthUpstreamOAuth2Config) {
			c.ServiceAccountKey = &v1alpha1.SecretValue{SecretRef: keyRef()}
		}), []finding{{CodeConfig, "serviceAccountKey"}}},
		{"grant type the release does not serve", with(validConfig(token), func(c *v1alpha1.AuthUpstreamOAuth2Config) {
			c.GrantType = grantOfType("password")
		}), []finding{{CodeConfig, "grantType"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []finding
			ds := runCheck(tc.cfg)
			for _, d := range ds {
				got = append(got, finding{d.Code, d.Path.String()[len("spec.config."):]})
				if d.Message == "" || d.Location != fakeLocation(d.Path) {
					t.Errorf("finding %s at %s: message %q, location %+v", d.Code, d.Path, d.Message, d.Location)
				}
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("Check = %v, want %v", got, tc.want)
			}
			_, err := settingsOf(tc.cfg)
			if len(tc.want) == 0 {
				if err != nil {
					t.Fatalf("settingsOf = %v, want no refusal", err)
				}
				return
			}
			if code, _ := errcode.CodeOf(err); code != tc.want[0].code || err.Error() != code+": "+ds[0].Message {
				t.Errorf("settingsOf = %v, want %s: %s", err, tc.want[0].code, ds[0].Message)
			}
		})
	}
}

// TestGrantSettings checks the fetch parameters of each Google grant: its
// credential reference, the tokenUrl origin the subject token is bound to,
// the provider, the account to impersonate and the default subject token
// type; metadata-server has neither a tokenUrl nor a credential.
func TestGrantSettings(t *testing.T) {
	cfg := exchangeConfig("https://STS.googleapis.com/v1/token")
	cfg.ServiceAccount = "orders@shop.iam.gserviceaccount.com"
	cfg.Scopes = []string{"a", "b"}
	s, err := settingsOf(cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := settings{
		grant: grantTokenExchange, endpoint: "https://STS.googleapis.com/v1/token", origin: "https://sts.googleapis.com:443",
		ref: secret.RefOf(subjectRef()), scope: "a b", subjectTokenType: DefaultSubjectTokenType,
		provider: testProvider, serviceAccount: "orders@shop.iam.gserviceaccount.com", timeout: DefaultTimeout,
	}
	if s != want {
		t.Errorf("token-exchange settings = %+v\nwant %+v", s, want)
	}
	s, err = settingsOf(jwtConfig("https://oauth2.googleapis.com/token"))
	if err != nil || s.grant != grantJWTBearer || s.ref != secret.RefOf(keyRef()) || s.origin != "https://oauth2.googleapis.com:443" {
		t.Errorf("jwt-bearer settings = %+v, %v", s, err)
	}
	m := metadataConfig()
	m.Timeout = duration(500 * time.Millisecond)
	s, err = settingsOf(m)
	if err != nil || s != (settings{grant: grantMetadataServer, timeout: 500 * time.Millisecond}) {
		t.Errorf("metadata-server settings = %+v, %v", s, err)
	}
}

// TestBuildChecksGrantSecrets checks that the Factory refuses a Google
// grant whose credential secret the Revision did not resolve (RZ-CFG-026),
// and builds metadata-server, which has none, without a secret table.
func TestBuildChecksGrantSecrets(t *testing.T) {
	reg := NewRegistry(Options{})
	empty := &fakeSecrets{vals: map[secret.Ref]secret.Value{}}
	for _, cfg := range []*v1alpha1.AuthUpstreamOAuth2Config{jwtConfig("https://oauth2.googleapis.com/token"), exchangeConfig("https://sts.googleapis.com/v1/token")} {
		_, err := Factory{reg}.Build(t.Context(), filter.BuildEnv{Config: cfg, Secrets: empty})
		if code, _ := errcode.CodeOf(err); code != CodeUnresolved {
			t.Errorf("Build of %s without its secret = %v, want %s", *cfg.GrantType, err, CodeUnresolved)
		}
	}
	f, err := Factory{reg}.Build(t.Context(), filter.BuildEnv{Config: metadataConfig()})
	if err != nil {
		t.Fatalf("Build of metadata-server = %v", err)
	}
	_ = f.(*Filter).Close()
}
