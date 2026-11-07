// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package upstreamsigv4

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/filtertest"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/routematch"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// The AWS documentation's example credentials, which the S3 examples sign
// with.
const (
	exampleKeyID  = "AKIAIOSFODNN7EXAMPLE"                     //nolint:gosec // G101: AWS's published example key ID, not a credential
	exampleSecret = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY" //nolint:gosec // G101: AWS's published example key, not a credential
	exampleToken  = "session-token-canary"
)

// staticSource is a CredentialSource answering creds or err, counting
// calls; closed counts Close.
type staticSource struct {
	creds  Credentials
	err    error
	calls  atomic.Int64
	closed atomic.Int64
}

func (s *staticSource) Credentials(context.Context) (Credentials, error) {
	s.calls.Add(1)
	return s.creds, s.err
}

func (s *staticSource) Close() error {
	s.closed.Add(1)
	return nil
}

// example returns a source of the AWS example credentials.
func example(token string) *staticSource {
	c := Credentials{AccessKeyID: exampleKeyID, SecretAccessKey: secret.NewValue([]byte(exampleSecret))}
	if token != "" {
		c.SessionToken = secret.NewValue([]byte(token))
	}
	return &staticSource{creds: c}
}

// payload returns a config.payload value.
func payload(p v1alpha1.SigV4Payload) *v1alpha1.SigV4Payload { return &p }

// build builds a Filter for cfg on src, signing on clk.
func build(t *testing.T, cfg *v1alpha1.AuthUpstreamSigV4Config, src CredentialSource, clk *clocktest.Fake) *Filter {
	t.Helper()
	fa := Factory{Credentials: func(filter.BuildEnv) (CredentialSource, error) { return src, nil }}
	f, err := fa.Build(t.Context(), filter.BuildEnv{Config: cfg, Clock: clk, Scope: phase.ScopeUpstream, Phases: phase.Of(phase.OnUpstreamRequest)})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = f.(*Filter).Close() })
	return f.(*Filter)
}

// outgoingRequest returns an attempt's outgoing request to host with path
// (escaped) and query, and body readable through GetBody when non-nil.
func outgoingRequest(method, host, path, query string, body []byte) *http.Request {
	u := &url.URL{Scheme: "https", Host: host, RawQuery: query}
	routematch.SetRequestPath(u, path)
	r := &http.Request{Method: method, URL: u, Header: http.Header{}, Body: http.NoBody}
	if body != nil {
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	}
	return r
}

// leg returns an onUpstreamRequest leg view of a first attempt over r.
func leg(r *http.Request) *filtertest.Exchange {
	return &filtertest.Exchange{Msg: &filtertest.Message{Hdr: r.Header}, LegV: &filter.Leg{Upstream: "aws", Attempt: 1, Request: r}}
}

// signOn runs onUpstreamRequest and the signing hook on x, failing the
// test unless both continue.
func signOn(t *testing.T, f *Filter, x *filtertest.Exchange) {
	t.Helper()
	if r := f.Handle(t.Context(), phase.OnUpstreamRequest, x); r.Outcome != filter.Continue {
		t.Fatalf("Handle = outcome %d, err %v; want Continue", r.Outcome, r.Err)
	}
	if r := f.Sign(t.Context(), x); r.Outcome != filter.Continue {
		t.Fatalf("Sign = outcome %d, err %v; want Continue", r.Outcome, r.Err)
	}
}

// wantUndecided fails unless r is CannotDecide with RZ-AUTH-020 (401) and
// an error matching target.
func wantUndecided(t *testing.T, r filter.Result, target error) {
	t.Helper()
	if r.Outcome != filter.CannotDecide || r.Code != CodeUpstreamAuth {
		t.Fatalf("result = outcome %d code %q, want CannotDecide %s", r.Outcome, r.Code, CodeUpstreamAuth)
	}
	if target != nil && !errors.Is(r.Err, target) {
		t.Fatalf("error = %v, want %v", r.Err, target)
	}
	if st := errcode.Status(r.Code); st != http.StatusUnauthorized {
		t.Fatalf("registered status of %s = %d, want 401", r.Code, st)
	}
}

