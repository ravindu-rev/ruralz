// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration && linux

package cel

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/mockup"
	"github.com/ravindu-rev/ruralz/internal/testkit/promtext"
	"github.com/ravindu-rev/ruralz/internal/testkit/topology"
)

// The inputs of the runtime-error kinds
// (docs/architecture/02-configuration-model.md "Limits", targets).
const (
	// celPrefix names the headers every expression but accessLog.when
	// reads; logPrefix those accessLog.when reads.
	celPrefix = "x-cel-"
	logPrefix = "x-log-"
	// logGate is the header without which accessLog.when is true at once,
	// so no other case's request reaches its expression.
	logGate = "x-log"
	// limitEntries is the one-letter entries of a comprehension case's
	// list header, beside its needle of needleBytes: each entry's
	// startsWith charges 0.1 unit a needle byte, 1,639 units, so the
	// comprehension passes the 1,000,000-unit limit after about 610
	// entries, before its end, with the header block within the 64 KiB
	// cap. Entries are never empty, since a recipient may drop empty list
	// elements.
	limitEntries = 2000
	needleBytes  = 16 << 10
	// costBytes is the size of a cost case's a and b headers: one contains
	// call costs 0.01 × 16,384² units, about 2.7 million, past the
	// 1,000,000 limit.
	costBytes = 16 << 10
	// clockStep is the clock step of a send that waits on a Node timer;
	// the answer must come within clockBound of the clock.
	clockStep  = 10 * time.Millisecond
	clockBound = 5 * time.Second
	// apiKey is the Consumer's API key.
	apiKey = "cel-suite-key" //nolint:gosec // G101: the suite's test Consumer key, not a credential
)

// boolExpr is the expression of every case, over the headers of prefix p.
// Without the list header it errs with no_such_key; comparing a and b
// headers of costBytes reaches the cost limit in one call; over a list
// header of limitEntries entries and a needle of needleBytes it reaches
// the cost limit inside its comprehension, whose error no logical
// operator absorbs; with small headers it is true when a holds b. At the
// nominal sizes of RZ-CFG-015 it stays within 10,000 units, and at the
// 64 KiB header cap it can pass the runtime limit, so the caps leave it
// tracked.
func boolExpr(p string) string {
	return `request.headers["` + p + `list"].split(",").exists(s, s.startsWith(request.headers["` + p + `needle"])) || ` +
		`request.headers["` + p + `a"].contains(request.headers["` + p + `b"])`
}

// stringExpr, intExpr, dynExpr and pathExpr are boolExpr over celPrefix in
// the places of those result types: "yes", 1, {"r": "yes"} and "/c/yes"
// when true.
func stringExpr() string { return "(" + boolExpr(celPrefix) + `) ? "yes" : "no"` }
func intExpr() string    { return "(" + boolExpr(celPrefix) + ") ? 1 : 2" }
func dynExpr() string    { return "(" + boolExpr(celPrefix) + `) ? {"r": "yes"} : {"r": "no"}` }
func pathExpr() string   { return "(" + boolExpr(celPrefix) + `) ? "/c/yes" : "/c/no"` }

