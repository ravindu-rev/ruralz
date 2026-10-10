// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package profile

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// The YAML Test Suite runner (01 test plan "Upstream suites", 11 req 18,
// architecture R-31): every case of the vendored data release goes through
// the restricted profile. A case with an error file must be rejected; any
// other case must parse without diagnostics to the documents of its
// test.event stream and, when it has one, its in.json. Only the cases in
// expected-failures.txt may fail, each with its reason, which its findings
// must show (reasonShown), and a listed case that passes fails the run, so
// the list only shrinks.

const (
	suiteDir     = "../../../test/fixtures/yaml-test-suite"
	failuresFile = "expected-failures.txt"
)

// suiteCase is one test of the suite: a directory holding in.yaml.
type suiteCase struct {
	id  string // "229Q" or "M2N8/00"
	dir string
}

func loadSuite(t *testing.T) []suiteCase {
	t.Helper()
	root := filepath.Join(suiteDir, "data")
	var cases []suiteCase
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || d.Name() != "in.yaml" {
			return nil
		}
		dir := filepath.Dir(path)
		rel, err := filepath.Rel(root, dir)
		if err != nil {
			return err
		}
		cases = append(cases, suiteCase{id: filepath.ToSlash(rel), dir: dir})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v (run scripts/update-test-suites.sh yaml)", root, err)
	}
	if len(cases) < 300 {
		t.Fatalf("found %d cases under %s, want the full data release", len(cases), root)
	}
	slices.SortFunc(cases, func(a, b suiteCase) int { return strings.Compare(a.id, b.id) })
	return cases
}

// loadExpectedFailures reads "<ID> <reason>" lines; # starts a comment.
func loadExpectedFailures(t *testing.T) map[string]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(suiteDir, failuresFile))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		id, reason, ok := strings.Cut(line, " ")
		reason = strings.TrimSpace(reason)
		if !ok || reason == "" {
			t.Fatalf("%s:%d: want \"<ID> <reason>\"", failuresFile, i+1)
		}
		if !validReason(reason) {
			t.Fatalf("%s:%d: reason %q is not profile:<feature> or reviewed:<goccy issue>", failuresFile, i+1, reason)
		}
		if _, dup := out[id]; dup {
			t.Fatalf("%s:%d: %s listed twice", failuresFile, i+1, id)
		}
		out[id] = reason
	}
	return out
}

// validReason reports the reasons of the 01 test plan "Upstream suites":
// a restriction of the profile, or a reviewed goccy limitation.
func validReason(r string) bool {
	switch {
	case r == "profile:anchor", r == "profile:tag", r == "profile:complex-key", r == "profile:directive":
		return true
	case strings.HasPrefix(r, "reviewed:"):
		return len(r) > len("reviewed:")
	}
	return false
}

// reasonShown reports whether the findings of a listed case show its
// reason, so a case that fails for another reason than the one listed
// fails the run (minor finding of the seventh WP-33 review): an
// RZ-CFG-003 for an anchor, an RZ-CFG-004 for a tag, the %YAML message for
// a directive, and a message about a key that is not a scalar for a
// complex key. A reviewed goccy limitation may show anything, an accepted
// invalid stream included.
func reasonShown(reason string, diags diag.List) bool {
	keyMessages := []string{
		msgExplicitKey, msgExplicitKeyLine,
		"syntax error: a mapping key must be a scalar",
		"syntax error: found an invalid key for this map",
	}
	return strings.HasPrefix(reason, "reviewed:") || slices.ContainsFunc(diags, func(d diag.Diagnostic) bool {
		switch reason {
		case "profile:anchor":
			return d.Code == codeAnchor
		case "profile:tag":
			return d.Code == codeTag
		case "profile:directive":
			return d.Code == codeParse && strings.HasPrefix(d.Message, "%YAML ")
		case "profile:complex-key":
			return d.Code == codeParse && slices.Contains(keyMessages, d.Message)
		default:
			return false
		}
	})
}