// s3Date is the signing time of the AWS documentation's S3 examples.
func s3Date() time.Time { return time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC) }

// s3Scope opens the Authorization value of every S3 example: the algorithm
// and the credential of its key on its day, region and service.
const s3Scope = "AWS4-HMAC-SHA256 Credential=" + exampleKeyID + "/20130524/us-east-1/s3/aws4_request, "

// TestS3Examples signs the header-based authentication examples of the
// Amazon S3 API reference ([source]
// https://docs.aws.amazon.com/AmazonS3/latest/API/sig-v4-header-based-auth.html)
// through the whole signing hook with config.service s3: the payload hash
// in X-Amz-Content-Sha256, S3's path signed as sent, which the hook gives
// AWS's encoding first (the PUT example's key test$file.text goes out as
// /test%24file.text), and the documented signatures. The PUT example's
// request carries no Content-Length, so its body goes with an unknown
// length.
func TestS3Examples(t *testing.T) {
	const host = "examplebucket.s3.amazonaws.com"
	for _, tc := range []struct {
		name, method, path, query string
		header                    http.Header
		body                      []byte
		unknownLength             bool
		wantPath, wantAuth        string
	}{
		{
			name: "GET Object", method: http.MethodGet, path: "/test.txt",
			header:   http.Header{"Range": {"bytes=0-9"}},
			wantPath: "/test.txt",
			wantAuth: s3Scope + "SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41",
		},
		{
			name: "PUT Object", method: http.MethodPut, path: "/test$file.text",
			header:        http.Header{"Date": {"Fri, 24 May 2013 00:00:00 GMT"}, "X-Amz-Storage-Class": {"REDUCED_REDUNDANCY"}},
			body:          []byte("Welcome to Amazon S3."),
			unknownLength: true,
			wantPath:      "/test%24file.text",
			wantAuth:      s3Scope + "SignedHeaders=date;host;x-amz-content-sha256;x-amz-date;x-amz-storage-class, Signature=98ad721746da40c64f1a55b78f14c238d841ea1380cd77a1b5971af0ece108bd",
		},
		{
			name: "GET Bucket Lifecycle", method: http.MethodGet, path: "/", query: "lifecycle",
			wantPath: "/",
			wantAuth: s3Scope + "SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=fea454ca298b7da1c68078a5d1bdbfbbe0d65c699e0f91ac7a200a0136783543",
		},
		{
			name: "GET Bucket", method: http.MethodGet, path: "/", query: "max-keys=2&prefix=J",
			wantPath: "/",
			wantAuth: s3Scope + "SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := build(t, &v1alpha1.AuthUpstreamSigV4Config{Region: "us-east-1", Service: "s3"}, example(""), clocktest.New(s3Date()))
			r := outgoingRequest(tc.method, host, tc.path, tc.query, tc.body)
			for k, vs := range tc.header {
				r.Header[k] = vs
			}
			if tc.unknownLength {
				r.ContentLength = -1
			}
			signOn(t, f, leg(r))
			if got := r.Header.Get(headerAuthorization); got != tc.wantAuth {
				t.Errorf("Authorization =\n %s\nwant\n %s", got, tc.wantAuth)
			}
			if got := routematch.RequestPath(r.URL); got != tc.wantPath {
				t.Errorf("path sent = %q, want %q", got, tc.wantPath)
			}
			if got := r.Header.Get(headerDate); got != "20130524T000000Z" {
				t.Errorf("X-Amz-Date = %q", got)
			}
		})
	}
}

