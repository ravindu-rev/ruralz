// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/auth/upstreamsigv4"
	"github.com/ravindu-rev/ruralz/internal/gateway/handler/handlertest"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/forward"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// An end-to-end run of AWS Signature Version 4 on the data path
// (foundation pack section 8.12 rule 6): the real handler, exchange,
// executor, Route forwarder and Upstream layer, an auth.upstream-sigv4
// Filter followed in onUpstreamRequest by a header and body transform of
// the attempt, and a retried attempt. The Upstream checks every signature
// with a SigV4 verifier of its own, as an AWS service does, so the request
// as received is the request as signed.

// The AWS documentation's example credentials.
const (
	sigv4KeyID  = "AKIAIOSFODNN7EXAMPLE"                     //nolint:gosec // G101: AWS's published example key ID, not a credential
	sigv4Secret = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY" //nolint:gosec // G101: AWS's published example key, not a credential
	sigv4Token  = "session-token-canary"
)

// staticCredentials is an upstreamsigv4.CredentialSource of the example
// credentials with a session token.
type staticCredentials struct{}

func (staticCredentials) Credentials(context.Context) (upstreamsigv4.Credentials, error) {
	return upstreamsigv4.Credentials{
		AccessKeyID:     sigv4KeyID,
		SecretAccessKey: secret.NewValue([]byte(sigv4Secret)),
		SessionToken:    secret.NewValue([]byte(sigv4Token)),
	}, nil
}

// lowDraws always draws 2, so every bounded draw maps to 0: the first
// Endpoint and a zero retry backoff.
type lowDraws struct{}

func (lowDraws) Uint64() uint64 { return 2 }

// received is what the Upstream verified of one request.
type received struct {
	date, tenant, body string
	signed             []string
	err                error
}

// sigv4Upstream is a loopback Upstream verifying every request's
// signature; answer picks the status of the n-th request (from 1).
type sigv4Upstream struct {
	srv    *httptest.Server
	answer func(n int) int
	mu     sync.Mutex
	got    []received
}

func newSigV4Upstream(t *testing.T, answer func(n int) int) *sigv4Upstream {
	t.Helper()
	u := &sigv4Upstream{answer: answer}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		rec := received{date: r.Header.Get("X-Amz-Date"), tenant: r.Header.Get("X-Tenant"), body: string(b), err: err}
		if err == nil {
			rec.signed, rec.err = verifySigV4(r, b, sigv4Secret)
		}
		u.mu.Lock()
		u.got = append(u.got, rec)
		n := len(u.got)
		u.mu.Unlock()
		w.WriteHeader(u.answer(n))
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *sigv4Upstream) requests() []received {
	u.mu.Lock()
	defer u.mu.Unlock()
	return slices.Clone(u.got)
}

