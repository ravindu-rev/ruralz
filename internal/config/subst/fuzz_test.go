// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package subst

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// shape describes a tree's structure: object keys, list lengths and
// scalars as one class, so substitution may re-type a scalar but never
// change the shape.
func shape(n *tree.Node) string {
	var b strings.Builder
	var walk func(n *tree.Node)
	walk = func(n *tree.Node) {
		switch n.Kind {
		case tree.KindMap:
			b.WriteByte('{')
			for _, m := range n.Members {
				b.WriteString(strconv.Quote(m.Key))
				b.WriteByte(':')
				walk(m.Value)
				b.WriteByte(',')
			}
			b.WriteByte('}')
		case tree.KindList:
			b.WriteByte('[')
			for _, it := range n.Items {
				walk(it)
				b.WriteByte(',')
			}
			b.WriteByte(']')
		default:
			b.WriteByte('s')
		}
	}
	walk(n)
	return b.String()
}

// referenceResult is reference's reading of one text.
type referenceResult struct {
	// out is the substituted text.
	out string
	// ok is false for malformed text.
	ok bool
	// undefined counts the expressions without value or default, exprs
	// all expressions, sourced those that took the variable's value, and
	// secretUses those whose name looks secret.
	undefined, exprs, sourced, secretUses int
	// runs are the authored runs (eachRun): the literal text, escapes
	// resolved, and every default, taken or not, cut at each expression
	// without default; lead marks the run at the start of the text.
	runs []referenceRun
}

type referenceRun struct {
	text string
	lead bool
}

// reference substitutes text by an anchored regular-expression reading of
// the substitution grammar, independent of scan. With lenient set, a
// malformed ${ is literal text and the reading goes on (how the detectors
// read the text an error leaves in place); otherwise malformed text gives
// a zero result.
func reference(text string, vars Map, lenient bool) referenceResult {
	expr := regexp.MustCompile(`^\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?\}`)
	secret := regexp.MustCompile(`(?i)(secret|password|token|apikey)`)
	var out, run strings.Builder
	var r referenceResult
	lead := true
	flush := func() {
		if run.Len() > 0 {
			r.runs = append(r.runs, referenceRun{text: run.String(), lead: lead})
			run.Reset()
		}
	}
	for pos := 0; pos < len(text); {
		rest := text[pos:]
		switch {
		case strings.HasPrefix(rest, "$${"):
			out.WriteString("${")
			run.WriteString("${")
			pos += 3
		case strings.HasPrefix(rest, "${"):
			m := expr.FindStringSubmatch(rest)
			if m == nil || strings.Contains(m[3], "${") {
				if !lenient {
					return referenceResult{}
				}
				out.WriteString("${")
				run.WriteString("${")
				pos += 2
				continue
			}
			r.exprs++
			if secret.MatchString(m[1]) {
				r.secretUses++
			}
			if m[2] != "" {
				run.WriteString(m[3])
			} else {
				flush()
				lead = false
			}
			v, set := vars[m[1]]
			switch {
			case m[2] != "" && (!set || v == ""):
				v = m[3]
			case !set:
				r.undefined++
			default:
				r.sourced++
			}
			out.WriteString(v)
			pos += len(m[0])
		default:
			out.WriteByte(text[pos])
			run.WriteByte(text[pos])
			pos++
		}
	}
	flush()
	r.out, r.ok = out.String(), true
	return r
}

// literalHeader is the reference reading of the credential header
// literal rule (walker.literal): no expression from the source, or an
// authored run that is not framing, a leading "Bearer " or "Basic "
// scheme in any case of the run at the start and otherwise text without
// letters or digits.
func (r referenceResult) literalHeader() bool {
	scheme := regexp.MustCompile(`(?i)^(bearer|basic) `)
	alnum := regexp.MustCompile(`[\pL\p{Nd}]`)
	if r.sourced == 0 {
		return true
	}
	for _, run := range r.runs {
		text := run.text
		if run.lead {
			text = scheme.ReplaceAllString(text, "")
		}
		if alnum.MatchString(text) {
			return true
		}
	}
	return false
}

// detected reports an authored run a detector matches.
func (r referenceResult) detected(ds []detector) bool {
	for _, run := range r.runs {
		if detect(ds, run.text, noMeter) != "" {
			return true
		}
	}
	return false
}