// TestPayloadHash checks the payload modes: a signed payload sends the hex
// SHA-256 of the body the attempt sends (empty for no body) in
// X-Amz-Content-Sha256 and signs it; an unsigned one sends
// UNSIGNED-PAYLOAD and reads no body, so it may stream; a signed payload
// whose body streams cannot be hashed and is RZ-AUTH-020 before anything is
// sent.
func TestPayloadHash(t *testing.T) {
	body := []byte(`{"order":42}`)
	sum := sha256.Sum256(body)
	for _, tc := range []struct {
		name     string
		mode     *v1alpha1.SigV4Payload
		req      func() *http.Request
		wantHash string
		wantErr  error
	}{
		{"signed body", nil, func() *http.Request {
			return outgoingRequest(http.MethodPost, "lambda.us-east-1.amazonaws.com", "/fn", "", body)
		}, hex.EncodeToString(sum[:]), nil},
		{"signed empty body", payload(v1alpha1.SigV4PayloadSigned), func() *http.Request {
			return outgoingRequest(http.MethodGet, "lambda.us-east-1.amazonaws.com", "/fn", "", nil)
		}, emptyPayloadHash, nil},
		{"unsigned streams", payload(v1alpha1.SigV4PayloadUnsigned), func() *http.Request {
			r := outgoingRequest(http.MethodPut, "lambda.us-east-1.amazonaws.com", "/fn", "", nil)
			r.Body, r.ContentLength = io.NopCloser(strings.NewReader("streamed")), -1
			return r
		}, PayloadUnsigned, nil},
		{"signed streamed body", nil, func() *http.Request {
			r := outgoingRequest(http.MethodPut, "lambda.us-east-1.amazonaws.com", "/fn", "", nil)
			r.Body, r.ContentLength = io.NopCloser(strings.NewReader("streamed")), -1
			return r
		}, "", errStreamedBody},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := build(t, &v1alpha1.AuthUpstreamSigV4Config{Region: "us-east-1", Service: "lambda", Payload: tc.mode}, example(""), clocktest.New(s3Date()))
			r := tc.req()
			x := leg(r)
			if res := f.Handle(t.Context(), phase.OnUpstreamRequest, x); res.Outcome != filter.Continue {
				t.Fatalf("Handle = %v", res.Err)
			}
			res := f.Sign(t.Context(), x)
			if tc.wantErr != nil {
				wantUndecided(t, res, tc.wantErr)
				if r.Header.Get(headerAuthorization) != "" {
					t.Error("a refused request carries a signature")
				}
				return
			}
			if res.Outcome != filter.Continue {
				t.Fatalf("Sign = %v", res.Err)
			}
			if got := r.Header.Get(headerContentSHA256); got != tc.wantHash {
				t.Errorf("X-Amz-Content-Sha256 = %q, want %q", got, tc.wantHash)
			}
			if !strings.Contains(r.Header.Get(headerAuthorization), "x-amz-content-sha256") {
				t.Errorf("Authorization %q does not sign X-Amz-Content-Sha256", r.Header.Get(headerAuthorization))
			}
		})
	}
}

// TestSigningTimeIsTheClock checks that each attempt is signed at the
// clock's time of its signing hook: a retry an hour later carries a new
// X-Amz-Date and a new signature, and the credentials are asked for anew
// in each attempt's onUpstreamRequest.
func TestSigningTimeIsTheClock(t *testing.T) {
	clk := clocktest.New(time.Date(2026, 11, 7, 9, 30, 15, 0, time.UTC))
	src := example("")
	f := build(t, &v1alpha1.AuthUpstreamSigV4Config{Region: "eu-west-1", Service: "execute-api"}, src, clk)
	first := outgoingRequest(http.MethodGet, "abc.execute-api.eu-west-1.amazonaws.com", "/prod/orders", "", nil)
	x := leg(first)
	signOn(t, f, x)
	clk.Advance(time.Hour)
	second := outgoingRequest(http.MethodGet, "abc.execute-api.eu-west-1.amazonaws.com", "/prod/orders", "", nil)
	x.LegV = &filter.Leg{Upstream: "aws", Attempt: 2, Request: second}
	signOn(t, f, x)
	if d := first.Header.Get(headerDate); d != "20261107T093015Z" {
		t.Errorf("first X-Amz-Date = %q", d)
	}
	if d := second.Header.Get(headerDate); d != "20261107T103015Z" {
		t.Errorf("retry X-Amz-Date = %q, want the clock's time an hour later", d)
	}
	if a, b := first.Header.Get(headerAuthorization), second.Header.Get(headerAuthorization); a == b || !strings.Contains(a, "/20261107/eu-west-1/execute-api/aws4_request") {
		t.Errorf("signatures %q and %q, want two scoped to the day, region and service", a, b)
	}
	if n := src.calls.Load(); n != 2 {
		t.Errorf("credentials asked %d times, want once per attempt", n)
	}
}

