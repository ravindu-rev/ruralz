// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package upstreamsigv4

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"

	"github.com/ravindu-rev/ruralz/internal/routematch"
)

// AWS's published Signature Version 4 test vectors: the aws-c-auth signing
// test suite (testdata/aws-signing-test-suite, SOURCE names its pin), the
// suite AWS's SDKs sign against. Every header-signing case runs but three
// kinds the design never signs: the query-signing (presigned URL) files,
// the "-unnormalized" cases, which encode the path twice without
// normalizing it, a mode no service uses here (S3 neither normalizes nor
// encodes twice, every other service does both), and post-sts-header-after,
// which leaves the session token out of the signature, as no service here
// does.

// suiteDir is the directory of the vendored v4 cases.
func suiteDir() string { return filepath.Join("testdata", "aws-signing-test-suite", "v4") }

// suiteContext is a case's context.json.
type suiteContext struct {
	Credentials struct {
		AccessKeyID     string `json:"access_key_id"`
		SecretAccessKey string `json:"secret_access_key"`
		Token           string `json:"token"`
	} `json:"credentials"`
	Normalize        bool   `json:"normalize"`
	Region           string `json:"region"`
	Service          string `json:"service"`
	SignBody         bool   `json:"sign_body"`
	Timestamp        string `json:"timestamp"`
	OmitSessionToken bool   `json:"omit_session_token"`
}

// suiteRequest is a parsed request.txt or header-signed-request.txt.
type suiteRequest struct {
	method, target string
	host           string
	header         http.Header
	contentLength  int64
	body           []byte
}

// readSuiteRequest parses a request file of the suite: a request line, then
// "Name:value" header lines, a line that starts with a space or a tab
// continuing the one before it (unfolded with one space, as net/http's
// reader does), then a blank line and the body. The target keeps its path
// verbatim, spaces included.
func readSuiteRequest(t *testing.T, file string) suiteRequest {
	t.Helper()
	data, err := os.ReadFile(file) //nolint:gosec // G304: a vendored test vector of this package
	if err != nil {
		t.Fatal(err)
	}
	head, body, _ := bytes.Cut(data, []byte("\n\n"))
	sc := bufio.NewScanner(bytes.NewReader(head))
	if !sc.Scan() {
		t.Fatalf("%s: no request line", file)
	}
	line := sc.Text()
	sp, last := strings.IndexByte(line, ' '), strings.LastIndexByte(line, ' ')
	if sp < 0 || last <= sp {
		t.Fatalf("%s: request line %q", file, line)
	}
	r := suiteRequest{method: line[:sp], target: line[sp+1 : last], header: http.Header{}, body: body}
	var prev string
	for sc.Scan() {
		l := sc.Text()
		if l == "" {
			continue
		}
		if l[0] == ' ' || l[0] == '\t' {
			vs := r.header[prev]
			vs[len(vs)-1] += " " + strings.TrimSpace(l)
			continue
		}
		name, value, ok := strings.Cut(l, ":")
		if !ok {
			t.Fatalf("%s: header line %q", file, l)
		}
		key := http.CanonicalHeaderKey(name)
		value = strings.TrimSpace(value)
		switch key {
		case "Host":
			r.host = value
		case "Content-Length":
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				t.Fatalf("%s: Content-Length %q", file, value)
			}
			r.contentLength = n
		default:
			r.header[key] = append(r.header[key], value)
			prev = key
		}
	}
	return r
}

// outgoing returns the request of a case as the Upstream layer hands it to
// the signing hook: the target's path as the escaped path it sends, the
// query raw, the host from Host and the body readable through GetBody.
func (s suiteRequest) outgoing() *http.Request {
	path, query, _ := strings.Cut(s.target, "?")
	u := &url.URL{Scheme: "https", Host: s.host, RawQuery: query}
	routematch.SetRequestPath(u, path)
	if u.RawPath == "" {
		// A path that does not decode, such as one holding a raw space,
		// still goes out as written.
		u.Path, u.RawPath = path, path
	}
	r := &http.Request{Method: s.method, URL: u, Header: s.header, Body: http.NoBody, ContentLength: s.contentLength}
	if len(s.body) > 0 {
		b := s.body
		r.Body = io.NopCloser(bytes.NewReader(b))
		r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }
	}
	return r
}

// TestAWSSigningTestSuite signs every vendored case and checks the
// signature fields against the case's signed request: Authorization (whose
// signature covers the canonical request), X-Amz-Date, X-Amz-Security-Token
// for temporary credentials and, for a case that signs its body, the
// X-Amz-Content-Sha256 the signing hook sends. A case that signs its body
// runs the whole signing hook (params.sign); the others, which sign no
// X-Amz-Content-Sha256, check the canonicalization the hook signs with
// (params.signCanonical) over the empty payload's hash, as the suite does.
func TestAWSSigningTestSuite(t *testing.T) {
	entries, err := os.ReadDir(suiteDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 30 {
		t.Fatalf("%d vendored cases, want 30", len(entries))
	}
	for _, e := range entries {
		t.Run(e.Name(), func(t *testing.T) {
			dir := filepath.Join(suiteDir(), e.Name())
			raw, err := os.ReadFile(filepath.Join(dir, "context.json")) //nolint:gosec // G304: a vendored test vector of this package
			if err != nil {
				t.Fatal(err)
			}
			var c suiteContext
			if err := json.Unmarshal(raw, &c); err != nil {
				t.Fatal(err)
			}
			if !c.Normalize || c.OmitSessionToken {
				t.Fatalf("the case normalizes %v and omits the session token %v: not a mode the design signs", c.Normalize, c.OmitSessionToken)
			}
			at, err := time.Parse(time.RFC3339, c.Timestamp)
			if err != nil {
				t.Fatal(err)
			}
			creds := aws.Credentials{AccessKeyID: c.Credentials.AccessKeyID, SecretAccessKey: c.Credentials.SecretAccessKey, SessionToken: c.Credentials.Token}
			p := params{region: c.Region, service: c.Service}
			r := readSuiteRequest(t, filepath.Join(dir, "request.txt")).outgoing()
			sentPath := routematch.RequestPath(r.URL)
			if c.SignBody {
				err = p.sign(t.Context(), v4.NewSigner(), r, creds, at)
			} else {
				err = p.signCanonical(t.Context(), v4.NewSigner(), r, creds, emptyPayloadHash, at)
			}
			if err != nil {
				t.Fatalf("sign: %v", err)
			}
			want := readSuiteRequest(t, filepath.Join(dir, "header-signed-request.txt"))
			for _, k := range []string{headerAuthorization, headerDate, headerSecurityToken, headerContentSHA256} {
				if got, w := r.Header.Values(k), want.header.Values(k); strings.Join(got, ",") != strings.Join(w, ",") {
					t.Errorf("%s = %q, want %q", k, got, w)
				}
			}
			if got := routematch.RequestPath(r.URL); got != sentPath {
				t.Errorf("path sent = %q, want the request's own %q", got, sentPath)
			}
		})
	}
}