// TestYAMLTestSuite runs the suite with the ratchet (11 req 18).
func TestYAMLTestSuite(t *testing.T) {
	cases := loadSuite(t)
	expected := loadExpectedFailures(t)
	known := map[string]bool{}
	passed, failed := 0, 0
	for _, c := range cases {
		known[c.id] = true
		diags, err := runCase(c)
		reason, listed := expected[c.id]
		switch {
		case err == nil && listed:
			t.Errorf("%s passes but is listed in %s (%s): remove it", c.id, failuresFile, reason)
		case err != nil && !listed:
			t.Errorf("%s: %v", c.id, err)
			failed++
		case err == nil:
			passed++
		case !reasonShown(reason, diags):
			t.Errorf("%s is listed as %s, but its findings do not show it: %v", c.id, reason, err)
			failed++
		default:
			failed++
		}
	}
	for id := range expected {
		if !known[id] {
			t.Errorf("%s lists %s, which is not a case of the data release", failuresFile, id)
		}
	}
	t.Logf("YAML Test Suite: %d cases, %d pass, %d expected failures", len(cases), passed, failed)
}

// TestReasonShown checks the reason check of the runner (11 req 18): RZP5
// was listed as an anchor case while its findings show only its complex
// key, and that passed (minor finding of the seventh WP-33 review).
func TestReasonShown(t *testing.T) {
	d := func(code, msg string) diag.List {
		return diag.List{{Code: code, Severity: diag.SeverityError, Message: msg}}
	}
	tests := []struct {
		reason string
		diags  diag.List
		want   bool
	}{
		{"profile:anchor", d(codeAnchor, "anchors are not allowed (&a)"), true},
		{"profile:anchor", d(codeParse, msgExplicitKey), false},
		{"profile:complex-key", d(codeParse, msgExplicitKey), true},
		{"profile:complex-key", d(codeParse, "syntax error: a mapping key must be a scalar"), true},
		{"profile:complex-key", d(codeParse, msgEmptyKey), false},
		{"profile:tag", d(codeTag, "%TAG directives are not allowed"), true},
		{"profile:tag", d(codeAnchor, "anchors are not allowed (&a)"), false},
		{"profile:directive", d(codeParse, "%YAML 1.1 directive: only YAML 1.2 is accepted"), true},
		{"profile:directive", d(codeParse, msgEmptyKey), false},
		{"reviewed:goccy-v1.19.2/x", nil, true},
		{"profile:other", d(codeParse, msgEmptyKey), false},
	}
	for _, tt := range tests {
		if got := reasonShown(tt.reason, tt.diags); got != tt.want {
			t.Errorf("reasonShown(%q, %q) = %v, want %v", tt.reason, codes(tt.diags), got, tt.want)
		}
	}
	for r, want := range map[string]bool{"profile:anchor": true, "profile:implicit-key": false, "reviewed:goccy-v1.19.2/a": true, "reviewed:": false} {
		if got := validReason(r); got != want {
			t.Errorf("validReason(%q) = %v, want %v", r, got, want)
		}
	}
}