// headerSpot places text, a JSON-safe string, as the value of an
// authorization entry of a headers Policy request.set[] list.
func headerSpot(text string) string {
	return `{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"h"},"spec":{"type":"headers","config":{"request":{"set":[{"name":"authorization","value":` +
		quote(text) + `}]}}}}`
}

// forbiddenSpots places text, a JSON-safe string, in three positions
// where substitution is forbidden: a CEL field, a reference field and a
// name inside an x-ruralz-secret subtree. Each entry is a document and
// the at path of the text.
func forbiddenSpots(text string) [][2]string {
	q := quote(text)
	return [][2]string{
		{`{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r"},"spec":{"match":{"when":` + q + `}}}`, "spec.match.when"},
		{`{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r"},"spec":{"upstreams":[{"name":` + q + `}]}}`, "spec.upstreams.0.name"},
		{`{"apiVersion":"ruralz/v1alpha1","kind":"Gateway","metadata":{"name":"g"},"spec":{"stateStore":{"url":{"secretRef":{"provider":"env","name":` + q + `}}}}}`, "spec.stateStore.url.secretRef.name"},
	}
}

// FuzzSubst checks that substitution never injects structure: for any
// resource and variable value (newlines, ": ", "- ", "{", "#", "${", "$${"
// included), substitution never changes the tree's shape, emits only
// registered codes and is deterministic; in a free string position the text
// equals an independent reading of the grammar, so values are inserted
// literally and never re-scanned; in a forbidden position (CEL, reference,
// x-ruralz-secret subtree) text holding an unescaped ${, an expression or a
// malformed one, is RZ-CFG-011 alone and stays as authored, and any other
// text becomes exactly its literal reading with no diagnostic but
// RZ-CFG-013, so no unescaped ${ is ever substituted there; in a free
// position the secret-like name warns at each well-formed use, read
// leniently when the text is malformed, and the text warns once more
// exactly when a detector matches one of its authored runs (literal text
// and every default, taken or not, cut at each expression without default;
// read leniently when the text is malformed); as the value of a credential
// header set[] entry, it also warns once when the value is non-empty and a
// literal (the rule of walker.literal), and a text an error leaves in place
// takes the detector warning only; and substituting Escape(text) yields
// text.
func FuzzSubst(f *testing.F) {
	for _, s := range [][3]string{
		{prefixRoute("x"), "a: b\n- c", "pre ${A} mid ${B:-d} post $${A}"},
		{`{"kind":"Policy","spec":{"type":"${T}","config":{"rule":"${A}","x":["${A}",{"${A}":1}]}}}`, "authz.cel", "${A}${A}"},
		{`{"kind":"Gateway","spec":{"listeners":[{"name":"${A}","port":"${A}","tls":{"certificates":[{"privateKey":{"secretRef":{"name":"${A}"}}}]}}]}}`, "8443", "${A:-${B}}"},
		{`{"kind":"Policy","spec":{"type":"validation.json-schema","config":{"schema":{"enum":["${A}","$${A}"]}}}}`, "${B} #{", "$$${A}"},
		{`{"metadata":{"labels":{"a":"${TOKEN}"}},"spec":{"match":{"when":"$${A}"}}}`, "", "${1X} ${X-d} ${X:=d} ${"},
		{`{"spec":{"upstreams":[{"name":"${A}"}],"timeout":"${A}${B}"}}`, "$${", "${A:-}${B}"},
		{prefixRoute("x"), "v", "$${A} AKIAABCDEFGHIJKLMNOP $"},
		{prefixRoute("x"), "v", "a $${ b"},
		{headerSpot("x"), "", "${A:-hunter2}"},
		{headerSpot("x"), "v", "Bearer ${A}${TOKEN:-t}"},
		{headerSpot("x"), "Bearer abcdefghijklmnopqrstuvwx", "${TOKEN}"},
		{headerSpot("x"), "v", "Bearer ${A:-}"},
		{headerSpot("x"), "tok", "${A}${B:-}"},
		{headerSpot("x"), "", "hardcodedkey${A}"},
		{headerSpot("x"), "v", "bearer ${A}; ${T}"},
		{headerSpot("x"), "v", "AKIAABCDEFGHIJKLMNOP ${U}"},
		{headerSpot("x"), "v", "AKIAABCDEFGHIJKLMNOP ${1"},
		{headerSpot("x"), "v", "${1 AKIAABCDEFGHIJKLMNOP ${A}"},
		// Several authored runs: a literal run before framing, and a
		// framing default taken beside a sourced variable.
		{headerSpot("x"), "v", "key${A};"},
		{headerSpot("x"), "v", "0${A} "},
		{headerSpot("x"), "sid=1", "${A}${B:-; }"},
		{prefixRoute("x"), "v", "https://u:${A}@h ${U}"},
	} {
		f.Add(s[0], s[1], s[2])
	}
	f.Fuzz(func(t *testing.T, doc, value, text string) {
		if len(doc)+len(value)+len(text) > 8192 {
			return
		}
		set := schemas(t)
		vars := Map{"A": value, "T": value, "TOKEN": value}
		s := New(Options{Schemas: set, Variables: vars})
		if root, err := parseJSON(doc, 0); err == nil && root.Kind == tree.KindMap {
			kind := v1alpha1.KindRoute
			x, _ := set.Index(v1)
			if k, ok := root.Get("kind"); ok && k.Kind == tree.KindString && slices.Contains(x.Kinds(), k.Text) {
				kind = v1alpha1.Kind(k.Text)
			}
			before := shape(root)
			r1 := &tree.Resource{ID: tree.ID{Kind: kind, Name: "x"}, APIVersion: v1, Root: root.Clone()}
			r2 := &tree.Resource{ID: r1.ID, APIVersion: v1, Root: root.Clone()}
			ds1, ds2 := s.Apply(r1), s.Apply(r2)
			if after := shape(r1.Root); after != before {
				t.Fatalf("shape changed:\n%s\n%s", before, after)
			}
			if dump(r1.Root) != dump(r2.Root) || !slices.Equal(texts(ds1), texts(ds2)) {
				t.Fatal("not deterministic")
			}
			for _, d := range ds1 {
				if _, ok := errcode.Lookup(d.Code); !ok {
					t.Fatalf("unregistered code %q", d.Code)
				}
			}
		}
		if strings.ContainsAny(text, "\"\\\x00") || !utf8.ValidString(text) {
			return
		}
		files := &tree.FileTable{}
		rs, err := docsErr(files, "r.yaml", prefixRoute(text))
		if err != nil || len(rs) != 1 {
			return
		}
		ds := s.Apply(rs[0])
		got := at(t, rs[0].Root, "spec.match.path.prefix")
		ref := reference(text, vars, false)
		want, ok, undefined, exprs := ref.out, ref.ok, ref.undefined, ref.exprs
		errs := 0
		for _, d := range ds {
			if d.Code != CodeWarning {
				errs++
			}
		}
		lenient := reference(text, vars, true)
		switch {
		case !ok:
			// One RZ-CFG-005 and one RZ-CFG-010 per well-formed use of an
			// undefined variable, read leniently.
			if g := codes(ds); !slices.Contains(g, CodeMalformed) || errs != 1+lenient.undefined || got.Text != text {
				t.Fatalf("malformed %q: %q (%d undefined uses read leniently), text %q", text, texts(ds), lenient.undefined, got.Text)
			}
		case undefined > 0:
			if errs != undefined || got.Text != text {
				t.Fatalf("%q: %d undefined, got %q", text, undefined, texts(ds))
			}
		default:
			if errs != 0 || got.Kind != tree.KindString || got.Text != want {
				t.Fatalf("%q with A=%q: got %q (%q), want %q", text, value, got.Text, texts(ds), want)
			}
		}
		// A secret-like name warns at each well-formed use, also in
		// malformed text read leniently, so a malformed scalar takes the
		// warning too.
		wantFree := lenient.secretUses
		if ok {
			wantFree = ref.secretUses
		}
		if (ok && ref.detected(s.detectors)) || (!ok && lenient.detected(s.detectors)) {
			wantFree++
		}
		if g := len(ds) - errs; g != wantFree {
			t.Fatalf("%q with A=%q: %d RZ-CFG-013 warnings, want %d: %q", text, value, g, wantFree, texts(ds))
		}
		for _, spot := range forbiddenSpots(text) {
			rs, err := docsErr(files, "f.yaml", spot[0])
			if err != nil || len(rs) != 1 {
				t.Fatalf("%s: %v", spot[0], err)
			}
			ds := s.Apply(rs[0])
			got := at(t, rs[0].Root, spot[1])
			if !ok || exprs > 0 {
				if g := codes(ds); !slices.Equal(g, []string{CodeForbidden}) || got.Text != text {
					t.Fatalf("%s holding %q: %q, text %q; want RZ-CFG-011 alone and the authored text", spot[1], text, texts(ds), got.Text)
				}
				continue
			}
			for _, d := range ds {
				if d.Code != CodeWarning {
					t.Fatalf("%s holding %q: %s", spot[1], text, d.AppendText(nil))
				}
			}
			if got.Kind != tree.KindString || got.Text != want {
				t.Fatalf("%s holding %q = %q, want the literal %q", spot[1], text, got.Text, want)
			}
		}
		if rs, err := docsErr(files, "h.yaml", headerSpot(text)); err == nil && len(rs) == 1 {
			warnings := 0
			for _, d := range s.Apply(rs[0]) {
				if d.Code == CodeWarning {
					warnings++
				}
			}
			wantWarnings := 0
			switch {
			case !ok:
				wantWarnings = lenient.secretUses
				if lenient.detected(s.detectors) {
					wantWarnings++
				}
			case undefined > 0:
				wantWarnings = ref.secretUses
				if ref.detected(s.detectors) {
					wantWarnings++
				}
			default:
				wantWarnings = ref.secretUses
				if ref.detected(s.detectors) || (want != "" && ref.literalHeader()) {
					wantWarnings++
				}
			}
			if warnings != wantWarnings {
				t.Fatalf("authorization value %q with A=%q: %d RZ-CFG-013 warnings, want %d", text, value, warnings, wantWarnings)
			}
		}
		rs, err = docsErr(files, "e.yaml", prefixRoute(Escape(text)))
		if err != nil || len(rs) != 1 {
			return
		}
		for _, d := range s.Apply(rs[0]) {
			if d.Code != CodeWarning {
				t.Fatalf("Escape(%q) = %q: %s", text, Escape(text), d.AppendText(nil))
			}
		}
		if g := at(t, rs[0].Root, "spec.match.path.prefix").Text; g != text {
			t.Fatalf("substituting Escape(%q) = %q", text, g)
		}
	})
}