// verifySigV4 checks r's AWS Signature Version 4 header authorization
// against secretKey, as an AWS service other than Amazon S3 does: it
// rebuilds the canonical request from the request as received, the
// request-target as sent, the signed header fields and the payload hash,
// which must be the SHA-256 of body, and compares the signature. It
// returns the signed header names.
func verifySigV4(r *http.Request, body []byte, secretKey string) ([]string, error) {
	const alg = "AWS4-HMAC-SHA256"
	auth, ok := strings.CutPrefix(r.Header.Get("Authorization"), alg+" ")
	if !ok {
		return nil, fmt.Errorf("authorization %q is not %s", r.Header.Get("Authorization"), alg)
	}
	fields := map[string]string{}
	for _, part := range strings.Split(auth, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			return nil, fmt.Errorf("authorization part %q", part)
		}
		fields[k] = v
	}
	cred := strings.Split(fields["Credential"], "/")
	if len(cred) != 5 || cred[0] != sigv4KeyID || cred[4] != "aws4_request" {
		return nil, fmt.Errorf("credential %q", fields["Credential"])
	}
	amzDate := r.Header.Get("X-Amz-Date")
	if !strings.HasPrefix(amzDate, cred[1]+"T") {
		return nil, fmt.Errorf("X-Amz-Date %q is not of the credential's day %s", amzDate, cred[1])
	}
	sum := sha256.Sum256(body)
	payload := r.Header.Get("X-Amz-Content-Sha256")
	if payload != hex.EncodeToString(sum[:]) {
		return nil, fmt.Errorf("X-Amz-Content-Sha256 %q is not the SHA-256 of the body received", payload)
	}
	signed := strings.Split(fields["SignedHeaders"], ";")
	var headers strings.Builder
	for _, name := range signed {
		var vals []string
		switch name {
		case "host":
			vals = []string{r.Host}
		case "content-length":
			vals = []string{strconv.FormatInt(r.ContentLength, 10)}
		default:
			vals = r.Header.Values(name)
		}
		if len(vals) == 0 {
			return nil, fmt.Errorf("signed header field %s did not arrive", name)
		}
		for i, v := range vals {
			vals[i] = strings.Join(strings.Fields(v), " ")
		}
		headers.WriteString(name + ":" + strings.Join(vals, ",") + "\n")
	}
	target, rawQuery, _ := strings.Cut(r.RequestURI, "?")
	query, err := canonicalQuery(rawQuery)
	if err != nil {
		return nil, err
	}
	canonical := strings.Join([]string{r.Method, awsEncode(target, false), query, headers.String(), fields["SignedHeaders"], payload}, "\n")
	digest := sha256.Sum256([]byte(canonical))
	scope := strings.Join(cred[1:], "/")
	toSign := alg + "\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(digest[:])
	key := []byte("AWS4" + secretKey)
	for _, part := range cred[1:] {
		key = hmacSHA256(key, part)
	}
	want := hex.EncodeToString(hmacSHA256(key, toSign))
	if !hmac.Equal([]byte(want), []byte(fields["Signature"])) {
		return nil, fmt.Errorf("signature %s, want %s for canonical request %q", fields["Signature"], want, canonical)
	}
	return signed, nil
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// awsEncode is AWS's UriEncode: every byte but the unreserved characters
// percent-encoded in upper-case hex, and "/" too when slash is set. The
// canonical path of a service other than S3 encodes the path as sent once
// more.
func awsEncode(s string, slash bool) string {
	var b strings.Builder
	for i := range len(s) {
		c := s[i]
		switch {
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9', c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && !slash:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// canonicalQuery returns the canonical query string of raw: each name and
// value decoded, then AWS-encoded, sorted by name and then value.
func canonicalQuery(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	var pairs []string
	for _, p := range strings.Split(raw, "&") {
		k, v, _ := strings.Cut(p, "=")
		dk, err := url.QueryUnescape(k)
		if err != nil {
			return "", err
		}
		dv, err := url.QueryUnescape(v)
		if err != nil {
			return "", err
		}
		pairs = append(pairs, awsEncode(dk, true)+"="+awsEncode(dv, true))
	}
	sort.Strings(pairs)
	return strings.Join(pairs, "&"), nil
}

// TestSigV4EndToEnd checks the signing hook on the data path: a PUT whose
// leg chain runs auth.upstream-sigv4 and then a transform that sets a
// header and rewrites the body of the attempt reaches the Upstream signed
// over the transformed request, payload hash included, so the Upstream's
// own verifier accepts it; the Upstream answers 503, and the retry, an
// attempt three seconds later on the Node clock, carries a new signature
// at its own time, which verifies too.
func TestSigV4EndToEnd(t *testing.T) {
	k := newKit(t)
	ups := newSigV4Upstream(t, func(n int) int {
		if n == 1 {
			k.clk.Advance(3 * time.Second)
			return http.StatusServiceUnavailable
		}
		return http.StatusOK
	})
	m := upstream.NewManager(upstream.Options{Clock: k.clk, Rand: lowDraws{}, Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, address)
	}})
	aws := &v1alpha1.Upstream{
		Metadata: v1alpha1.ObjectMeta{Name: "aws"},
		Spec: v1alpha1.UpstreamSpec{
			Protocol:  v1alpha1.UpstreamProtocolHTTP,
			Endpoints: []v1alpha1.Endpoint{{Address: ups.srv.Listener.Addr().String()}},
		},
	}
	set, err := m.Compile(t.Context(), upstream.Input{Upstreams: []upstream.Config{{Upstream: aws}}})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	t.Cleanup(func() { _ = set.Close() })
	set.Activate()
	fwd, err := forward.New("signed", []v1alpha1.RouteUpstream{{Name: "aws"}}, set, lowDraws{})
	if err != nil {
		t.Fatal(err)
	}

	sig, err := upstreamsigv4.Factory{Credentials: func(filter.BuildEnv) (upstreamsigv4.CredentialSource, error) {
		return staticCredentials{}, nil
	}}.Build(t.Context(), filter.BuildEnv{
		Config: &v1alpha1.AuthUpstreamSigV4Config{Region: "eu-west-1", Service: "execute-api"},
		Clock:  k.clk, Scope: phase.ScopeUpstream, Phases: phase.Of(phase.OnUpstreamRequest),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = sig.(filter.Closer).Close() })
	transformed := `{"order":42,"tenant":"t1"}`
	edit := filterFunc(func(ctx context.Context, p phase.Phase, x filter.Exchange) filter.Result {
		if p != phase.OnUpstreamRequest {
			return filter.Next()
		}
		x.Message().Header().Set("X-Tenant", "t1")
		if err := x.Message().SetBody(ctx, []byte(transformed)); err != nil {
			return filter.Undecided("RZ-RT-011", err)
		}
		return filter.Next()
	})
	signer := handlertest.Policy("sign", phase.ClassUpstreamAuth, v1alpha1.PolicyTypeAuthUpstreamSigV4, sig, phase.OnUpstreamRequest)
	transform := handlertest.Policy("tenant", phase.ClassTransform, v1alpha1.PolicyTypeTransformRequest, edit, phase.OnUpstreamRequest)
	signer.Scope, transform.Scope = phase.ScopeUpstream, phase.ScopeUpstream
	k.publish(handlertest.Spec{Routes: []handlertest.Route{{
		Name: "signed", Forward: fwd, Body: snapshot.BodyNeeds{RequestGate: true},
		Legs: map[string][]*snapshot.Policy{"aws": {signer, transform}},
	}}})

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "http://api.shop.example/prod/orders/42:item?b=2&a=x%20y", strings.NewReader(`{"order":42}`))
	req.Header.Set("Content-Type", "application/json")
	res := k.do(req)
	if res.Code != http.StatusOK || res.Body.String() != "ok" {
		t.Fatalf("response = %d %q, want 200 from the retry", res.Code, res.Body.String())
	}
	got := ups.requests()
	if len(got) != 2 {
		t.Fatalf("the Upstream received %d requests, want the attempt and its retry", len(got))
	}
	for i, want := range []string{"20261001T120000Z", "20261001T120003Z"} {
		g := got[i]
		if g.err != nil {
			t.Errorf("attempt %d does not verify: %v", i+1, g.err)
			continue
		}
		if g.date != want || g.tenant != "t1" || g.body != transformed {
			t.Errorf("attempt %d = date %s, X-Tenant %q, body %q; want %s with the transform's header and body", i+1, g.date, g.tenant, g.body, want)
		}
		for _, name := range []string{"host", "x-tenant", "content-type", "x-amz-content-sha256", "x-amz-date", "x-amz-security-token"} {
			if !slices.Contains(g.signed, name) {
				t.Errorf("attempt %d signs %v, want %s among them", i+1, g.signed, name)
			}
		}
	}
}
