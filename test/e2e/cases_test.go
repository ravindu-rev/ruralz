// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/profile"
)

// The Go driver of ruralz.test.v1 case files
// (docs/reference/01-cli-and-api-surface.md "Test case format"), which
// runs the end-to-end suite's cases until ruralz test run lands (Planned
// (M2)), so its cases move to the command unchanged. A file is one YAML
// document read under the restricted YAML profile: format
// ruralz.test.v1, a name and cases, each with a name, a request (method,
// path, headers whose values are strings or {file: PATH}, and an optional
// body string or json value) and an expect holding any of status, headers
// (exact), json (subset match) and bodyContains. A member the format does
// not define is rejected at every level, so a misspelled expectation
// never passes unchecked.

// caseFormat is the format identifier of a case file.
const caseFormat = "ruralz.test.v1"

// maxCaseBody bounds the response body a case reads.
const maxCaseBody = 16 << 20

// caseFile is one ruralz.test.v1 file.
type caseFile struct {
	Format string     `json:"format"`
	Name   string     `json:"name"`
	Cases  []testCase `json:"cases"`
	// path is the file's path, which relative {file: PATH} values start
	// from.
	path string
}

// testCase is one request and its expected response.
type testCase struct {
	Name    string      `json:"name"`
	Request caseRequest `json:"request"`
	Expect  caseExpect  `json:"expect"`
}

// caseRequest is the request of a case.
type caseRequest struct {
	Method  string                 `json:"method"`
	Path    string                 `json:"path"`
	Headers map[string]headerValue `json:"headers"`
	Body    *string                `json:"body"`
	JSON    json.RawMessage        `json:"json"`
}

// headerValue is a request header's value: a string, or {file: PATH}
// read when the case runs, never inlined into the case file.
type headerValue struct {
	value string
	file  string
}

// UnmarshalJSON reads a string or an object holding exactly file.
func (h *headerValue) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		return json.Unmarshal(b, &h.value)
	}
	var f struct {
		File *string `json:"file"`
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return fmt.Errorf("a header value is a string or {file: PATH}: %w", err)
	}
	if f.File == nil || *f.File == "" {
		return errors.New("a header value is a string or {file: PATH}")
	}
	h.file = *f.File
	return nil
}

// caseExpect is what a case expects of the response.
type caseExpect struct {
	Status       *int              `json:"status"`
	Headers      map[string]string `json:"headers"`
	JSON         json.RawMessage   `json:"json"`
	BodyContains *string           `json:"bodyContains"`
}

