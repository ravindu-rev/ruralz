// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package subst

import (
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// TestCredentialLiteralsWithErrors covers the RZ-CFG-013 credential rule,
// which reads any string outside an x-ruralz-secret subtree, on scalars an
// error leaves holding their authored text: an RZ-CFG-005 or RZ-CFG-010
// error never hides a credential-shaped literal, each literal segment is
// read alone so text around an expression never forms a match, and a
// forbidden position still takes RZ-CFG-011 alone.
func TestCredentialLiteralsWithErrors(t *testing.T) {
	const aws = "AKIAABCDEFGHIJKLMNOP"
	pw := "https://user:" + "${PASS}@otel.example"
	cases := []struct {
		name, src string
		want      []string
	}{
		{"undefined variable beside a key", prefixRoute(aws + " ${U}"), []string{CodeUndefined, CodeWarning}},
		{"malformed expression beside a key", prefixRoute(aws + " ${1"), []string{CodeMalformed, CodeWarning}},
		{"key after a malformed expression", prefixRoute("${1X} " + aws), []string{CodeMalformed, CodeWarning}},
		{"key after a malformed and a valid expression", prefixRoute("${1X} ${A} " + aws), []string{CodeMalformed, CodeWarning}},
		{"key in the second literal segment", prefixRoute("${U} x ${V} " + aws), []string{CodeUndefined, CodeUndefined, CodeWarning}},
		{"a password from a variable is no match", prefixRoute(pw), []string{CodeUndefined}},
		// PASS is undefined: the lenient read reports it.
		{"a password from a variable beside a malformed one", prefixRoute(pw + " ${1"), []string{CodeMalformed, CodeUndefined}},
		{"a key split by an expression is no match", prefixRoute("AKIA${U}ABCDEFGHIJKLMNOP"), []string{CodeUndefined}},
		{"an escaped key stays a literal", prefixRoute("$${" + aws + "} ${U}"), []string{CodeUndefined, CodeWarning}},
		{"no key, no warning", prefixRoute("plain ${U}"), []string{CodeUndefined}},
		{
			"forbidden position takes RZ-CFG-011 alone",
			`{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r"},"spec":{"match":{"when":"'` + aws + `' == '${U}'"}}}`,
			[]string{CodeForbidden},
		},
		{
			"secret subtree takes RZ-CFG-011 alone",
			`{"apiVersion":"ruralz/v1alpha1","kind":"Gateway","metadata":{"name":"g"},"spec":{"stateStore":{"url":{"secretRef":{"provider":"env","name":"` + aws + `${U}"}}}}}`,
			[]string{CodeForbidden},
		},
		{
			"credential header with an undefined variable warns by detector",
			`{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"h"},"spec":{"type":"headers","config":{"request":{"set":[{"name":"authorization","value":"` + aws + `${U}"}]}}}}`,
			[]string{CodeUndefined, CodeWarning},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ds := apply(t, tc.src, Map{"A": "a"})
			got := codes(ds)
			slices.Sort(got)
			want := slices.Clone(tc.want)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Fatalf("codes = %v, want %v: %q", got, want, texts(ds))
			}
			for _, d := range ds {
				if d.Code == CodeWarning && !strings.Contains(d.Message, "AWS access key ID") {
					t.Errorf("warning %q does not name the detector", d.Message)
				}
				if strings.Contains(string(d.AppendText(nil)), aws) {
					t.Errorf("diagnostic reveals the literal: %s", d.AppendText(nil))
				}
			}
		})
	}
}

// TestLiterals pins the lenient reading of authored text an RZ-CFG-005
// error leaves in place: a malformed ${ is literal text and the scan goes
// on after it.
func TestLiterals(t *testing.T) {
	for in, want := range map[string]string{
		"a ${1X} b":       "|a ${1X} b|",
		"${A:-${B}} c":    "|${A:-|{B}|} c|",
		"${1 ${A} ${":     "|${1 |{A}| ${|",
		"$${X} ${Y":       "|${X} ${Y|",
		"x":               "|x|",
		"${A}":            "{A}",
		"pre ${X-d} post": "|pre ${X-d} post|",
	} {
		var b strings.Builder
		for _, sg := range literals(in) {
			if sg.name == "" {
				b.WriteString("|" + sg.lit + "|")
			} else {
				b.WriteString("{" + sg.name + "}")
			}
		}
		if got := b.String(); got != want {
			t.Errorf("literals(%q) = %s, want %s", in, got, want)
		}
	}
}

