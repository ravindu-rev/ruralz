// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package schemaview

import (
	"fmt"
	"path"
	"strings"
	"testing"
)

// ignoreRule is one pattern of a .ruralzignore file (01 req 4).
type ignoreRule struct {
	// segments are the pattern's slash-separated parts; "**" stands for
	// any number of directories (one or more when it ends the pattern).
	segments []string
	negate   bool
	dirOnly  bool
}

// ignoreRules is a parsed .ruralzignore: the meta test applies it to
// examples/ as the loader's discovery does (WP-55 owns the loader's
// matcher; this one exists only so the meta test reads the files a Bundle
// holds).
type ignoreRules []ignoreRule

// parseIgnore parses .ruralzignore text with the 01 req 4 syntax: blank
// lines and # comments skipped (\# escapes), trailing unescaped spaces
// trimmed, ! negates (\! is literal), a / at the start or in the middle
// anchors to the root, a trailing / matches directories only, * and ?
// never match /, [...] classes ([!...] negated), and **/ leading, /**
// trailing and /**/ middle. It returns an error naming the line of an
// invalid pattern.
func parseIgnore(text string) (ignoreRules, error) {
	var rules ignoreRules
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSuffix(line, "\r")
		for strings.HasSuffix(line, " ") && !strings.HasSuffix(line, `\ `) {
			line = line[:len(line)-1]
		}
		if line == "" || line[0] == '#' {
			continue
		}
		var r ignoreRule
		switch {
		case line[0] == '!':
			r.negate, line = true, line[1:]
		case strings.HasPrefix(line, `\!`), strings.HasPrefix(line, `\#`):
			line = line[1:]
		}
		if strings.HasSuffix(line, "/") {
			r.dirOnly, line = true, strings.TrimSuffix(line, "/")
		}
		anchored := strings.Contains(line, "/")
		line = strings.TrimPrefix(line, "/")
		if line == "" {
			return nil, fmt.Errorf("line %d: empty pattern", i+1)
		}
		r.segments = strings.Split(line, "/")
		if !anchored && r.segments[0] != "**" {
			r.segments = append([]string{"**"}, r.segments...)
		}
		for j, seg := range r.segments {
			if seg == "**" {
				continue
			}
			// Other runs of asterisks are ordinary asterisks, and [!...]
			// negates a class as path.Match's [^...] does.
			for strings.Contains(seg, "**") {
				seg = strings.ReplaceAll(seg, "**", "*")
			}
			seg = strings.ReplaceAll(seg, "[!", "[^")
			if _, err := path.Match(seg, ""); err != nil {
				return nil, fmt.Errorf("line %d: %w", i+1, err)
			}
			r.segments[j] = seg
		}
		rules = append(rules, r)
	}
	return rules, nil
}

// matchSegments matches path segments against pattern segments, "**"
// matching any number of segments, or at least one at the end.
func matchSegments(pat, segs []string) bool {
	if len(pat) == 0 {
		return len(segs) == 0
	}
	if pat[0] == "**" {
		least := 0
		if len(pat) == 1 {
			least = 1
		}
		for k := least; k <= len(segs); k++ {
			if matchSegments(pat[1:], segs[k:]) {
				return true
			}
		}
		return false
	}
	if len(segs) == 0 {
		return false
	}
	ok, _ := path.Match(pat[0], segs[0])
	return ok && matchSegments(pat[1:], segs[1:])
}

// decide returns whether the last rule matching rel decides to ignore it.
func (rs ignoreRules) decide(rel string, dir bool) bool {
	segs := strings.Split(rel, "/")
	ignored := false
	for _, r := range rs {
		if (!r.dirOnly || dir) && matchSegments(r.segments, segs) {
			ignored = !r.negate
		}
	}
	return ignored
}

// ignored reports whether the root-relative file path rel is excluded:
// a directory above it is ignored (a path under an excluded directory
// cannot be re-included), or the last pattern matching it ignores it.
func (rs ignoreRules) ignored(rel string) bool {
	segs := strings.Split(rel, "/")
	for i := 1; i < len(segs); i++ {
		if rs.decide(strings.Join(segs[:i], "/"), true) {
			return true
		}
	}
	return rs.decide(rel, false)
}

