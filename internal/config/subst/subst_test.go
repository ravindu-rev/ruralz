// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package subst

import (
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// apply parses one resource document, substitutes it and returns it with
// the diagnostics.
func apply(t *testing.T, src string, vars Map, hints ...string) (*tree.Resource, diag.List) {
	t.Helper()
	files := &tree.FileTable{}
	rs := docs(t, files, tree.RoleBase, "r.yaml", src)
	if len(rs) != 1 {
		t.Fatalf("got %d documents", len(rs))
	}
	s := New(Options{Schemas: schemas(t), Files: files, Variables: vars, HintNames: hints})
	return rs[0], s.Apply(rs[0])
}

// prefixRoute is a Route whose spec.match.path.prefix (a plain string
// field) holds text.
func prefixRoute(text string) string {
	return `{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r"},"spec":{"match":{"path":{"prefix":` + quote(text) + `}}}}`
}

func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// TestScan covers the grammar of 01 req 26.
func TestScan(t *testing.T) {
	cases := []struct {
		in   string
		want string // segments as |lit| and {name} / {name:-def}
		bad  string
	}{
		{in: "", want: ""},
		{in: "plain", want: "|plain|"},
		{in: "${A}", want: "{A}"},
		{in: "${_a1}", want: "{_a1}"},
		{in: "${A:-}", want: "{A:-}"},
		{in: "${A:-x y:z-}", want: "{A:-x y:z-}"},
		{in: "a${A}b${B:-c}d", want: "|a|{A}|b|{B:-c}|d|"},
		{in: "$${A}", want: "|${A}|"},
		{in: "$$${A}", want: "|$${A}|"},
		{in: "$$$${A}", want: "|$$${A}|"},
		{in: "$x $ {y} $$ a$", want: "|$x $ {y} $$ a$|"},
		{in: "${A}}", want: "{A}|}|"},
		{in: "${", bad: reasonUnterminated},
		{in: "x${A", bad: reasonUnterminated},
		{in: "${A:", bad: reasonUnterminated},
		{in: "${A:-x", bad: reasonUnterminated},
		{in: "${}", bad: reasonName},
		{in: "${1X}", bad: reasonName},
		{in: "${-}", bad: reasonName},
		{in: "${X-d}", bad: reasonOperator},
		{in: "${X:=d}", bad: reasonOperator},
		{in: "${X d}", bad: reasonOperator},
		{in: "${A:-${B}}", bad: reasonNested},
		{in: "${A:-$${B}}", bad: reasonNested},
		{in: "ok ${A} then ${", bad: reasonUnterminated},
	}
	for _, tc := range cases {
		segs, bad := scan(tc.in)
		if bad != tc.bad {
			t.Errorf("scan(%q) reason = %q, want %q", tc.in, bad, tc.bad)
			continue
		}
		if bad != "" {
			continue
		}
		var b strings.Builder
		for _, s := range segs {
			switch {
			case s.name == "":
				b.WriteString("|" + s.lit + "|")
			case s.hasDef:
				b.WriteString("{" + s.name + ":-" + s.def + "}")
			default:
				b.WriteString("{" + s.name + "}")
			}
		}
		if got := strings.ReplaceAll(b.String(), "||", "|"); got != tc.want {
			t.Errorf("scan(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

// TestSubstitute covers 01 req 26 and 27: escapes, defaults with :-
// treating empty as unset, plain ${NAME} with an empty value, values
// inserted literally (never re-scanned, never structure), RZ-CFG-010 per
// occurrence with an Environment hint, and RZ-CFG-005 for malformed text.
func TestSubstitute(t *testing.T) {
	vars := Map{"A": "x", "EMPTY": "", "NESTED": "${A}", "STRUCT": "a: b\n- c {d} # e", "PORT": "8443"}
	cases := []struct {
		name, text, want string
		vars             []string
		codes            []string
	}{
		{name: "req26 one expression", text: "${A}", want: "x", vars: []string{"A"}},
		{name: "req26 mixed text, escape and default", text: "pre-${A}-${B:-d}-$${A}", want: "pre-x-d-${A}", vars: []string{"A", "B"}},
		{name: "req26 a $ not followed by { is literal", text: "$5 and $A and $", want: "$5 and $A and $"},
		{name: "req27 :- treats empty as unset", text: "${EMPTY:-d}", want: "d", vars: []string{"EMPTY"}},
		{name: "req27 plain ${NAME} set to empty yields empty", text: "${EMPTY}", want: "", vars: []string{"EMPTY"}},
		{name: "req27 empty default", text: "${U:-}", want: "", vars: []string{"U"}},
		{name: "req27 not recursive", text: "${NESTED}", want: "${A}", vars: []string{"NESTED"}},
		{name: "req27 never injects structure", text: "${STRUCT}", want: "a: b\n- c {d} # e", vars: []string{"STRUCT"}},
		{name: "req27 repeated variable recorded once", text: "${A}${A}", want: "xx", vars: []string{"A"}},
		{name: "req27 undefined without default", text: "${U}", want: "${U}", codes: []string{CodeUndefined}},
		{name: "req27 one RZ-CFG-010 per occurrence", text: "${U}-${U}-${A}", want: "${U}-${U}-${A}", codes: []string{CodeUndefined, CodeUndefined}},
		{name: "req26 malformed nested default", text: "${A:-${B}}", want: "${A:-${B}}", codes: []string{CodeMalformed}},
		{name: "req26 malformed name", text: "${1X}", want: "${1X}", codes: []string{CodeMalformed}},
		{name: "req26 malformed operator", text: "${X-d}", want: "${X-d}", codes: []string{CodeMalformed}},
		{name: "req26 malformed assignment", text: "${X:=d}", want: "${X:=d}", codes: []string{CodeMalformed}},
		{name: "req26 unterminated", text: "a ${A", want: "a ${A", codes: []string{CodeMalformed}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, ds := apply(t, prefixRoute(tc.text), vars)
			if g := codes(ds); !slices.Equal(g, tc.codes) {
				t.Fatalf("codes = %v, want %v: %q", g, tc.codes, texts(ds))
			}
			n := at(t, r.Root, "spec.match.path.prefix")
			if n.Kind != tree.KindString || n.Text != tc.want {
				t.Errorf("text = %v %q, want string %q", n.Kind, n.Text, tc.want)
			}
			if !slices.Equal(n.Vars, tc.vars) {
				t.Errorf("Vars = %v, want %v", n.Vars, tc.vars)
			}
		})
	}
}

// TestDiagnosticText pins the wording, position and hint of RZ-CFG-005,
// -010 and -011 (01 req 27: the hint names the nearest spec.variables key
// for an Environment source and nothing for the process environment).
func TestDiagnosticText(t *testing.T) {
	src := `{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r"},
"spec":{"match":{"path":{"prefix":"${HTTPS_PROT}"},"when":"${X}"},
"timeout":"${1X}","upstreams":[{"name":"${U}"}]}}`
	_, ds := apply(t, src, Map{"HTTPS_PORT": "1"}, "HTTPS_PORT", "OTHER")
	want := []string{
		`r.yaml:2:35 error RZ-CFG-010 Route/r spec.match.path.prefix: variable "HTTPS_PROT" is not defined and has no default (did you mean "HTTPS_PORT"?)`,
		`r.yaml:2:59 error RZ-CFG-011 Route/r spec.match.when: substitution is not allowed in a CEL expression (write $${ for a literal ${)`,
		`r.yaml:3:11 error RZ-CFG-005 Route/r spec.timeout: malformed substitution (a variable name matches [A-Za-z_][A-Za-z0-9_]*); write $${ for a literal ${`,
		`r.yaml:3:40 error RZ-CFG-011 Route/r spec.upstreams[name="${U}"].name: substitution is not allowed in a reference field (references resolve statically) (write $${ for a literal ${)`,
	}
	if g := texts(ds); !slices.Equal(g, want) {
		t.Errorf("diagnostics =\n%q\nwant\n%q", g, want)
	}
	_, ds = apply(t, prefixRoute("${HTTPS_PROT}"), Map{"HTTPS_PORT": "1"})
	if len(ds) != 1 || ds[0].Hint != "" {
		t.Errorf("process environment: diagnostics %q, want one without hint", texts(ds))
	}
}

// TestPathsUseAuthoredKeys pins the path convention of Substituter.Apply:
// list entries are named by their authored key or element values, never
// by substituted text, so no stage E diagnostic reveals a variable's
// value (01 req 27 and 30); the tree holds the substituted values.
func TestPathsUseAuthoredKeys(t *testing.T) {
	src := `{"apiVersion":"ruralz/v1alpha1","kind":"Gateway","metadata":{"name":"g"},"spec":{
"listeners":[{"name":"${N}","protocol":"http","port":"${U}"},{"name":"${MY_TOKEN}","protocol":"http","port":1}],
"trustedProxies":["${P}"]}}`
	r, ds := apply(t, src, Map{"N": "h", "MY_TOKEN": "s3cr3t"})
	want := []string{
		`r.yaml:2:54 error RZ-CFG-010 Gateway/g spec.listeners[name="${N}"].port: variable "U" is not defined and has no default`,
		`r.yaml:2:70 warning RZ-CFG-013 Gateway/g spec.listeners[name="${MY_TOKEN}"].name: variable "MY_TOKEN" looks like a secret (use a secretRef with provider: env instead of substitution)`,
		`r.yaml:3:19 error RZ-CFG-010 Gateway/g spec.trustedProxies[item="${P}"]: variable "P" is not defined and has no default`,
	}
	if g := texts(ds); !slices.Equal(g, want) {
		t.Errorf("diagnostics =\n%q\nwant\n%q", g, want)
	}
	for _, d := range ds {
		if text := string(d.AppendText(nil)); strings.Contains(text, "s3cr3t") {
			t.Errorf("diagnostic reveals a variable value: %s", text)
		}
	}
	if got := at(t, r.Root, "spec.listeners.0.name").Text; got != "h" {
		t.Errorf("listener name = %q, want the substituted h", got)
	}
}

// TestRetype covers 01 req 29: a scalar that is exactly one expression
// takes the rendered view's type at its path; partial substitution and
// text that does not fit stay strings.
func TestRetype(t *testing.T) {
	gw := func(path, member, text string) string {
		inner := `"` + member + `":` + quote(text)
		switch path {
		case "listener":
			return `{"apiVersion":"ruralz/v1alpha1","kind":"Gateway","metadata":{"name":"g"},"spec":{"listeners":[{"name":"h",` + inner + `}]}}`
		case "telemetry":
			return `{"apiVersion":"ruralz/v1alpha1","kind":"Gateway","metadata":{"name":"g"},"spec":{"telemetry":{` + inner + `}}}`
		case "limits":
			return `{"apiVersion":"ruralz/v1alpha1","kind":"Gateway","metadata":{"name":"g"},"spec":{"limits":{` + inner + `}}}`
		case "discovery":
			return `{"apiVersion":"ruralz/v1alpha1","kind":"Upstream","metadata":{"name":"u"},"spec":{"discovery":{"type":"dns",` + inner + `}}}`
		case "labels":
			return `{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r","labels":{` + inner + `}},"spec":{}}`
		case "plugin":
			return `{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"p"},"spec":{"type":"plugin","config":{` + inner + `}}}`
		case "schema":
			return `{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"p"},"spec":{"type":"validation.json-schema","config":{"schema":{"properties":{"a":{` + inner + `}}}}}}`
		case "schemadoc":
			return `{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"p"},"spec":{"type":"validation.json-schema","config":{` + inner + `}}}`
		}
		return ""
	}
	resultPath := map[string]string{
		"listener": "spec.listeners.0.", "telemetry": "spec.telemetry.", "limits": "spec.limits.",
		"discovery": "spec.discovery.", "labels": "metadata.labels.", "plugin": "spec.config.",
		"schema": "spec.config.schema.properties.a.", "schemadoc": "spec.config.",
	}
	cases := []struct {
		name, where, member, text, value string
		kind                             tree.Kind
		want                             string // Text, or "true"/"false" for booleans
	}{
		{"integer", "listener", "port", "${V}", "8443", tree.KindInt, "8443"},
		{"integer from default", "listener", "port", "${UNSET:-8443}", "", tree.KindInt, "8443"},
		{"integer leading zeros", "listener", "port", "${V}", "0777", tree.KindInt, "777"},
		{"integer octal", "listener", "port", "${V}", "0o17", tree.KindInt, "15"},
		{"integer hex", "listener", "port", "${V}", "0x1F", tree.KindInt, "31"},
		{"integer sign", "listener", "port", "${V}", "+5", tree.KindInt, "5"},
		{"integer minus zero", "listener", "port", "${V}", "-0", tree.KindInt, "0"},
		{"integer that does not fit", "listener", "port", "${V}", "abc", tree.KindString, "abc"},
		{"integer field with a float", "listener", "port", "${V}", "1.5", tree.KindString, "1.5"},
		{"integer outside int64", "listener", "port", "${V}", "99999999999999999999", tree.KindString, "99999999999999999999"},
		{"signed hex is not core", "listener", "port", "${V}", "-0x1", tree.KindString, "-0x1"},
		{"underscore digits are not core", "listener", "port", "${V}", "1_000", tree.KindString, "1_000"},
		{"partial substitution is a string", "listener", "port", "${V}0", "844", tree.KindString, "8440"},
		{"quoted style does not matter", "listener", "port", "${V}", "1", tree.KindInt, "1"},
		{"number float", "telemetry", "traceSampling", "${V}", "0.5", tree.KindFloat, "0.5"},
		{"number leading dot", "telemetry", "traceSampling", "${V}", ".5", tree.KindFloat, "0.5"},
		{"number plus and trailing dot", "telemetry", "traceSampling", "${V}", "+1.", tree.KindFloat, "1"},
		{"number exponent", "telemetry", "traceSampling", "${V}", "-1e3", tree.KindFloat, "-1e3"},
		{"number leading zeros", "telemetry", "traceSampling", "${V}", "007.50", tree.KindFloat, "7.50"},
		{"number exponent sign", "telemetry", "traceSampling", "${V}", ".5E+2", tree.KindFloat, "0.5E+2"},
		{"number integer", "telemetry", "traceSampling", "${V}", "1", tree.KindInt, "1"},
		{"req12 number field with an integer outside int64 stays a string", "telemetry", "traceSampling", "${V}", "99999999999999999999", tree.KindString, "99999999999999999999"},
		{"req12 number field with a hex integer outside int64 stays a string", "telemetry", "traceSampling", "${V}", "0x10000000000000000", tree.KindString, "0x10000000000000000"},
		{"number field with a large float", "telemetry", "traceSampling", "${V}", "99999999999999999999.0", tree.KindFloat, "99999999999999999999.0"},
		{"number inf does not fit", "telemetry", "traceSampling", "${V}", ".inf", tree.KindString, ".inf"},
		{"number nan does not fit", "telemetry", "traceSampling", "${V}", ".nan", tree.KindString, ".nan"},
		{"number lone dot", "telemetry", "traceSampling", "${V}", ".", tree.KindString, "."},
		{"number bad exponent", "telemetry", "traceSampling", "${V}", "1e", tree.KindString, "1e"},
		{"number double sign exponent", "telemetry", "traceSampling", "${V}", "1e+-2", tree.KindString, "1e+-2"},
		{"boolean", "listener", "proxyProtocol", "${V}", "True", tree.KindBool, "true"},
		{"boolean false", "listener", "proxyProtocol", "${V}", "FALSE", tree.KindBool, "false"},
		{"boolean yes is a string", "listener", "proxyProtocol", "${V}", "yes", tree.KindString, "yes"},
		{"ByteSize quantity stays a string", "limits", "maxResponseBodyBytes", "${V}", "10Mi", tree.KindString, "10Mi"},
		{"ByteSize integer", "limits", "maxResponseBodyBytes", "${V}", "1024", tree.KindInt, "1024"},
		{"IntOrString integer", "discovery", "port", "${V}", "8080", tree.KindInt, "8080"},
		{"IntOrString name", "discovery", "port", "${V}", "http", tree.KindString, "http"},
		{"string field keeps 0777", "labels", "a", "${V}", "0777", tree.KindString, "0777"},
		{"free content is a string", "plugin", "port", "${V}", "8080", tree.KindString, "8080"},
		{"inside JSONSchemaDocument never re-typed", "schema", "minimum", "${V}", "5", tree.KindString, "5"},
		{"JSONSchemaDocument itself takes its boolean type", "schemadoc", "schema", "${V}", "true", tree.KindBool, "true"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, ds := apply(t, gw(tc.where, tc.member, tc.text), Map{"V": tc.value})
			if len(ds) != 0 {
				t.Fatalf("diagnostics: %q", texts(ds))
			}
			n := at(t, r.Root, resultPath[tc.where]+tc.member)
			got := n.Text
			if n.Kind == tree.KindBool {
				got = map[bool]string{true: "true", false: "false"}[n.Bool]
			}
			if n.Kind != tc.kind || got != tc.want {
				t.Errorf("%s = kind %d %q, want kind %d %q", tc.text, n.Kind, got, tc.kind, tc.want)
			}
			if len(n.Vars) != 1 {
				t.Errorf("Vars = %v, want the substituted variable", n.Vars)
			}
		})
	}
}

// TestForbiddenPositions covers 01 req 28: keys, apiVersion, kind,
// metadata.name, x-ruralz-ref, the x-ruralz-secret subtree and
// x-ruralz-cel positions report RZ-CFG-011 only (never -010 or -013), and
// $${ is still unescaped there.
func TestForbiddenPositions(t *testing.T) {
	cases := []struct {
		name, src, path string
		want            []string
		text            string
	}{
		{
			name: "mapping key",
			src:  `{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r","labels":{"${K}":"v"}},"spec":{}}`,
			want: []string{CodeForbidden},
		},
		{
			name: "mapping key with an escape",
			src:  `{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r","labels":{"$${K}":"v"}},"spec":{}}`,
			want: []string{CodeForbidden},
		},
		{
			name: "apiVersion",
			src:  `{"apiVersion":"${V}","kind":"Route","metadata":{"name":"r"},"spec":{}}`,
			want: []string{CodeForbidden},
		},
		{
			name: "kind",
			src:  `{"apiVersion":"ruralz/v1alpha1","kind":"${K}","metadata":{"name":"r"},"spec":{}}`,
			want: []string{CodeForbidden},
		},
		{
			name: "metadata.name",
			src:  `{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"${N}"},"spec":{}}`,
			want: []string{CodeForbidden},
		},
		{
			name: "x-ruralz-ref",
			src:  `{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r"},"spec":{"upstreams":[{"name":"${UNDEFINED}"}]}}`,
			want: []string{CodeForbidden},
		},
		{
			name: "x-ruralz-secret subtree with a secret-like name",
			src: `{"apiVersion":"ruralz/v1alpha1","kind":"Gateway","metadata":{"name":"g"},"spec":{"listeners":[{"name":"h",
				"tls":{"certificates":[{"name":"c","privateKey":{"secretRef":{"provider":"file","name":"${KEY_SECRET}"}}}]}}]}}`,
			want: []string{CodeForbidden},
		},
		{
			name: "x-ruralz-cel",
			src:  `{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r"},"spec":{"match":{"when":"request.path == '${X}'"}}}`,
			want: []string{CodeForbidden},
		},
		{
			name: "malformed in a forbidden position is RZ-CFG-011 only",
			src:  `{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r"},"spec":{"match":{"when":"'${1X'"}}}`,
			want: []string{CodeForbidden},
		},
		{
			name: "escape unescaped in a CEL field",
			src:  `{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r"},"spec":{"match":{"when":"request.path == '$${X}'"}}}`,
			path: "spec.match.when",
			text: "request.path == '${X}'",
		},
		{
			name: "escape unescaped in a secret field",
			src:  `{"apiVersion":"ruralz/v1alpha1","kind":"Gateway","metadata":{"name":"g"},"spec":{"stateStore":{"url":{"secretRef":{"provider":"env","name":"$${X}"}}}}}`,
			path: "spec.stateStore.url.secretRef.name",
			text: "${X}",
		},
		{
			name: "dispatch reads the substituted spec.type: config.rule is CEL",
			src:  `{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"p"},"spec":{"type":"${PT}","config":{"rule":"${X}"}}}`,
			want: []string{CodeForbidden},
		},
		{
			name: "dispatch reads the substituted spec.type: rule of another type is free",
			src:  `{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"p"},"spec":{"type":"${PT2}","config":{"rule":"${X}"}}}`,
			path: "spec.config.rule",
			text: "x",
		},
		{
			name: "element of a reference list",
			src:  `{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r"},"spec":{"policies":[{"name":"${X}"}]}}`,
			want: []string{CodeForbidden},
		},
	}
	vars := Map{"PT": "authz.cel", "PT2": "plugin", "X": "x", "K": "k", "V": "v", "N": "n", "KEY_SECRET": "s"}
	for _, tc := range cases {
		t.Run("req28 "+tc.name, func(t *testing.T) {
			r, ds := apply(t, tc.src, vars)
			if g := codes(ds); !slices.Equal(g, tc.want) {
				t.Fatalf("codes = %v, want %v: %q", g, tc.want, texts(ds))
			}
			if tc.path != "" {
				if n := at(t, r.Root, tc.path); n.Text != tc.text {
					t.Errorf("%s = %q, want %q", tc.path, n.Text, tc.text)
				}
			}
		})
	}
}

// TestCredentialWarnings covers RZ-CFG-013 (01 req 30 and the 01 J row
// for 013, owned by this stage per architecture 3.1): each use of a
// secret-like variable name, and each credential-shaped literal outside
// an x-ruralz-secret subtree, CEL strings included, warns without the
// value; a headers set[] value for a credential header warns too.
func TestCredentialWarnings(t *testing.T) {
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U"
	ghp := "ghp_" + strings.Repeat("A1", 18)
	cases := []struct {
		name, text string
		vars       Map
		want       int // RZ-CFG-013 warnings
		detector   string
	}{
		{name: "req30 secret-like name", text: "${DB_PASSWORD}", vars: Map{"DB_PASSWORD": "x"}, want: 1},
		{name: "req30 case-insensitive name", text: "${myToken}-${ApiKey}-${client_secret}", vars: Map{"myToken": "", "ApiKey": "", "client_secret": ""}, want: 3},
		{name: "req30 warns even when undefined", text: "${TOKEN:-x}", want: 1},
		{name: "req30 API_KEY does not match apikey", text: "${API_KEY}", vars: Map{"API_KEY": "x"}, want: 0},
		{name: "PEM private key", text: "-----BEGIN RSA PRIVATE " + "KEY-----\nMIIB", want: 1, detector: "PEM private key"},
		{name: "PEM certificate is fine", text: "-----BEGIN CERTIFICATE-----", want: 0},
		{name: "URL with a password", text: "https://user:" + "pass@otel.example:4318", want: 1, detector: "URL with a password"},
		{name: "URL without a password", text: "https://user@otel.example:4318", want: 0},
		{name: "JWT", text: "token " + jwt, want: 1, detector: "JSON Web Token"},
		{name: "AWS key", text: "AKIAABCDEFGHIJKLMNOP", want: 1, detector: "AWS access key ID"},
		{name: "AWS key too short", text: "AKIAABCDEFGHIJKLMNO", want: 0},
		{name: "GitHub token", text: ghp, want: 1, detector: "GitHub token"},
		{name: "GitHub fine-grained token", text: "github_pat_" + strings.Repeat("a", 22), want: 1, detector: "GitHub fine-grained token"},
		{name: "Slack token", text: "xoxb-1234567890-abc", want: 1, detector: "Slack token"},
		{name: "API secret key", text: "sk-ant-" + strings.Repeat("x", 20), want: 1, detector: "API secret key"},
		{name: "Bearer credentials", text: "Bearer abcdefghijklmnopqrstu==", want: 1, detector: "Authorization header credentials"},
		{name: "short Bearer is fine", text: "Bearer abc", want: 0},
		{name: "substituted value is checked", text: "${V}", vars: Map{"V": "AKIAABCDEFGHIJKLMNOP"}, want: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ds := apply(t, prefixRoute(tc.text), tc.vars)
			got := 0
			for _, d := range ds {
				if d.Code != CodeWarning {
					continue
				}
				got++
				if d.Severity != diag.SeverityWarning {
					t.Errorf("severity %v, want warning", d.Severity)
				}
				if tc.text != "" && strings.Contains(string(d.AppendText(nil)), tc.text) && tc.detector != "" {
					t.Errorf("diagnostic contains the value: %s", d.AppendText(nil))
				}
				if tc.detector != "" && !strings.Contains(d.Message, tc.detector) {
					t.Errorf("message %q does not name detector %q", d.Message, tc.detector)
				}
			}
			if got != tc.want {
				t.Errorf("%d RZ-CFG-013 warnings, want %d: %q", got, tc.want, texts(ds))
			}
			if ds.HasErrors() {
				t.Errorf("unexpected errors: %q", texts(ds))
			}
		})
	}
	t.Run("JWT in a CEL literal warns", func(t *testing.T) {
		src := `{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r"},"spec":{"match":{"when":"request.headers['a'] == '` + jwt + `'"}}}`
		_, ds := apply(t, src, nil)
		if g := codes(ds); !slices.Equal(g, []string{CodeWarning}) {
			t.Errorf("codes = %v", g)
		}
	})
	t.Run("literal inside an x-ruralz-secret subtree does not warn", func(t *testing.T) {
		src := `{"apiVersion":"ruralz/v1alpha1","kind":"Gateway","metadata":{"name":"g"},"spec":{"stateStore":{"url":{"secretRef":{"provider":"env","name":"https://u:` + `p@x"}}}}}`
		if _, ds := apply(t, src, nil); len(ds) != 0 {
			t.Errorf("diagnostics: %q", texts(ds))
		}
	})
	t.Run("PEM in a label warns", func(t *testing.T) {
		src := `{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r","labels":{"k":"-----BEGIN PRIVATE KEY-----"}},"spec":{}}`
		if _, ds := apply(t, src, nil); !slices.Equal(codes(ds), []string{CodeWarning}) {
			t.Errorf("diagnostics: %q", texts(ds))
		}
	})
	headers := func(side, list, entry string) string {
		return `{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"h"},"spec":{"type":"headers","config":{"` + side + `":{"` + list + `":[` + entry + `]}}}}`
	}
	for _, tc := range []struct {
		name, src string
		want      []string
		vars      Map
	}{
		{"request set authorization literal", headers("request", "set", `{"name":"Authorization","value":"secret"}`), []string{
			`r.yaml:1:158 warning RZ-CFG-013 Policy/h spec.config.request.set[name=Authorization].value: literal value for the credential header "authorization" (keep credentials out of the Bundle: reference a secret with secretRef)`,
		}, nil},
		{"response set cookie literal", headers("response", "set", `{"name":"cookie","value":"a=b"}`), []string{
			`r.yaml:1:152 warning RZ-CFG-013 Policy/h spec.config.response.set[name=cookie].value: literal value for the credential header "cookie" (keep credentials out of the Bundle: reference a secret with secretRef)`,
		}, nil},
		{"x-api-key and proxy-authorization", headers("request", "set", `{"name":"X-API-Key","value":"k"},{"name":"proxy-authorization","value":"p"}`), []string{
			`r.yaml:1:154 warning RZ-CFG-013 Policy/h spec.config.request.set[name=X-API-Key].value: literal value for the credential header "x-api-key" (keep credentials out of the Bundle: reference a secret with secretRef)`,
			`r.yaml:1:197 warning RZ-CFG-013 Policy/h spec.config.request.set[name=proxy-authorization].value: literal value for the credential header "proxy-authorization" (keep credentials out of the Bundle: reference a secret with secretRef)`,
		}, nil},
		{"one warning when a detector also matches", headers("request", "set", `{"name":"authorization","value":"Bearer abcdefghijklmnopqrstuvwx"}`), []string{
			`r.yaml:1:158 warning RZ-CFG-013 Policy/h spec.config.request.set[name=authorization].value: credential-shaped value (Authorization header credentials) (keep credentials out of the Bundle: reference a secret with secretRef)`,
		}, nil},
		{"empty value", headers("request", "set", `{"name":"authorization","value":""}`), nil, nil},
		{"valueExpression", headers("request", "set", `{"name":"authorization","valueExpression":"auth.token"}`), nil, nil},
		{"other header", headers("request", "set", `{"name":"x-trace","value":"1"}`), nil, nil},
		{"add list is not the set list", headers("request", "add", `{"name":"authorization","value":"secret"}`), nil, nil},
		{"an unresolved expression is not a literal", headers("request", "set", `{"name":"authorization","value":"${U}"}`), []string{
			`r.yaml:1:158 error RZ-CFG-010 Policy/h spec.config.request.set[name=authorization].value: variable "U" is not defined and has no default`,
		}, nil},
		{"a malformed expression is not a literal", headers("request", "set", `{"name":"authorization","value":"${1U}"}`), []string{
			`r.yaml:1:158 error RZ-CFG-005 Policy/h spec.config.request.set[name=authorization].value: malformed substitution (a variable name matches [A-Za-z_][A-Za-z0-9_]*); write $${ for a literal ${`,
		}, nil},
		{"a secret-like variable warns once", headers("request", "set", `{"name":"authorization","value":"${AUTH_TOKEN}"}`), []string{
			`r.yaml:1:158 warning RZ-CFG-013 Policy/h spec.config.request.set[name=authorization].value: variable "AUTH_TOKEN" looks like a secret (use a secretRef with provider: env instead of substitution)`,
		}, Map{"AUTH_TOKEN": "t"}},
		{"a value from a variable is not a literal", headers("request", "set", `{"name":"authorization","value":"Basic ${CRED}"}`), nil, Map{"CRED": "x"}},
		{"an escaped ${ is a literal", headers("request", "set", `{"name":"cookie","value":"$${C}"}`), []string{
			`r.yaml:1:151 warning RZ-CFG-013 Policy/h spec.config.request.set[name=cookie].value: literal value for the credential header "cookie" (keep credentials out of the Bundle: reference a secret with secretRef)`,
		}, nil},
	} {
		t.Run("headers "+tc.name, func(t *testing.T) {
			_, ds := apply(t, tc.src, tc.vars)
			if g := texts(ds); !slices.Equal(g, tc.want) {
				t.Errorf("diagnostics =\n%q\nwant\n%q", g, tc.want)
			}
		})
	}
}

// TestEscape covers 01 req 31: Escape writes every literal ${ as $${ and
// substituting the escaped text gives the literal back.
func TestEscape(t *testing.T) {
	for _, s := range []string{"", "plain", "${A}", "$${A}", "$$${", "a${b}${", "${A:-${B}}", "$", "{$", "$${"} {
		e := Escape(s)
		segs, bad := scan(e)
		if bad != "" || hasExpression(segs) {
			t.Errorf("Escape(%q) = %q scans as malformed or an expression", s, e)
			continue
		}
		if got := literal(segs); got != s {
			t.Errorf("unescape(Escape(%q)) = %q", s, got)
		}
		r, ds := apply(t, prefixRoute(e), nil)
		if n := at(t, r.Root, "spec.match.path.prefix"); len(ds) != 0 || n.Text != s {
			t.Errorf("substituting Escape(%q) = %q with %q", s, n.Text, texts(ds))
		}
	}
	if got := Escape("a ${B} $${C}"); got != "a $${B} $$${C}" {
		t.Errorf("Escape = %q", got)
	}
}

// TestEnviron covers the os.Environ snapshot Source (01 req 55: the
// package never reads the process environment itself).
func TestEnviron(t *testing.T) {
	m := Environ([]string{"A=1", "B=x=y", "A=2", "=C:=C:\\", "NOEQ", "EMPTY="})
	want := Map{"A": "1", "B": "x=y", "EMPTY": ""}
	if len(m) != len(want) {
		t.Fatalf("Environ = %v, want %v", m, want)
	}
	for k, v := range want {
		if got, ok := m.Lookup(k); !ok || got != v {
			t.Errorf("Lookup(%q) = %q, %v; want %q", k, got, ok, v)
		}
	}
	if _, ok := m.Lookup("NOEQ"); ok {
		t.Error("an entry without = was kept")
	}
}

// TestNoSchema: a resource whose apiVersion or kind has no schema is
// RZ-CFG-007 and stays untouched; nil and non-object resources are
// skipped; nil Variables define nothing.
func TestNoSchema(t *testing.T) {
	r, ds := apply(t, `{"apiVersion":"ruralz/v9","kind":"Route","metadata":{"name":"r"},"spec":{"timeout":"${A}"}}`, Map{"A": "x"})
	if g := codes(ds); !slices.Equal(g, []string{CodeAPIVersion}) || at(t, r.Root, "spec.timeout").Text != "${A}" {
		t.Errorf("codes %v, timeout %q", g, at(t, r.Root, "spec.timeout").Text)
	}
	_, ds = apply(t, `{"apiVersion":"ruralz/v1alpha1","kind":"Rout","metadata":{"name":"r"}}`, nil)
	if g := codes(ds); !slices.Equal(g, []string{CodeAPIVersion}) {
		t.Errorf("unknown kind codes %v", g)
	}
	s := New(Options{Schemas: schemas(t)})
	if ds := s.Apply(nil); ds != nil {
		t.Errorf("Apply(nil) = %v", ds)
	}
	if ds := s.Apply(&tree.Resource{Root: &tree.Node{Kind: tree.KindList}}); ds != nil {
		t.Errorf("Apply(list root) = %v", ds)
	}
	if ds := New(Options{}).Apply(&tree.Resource{APIVersion: v1, Root: &tree.Node{Kind: tree.KindMap}}); len(ds) != 1 {
		t.Errorf("no schema set: %v", ds)
	}
	files := &tree.FileTable{}
	r = docs(t, files, tree.RoleBase, "r.yaml", prefixRoute("${A}"))[0]
	if g := codes(New(Options{Schemas: schemas(t), Files: files}).Apply(r)); !slices.Equal(g, []string{CodeUndefined}) {
		t.Errorf("nil Variables: codes %v", g)
	}
}