// BenchmarkApply substitutes a Gateway-sized resource with expressions,
// escapes and plain strings (stage E's share of the load budget).
func BenchmarkApply(b *testing.B) {
	var ls []string
	for i := range 50 {
		n := strconv.Itoa(i)
		ls = append(ls, `{"name":"l`+n+`","protocol":"http","port":"${PORT:-80`+n+`}","hostnames":["a`+n+`.example","$${x}"]}`)
	}
	src := `{"apiVersion":"ruralz/v1alpha1","kind":"Gateway","metadata":{"name":"g","labels":{"env":"${ENV}"}},"spec":{"listeners":[` +
		strings.Join(ls, ",") + `],"telemetry":{"traceSampling":"${RATE}"}}}`
	files := &tree.FileTable{}
	rs := docs(b, files, "ruralz.yaml", src)
	s := New(Options{Schemas: schemas(b), Files: files, Variables: Map{"ENV": "prod", "RATE": "0.25"}})
	b.ReportAllocs()
	for range b.N {
		b.StopTimer()
		r := &tree.Resource{ID: rs[0].ID, APIVersion: v1, Root: rs[0].Root.Clone()}
		b.StartTimer()
		if ds := s.Apply(r); len(ds) != 0 {
			b.Fatal(texts(ds))
		}
	}
}