// TestSignedFieldsOwned checks that signing replaces the fields the
// signature owns, whatever the request carried, without writing into the
// value arrays the outgoing header shares with the client's (the
// copy-free forwarding writer caps them), and that a session token is sent
// and signed.
func TestSignedFieldsOwned(t *testing.T) {
	client := []string{"Bearer client", "20000101T000000Z", "client-token", "bogus"}
	h := http.Header{
		headerAuthorization: client[0:1:1],
		headerDate:          client[1:2:2],
		headerSecurityToken: client[2:3:3],
		headerContentSHA256: client[3:4:4],
	}
	r := outgoingRequest(http.MethodGet, "sts.amazonaws.com", "/", "Action=GetCallerIdentity&Version=2011-06-15", nil)
	r.Header = h
	f := build(t, &v1alpha1.AuthUpstreamSigV4Config{Region: "us-east-1", Service: "sts"}, example(exampleToken), clocktest.New(s3Date()))
	signOn(t, f, leg(r))
	if got := strings.Join(client, "|"); got != "Bearer client|20000101T000000Z|client-token|bogus" {
		t.Errorf("client values became %q: signing wrote into shared arrays", got)
	}
	for k, want := range map[string]string{headerDate: "20130524T000000Z", headerSecurityToken: exampleToken, headerContentSHA256: emptyPayloadHash} {
		if got := r.Header.Values(k); len(got) != 1 || got[0] != want {
			t.Errorf("%s = %q, want [%q]", k, got, want)
		}
	}
	auth := r.Header.Values(headerAuthorization)
	if len(auth) != 1 || !strings.HasPrefix(auth[0], "AWS4-HMAC-SHA256 Credential="+exampleKeyID+"/20130524/us-east-1/sts/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date;x-amz-security-token, Signature=") {
		t.Errorf("Authorization = %q, want one SigV4 value signing the session token", auth)
	}
}

// TestCaseVariantsMerged checks that a field held under names that differ
// only in letter case is merged under the canonical name, canonical lines
// first, so the transport writes the one field the signature covers.
func TestCaseVariantsMerged(t *testing.T) {
	r := outgoingRequest(http.MethodGet, "example.amazonaws.com", "/", "", nil)
	lower, upper := "x-tenant", "X-TENANT"
	r.Header["X-Tenant"] = []string{"a"}
	r.Header[lower] = []string{"b"}
	r.Header[upper] = []string{"c"}
	f := build(t, &v1alpha1.AuthUpstreamSigV4Config{Region: "us-east-1", Service: "service"}, example(""), clocktest.New(s3Date()))
	signOn(t, f, leg(r))
	if got := r.Header["X-Tenant"]; strings.Join(got, ",") != "a,c,b" {
		t.Errorf("X-Tenant = %q, want the canonical line, then X-TENANT, then x-tenant", got)
	}
	if _, ok := r.Header[lower]; ok {
		t.Error("a case variant stays in the header map")
	}
	if !strings.Contains(r.Header.Get(headerAuthorization), "x-tenant") {
		t.Errorf("Authorization %q does not sign X-Tenant", r.Header.Get(headerAuthorization))
	}
}

// TestHostAndQuerySent checks that the request carries what was signed: the
// host without its default port and the query in canonical form.
func TestHostAndQuerySent(t *testing.T) {
	r := outgoingRequest(http.MethodGet, "search.eu-west-1.es.amazonaws.com:443", "/orders/_search", "q=status%3Aopen&size=10&from=0", nil)
	f := build(t, &v1alpha1.AuthUpstreamSigV4Config{Region: "eu-west-1", Service: "es"}, example(""), clocktest.New(s3Date()))
	signOn(t, f, leg(r))
	if r.Host != "search.eu-west-1.es.amazonaws.com" {
		t.Errorf("Host = %q, want the authority without its default port", r.Host)
	}
	if r.URL.RawQuery != "from=0&q=status%3Aopen&size=10" {
		t.Errorf("query sent = %q, want the canonical form", r.URL.RawQuery)
	}
}