// readCaseFile reads and checks one case file.
func readCaseFile(path string) (*caseFile, error) {
	src, err := os.ReadFile(path) //nolint:gosec // G304: a case file of the suite
	if err != nil {
		return nil, err
	}
	docs, diags, err := profile.Parse(context.Background(), src, 1, path, profile.FormatYAML, profile.Options{})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(diags) > 0 {
		return nil, fmt.Errorf("%s:%d:%d: %s %s", path, diags[0].Line, diags[0].Column, diags[0].Code, diags[0].Message)
	}
	if len(docs) != 1 {
		return nil, fmt.Errorf("%s holds %d documents, want one", path, len(docs))
	}
	raw, err := json.Marshal(docs[0].Root.JSONValue())
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	f := &caseFile{path: path}
	if err := dec.Decode(f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := f.check(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return f, nil
}

// check applies the format's rules beyond the members' shapes.
func (f *caseFile) check() error {
	switch {
	case f.Format != caseFormat:
		return fmt.Errorf("format %q, want %s", f.Format, caseFormat)
	case f.Name == "":
		return errors.New("name is required")
	case len(f.Cases) == 0:
		return errors.New("cases is empty")
	}
	seen := map[string]bool{}
	for i, c := range f.Cases {
		switch {
		case c.Name == "":
			return fmt.Errorf("cases[%d]: name is required", i)
		case seen[c.Name]:
			return fmt.Errorf("cases[%d]: the name %q is taken", i, c.Name)
		case c.Request.Method == "" || c.Request.Path == "":
			return fmt.Errorf("case %q: request needs method and path", c.Name)
		case !strings.HasPrefix(c.Request.Path, "/"):
			return fmt.Errorf("case %q: request path %q does not start with /", c.Name, c.Request.Path)
		case c.Request.Body != nil && c.Request.JSON != nil:
			return fmt.Errorf("case %q: request holds both body and json", c.Name)
		case c.Expect.Status == nil && c.Expect.Headers == nil && c.Expect.JSON == nil && c.Expect.BodyContains == nil:
			return fmt.Errorf("case %q: expect holds none of status, headers, json and bodyContains", c.Name)
		}
		seen[c.Name] = true
	}
	return nil
}

// readCaseDir reads every *.yaml case file directly under dir, in name
// order.
func readCaseDir(dir string) ([]*caseFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []*caseFile
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		f, err := readCaseFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s holds no case file: %w", dir, fs.ErrNotExist)
	}
	return out, nil
}

// caseResult is the outcome of one case.
type caseResult struct {
	file, name string
	failures   []string
}

// passed reports a case without failures.
func (r caseResult) passed() bool { return len(r.failures) == 0 }

// String is the case's result line, as ruralz test run prints it.
func (r caseResult) String() string {
	if r.passed() {
		return "ok   " + r.file + ": " + r.name
	}
	return "FAIL " + r.file + ": " + r.name + ": " + strings.Join(r.failures, "; ")
}

// run sends each case of f to base, the target URL, through client, and
// returns one result per case in file order.
func (f *caseFile) run(ctx context.Context, client *http.Client, base string) []caseResult {
	out := make([]caseResult, 0, len(f.Cases))
	for _, c := range f.Cases {
		r := caseResult{file: f.Name, name: c.Name}
		if err := f.runCase(ctx, client, base, c, &r); err != nil {
			r.failures = append(r.failures, err.Error())
		}
		out = append(out, r)
	}
	return out
}

// runCase sends one case's request and checks its expectations.
func (f *caseFile) runCase(ctx context.Context, client *http.Client, base string, c testCase, r *caseResult) error {
	req, err := f.request(ctx, base, c.Request)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxCaseBody+1))
	if err != nil {
		return fmt.Errorf("read the response body: %w", err)
	}
	if len(body) > maxCaseBody {
		return fmt.Errorf("the response body is over %d bytes", maxCaseBody)
	}
	r.failures = append(r.failures, c.Expect.check(resp, body)...)
	return nil
}