// TestReviewedFindings pins the first finding of every case listed as a
// reviewed goccy limitation (11 req 18; minor finding of the eighth WP-33
// review). reasonShown lets such a case show any finding, so a change that
// refused one of them for another reason, or accepted it, would pass the
// runner unseen; several are refused by the profile's own checks, which
// guard goccy's misreading of them. "" marks an invalid stream that goccy
// accepts and the profile cannot tell from a valid one.
func TestReviewedFindings(t *testing.T) {
	want := map[string]string{
		"2JQS":     "RZ-CFG-001@2:1 " + msgEmptyKey,
		"4MUZ/02":  "RZ-CFG-001@1:2 syntax error: map key definition includes an implicit line break",
		"9C9N":     "",
		"9JBA":     "",
		"9MMW":     "RZ-CFG-001@2:16 " + msgFlowSeqEntry,
		"CFD4":     "RZ-CFG-001@1:5 syntax error: found an invalid key for this map",
		"CVW2":     "",
		"DFF7":     "RZ-CFG-001@4:1 " + msgEmptyExplicitKey,
		"DK95/04":  "RZ-CFG-001@2:1 syntax error: found character '\t' that cannot start any token",
		"FH7J":     "RZ-CFG-001@1:3 a tagged node has no value",
		"FRK4":     "RZ-CFG-001@3:3 syntax error: found an invalid key for this map",
		"M7A3":     "RZ-CFG-001@6:1 " + msgBlockScalar,
		"NHX8":     "RZ-CFG-001@1:1 " + msgEmptyKey,
		"NKF9":     "RZ-CFG-001@3:1 " + msgEmptyKey,
		"QB6E":     "",
		"S3PD":     "RZ-CFG-001@2:1 " + msgEmptyKey,
		"SM9W/01":  "RZ-CFG-001@1:1 " + msgEmptyKey,
		"SU5Z":     "",
		"UKK6/00":  "RZ-CFG-001@1:3 " + msgEmptyKey,
		"VJP3/01":  "RZ-CFG-001@2:2 syntax error: map key definition includes an implicit line break",
		"Y79Y/003": "",
	}
	expected := loadExpectedFailures(t)
	for _, c := range loadSuite(t) {
		reason := expected[c.id]
		if !strings.HasPrefix(reason, "reviewed:") {
			continue
		}
		w, pinned := want[c.id]
		if !pinned {
			t.Errorf("%s is listed as %s, and its finding is not pinned here", c.id, reason)
			continue
		}
		delete(want, c.id)
		diags, _ := runCase(c)
		got := ""
		if len(diags) > 0 {
			got = fmt.Sprintf("%s@%d:%d %s", diags[0].Code, diags[0].Line, diags[0].Column, diags[0].Message)
		}
		if got != w {
			t.Errorf("%s: first finding %q, want %q", c.id, got, w)
		}
	}
	for id := range want {
		t.Errorf("%s is pinned here and not listed as reviewed in %s", id, failuresFile)
	}
}

// runCase returns the findings of a case, and nil when the profile agrees
// with the case.
func runCase(c suiteCase) (diag.List, error) {
	in, err := os.ReadFile(filepath.Join(c.dir, "in.yaml"))
	if err != nil {
		return nil, err
	}
	docs, diags, err := parse(context.Background(), in, 0, "in.yaml", FormatYAML, Options{}, true)
	if err != nil {
		return nil, err
	}
	return diags, compareCase(c, docs, diags)
}

// compareCase compares a case's parse with its files.
func compareCase(c suiteCase, docs []Document, diags diag.List) error {
	if _, err := os.Stat(filepath.Join(c.dir, "error")); err == nil {
		if len(diags) == 0 {
			return errors.New("accepted a stream the suite marks invalid")
		}
		return nil
	}
	if len(diags) > 0 {
		var b strings.Builder
		for _, d := range diags {
			fmt.Fprintf(&b, "%d:%d %s %s; ", d.Line, d.Column, d.Code, d.Message)
		}
		return errors.New("rejected a valid stream: " + b.String())
	}
	events, err := os.ReadFile(filepath.Join(c.dir, "test.event"))
	if err != nil {
		return err
	}
	want, err := eventDocuments(events)
	if err != nil {
		return fmt.Errorf("test.event: %w", err)
	}
	if len(want) != len(docs) {
		return fmt.Errorf("got %d documents, test.event has %d", len(docs), len(want))
	}
	for i := range want {
		if err := compareEvent(want[i], docs[i].Root, "$"); err != nil {
			return fmt.Errorf("document %d: %w", i, err)
		}
	}
	if j, err := os.ReadFile(filepath.Join(c.dir, "in.json")); err == nil {
		values, err := jsonDocuments(j)
		if err != nil {
			return fmt.Errorf("in.json: %w", err)
		}
		if len(values) != len(docs) {
			return fmt.Errorf("got %d documents, in.json has %d", len(docs), len(values))
		}
		for i := range values {
			if err := compareJSON(values[i], docs[i].Root.JSONValue(), "$"); err != nil {
				return fmt.Errorf("document %d against in.json: %w", i, err)
			}
		}
	}
	return nil
}