// TestParseIgnore checks the meta test's .ruralzignore matcher against
// every rule of 01 req 4, so the files the meta test skips are the files
// a Bundle does not hold.
func TestParseIgnore(t *testing.T) {
	for _, tc := range []struct {
		name, rules string
		ignored     []string
		kept        []string
	}{
		{"blank lines and comments", "\n# notes/\n   \n", nil, []string{"notes/a.yaml", "# notes/a.yaml"}},
		{"escaped hash", `\#x.yaml`, []string{"#x.yaml"}, []string{"x.yaml"}},
		{"trailing spaces trimmed", "draft.yaml   ", []string{"draft.yaml"}, []string{"draft.yaml  "}},
		{"escaped trailing space", `draft.yaml\ `, []string{"draft.yaml "}, []string{"draft.yaml"}},
		{"unanchored name at any depth", "draft.yaml", []string{"draft.yaml", "a/b/draft.yaml"}, []string{"a/draft.yaml.bak"}},
		{"directory only", "notes/", []string{"notes/draft.yaml", "a/notes/x/y.json"}, []string{"notes", "a/notes.yaml"}},
		{"anchored by a leading slash", "/top.yaml", []string{"top.yaml"}, []string{"a/top.yaml"}},
		{"anchored by a middle slash", "a/b.yaml", []string{"a/b.yaml"}, []string{"x/a/b.yaml"}},
		{"star never matches a slash", "a/*.yaml", []string{"a/x.yaml"}, []string{"a/b/x.yaml"}},
		{"question mark", "?.json", []string{"a.json", "d/b.json"}, []string{"ab.json"}},
		{"class and negated class", "[ab].yaml\n[!c]x.yaml", []string{"a.yaml", "b.yaml", "dx.yaml"}, []string{"c.yaml", "cx.yaml"}},
		{"leading double star", "**/drafts/x.yaml", []string{"drafts/x.yaml", "a/b/drafts/x.yaml"}, []string{"a/drafts/y.yaml"}},
		{"trailing double star", "drafts/**", []string{"drafts/x.yaml", "drafts/a/b.json"}, []string{"drafts.yaml", "a/drafts/x.yaml"}},
		{"middle double star", "a/**/b.yaml", []string{"a/b.yaml", "a/x/b.yaml", "a/x/y/b.yaml"}, []string{"b.yaml", "x/a/b.yaml"}},
		{"other asterisks are ordinary", "a**b.yaml", []string{"ab.yaml", "axxb.yaml"}, []string{"a/b.yaml"}},
		{"negation re-includes", "*.yaml\n!keep.yaml", []string{"drop.yaml"}, []string{"keep.yaml", "d/keep.yaml"}},
		{"last match wins", "!keep.yaml\n*.yaml", []string{"keep.yaml"}, nil},
		{"escaped exclamation mark", `\!x.yaml`, []string{"!x.yaml"}, []string{"x.yaml"}},
		{"no re-include under an excluded directory", "notes/\n!notes/keep.yaml", []string{"notes/keep.yaml"}, nil},
		{"case-sensitive", "Draft.yaml", []string{"Draft.yaml"}, []string{"draft.yaml"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rules, err := parseIgnore(tc.rules)
			if err != nil {
				t.Fatalf("parseIgnore(%q) error = %v", tc.rules, err)
			}
			for _, p := range tc.ignored {
				if !rules.ignored(p) {
					t.Errorf("parseIgnore(%q).ignored(%q) = false, want true", tc.rules, p)
				}
			}
			for _, p := range tc.kept {
				if rules.ignored(p) {
					t.Errorf("parseIgnore(%q).ignored(%q) = true, want false", tc.rules, p)
				}
			}
		})
	}
	for _, bad := range []string{"a/[b.yaml", "/", "!"} {
		if _, err := parseIgnore("ok.yaml\n" + bad); err == nil || !strings.Contains(err.Error(), "line 2") {
			t.Errorf("parseIgnore(%q) error = %v, want one at line 2", bad, err)
		}
	}
}