// referenceScan is the segment reading as the first implementation
// computed it: a byte loop that searches each default's } to the end of
// the string. It is quadratic on malformed input, and it is the reference
// FuzzScan compares the linear scanEach against.
func referenceScan(s string, lenient bool) ([]segment, string) {
	name := regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*`)
	var segs []segment
	var lit strings.Builder
	flush := func() {
		if lit.Len() > 0 {
			segs = append(segs, segment{lit: lit.String()})
			lit.Reset()
		}
	}
	for i := 0; i < len(s); {
		switch {
		case strings.HasPrefix(s[i:], "$${"):
			lit.WriteString("${")
			i += 3
		case strings.HasPrefix(s[i:], "${"):
			e, n, reason := referenceExpression(s[i+2:], len(name.FindString(s[i+2:])))
			if reason != "" && lenient {
				lit.WriteString("${")
				i += 2
				continue
			}
			if reason != "" {
				return nil, reason
			}
			flush()
			segs = append(segs, e)
			i += 2 + n
		default:
			lit.WriteByte(s[i])
			i++
		}
	}
	flush()
	return segs, ""
}

// referenceExpression parses s, the text after a ${ whose name is n
// bytes long.
func referenceExpression(s string, n int) (segment, int, string) {
	if n == 0 {
		if s == "" {
			return segment{}, 0, reasonUnterminated
		}
		return segment{}, 0, reasonName
	}
	e := segment{name: s[:n]}
	rest := s[n:]
	switch {
	case rest == "":
		return segment{}, 0, reasonUnterminated
	case rest[0] == '}':
		return e, n + 1, ""
	case strings.HasPrefix(rest, ":-"):
		end := strings.IndexByte(rest[2:], '}')
		if end < 0 {
			return segment{}, 0, reasonUnterminated
		}
		e.def, e.hasDef = rest[2:2+end], true
		if strings.Contains(e.def, "${") {
			return segment{}, 0, reasonNested
		}
		return e, n + 2 + end + 1, ""
	case rest == ":":
		return segment{}, 0, reasonUnterminated
	default:
		return segment{}, 0, reasonOperator
	}
}

// referenceRuns cuts referenceScan's lenient segments into the authored
// runs (eachRun): literal text and defaults, cut at each expression
// without default.
func referenceRuns(s string) []run {
	segs, _ := referenceScan(s, true)
	var out []run
	var b strings.Builder
	lead := true
	for _, sg := range segs {
		switch {
		case sg.name == "":
			b.WriteString(sg.lit)
		case sg.hasDef:
			b.WriteString(sg.def)
		default:
			if b.Len() > 0 {
				out = append(out, run{text: b.String(), lead: lead})
				b.Reset()
			}
			lead = false
		}
	}
	if b.Len() > 0 {
		out = append(out, run{text: b.String(), lead: lead})
	}
	return out
}

// scanSeeds are texts that exercise every branch of the scanner.
func scanSeeds() []string {
	return []string{
		"", "plain", "$", "$$", "$${", "$${A}", "$$${A}", "${A}", "${A:-}", "${A:-d}", "${A:-a$${b}", "${A:-${B}}",
		"${A:-", "${A:-${A:-", "${A:-${A:-}", "${1X}", "${X-d}", "${X:=d}", "${", "${A", "${A:", "a ${B} $${C} ${D:-e} $",
		"${A:-}}}", "}${A:-x}${", "${A}${B:-c}${", "x$y${z}$", "${A:-$}", "${A:-$$}", "${A:-{}", "${_}", "${a1_}",
	}
}

// FuzzScan is the differential property of the linear scanner, whose
// lenient re-read never searches to the end of the string at each ${: in
// both modes, for any text, scanEach's pieces joined as scan does equal
// referenceScan's segments and reason, and eachRun's runs equal the
// reference runs.
func FuzzScan(f *testing.F) {
	for _, s := range scanSeeds() {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if len(s) > 1<<14 {
			return
		}
		for _, lenient := range []bool{false, true} {
			got, gotBad := collect(s, lenient)
			want, wantBad := referenceScan(s, lenient)
			if gotBad != wantBad || !slices.Equal(got, want) {
				t.Fatalf("scan(%q, lenient %v) = %v %q, want %v %q", s, lenient, got, gotBad, want, wantBad)
			}
		}
		var got []run
		eachRun(s, nil, func(r run) bool {
			got = append(got, r)
			return true
		})
		if want := referenceRuns(s); !slices.Equal(got, want) {
			t.Fatalf("eachRun(%q) = %v, want %v", s, got, want)
		}
	})
}