// eventNode is a node of a test.event stream.
type eventNode struct {
	kind  byte // 'M' map, 'S' sequence, 'V' scalar, 'A' alias
	style byte // ':', '\'', '"', '|', '>' for scalars
	tag   string
	text  string
	items []*eventNode // sequence items, or alternating map keys and values
}

// eventDocuments parses a test.event stream into its documents, leaving
// out null documents as Parse does.
func eventDocuments(data []byte) ([]*eventNode, error) {
	var docs []*eventNode
	var stack []*eventNode
	var root *eventNode
	add := func(n *eventNode) error {
		if len(stack) == 0 {
			if root != nil {
				return errors.New("two document roots")
			}
			root = n
			return nil
		}
		top := stack[len(stack)-1]
		top.items = append(top.items, n)
		return nil
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := sc.Text()
		head, rest, _ := strings.Cut(line, " ")
		switch head {
		case "+STR", "-STR":
		case "+DOC":
			root = nil
		case "-DOC":
			if root == nil {
				return nil, errors.New("document without a root")
			}
			if root.kind != 'V' || !isNullEvent(root) {
				docs = append(docs, root)
			}
		case "+MAP", "+SEQ":
			n := &eventNode{kind: head[1]}
			n.tag = eventTag(rest)
			if err := add(n); err != nil {
				return nil, err
			}
			stack = append(stack, n)
		case "-MAP", "-SEQ":
			if len(stack) == 0 {
				return nil, errors.New("unbalanced " + head)
			}
			stack = stack[:len(stack)-1]
		case "=VAL":
			n, err := eventScalar(rest)
			if err != nil {
				return nil, err
			}
			if err := add(n); err != nil {
				return nil, err
			}
		case "=ALI":
			if err := add(&eventNode{kind: 'A', text: rest}); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("unknown event %q", line)
		}
	}
	return docs, sc.Err()
}

// eventTag returns the core tag of a node's properties, written as
// "<tag:yaml.org,2002:str>", in the !!str form; other tags are returned
// verbatim.
func eventTag(props string) string {
	for _, f := range strings.Fields(props) {
		if strings.HasPrefix(f, "<") && strings.HasSuffix(f, ">") {
			tag := f[1 : len(f)-1]
			if name, ok := strings.CutPrefix(tag, "tag:yaml.org,2002:"); ok {
				return "!!" + name
			}
			return tag
		}
	}
	return ""
}

// eventScalar parses "[&anchor] [<tag>] <style><text>".
func eventScalar(rest string) (*eventNode, error) {
	n := &eventNode{kind: 'V'}
	for rest != "" && (rest[0] == '&' || rest[0] == '<') {
		var f string
		if rest[0] == '<' {
			end := strings.IndexByte(rest, '>')
			if end < 0 {
				return nil, errors.New("unterminated tag")
			}
			f, rest = rest[:end+1], strings.TrimPrefix(rest[end+1:], " ")
		} else {
			f, rest, _ = strings.Cut(rest, " ")
		}
		if t := eventTag(f); t != "" {
			n.tag = t
		}
	}
	if rest == "" {
		return nil, errors.New("scalar without style")
	}
	n.style = rest[0]
	n.text = unescapeEvent(rest[1:])
	return n, nil
}