// TestVars covers the tree.Node.Vars record, from which messages name the
// variables: each variable once, in first-use order, below and above the
// length where a set replaces the scan, so the dedupe stays linear in the
// number of distinct variables.
func TestVars(t *testing.T) {
	var names, text []string
	vars := Map{}
	for i := range 3 * linearScan {
		n := "V" + strconv.Itoa(i)
		names = append(names, n)
		vars[n] = "x"
	}
	for _, n := range names {
		text = append(text, "${"+n+"}")
	}
	for _, tc := range []struct {
		name, text string
		want       []string
	}{
		{"short, repeated", "${A}${B}${A}${C}${B}", []string{"A", "B", "C"}},
		{"exactly linearScan names", strings.Join(text[:linearScan], ""), names[:linearScan]},
		{"one past linearScan, repeated", strings.Join(text[:linearScan+1], "") + strings.Join(text[:linearScan+1], ""), names[:linearScan+1]},
		{"many, repeated in reverse", strings.Join(text, "") + strings.Join(reversed(slices.Clone(text)), ""), names},
		{"repeats before the set takes over", "${V0}${V0}" + strings.Join(text, "") + "${V0}", names},
	} {
		v := Map{"A": "a", "B": "b", "C": "c"}
		for k, x := range vars {
			v[k] = x
		}
		r, ds := apply(t, prefixRoute(tc.text), v)
		if len(ds) != 0 {
			t.Fatalf("%s: %q", tc.name, texts(ds))
		}
		if got := at(t, r.Root, "spec.match.path.prefix").Vars; !slices.Equal(got, tc.want) {
			t.Errorf("%s: Vars = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func reversed(s []string) []string {
	slices.Reverse(s)
	return s
}

// TestHintOrder checks that the RZ-CFG-010 hint, and so the output, never
// depends on the order of HintNames, which an Environment's
// spec.variables map yields in random order; diag.Nearest compares at most
// 64 length-compatible names, so the order would decide whether "ABCX" is
// seen. New keeps a sorted copy and leaves the caller's slice alone.
func TestHintOrder(t *testing.T) {
	var names []string
	for i := range 70 {
		names = append(names, "V"+strconv.Itoa(100+i))
	}
	text := prefixRoute("${ABCD}")
	_, ds := apply(t, text, nil, append([]string{"ABCX"}, names...)...)
	want := texts(ds)
	if len(want) != 1 || !strings.Contains(want[0], `did you mean "ABCX"?`) {
		t.Fatalf("diagnostics %q, want the ABCX hint", want)
	}
	orders := map[string][]string{
		"listed last":       append(slices.Clone(names), "ABCX"),
		"listed in reverse": reversed(append(slices.Clone(names), "ABCX")),
		"with duplicates":   append(append(slices.Clone(names), "ABCX"), names...),
	}
	for i := range 5 {
		rot := append(slices.Clone(names), "ABCX")
		k := (i*17 + 3) % len(rot)
		orders["rotated "+strconv.Itoa(k)] = append(rot[k:], rot[:k]...)
	}
	for name, order := range orders {
		before := slices.Clone(order)
		_, ds := apply(t, text, nil, order...)
		if got := texts(ds); !slices.Equal(got, want) {
			t.Errorf("%s: diagnostics %q, want %q", name, got, want)
		}
		if !slices.Equal(order, before) {
			t.Errorf("%s: New changed the caller's HintNames", name)
		}
	}
}

// TestScanMatchesReference runs the FuzzScan property over its seeds and
// over long malformed texts, so the linear scanner is pinned without the
// fuzzer.
func TestScanMatchesReference(t *testing.T) {
	texts := append(scanSeeds(),
		strings.Repeat("${A:-", 300), strings.Repeat("${A:-", 300)+"}", strings.Repeat("${A:-", 30)+strings.Repeat("}", 30),
		strings.Repeat("${A:-x}${", 50), strings.Repeat("$${A:-", 40)+"}", "${"+strings.Repeat("N", 500), strings.Repeat("${1X} ${A:-${B}} ", 20),
		// Names across the scanner's read windows.
		"${A"+strings.Repeat("9", searchWindow)+"}", "${"+strings.Repeat("A", searchWindow-1)+"9"+strings.Repeat("_", 10)+"}",
		"${"+strings.Repeat("A", searchWindow)+"-}", "${"+strings.Repeat("A", 2*searchWindow)+":-d}", "${"+strings.Repeat("A", searchWindow-2)+"}",
		"${"+strings.Repeat("A", searchWindow), "${A:-"+strings.Repeat("d", searchWindow)+"}"+strings.Repeat("e", searchWindow)+"$",
	)
	for _, s := range texts {
		for _, lenient := range []bool{false, true} {
			got, gotBad := collect(s, lenient)
			want, wantBad := referenceScan(s, lenient)
			if gotBad != wantBad || !slices.Equal(got, want) {
				t.Errorf("scan(%q, lenient %v) = %v %q, want %v %q", s, lenient, got, gotBad, want, wantBad)
			}
		}
		var got []run
		eachRun(s, nil, func(r run) bool {
			got = append(got, r)
			return true
		})
		if want := referenceRuns(s); !slices.Equal(got, want) {
			t.Errorf("eachRun(%q) = %v, want %v", s, got, want)
		}
	}
}

// TestScanMemory covers the memory side on hostile input: the scan
// allocates nothing per expression, so a scalar of 262,144 expressions
// whose result is empty, and a malformed one of the same size, allocate
// a small fraction of their text (a segment list would cost 56 bytes per
// expression, 14 times the text).
func TestScanMemory(t *testing.T) {
	n := raceScaled(1<<18, 1<<16)
	for name, text := range map[string]string{
		"empty results":         strings.Repeat("${E}", n),
		"empty defaults":        strings.Repeat("${U:-}", n),
		"unterminated defaults": strings.Repeat("${A:-", n),
		"malformed names":       strings.Repeat("${1", n),
	} {
		r := docs(t, &tree.FileTable{}, "r.yaml", prefixRoute(text))[0]
		s := New(Options{Schemas: schemas(t), Variables: Map{"E": ""}})
		if bytes := allocated(func() { _ = s.Apply(r) }); bytes > uint64(len(text)/4) {
			t.Errorf("%s: Apply allocated %d bytes for a %d-byte scalar, want at most %d", name, bytes, len(text), len(text)/4)
		}
	}
}

// TestSecretLike pins the secret-like name pattern
// (?i)(secret|password|token|apikey) on names of the grammar, without
// allocating.
func TestSecretLike(t *testing.T) {
	for name, want := range map[string]bool{
		"DB_PASSWORD": true, "myToken": true, "ApiKey": true, "client_secret": true, "SeCrEt": true, "TOKEN": true,
		"API_KEY": false, "TOKE": false, "PASSWOR_D": false, "_": false, "": false, "xapikeyx": true, "S3CRET": false,
	} {
		if got := secretLike(name, noMeter); got != want {
			t.Errorf("secretLike(%q) = %v, want %v", name, got, want)
		}
	}
	if a := testing.AllocsPerRun(10, func() { _ = secretLike("SOME_LONG_VARIABLE_NAME_WITHOUT_IT", noMeter) }); a != 0 {
		t.Errorf("secretLike allocates %.0f times", a)
	}
}

// TestNeverSubstituted covers mapping keys and the envelope fields: any ${
// in a key, apiVersion, kind or metadata.name, $${ and malformed forms
// included, is RZ-CFG-011 alone, the text stays as authored, and the hint
// says $${ is not an escape there; other forbidden positions still offer
// it.
func TestNeverSubstituted(t *testing.T) {
	keyHint := "(a mapping key must not contain ${; keys are never substituted, so $${ is not an escape there)"
	route := func(md, spec string) string {
		return `{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":` + md + `,"spec":` + spec + `}`
	}
	cases := []struct {
		name, src string
		want      []string
	}{
		{"an expression in a label key", route(`{"name":"r","labels":{"${K}":"v"}}`, `{}`), []string{
			`r.yaml:1:81 error RZ-CFG-011 Route/r metadata.labels["${K}"]: substitution is not allowed in a mapping key ` + keyHint,
		}},
		{"an escape in a label key", route(`{"name":"r","labels":{"$${K}":"v"}}`, `{}`), []string{
			`r.yaml:1:81 error RZ-CFG-011 Route/r metadata.labels["$${K}"]: substitution is not allowed in a mapping key ` + keyHint,
		}},
		{"a malformed expression in an annotation key", route(`{"name":"r","annotations":{"a${":"v"}}`, `{}`), []string{
			`r.yaml:1:86 error RZ-CFG-011 Route/r metadata.annotations["a${"]: substitution is not allowed in a mapping key ` + keyHint,
		}},
		{"an escape in a key of free content", `{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"p"},"spec":{"type":"cors","config":{"$${X}":1}}}`, []string{
			`r.yaml:1:105 error RZ-CFG-011 Policy/p spec.config["$${X}"]: substitution is not allowed in a mapping key ` + keyHint,
		}},
		{"an escape in metadata.name", route(`{"name":"$${N}"}`, `{}`), []string{
			`r.yaml:1:67 error RZ-CFG-011 Route/$${N} metadata.name: substitution is not allowed in metadata.name (metadata.name must not contain ${; it is never substituted, so $${ is not an escape there)`,
		}},
		{"an expression in metadata.name", route(`{"name":"${N}"}`, `{}`), []string{
			`r.yaml:1:67 error RZ-CFG-011 Route/${N} metadata.name: substitution is not allowed in metadata.name (metadata.name must not contain ${; it is never substituted, so $${ is not an escape there)`,
		}},
		{"a credential beside an escape in metadata.name takes RZ-CFG-011 alone", route(`{"name":"AKIAABCDEFGHIJKLMNOP$${N}"}`, `{}`), []string{
			`r.yaml:1:67 error RZ-CFG-011 Route/AKIAABCDEFGHIJKLMNOP$${N} metadata.name: substitution is not allowed in metadata.name (metadata.name must not contain ${; it is never substituted, so $${ is not an escape there)`,
		}},
		{"an escape in apiVersion", `{"apiVersion":"$${V}","kind":"Route","metadata":{"name":"r"},"spec":{}}`, []string{
			`r.yaml:1:15 error RZ-CFG-011 Route/r apiVersion: substitution is not allowed in apiVersion (apiVersion must not contain ${; it is never substituted, so $${ is not an escape there)`,
		}},
		{"an escape in kind", `{"apiVersion":"ruralz/v1alpha1","kind":"$${K}","metadata":{"name":"r"},"spec":{}}`, []string{
			`r.yaml:1:40 error RZ-CFG-011 $${K}/r kind: substitution is not allowed in kind (kind must not contain ${; it is never substituted, so $${ is not an escape there)`,
		}},
		{"a CEL field still offers the escape", route(`{"name":"r"}`, `{"match":{"when":"'${X}'"}}`), []string{
			`r.yaml:1:96 error RZ-CFG-011 Route/r spec.match.when: substitution is not allowed in a CEL expression (write $${ for a literal ${)`,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, ds := apply(t, tc.src, Map{"K": "k", "N": "n", "X": "x", "V": "v"})
			if g := texts(ds); !slices.Equal(g, tc.want) {
				t.Errorf("diagnostics =\n%q\nwant\n%q", g, tc.want)
			}
			if md, ok := r.Root.Get("metadata"); ok {
				if n, ok := md.Get("name"); ok && !strings.Contains(tc.src, `"name":"`+n.Text+`"`) {
					t.Errorf("metadata.name = %q, want the authored text", n.Text)
				}
			}
		})
	}
}

// TestAuthoredReading covers the reading of the RZ-CFG-013 credential
// rules: both read the authored runs, the literal text and every default
// whether taken or not, cut at each expression without default, never a
// variable's value, so a detector's result never depends on the
// Environment or on another expression failing, and a default that is
// only framing frames like literal text. The header literal rule also
// reads where each expression takes its text from: a value whose runs are
// only framing is a literal unless some expression takes its text from
// the variable source rather than from a default, so "${A:-;}" warns when
// A is unset or empty and not when A is set (the rows marked origin).
func TestAuthoredReading(t *testing.T) {
	annotation := func(text string) string {
		return `{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r","annotations":{"note":` + quote(text) + `}},"spec":{}}`
	}
	header := func(name, value string) string {
		return `{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"h"},"spec":{"type":"headers","config":{"request":{"set":[{"name":"` + name + `","value":` + quote(value) + `}]}}}}`
	}
	const url = "URL with a password"
	const literalHeader = "literal value for the credential header"
	cases := []struct {
		name, src string
		vars      Map
		// want is the message of each RZ-CFG-013 warning, the codes of
		// the errors besides.
		want, errs []string
	}{
		{"a credential default not taken warns", annotation("${E:-https://u:p@h}"), Map{"E": "https://example"}, []string{url}, nil},
		{"a credential default taken warns", annotation("${E:-https://u:p@h}"), nil, []string{url}, nil},
		{"a credential header default not taken warns", header("authorization", "${H:-hunter2}"), Map{"H": "v"}, []string{literalHeader}, nil},
		{"a credential header default taken warns", header("authorization", "${H:-hunter2}"), nil, []string{literalHeader}, nil},
		{"a password from a variable is no match", annotation("https://user:${PASS}@host"), Map{"PASS": "pw"}, nil, nil},
		{"the same beside an undefined variable", annotation("https://user:${PASS}@host ${UNDEF}"), Map{"PASS": "pw"}, nil, []string{CodeUndefined}},
		{"the same beside a malformed expression", annotation("https://user:${PASS}@host ${1"), Map{"PASS": "pw"}, nil, []string{CodeMalformed}},
		{"a password written as a default joins its run", annotation("https://user:${P:-pw}@host"), Map{"P": "x"}, []string{url}, nil},
		{"a credential split by a default joins its run", annotation("AKIA${X:-}ABCDEFGHIJKLMNOP"), Map{"X": "v"}, []string{"AWS access key ID"}, nil},
		{"a value a detector matches does not warn", annotation("${V}"), Map{"V": "AKIAABCDEFGHIJKLMNOP"}, nil, nil},
		{"a framing default between variables", header("cookie", "${A}${S:-; }${B}"), Map{"A": "a", "B": "b"}, nil, nil},
		{"a framing default taken between variables", header("cookie", "${A}${S:-; }${B}"), Map{"A": "a", "B": "b", "S": ""}, nil, nil},
		{"the same framing written as literal text", header("cookie", "${A}; ${B}"), Map{"A": "a", "B": "b"}, nil, nil},
		{"a scheme default before a variable", header("authorization", "${P:-Bearer }${T}"), Map{"T": "t"}, nil, nil},
		{"the same scheme written as literal text", header("authorization", "Bearer ${T}"), Map{"T": "t"}, nil, nil},
		{"a scheme default after a variable is literal text", header("authorization", "${T}${P:-Bearer }${U}"), Map{"T": "t", "U": "u"}, []string{literalHeader}, nil},
		{"a default holding a letter is literal text", header("x-api-key", "${A}${S:-k}${B}"), Map{"A": "a", "B": "b"}, []string{literalHeader}, nil},
		{"a header with every default unset and no other text", header("authorization", "${A:-;}"), nil, []string{literalHeader}, nil},
		{"origin: the same with the variable empty", header("authorization", "${A:-;}"), Map{"A": ""}, []string{literalHeader}, nil},
		{"origin: the same with the variable set", header("authorization", "${A:-;}"), Map{"A": "tok"}, nil, nil},
		{"an empty default supplies no text", header("authorization", "${A}${B:-}"), Map{"A": "tok"}, nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ds := apply(t, tc.src, tc.vars)
			var warns, errs []string
			for _, d := range ds {
				if d.Code == CodeWarning {
					msg := d.Message
					if i := strings.Index(msg, " \""); i >= 0 && strings.HasPrefix(msg, literalHeader) {
						msg = msg[:i]
					}
					msg = strings.TrimSuffix(strings.TrimPrefix(msg, "credential-shaped value ("), ")")
					warns = append(warns, msg)
					continue
				}
				errs = append(errs, d.Code)
			}
			if !slices.Equal(warns, tc.want) || !slices.Equal(errs, tc.errs) {
				t.Errorf("warnings %q and errors %q, want %q and %q: %q", warns, errs, tc.want, tc.errs, texts(ds))
			}
		})
	}
}

// TestApplyWithYield covers the yield for the loader's worker pool, whose
// workers each track the time since their own last yield, so Yield is not
// fixed at New: one Substituter serves workers that each pass their own
// Yield to ApplyWith; each worker's function is called exactly as often
// as a sequential run of its resources calls it, and Options.Yield is not
// called at all.
func TestApplyWithYield(t *testing.T) {
	src := labelsRoute(2000, keyN, func(i int) string { return "a${A}" + strconv.Itoa(i) })
	shared := 0
	s := New(Options{Schemas: schemas(t), Variables: Map{"A": "a"}, Yield: func() { shared++ }})
	want := 0
	_ = s.ApplyWith(docs(t, &tree.FileTable{}, "r.yaml", src)[0], func() { want++ })
	if want == 0 {
		t.Fatal("the fixture does not yield")
	}
	const workers, each = 2, 4
	counts := make([]int, workers)
	var wg sync.WaitGroup
	for k := range workers {
		rs := make([]*tree.Resource, each)
		for i := range rs {
			rs[i] = docs(t, &tree.FileTable{}, "r.yaml", src)[0]
		}
		wg.Go(func() {
			for _, r := range rs {
				_ = s.ApplyWith(r, func() { counts[k]++ })
			}
		})
	}
	wg.Wait()
	for k, c := range counts {
		if c != each*want {
			t.Errorf("worker %d: %d Yield calls, want %d", k, c, each*want)
		}
	}
	if shared != 0 {
		t.Errorf("Options.Yield called %d times by ApplyWith", shared)
	}
	if _ = s.Apply(docs(t, &tree.FileTable{}, "r.yaml", src)[0]); shared != want {
		t.Errorf("Apply called Options.Yield %d times, want %d", shared, want)
	}
}

// TestKeyCredentials covers credentials in mapping keys (RZ-CFG-013 warns
// at a credential-shaped literal in any string outside x-ruralz-secret,
// and keys are strings): every detector warns at a key that holds its
// shape, in an annotation and in a plugin config, at the key's position,
// naming the detector and never the key, neither in its message nor in its
// path, which is the object holding the key, not the key itself, in the
// text form and the JSON form alike; a key inside an x-ruralz-secret
// subtree, a key holding ${ (RZ-CFG-011 alone) and an ordinary key take no
// warning.
func TestKeyCredentials(t *testing.T) {
	samples := map[string]string{ //nolint:gosec // G101: test samples of each detector, not credentials.
		"PEM private key":                  "-----BEGIN RSA PRIVATE KEY-----",
		"URL with a password":              "https://user:pass@host",
		"JSON Web Token":                   "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N",
		"AWS access key ID":                "AKIAABCDEFGHIJKLMNOP",
		"GitHub token":                     "ghp_" + strings.Repeat("A1", 18),
		"GitHub fine-grained token":        "github_pat_" + strings.Repeat("a", 22),
		"Slack token":                      "xoxb-1234567890-abc",
		"API secret key":                   "sk-ant-" + strings.Repeat("x", 20),
		"Authorization header credentials": "Bearer abcdefghijklmnopqrstu==",
	}
	for _, d := range newDetectors() {
		key, ok := samples[d.name]
		if !ok {
			t.Fatalf("no sample for detector %q", d.name)
		}
		for where, tc := range map[string]struct{ src, path string }{
			"annotation": {
				`{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r","annotations":{` + quote(key) + `:"x"}},"spec":{}}`,
				"metadata.annotations",
			},
			"plugin config": {
				`{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"p"},"spec":{"type":"plugin","plugin":"g","config":{"tenants":{` + quote(key) + `:"a"}}}}`,
				"spec.config.tenants",
			},
		} {
			src := tc.src
			_, ds := apply(t, src, nil)
			if len(ds) != 1 || ds[0].Code != CodeWarning || ds[0].Message != "credential-shaped mapping key ("+d.name+")" {
				t.Errorf("%s in a %s key: %q, want one RZ-CFG-013 naming the detector", d.name, where, texts(ds))
				continue
			}
			if got := ds[0].Path.String(); got != tc.path {
				t.Errorf("%s in a %s key: path %s, want the object holding the key, %s", d.name, where, got, tc.path)
			}
			if s := revealed(t, ds, []string{key}); s != "" {
				t.Errorf("%s in a %s key: the diagnostic reveals the key: %q", d.name, where, texts(ds))
			}
			if ds[0].Line != 1 || ds[0].Column != strings.Index(src, quote(key))+1 {
				t.Errorf("%s in a %s key: at %v, want the key's position", d.name, where, ds[0].Location)
			}
		}
	}
	aws := samples["AWS access key ID"]
	for name, tc := range map[string]struct {
		src  string
		want []string
	}{
		"inside an x-ruralz-secret subtree": {
			`{"apiVersion":"ruralz/v1alpha1","kind":"Gateway","metadata":{"name":"g"},"spec":{"stateStore":{"url":{"secretRef":{` + quote(aws) + `:"x"}}}}}`, nil,
		},
		"a key holding ${ takes RZ-CFG-011 alone": {
			`{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r","annotations":{` + quote(aws+"${A}") + `:"x"}},"spec":{}}`,
			[]string{CodeForbidden},
		},
		"an ordinary key": {`{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r","annotations":{"team":"x"}},"spec":{}}`, nil},
		"a credential key and a credential value": {
			`{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r","annotations":{` + quote(aws) + `:` + quote(aws) + `}},"spec":{}}`,
			[]string{CodeWarning, CodeWarning},
		},
	} {
		_, ds := apply(t, tc.src, Map{"A": "a"})
		if got := codes(ds); !slices.Equal(got, tc.want) {
			t.Errorf("%s: %q, want codes %v", name, texts(ds), tc.want)
		}
	}
}

// TestMalformedSecretNames covers the secret-like name warning in a
// malformed scalar: a secret-like name warns at each well-formed use read
// leniently, as it warns beside an undefined variable (the two error
// classes read alike), and a name inside a malformed expression is no use.
// The names are defined, except where a row says otherwise:
// TestMalformedUndefined covers the RZ-CFG-010 of an undefined one.
func TestMalformedSecretNames(t *testing.T) {
	defined := Map{"DB_PASSWORD": "x", "TOKEN": "t", "NAME": "x"}
	for _, tc := range []struct {
		name, text string
		vars       Map
		want       []string
	}{
		{"secret-like name before a malformed ${", "${DB_PASSWORD} ${", defined, []string{CodeMalformed, CodeWarning}},
		{"secret-like name after a malformed ${", "${1X} ${DB_PASSWORD}", defined, []string{CodeMalformed, CodeWarning}},
		{"two uses in a malformed scalar", "${TOKEN}${TOKEN:-d} ${", defined, []string{CodeMalformed, CodeWarning, CodeWarning}},
		{"two uses in a malformed scalar, the variable undefined", "${TOKEN}${TOKEN:-d} ${", nil, []string{CodeMalformed, CodeUndefined, CodeWarning, CodeWarning}},
		{"secret-like name with an undefined sibling", "${DB_PASSWORD}${U}", defined, []string{CodeUndefined, CodeWarning}},
		{"a name inside an unterminated expression is no use", "${DB_PASSWORD", nil, []string{CodeMalformed}},
		{"a name inside a malformed operator is no use", "${DB_PASSWORD-d}", nil, []string{CodeMalformed}},
		{"a nested default is read after its malformed opening", "${A:-${TOKEN}}", defined, []string{CodeMalformed, CodeWarning}},
		{"an ordinary name in a malformed scalar", "${NAME} ${", defined, []string{CodeMalformed}},
	} {
		_, ds := apply(t, prefixRoute(tc.text), tc.vars)
		if got := codes(ds); !slices.Equal(got, tc.want) {
			t.Errorf("%s: %q, want codes %v", tc.name, texts(ds), tc.want)
		}
	}
}

// TestMessagesClipped: a message quotes at most maxShown bytes of a name
// from the input, so quoting a long name is never one uncounted pass nor
// the message as long as the name: the undefined variable, its hint and
// the secret-like name.
func TestMessagesClipped(t *testing.T) {
	long := strings.Repeat("A", 1<<16)
	for name, tc := range map[string]struct {
		text  string
		hints []string
	}{
		"undefined name with a hint": {"${" + long + "B}", []string{long + "C"}},
		"secret-like name":           {"${" + long + "_TOKEN:-d}", nil},
	} {
		s := New(Options{Schemas: schemas(t), HintNames: tc.hints})
		ds := s.Apply(docs(t, &tree.FileTable{}, "r.yaml", prefixRoute(tc.text))[0])
		if len(ds) == 0 {
			t.Fatalf("%s: no diagnostic", name)
		}
		for _, d := range ds {
			if len(d.Message)+len(d.Hint) > 4*maxShown+200 {
				t.Errorf("%s: a message and hint of %d bytes, want the name clipped", name, len(d.Message)+len(d.Hint))
			}
		}
	}
}