// TestEscapePath checks the path form the hook sends, AWS's URI encoding of
// each decoded segment.
func TestEscapePath(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"/", "/"},
		{"", "/"},
		{"/orders/42", "/orders/42"},
		{"/a:b@c", "/a%3Ab%40c"},
		{"/key+with=sub-delims!$&'()*,;", "/key%2Bwith%3Dsub-delims%21%24%26%27%28%29%2A%2C%3B"},
		{"/a%2Fb/c", "/a%2Fb/c"},
		{"/a%20b", "/a%20b"},
		{"/%7Euser/-._", "/~user/-._"},
		{"/caf%C3%A9", "/caf%C3%A9"},
		{"//a//", "//a//"},
	} {
		got, err := escapePath(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("escapePath(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	if _, err := escapePath("/a%zz"); !errors.Is(err, errBadEscape) {
		t.Errorf("escapePath of an invalid escape = %v, want %v", err, errBadEscape)
	}
}

// TestCanonicalPath checks the canonical path by service: every service
// but S3 resolves dot segments and drops empty ones, keeping a trailing
// slash, and S3 takes the path as sent.
func TestCanonicalPath(t *testing.T) {
	for _, tc := range []struct{ in, other, s3 string }{
		{"/", "/", "/"},
		{"//a//b//", "/a/b/", "//a//b//"},
		{"/a/./b/../c", "/a/c", "/a/./b/../c"},
		{"/a/b/..", "/a/", "/a/b/.."},
		{"/a/.", "/a/", "/a/."},
		{"/..", "/", "/.."},
		{"/a%2Fb", "/a%2Fb", "/a%2Fb"},
	} {
		if got := (params{service: "execute-api"}).canonicalPath(tc.in); got != tc.other {
			t.Errorf("canonicalPath(%q) for execute-api = %q, want %q", tc.in, got, tc.other)
		}
		if got := (params{service: serviceS3}).canonicalPath(tc.in); got != tc.s3 {
			t.Errorf("canonicalPath(%q) for s3 = %q, want %q", tc.in, got, tc.s3)
		}
	}
}

// TestBuild checks the Factory's refusals: a config of the wrong type is
// RZ-CFG-005, a Factory without a credential source and a source that
// fails are the Node's faults, and an ended context its error.
func TestBuild(t *testing.T) {
	cfg := &v1alpha1.AuthUpstreamSigV4Config{Region: "us-east-1", Service: "lambda"}
	errSource := errors.New("test: no source")
	ended, cancel := context.WithCancel(t.Context())
	cancel()
	for _, tc := range []struct {
		name string
		ctx  context.Context
		fa   Factory
		cfg  any
		code string
		err  error
	}{
		{"wrong config", t.Context(), Factory{Credentials: func(filter.BuildEnv) (CredentialSource, error) { return example(""), nil }}, &v1alpha1.AuthUpstreamOAuth2Config{}, CodeConfig, nil},
		{"no credential source", t.Context(), Factory{}, cfg, "", ErrNoCredentialSource},
		{"source refused", t.Context(), Factory{Credentials: func(filter.BuildEnv) (CredentialSource, error) { return nil, errSource }}, cfg, "", errSource},
		{"nil source", t.Context(), Factory{Credentials: func(filter.BuildEnv) (CredentialSource, error) { return nil, nil }}, cfg, "", ErrNoCredentialSource},
		{"ended", ended, Factory{}, cfg, "", context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, err := tc.fa.Build(tc.ctx, filter.BuildEnv{Config: tc.cfg})
			if err == nil || f != nil {
				t.Fatalf("Build = %v, %v; want a refusal", f, err)
			}
			if code, _ := errcode.CodeOf(err); code != tc.code {
				t.Errorf("code = %q, want %q (%v)", code, tc.code, err)
			}
			if tc.err != nil && !errors.Is(err, tc.err) {
				t.Errorf("error = %v, want %v", err, tc.err)
			}
		})
	}
}