// unescapeEvent decodes the escapes of test.event values.
func unescapeEvent(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 == len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case 'b':
			b.WriteByte('\b')
		case '0':
			b.WriteByte(0)
		case '\\':
			b.WriteByte('\\')
		default:
			b.WriteByte('\\')
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// isNullEvent reports a scalar the profile reads as null.
func isNullEvent(n *eventNode) bool {
	t := expectScalar(n)
	return t.code == "" && t.kind == tree.KindNull
}

// expectScalar types an event scalar as the profile must.
func expectScalar(n *eventNode) typed {
	switch {
	case n.tag != "":
		return resolveTagged(n.tag, n.text)
	case n.style == ':':
		return resolvePlain(n.text)
	default:
		return typed{kind: tree.KindString, text: n.text}
	}
}

// styleOf returns the tree style of a test.event scalar style.
func styleOf(c byte) tree.Style {
	switch c {
	case '\'':
		return tree.StyleSingleQuoted
	case '"':
		return tree.StyleDoubleQuoted
	case '|':
		return tree.StyleLiteral
	case '>':
		return tree.StyleFolded
	default:
		return tree.StylePlain
	}
}

// compareEvent compares a parsed node with its event node.
func compareEvent(want *eventNode, got *tree.Node, path string) error {
	switch want.kind {
	case 'A':
		return fmt.Errorf("%s: alias in a stream the profile accepted", path)
	case 'M':
		if got.Kind != tree.KindMap {
			return fmt.Errorf("%s: got kind %d, want a mapping", path, got.Kind)
		}
		if len(want.items) != 2*len(got.Members) {
			return fmt.Errorf("%s: got %d members, want %d", path, len(got.Members), len(want.items)/2)
		}
		for i, m := range got.Members {
			k := want.items[2*i]
			if k.kind != 'V' {
				return fmt.Errorf("%s: complex key in a stream the profile accepted", path)
			}
			if m.Key != k.text {
				return fmt.Errorf("%s: key %d is %q, want %q", path, i, m.Key, k.text)
			}
			if err := compareEvent(want.items[2*i+1], m.Value, path+"."+m.Key); err != nil {
				return err
			}
		}
		return nil
	case 'S':
		if got.Kind != tree.KindList {
			return fmt.Errorf("%s: got kind %d, want a sequence", path, got.Kind)
		}
		if len(want.items) != len(got.Items) {
			return fmt.Errorf("%s: got %d items, want %d", path, len(got.Items), len(want.items))
		}
		for i := range want.items {
			if err := compareEvent(want.items[i], got.Items[i], path+"["+strconv.Itoa(i)+"]"); err != nil {
				return err
			}
		}
		return nil
	}
	exp := expectScalar(want)
	if exp.code != "" {
		return fmt.Errorf("%s: accepted %q, which is %s", path, want.text, exp.code)
	}
	if got.Kind != exp.kind || got.Text != exp.text || got.Bool != exp.b {
		return fmt.Errorf("%s: got (%d %q %v), want (%d %q %v)", path, got.Kind, got.Text, got.Bool, exp.kind, exp.text, exp.b)
	}
	if st := styleOf(want.style); got.Style != st {
		return fmt.Errorf("%s: got style %d, want %d", path, got.Style, st)
	}
	return nil
}

// jsonDocuments decodes the concatenated JSON values of in.json, leaving
// out nulls (null documents) as Parse does.
func jsonDocuments(data []byte) ([]any, error) {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var out []any
	for {
		var v any
		err := d.Decode(&v)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if v != nil {
			out = append(out, v)
		}
	}
}

// compareJSON compares tree.Node.JSONValue output with an in.json value;
// numbers compare by exact value.
func compareJSON(want, got any, path string) error {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok || len(g) != len(w) {
			return fmt.Errorf("%s: got %v, want %v", path, got, want)
		}
		for k, v := range w {
			gv, ok := g[k]
			if !ok {
				return fmt.Errorf("%s: missing key %q", path, k)
			}
			if err := compareJSON(v, gv, path+"."+k); err != nil {
				return err
			}
		}
		return nil
	case []any:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			return fmt.Errorf("%s: got %v, want %v", path, got, want)
		}
		for i := range w {
			if err := compareJSON(w[i], g[i], path+"["+strconv.Itoa(i)+"]"); err != nil {
				return err
			}
		}
		return nil
	case json.Number:
		g, ok := got.(json.Number)
		if !ok {
			return fmt.Errorf("%s: got %v, want number %s", path, got, w)
		}
		a, okA := new(big.Rat).SetString(string(w))
		b, okB := new(big.Rat).SetString(string(g))
		if !okA || !okB || a.Cmp(b) != 0 {
			return fmt.Errorf("%s: got %s, want %s", path, g, w)
		}
		return nil
	default:
		if got != want {
			return fmt.Errorf("%s: got %#v, want %#v", path, got, want)
		}
		return nil
	}
}