// quoted is s as a YAML single-quoted scalar.
func quoted(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// input is a request's set of expression headers.
type input int

const (
	inTrue       input = iota // the expression is true
	inFalse                   // the expression is false
	inEval                    // no header: an evaluation error
	inCost                    // a and b of costBytes: the cost limit in one call
	inLimitTrue               // inTrue with a long list and needle: true only if it ran to its end
	inLimitFalse              // inFalse with a long list and needle
)

// headers returns the input's headers named with prefix p.
func (in input) headers(p string) http.Header {
	h := http.Header{}
	set := func(name, v string) { h.Set(p+name, v) }
	list, needle := "q", "zz"
	if in == inLimitTrue || in == inLimitFalse {
		list = strings.TrimSuffix(strings.Repeat("a,", limitEntries), ",")
		needle = strings.Repeat("z", needleBytes)
	}
	switch in {
	case inTrue, inLimitTrue:
		set("list", list)
		set("needle", needle)
		set("a", "ab")
		set("b", "b")
	case inFalse, inLimitFalse:
		set("list", list)
		set("needle", needle)
		set("a", "a")
		set("b", "zz")
	case inCost:
		set("list", list)
		set("needle", "zz")
		set("a", strings.Repeat("a", costBytes))
		set("b", strings.Repeat("b", costBytes))
	case inEval:
	}
	return h
}

// kind is a runtime-error kind.
type kind struct {
	name string
	// logged is the error kind a debug record names.
	logged string
	// comprehension marks the cost limit reached inside the comprehension.
	comprehension bool
}

func kinds() []kind {
	return []kind{
		{name: "evaluation error", logged: "no_such_key"},
		{name: "cost limit", logged: "cost_limit"},
		{name: "comprehension cost limit", logged: "cost_limit", comprehension: true},
	}
}

// input returns the kind's input for a place whose control evaluates to
// contrast: a comprehension case's expression, had it run to its end,
// gives that control's answer, so only the cost limit gives the error's.
func (k kind) input(contrast bool) input {
	switch {
	case k.logged == "no_such_key":
		return inEval
	case !k.comprehension:
		return inCost
	case contrast:
		return inLimitTrue
	default:
		return inLimitFalse
	}
}

// answer is a response as the client saw it.
type answer struct {
	status int
	header http.Header
	body   []byte
	// code is the problem document's code member, "" for another body.
	code string
}

func (a answer) String() string {
	return fmt.Sprintf("%d %s %q", a.status, a.code, bytes.TrimSpace(a.body[:min(len(a.body), 200)]))
}

// suite is the Node every place runs on.
type suite struct {
	top         *topology.Topology
	n           *topology.Node
	client      *http.Client
	main, retry *mockup.Server
}

// place is one M1 CEL place, with the failureMode or optional variant
// its runtime-error rule depends on.
type place struct {
	name   string
	path   string
	method string
	body   string
	// prefix is the expression headers' prefix; extra headers go on every
	// request.
	prefix string
	extra  http.Header
	// contrast is what the control's expression evaluates to: the value
	// whose answer differs from the runtime error's.
	contrast bool
	normal   func(t *testing.T, a answer)
	failed   func(t *testing.T, a answer)
	// signal reads the counter one runtime error adds 1 to; nil when the
	// answer's code is the signal.
	signal func(set promtext.Set) float64
	// policy is the Policy whose debug record message names the error
	// kind; "" for a place that logs none.
	policy, message string
	// control replaces the default control; run replaces the default
	// check of one kind.
	control func(t *testing.T, s *suite, p *place)
	run     func(t *testing.T, s *suite, p *place, k kind)
}

// contrastInput is the control's input.
func (p *place) contrastInput() input {
	if p.contrast {
		return inTrue
	}
	return inFalse
}

// request builds the place's request with the input's headers on path.
func (s *suite) request(t *testing.T, p *place, path string, in input) *http.Request {
	t.Helper()
	method := p.method
	if method == "" {
		method = http.MethodGet
	}
	var body io.Reader
	if p.body != "" {
		body = strings.NewReader(p.body)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, s.n.URL("web")+path, body)
	if err != nil {
		t.Fatal(err)
	}
	prefix := p.prefix
	if prefix == "" {
		prefix = celPrefix
	}
	for k, vs := range in.headers(prefix) {
		req.Header[k] = vs
	}
	for k, vs := range p.extra {
		req.Header[k] = vs
	}
	if p.body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

// do sends req and reads the answer.
func (s *suite) do(req *http.Request) (answer, error) { return s.doOn(s.client, req) }

// doOn sends req on client and reads the answer.
func (s *suite) doOn(client *http.Client, req *http.Request) (answer, error) {
	resp, err := client.Do(req)
	if err != nil {
		return answer{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return answer{}, err
	}
	a := answer{status: resp.StatusCode, header: resp.Header, body: body}
	var problem struct {
		Code string `json:"code"`
	}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "application/problem+json") && json.Unmarshal(body, &problem) == nil {
		a.code = problem.Code
	}
	return a, nil
}

// sendMode is how send treats the clock.
type sendMode int

const (
	// plain sends with the clock standing still: no evaluation waits on
	// time, so every runtime error comes without the clock.
	plain sendMode = iota
	// advancing moves the clock in clockStep steps, for an answer that
	// waits on a Node timer, such as a retry delay.
	advancing
)

// send sends the place's request on path in mode.
func (s *suite) send(t *testing.T, p *place, path string, in input, mode sendMode) answer {
	t.Helper()
	req := s.request(t, p, path, in)
	if mode == plain {
		a, err := s.do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", req.Method, path, err)
		}
		return a
	}
	type result struct {
		a   answer
		err error
	}
	done := make(chan result, 1)
	go func() {
		a, err := s.do(req)
		done <- result{a, err}
	}()
	for advanced := time.Duration(0); ; advanced += clockStep {
		select {
		case r := <-done:
			if r.err != nil {
				t.Fatalf("%s %s: %v", req.Method, path, r.err)
			}
			return r.a
		case <-time.After(2 * time.Millisecond):
		}
		if advanced > clockBound {
			t.Fatalf("%s %s: no answer after %v of the clock", req.Method, path, advanced)
		}
		if err := s.top.Advance(t.Context(), clockStep); err != nil {
			t.Fatal(err)
		}
	}
}

// waitRecord waits for the debug record message names for policy with
// the error kind.
func (s *suite) waitRecord(t *testing.T, message, policy, errKind string) {
	t.Helper()
	msg := `"msg":"` + regexp.QuoteMeta(message) + `"`
	pol := `"policy":"` + regexp.QuoteMeta(policy) + `"`
	kind := `"error":"[^"]*: ` + regexp.QuoteMeta(errKind) + `"`
	re := regexp.MustCompile(msg + `.*(` + pol + `.*` + kind + `|` + kind + `.*` + pol + `)`)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if _, err := s.n.Process().WaitOutput(ctx, re); err != nil {
		t.Errorf("no %q record for %s naming %s: %v; the Node's records naming it:\n%s", message, policy, errKind, err, s.recordsOf(policy))
	}
}

// recordsOf returns the last stdout lines naming policy, for a failure
// message.
func (s *suite) recordsOf(policy string) string {
	out, _ := s.n.Process().Output()
	var lines []string
	for line := range bytes.Lines(out) {
		if bytes.Contains(line, []byte(`"`+policy+`"`)) && !bytes.Contains(line, []byte(`"msg":"access"`)) {
			lines = append(lines, string(bytes.TrimSpace(line)))
		}
	}
	return strings.Join(lines[max(len(lines)-5, 0):], "\n")
}

// check runs the default check of kind k: one request, the place's
// runtime-error answer, its signal and its debug record.
func (s *suite) check(t *testing.T, p *place, k kind) {
	t.Helper()
	before := scrape(t, s.top, s.n)
	a := s.send(t, p, p.path, k.input(p.contrast), plain)
	p.failed(t, a)
	if p.signal != nil {
		after := scrape(t, s.top, s.n)
		if d := p.signal(after) - p.signal(before); d != 1 {
			t.Errorf("the runtime error added %v to its counter, want 1", d)
		}
	}
	if p.policy != "" {
		s.waitRecord(t, p.message, p.policy, k.logged)
	}
}

// TestPlaces drives every M1 CEL place through each runtime-error kind:
// first a control whose expression evaluates, then an evaluation error
// and the cost limit, reached in one call and inside the comprehension,
// each with the place's runtime-error behavior and signal and with the
// clock standing still, since the cost limit alone bounds an evaluation
// (docs/architecture/02-configuration-model.md "Limits").
func TestPlaces(t *testing.T) {
	requireClock(t)
	s := startSuite(t)
	for _, p := range places() {
		t.Run(p.name, func(t *testing.T) {
			if p.control != nil {
				p.control(t, s, &p)
			} else {
				p.normal(t, s.send(t, &p, p.path, p.contrastInput(), plain))
			}
			for _, k := range kinds() {
				t.Run(k.name, func(t *testing.T) {
					if p.run != nil {
						p.run(t, s, &p, k)
						return
					}
					s.check(t, &p, k)
				})
			}
		})
	}
}

// startSuite starts the mocks and the Node serving every place.
func startSuite(t *testing.T) *suite {
	t.Helper()
	top := newTopology(t, topology.Config{Env: []string{"RURALZ_LOG_LEVEL=debug"}})
	main, err := top.StartMockUpstream(mockup.Config{
		LogSize:  -1,
		Behavior: mockup.Behavior{Body: []byte(`{"ok":true}`), ContentType: "application/json"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]string{
		"/c/first": `{"step":"first"}`, "/c/second": `{"step":"second"}`, "/c/last": `{"step":"last"}`,
		"/c/yes": `{"path":"yes"}`, "/c/no": `{"path":"no"}`,
	} {
		if err := main.SetRoute(path, mockup.Behavior{Body: []byte(body), ContentType: "application/json"}); err != nil {
			t.Fatal(err)
		}
	}
	retry, err := top.StartMockUpstream(mockup.Config{LogSize: -1, Behavior: mockup.Behavior{Status: http.StatusServiceUnavailable}})
	if err != nil {
		t.Fatal(err)
	}
	n, err := top.AddNodes(t.Context(), 1, topology.NodeOptions{Bundle: []byte(bundle(main.Addr(), retry.Addr()))})
	if err != nil {
		t.Fatal(err)
	}
	return &suite{top: top, n: n[0], client: top.HTTPClient(), main: main, retry: retry}
}

// keyHash is the Consumer's API key hash.
func keyHash() string {
	sum := sha256.Sum256([]byte(apiKey))
	return "sha256:" + hex.EncodeToString(sum[:])
}