// TestHandleRefusals checks onUpstreamRequest: credentials the source
// cannot give, empty credentials and a leg view without a request are
// RZ-AUTH-020; every other Phase continues untouched.
func TestHandleRefusals(t *testing.T) {
	cfg := &v1alpha1.AuthUpstreamSigV4Config{Region: "us-east-1", Service: "lambda"}
	errFetch := errors.New("test: credentials unavailable")
	r := outgoingRequest(http.MethodGet, "lambda.us-east-1.amazonaws.com", "/", "", nil)

	f := build(t, cfg, &staticSource{err: errFetch}, clocktest.New(s3Date()))
	wantUndecided(t, f.Handle(t.Context(), phase.OnUpstreamRequest, leg(r)), errFetch)

	f = build(t, cfg, &staticSource{creds: Credentials{AccessKeyID: exampleKeyID}}, clocktest.New(s3Date()))
	wantUndecided(t, f.Handle(t.Context(), phase.OnUpstreamRequest, leg(r)), errNoCredentials)

	f = build(t, cfg, example(""), clocktest.New(s3Date()))
	wantUndecided(t, f.Handle(t.Context(), phase.OnUpstreamRequest, &filtertest.Exchange{}), errNoRequest)
	wantUndecided(t, f.Sign(t.Context(), &filtertest.Exchange{}), errNoRequest)
	for _, ph := range []phase.Phase{phase.OnUpstreamResponseHeaders, phase.OnUpstreamResponseBody, phase.OnRequestHeaders} {
		if res := f.Handle(t.Context(), ph, leg(r)); res.Outcome != filter.Continue {
			t.Errorf("Handle(%s) = outcome %d, want Continue", ph, res.Outcome)
		}
	}
}

// TestSignNeedsRecordedCredentials checks that the hook refuses an attempt
// whose onUpstreamRequest recorded no credentials, and that Finish drops
// them when the leg ends.
func TestSignNeedsRecordedCredentials(t *testing.T) {
	f := build(t, &v1alpha1.AuthUpstreamSigV4Config{Region: "us-east-1", Service: "lambda"}, example(""), clocktest.New(s3Date()))
	r := outgoingRequest(http.MethodGet, "lambda.us-east-1.amazonaws.com", "/", "", nil)
	x := leg(r)
	wantUndecided(t, f.Sign(t.Context(), x), errNotRecorded)
	signOn(t, f, x)
	f.Finish(t.Context(), x)
	if x.State != nil {
		t.Fatalf("PolicyState after Finish = %v, want nil", x.State)
	}
	wantUndecided(t, f.Sign(t.Context(), x), errNotRecorded)
}

// TestCloseClosesTheSource checks that Close closes a source that is an
// io.Closer, once.
func TestCloseClosesTheSource(t *testing.T) {
	src := example("")
	f := build(t, &v1alpha1.AuthUpstreamSigV4Config{Region: "us-east-1", Service: "lambda"}, src, clocktest.New(s3Date()))
	_ = f.Close()
	_ = f.Close()
	if n := src.closed.Load(); n != 1 {
		t.Errorf("source closed %d times, want 1", n)
	}
}

// TestCredentialsNeverPrint checks that credentials print, log and fail
// without their secrets.
func TestCredentialsNeverPrint(t *testing.T) {
	c := example(exampleToken).creds
	var logs bytes.Buffer
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("credentials", "creds", c)
	out := fmt.Sprintf("%v %+v %#v %s", c, c, c, c) + logs.String()
	for _, s := range []string{exampleSecret, exampleToken} {
		if strings.Contains(out, s) {
			t.Fatalf("output %q holds a secret", out)
		}
	}
	if !strings.Contains(out, exampleKeyID) {
		t.Errorf("output %q does not name the access key ID", out)
	}
	f := build(t, &v1alpha1.AuthUpstreamSigV4Config{Region: "us-east-1", Service: "lambda"}, example(exampleToken), clocktest.New(s3Date()))
	r := outgoingRequest(http.MethodPut, "lambda.us-east-1.amazonaws.com", "/", "", nil)
	r.Body, r.ContentLength = io.NopCloser(strings.NewReader("streamed")), -1
	x := leg(r)
	_ = f.Handle(t.Context(), phase.OnUpstreamRequest, x)
	res := f.Sign(t.Context(), x)
	if res.Err == nil || strings.Contains(res.Err.Error(), exampleSecret) || strings.Contains(res.Err.Error(), exampleToken) {
		t.Errorf("signing error = %v, want one without a secret", res.Err)
	}
}