// request builds the HTTP request of a case: the method, base plus the
// path, every header (a host header sets the Host) and the body, a json
// value sent as application/json unless the case sets content-type.
func (f *caseFile) request(ctx context.Context, base string, cr caseRequest) (*http.Request, error) {
	var body io.Reader
	switch {
	case cr.Body != nil:
		body = strings.NewReader(*cr.Body)
	case cr.JSON != nil:
		var v any
		if err := json.Unmarshal(cr.JSON, &v); err != nil {
			return nil, fmt.Errorf("request json: %w", err)
		}
		b, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("request json: %w", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, cr.Method, strings.TrimSuffix(base, "/")+cr.Path, body)
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	for _, name := range slices.Sorted(maps.Keys(cr.Headers)) {
		v, err := f.headerText(cr.Headers[name])
		if err != nil {
			return nil, fmt.Errorf("request header %s: %w", name, err)
		}
		if strings.EqualFold(name, "host") {
			req.Host = v
			continue
		}
		req.Header.Set(name, v)
	}
	if cr.JSON != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// headerText returns a header value: the string, or the content of the
// file, relative to the case file's directory, without a trailing line
// break.
func (f *caseFile) headerText(h headerValue) (string, error) {
	if h.file == "" {
		return h.value, nil
	}
	p := h.file
	if !filepath.IsAbs(p) {
		p = filepath.Join(filepath.Dir(f.path), filepath.FromSlash(p))
	}
	b, err := os.ReadFile(p) //nolint:gosec // G304: a file the case names beside it
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r"), nil
}

// check returns one failure per unmet expectation: the status, each
// header's exact value (a field's lines joined by ", "), the json subset
// and the body substring.
func (e caseExpect) check(resp *http.Response, body []byte) []string {
	var out []string
	if e.Status != nil && resp.StatusCode != *e.Status {
		out = append(out, fmt.Sprintf("status %d, want %d (body %.300s)", resp.StatusCode, *e.Status, body))
	}
	for _, name := range slices.Sorted(maps.Keys(e.Headers)) {
		want := e.Headers[name]
		got, ok := resp.Header[http.CanonicalHeaderKey(name)]
		if !ok {
			out = append(out, fmt.Sprintf("header %s missing, want %q", name, want))
			continue
		}
		if v := strings.Join(got, ", "); v != want {
			out = append(out, fmt.Sprintf("header %s = %q, want %q", name, v, want))
		}
	}
	if e.JSON != nil {
		var want, got any
		if err := decodeNumbers(e.JSON, &want); err != nil {
			out = append(out, "expect json: "+err.Error())
		} else if err := decodeNumbers(body, &got); err != nil {
			out = append(out, fmt.Sprintf("the body is not JSON (%v): %.300s", err, body))
		} else if why := subset(want, got, "$"); why != "" {
			out = append(out, why)
		}
	}
	if e.BodyContains != nil && !bytes.Contains(body, []byte(*e.BodyContains)) {
		out = append(out, fmt.Sprintf("the body does not contain %q: %.300s", *e.BodyContains, body))
	}
	return out
}

// decodeNumbers decodes JSON keeping numbers as json.Number.
func decodeNumbers(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("more than one JSON value")
	}
	return nil
}

// subset returns "" when got matches want as a subset, else why not, at
// path: an object matches when each of want's members matches got's
// member of that name, an array when it has want's length and each
// element matches, a number when it is numerically equal, and any other
// value when it is equal.
func subset(want, got any, path string) string {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return fmt.Sprintf("%s is %s, want an object", path, jsonText(got))
		}
		for _, k := range slices.Sorted(maps.Keys(w)) {
			gv, ok := g[k]
			if !ok {
				return fmt.Sprintf("%s.%s is missing, want %s", path, k, jsonText(w[k]))
			}
			if why := subset(w[k], gv, path+"."+k); why != "" {
				return why
			}
		}
		return ""
	case []any:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			return fmt.Sprintf("%s is %s, want an array of %d elements", path, jsonText(got), len(w))
		}
		for i := range w {
			if why := subset(w[i], g[i], fmt.Sprintf("%s[%d]", path, i)); why != "" {
				return why
			}
		}
		return ""
	case json.Number:
		g, ok := got.(json.Number)
		if !ok {
			return fmt.Sprintf("%s is %s, want %s", path, jsonText(got), w)
		}
		wf, werr := w.Float64()
		gf, gerr := g.Float64()
		if werr != nil || gerr != nil || wf != gf {
			return fmt.Sprintf("%s is %s, want %s", path, g, w)
		}
		return ""
	default:
		if want != got {
			return fmt.Sprintf("%s is %s, want %s", path, jsonText(got), jsonText(want))
		}
		return ""
	}
}

// jsonText formats a decoded value for a failure message.
func jsonText(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// runCases runs every case file under dir against each target through
// client and fails t for each failed case; it returns every result.
func runCases(t *testing.T, dir string, client *http.Client, targets ...string) []caseResult {
	t.Helper()
	files, err := readCaseDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var all []caseResult
	for _, target := range targets {
		for _, f := range files {
			for _, r := range f.run(t.Context(), client, target) {
				if !r.passed() {
					t.Errorf("%s against %s", r, target)
				}
				all = append(all, r)
			}
		}
	}
	return all
}
