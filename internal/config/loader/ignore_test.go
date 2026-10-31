// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package loader

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
)

// TestIgnoreMatching covers the gitignore syntax of .ruralzignore, one
// rule of the syntax per case: each pattern against paths it must and must
// not exclude.
func TestIgnoreMatching(t *testing.T) {
	type path struct {
		p   string
		dir bool
	}
	f := func(p string) path { return path{p: p} }
	d := func(p string) path { return path{p: p, dir: true} }
	for _, c := range []struct {
		name      string
		file      string
		match, no []path
	}{
		{"basename at any depth", "*.md", []path{f("README.md"), f("docs/a.md"), d("x.md")}, []path{f("a.mdx"), f("md")}},
		{"directory only", "build/", []path{d("build"), d("x/build")}, []path{f("build"), f("build.yaml")}},
		{"leading slash anchors", "/root.yaml", []path{f("root.yaml")}, []path{f("x/root.yaml")}},
		{"middle slash anchors", "docs/*.yaml", []path{f("docs/a.yaml")}, []path{f("x/docs/a.yaml"), f("docs/x/a.yaml")}},
		{"leading **/", "**/foo", []path{f("foo"), f("a/foo"), d("a/b/foo")}, []path{f("foox"), f("a/xfoo")}},
		{"leading **/ with a slash", "**/foo/bar", []path{f("foo/bar"), f("x/y/foo/bar")}, []path{f("bar"), f("foo/x/bar")}},
		{"trailing /**", "abc/**", []path{f("abc/x"), d("abc/x/y")}, []path{d("abc"), f("x/abc/y")}},
		{"middle /**/", "a/**/b", []path{f("a/b"), f("a/x/b"), f("a/x/y/b")}, []path{f("a/xb"), f("x/a/b")}},
		{"** alone", "**", []path{f("a"), d("a/b")}, nil},
		{"other runs of * are one *", "a**b", []path{f("ab"), f("axyb"), f("d/axb")}, []path{f("ax/yb")}},
		{"question mark", "?.yaml", []path{f("a.yaml"), f("é.yaml")}, []path{f("ab.yaml"), f(".yaml")}},
		{"set", "[ab].yaml", []path{f("a.yaml"), f("b.yaml")}, []path{f("c.yaml")}},
		{"negated set with !", "[!ab].yaml", []path{f("c.yaml")}, []path{f("a.yaml")}},
		{"negated set with ^", "[^ab].yaml", []path{f("c.yaml")}, []path{f("b.yaml")}},
		{"range", "[a-c]x", []path{f("bx")}, []path{f("dx")}},
		{"class", "[[:digit:]]*", []path{f("1abc")}, []path{f("abc")}},
		{"] first in a set", "[]a]", []path{f("]"), f("a")}, []path{f("b")}},
		{"- last in a set", "[a-]", []path{f("-"), f("a")}, []path{f("b")}},
		{"escape in a set", `[\]]x`, []path{f("]x")}, []path{f(`\x`)}},
		{"a set never matches /", "a[/]b", nil, []path{f("a/b")}},
		{"a negated set never matches /", "x/a[^c]b", []path{f("x/adb")}, []path{f("x/a/b")}},
		{"a range never matches /", "a[+-1]b", []path{f("a+b"), f("a0b")}, []path{f("a/b")}},
		{"inverted range matches nothing", "[z-a]", nil, []path{f("m"), f("z")}},
		{"unknown class matches nothing", "[[:bogus:]]", nil, []path{f("b")}},
		{"unclosed set matches nothing", "[abc", nil, []path{f("[abc"), f("a")}},
		{"escaped #", `\#lit`, []path{f("#lit")}, nil},
		{"escaped !", `\!x`, []path{f("!x")}, []path{f("x")}},
		{"escaped trailing space", `foo\ `, []path{f("foo ")}, []path{f("foo")}},
		{"trailing spaces dropped", "foo   ", []path{f("foo")}, []path{f("foo ")}},
		{"escaped *", `a\*`, []path{f("a*")}, []path{f("ab")}},
		{"trailing backslash matches nothing", `foo\`, nil, []path{f("foo"), f(`foo\`)}},
		{"case-sensitive", "*.YAML", []path{f("a.YAML")}, []path{f("a.yaml")}},
		{"comments and blank lines", "# *.yaml\n\n   \n", nil, []path{f("a.yaml"), f("   "), f("# *.yaml")}},
		{"CR LF lines and a BOM", "\xef\xbb\xbf*.md\r\n*.txt\r\n", []path{f("a.md"), f("a.txt")}, []path{f("a.md\r")}},
		{"negation re-includes", "*.yaml\n!keep.yaml", []path{f("a.yaml")}, []path{f("keep.yaml"), f("x/keep.yaml")}},
		{"the last match wins", "!keep.yaml\n*.yaml", []path{f("keep.yaml")}, nil},
		{"lone / and ! are no patterns", "/\n!\n", nil, []path{f("a"), d("a")}},
	} {
		rules, _, d := parseIgnore([]byte(c.file), IgnoreFile, 0, DefaultMaxIgnorePatterns)
		if d != nil {
			t.Fatalf("%s: %s", c.name, d.AppendText(nil))
		}
		for _, p := range c.match {
			if !rules.ignored(p.p, p.dir) {
				t.Errorf("%s: %q does not exclude %q (dir %v)", c.name, c.file, p.p, p.dir)
			}
		}
		for _, p := range c.no {
			if rules.ignored(p.p, p.dir) {
				t.Errorf("%s: %q excludes %q (dir %v)", c.name, c.file, p.p, p.dir)
			}
		}
	}
}

// TestIgnoreLimits covers the .ruralzignore limits: more than
// MaxIgnorePatterns patterns, or a pattern longer than 4,096 bytes, is
// RZ-CFG-001 at its line and stops the load. Comments and blank lines are
// not patterns.
func TestIgnoreLimits(t *testing.T) {
	l := newLoader(t, Options{Limits: Limits{MaxIgnorePatterns: 3}})
	m := mapFS(minimal())
	m[IgnoreFile] = &fstest.MapFile{Data: []byte("# header\na\n\nb\nc\n")}
	wantClean(t, "three patterns", load(t, l, m).Diagnostics)
	m[IgnoreFile] = &fstest.MapFile{Data: []byte("# header\na\n\nb\nc\nd\n")}
	r := load(t, l, m)
	wantTexts(t, "four patterns", r.Diagnostics, ".ruralzignore:6:1 error RZ-CFG-001: the Bundle's .ruralzignore files hold more than 3 patterns")
	if !r.Stopped {
		t.Error("the load did not stop")
	}
	m[IgnoreFile] = &fstest.MapFile{Data: []byte("a\n" + strings.Repeat("x", maxIgnorePatternBytes+1) + "\n")}
	wantTexts(t, "long pattern", load(t, l, m).Diagnostics, ".ruralzignore:2:1 error RZ-CFG-001: pattern is longer than 4096 bytes")
	m[IgnoreFile] = &fstest.MapFile{Data: []byte(strings.Repeat("x", maxIgnorePatternBytes) + "\n")}
	wantClean(t, "pattern at the length limit", load(t, l, m).Diagnostics)
}

// TestIgnoreFile covers .ruralzignore in a Bundle: an excluded directory
// is never read, nor re-included; a re-included file outside it is read;
// it applies to overlays too; it never applies to hidden paths, which are
// always skipped.
func TestIgnoreFile(t *testing.T) {
	m := mapFS(minimal())
	m[IgnoreFile] = &fstest.MapFile{Data: []byte("node_modules/\n*.json\n!keep.json\ndrafts/**\n!drafts/keep.yaml\n!node_modules/keep.yaml\n!.hidden.yaml\noverlays/staging/\n")}
	broken := []byte("{ not yaml")
	for _, p := range []string{"node_modules/x.yaml", "node_modules/keep.yaml", "a.json", "drafts/x.yaml", ".hidden.yaml", "overlays/staging/x.yaml"} {
		m[p] = &fstest.MapFile{Data: broken}
	}
	m["keep.json"] = &fstest.MapFile{Data: []byte(`{"apiVersion":"ruralz/v1alpha1","kind":"Upstream","metadata":{"name":"kept"},"spec":{"protocol":"http","endpoints":[{"address":"k:80"}]}}`)}
	m["drafts/keep.yaml"] = &fstest.MapFile{Data: []byte(upstream("draft"))}
	m["overlays/prod/x.yaml"] = &fstest.MapFile{Data: []byte(upstream("draft"))}
	b := newBundle(t, m)
	wantClean(t, "ignore", b.Diagnostics())
	var paths []string
	for _, f := range b.Files() {
		paths = append(paths, f.Path)
	}
	if want := []string{"drafts/keep.yaml", "keep.json", "overlays/prod/x.yaml", RootFile}; !slices.Equal(paths, want) {
		t.Errorf("Files = %q, want %q", paths, want)
	}
	if got := b.Overlays(); !slices.Equal(got, []string{"prod"}) {
		t.Errorf("Overlays = %q, want the overlay not excluded", got)
	}
}

// TestIgnoreExcludesRoot covers a .ruralzignore that excludes ruralz.yaml:
// the Bundle then has no root, RZ-CFG-016, which names the cause.
func TestIgnoreExcludesRoot(t *testing.T) {
	m := mapFS(minimal())
	m[IgnoreFile] = &fstest.MapFile{Data: []byte("*.yaml\n")}
	wantTexts(t, "root excluded", load(t, newLoader(t, Options{}), m).Diagnostics,
		".ruralzignore error RZ-CFG-016: .ruralzignore excludes ruralz.yaml, the Bundle root that declares the Gateway")
}

// TestIgnoreFileNotRegular covers a .ruralzignore that is a symbolic link:
// RZ-CFG-001, and the load goes on without rules.
func TestIgnoreFileNotRegular(t *testing.T) {
	m := mapFS(minimal())
	m["other"] = &fstest.MapFile{Data: []byte("*.yaml\n")}
	symlink(m, IgnoreFile, "other")
	r := load(t, newLoader(t, Options{}), m)
	if got := codes(r.Diagnostics); !slices.Equal(got, []string{CodeLimit}) || r.Diagnostics[0].File != IgnoreFile {
		t.Errorf("diagnostics = %q", texts(r.Diagnostics))
	}
	if len(r.Resources) != 1 {
		t.Errorf("resources = %q, want ruralz.yaml read without rules", ids(r.Resources))
	}
}

// TestIgnorePatternCountsOnlyPatterns checks the limit message counts
// patterns, not lines.
func TestIgnorePatternCountsOnlyPatterns(t *testing.T) {
	var b strings.Builder
	for i := range 5 {
		b.WriteString("# comment " + strconv.Itoa(i) + "\n\np" + strconv.Itoa(i) + "\n")
	}
	if _, _, d := parseIgnore([]byte(b.String()), IgnoreFile, 0, 5); d != nil {
		t.Errorf("five patterns among comments: %s", d.AppendText(nil))
	}
	if _, _, d := parseIgnore([]byte(b.String()), IgnoreFile, 0, 4); d == nil || d.Line != 15 {
		t.Errorf("limit 4: %+v, want RZ-CFG-001 at line 15", d)
	}
}

// TestNestedIgnoreFiles covers .ruralzignore files below the root, with
// Git's semantics: a file's patterns apply below its own directory and
// match paths relative to it, anchored patterns from that directory; a
// deeper file's patterns override a shallower file's.
func TestNestedIgnoreFiles(t *testing.T) {
	m := mapFS(minimal())
	broken := []byte("{")
	m[IgnoreFile] = &fstest.MapFile{Data: []byte("*.json\nskip-*.yaml\n")}
	m["routes/"+IgnoreFile] = &fstest.MapFile{Data: []byte("!keep.json\n!skip-keep.yaml\ndrafts/\n/local.yaml\n")}
	m["routes/sub/"+IgnoreFile] = &fstest.MapFile{Data: []byte("/x.yaml\n")}
	m["a.json"] = &fstest.MapFile{Data: broken}
	m["skip-keep.yaml"] = &fstest.MapFile{Data: broken}
	m["routes/keep.json"] = &fstest.MapFile{Data: []byte(`{"apiVersion":"ruralz/v1alpha1","kind":"Upstream","metadata":{"name":"kept"},"spec":{"protocol":"http","endpoints":[{"address":"k:80"}]}}`)}
	m["routes/skip-keep.yaml"] = &fstest.MapFile{Data: []byte(upstream("deeper-wins"))}
	m["routes/drafts/x.yaml"] = &fstest.MapFile{Data: broken}
	m["routes/sub/drafts/x.yaml"] = &fstest.MapFile{Data: broken}
	m["drafts/x.yaml"] = &fstest.MapFile{Data: []byte(upstream("outside-routes"))}
	m["routes/local.yaml"] = &fstest.MapFile{Data: broken}
	m["routes/sub/local.yaml"] = &fstest.MapFile{Data: []byte(upstream("anchored-to-routes"))}
	m["routes/sub/x.yaml"] = &fstest.MapFile{Data: broken}
	m["routes/sub/deeper/x.yaml"] = &fstest.MapFile{Data: []byte(upstream("anchored-to-sub"))}
	b := newBundle(t, m)
	wantClean(t, "nested", b.Diagnostics())
	var paths []string
	for _, f := range b.Files() {
		paths = append(paths, f.Path)
	}
	want := []string{"drafts/x.yaml", "routes/keep.json", "routes/skip-keep.yaml", "routes/sub/deeper/x.yaml", "routes/sub/local.yaml", RootFile}
	if !slices.Equal(paths, want) {
		t.Errorf("Files = %q\nwant %q", paths, want)
	}
}

// TestNestedIgnoreInExcludedDirectory covers Git's rule that an excluded
// directory is not read: its .ruralzignore never re-includes a file, and a
// broken one there is never reported.
func TestNestedIgnoreInExcludedDirectory(t *testing.T) {
	m := mapFS(minimal())
	m[IgnoreFile] = &fstest.MapFile{Data: []byte("vendor/\n")}
	m["vendor/"+IgnoreFile] = &fstest.MapFile{Data: []byte("!*.yaml\n")}
	m["vendor/x.yaml"] = &fstest.MapFile{Data: []byte("{")}
	m["other/x.yaml"] = &fstest.MapFile{Data: []byte(upstream("other"))}
	symlink(m, "other/vendor/"+IgnoreFile, "nowhere")
	m[IgnoreFile] = &fstest.MapFile{Data: []byte("vendor/\n")}
	b := newBundle(t, m)
	wantClean(t, "excluded directory", b.Diagnostics())
	if got := b.Files(); len(got) != 2 {
		t.Errorf("Files = %+v, want other/x.yaml and ruralz.yaml", got)
	}
}

// TestIgnorePatternsAcrossFiles covers the pattern limit across every
// .ruralzignore of a load: the first pattern past it, in any file, is
// RZ-CFG-001 at its line, and the load stops.
func TestIgnorePatternsAcrossFiles(t *testing.T) {
	l := newLoader(t, Options{Limits: Limits{MaxIgnorePatterns: 3}})
	m := mapFS(minimal())
	m[IgnoreFile] = &fstest.MapFile{Data: []byte("a\nb\n")}
	m["routes/"+IgnoreFile] = &fstest.MapFile{Data: []byte("# c\nc\n")}
	wantClean(t, "three patterns in two files", load(t, l, m).Diagnostics)
	m["routes/sub/"+IgnoreFile] = &fstest.MapFile{Data: []byte("\nd\n")}
	r := load(t, l, m)
	wantTexts(t, "four patterns in three files", r.Diagnostics,
		"routes/sub/.ruralzignore:2:1 error RZ-CFG-001: the Bundle's .ruralzignore files hold more than 3 patterns")
	if !r.Stopped {
		t.Error("the load did not stop")
	}
}

// TestNestedIgnoreBytesCounted covers the source budget: every
// .ruralzignore read counts toward it, before the configuration files.
func TestNestedIgnoreBytesCounted(t *testing.T) {
	m := mapFS(minimal())
	m["routes/"+IgnoreFile] = &fstest.MapFile{Data: []byte("# " + strings.Repeat("x", 100) + "\n")}
	limit := int64(len(gateway) + 100)
	r := load(t, newLoader(t, Options{Limits: Limits{MaxSourceBytes: limit}}), m)
	if got := codes(r.Diagnostics); !slices.Equal(got, []string{CodeLimit}) || r.Diagnostics[0].File != RootFile {
		t.Errorf("diagnostics %q, want the source limit in ruralz.yaml after the ignore file's bytes", texts(r.Diagnostics))
	}
	r = load(t, newLoader(t, Options{Limits: Limits{MaxSourceBytes: 50}}), m)
	wantTexts(t, "ignore file past the limit", r.Diagnostics, "routes/.ruralzignore:1:51 error RZ-CFG-001: the source text passes the limit of 50 bytes")
}

// TestNestedIgnoreInOverlays covers .ruralzignore files in overlays/:
// overlays/.ruralzignore applies to every overlay, overlays/<name>/'s to
// that overlay's files only.
func TestNestedIgnoreInOverlays(t *testing.T) {
	m := mapFS(minimal())
	m["overlays/"+IgnoreFile] = &fstest.MapFile{Data: []byte("*.json\nqa/\n")}
	m["overlays/prod/"+IgnoreFile] = &fstest.MapFile{Data: []byte("draft.yaml\n")}
	m["overlays/prod/draft.yaml"] = &fstest.MapFile{Data: []byte("{")}
	m["overlays/prod/x.json"] = &fstest.MapFile{Data: []byte("{")}
	m["overlays/prod/ok.yaml"] = &fstest.MapFile{Data: []byte(upstream("p"))}
	m["overlays/staging/draft.yaml"] = &fstest.MapFile{Data: []byte(upstream("s"))}
	m["overlays/qa/x.yaml"] = &fstest.MapFile{Data: []byte("{")}
	b := newBundle(t, m)
	var paths []string
	for _, f := range b.Files() {
		paths = append(paths, f.Path)
	}
	if want := []string{"overlays/prod/ok.yaml", "overlays/staging/draft.yaml", RootFile}; !slices.Equal(paths, want) {
		t.Errorf("Files = %q\nwant %q", paths, want)
	}
	if got := b.Overlays(); !slices.Equal(got, []string{"prod", "staging"}) {
		t.Errorf("Overlays = %q", got)
	}
}

// TestNestedIgnoreNotRegular covers a nested .ruralzignore that is a
// symbolic link or a directory: RZ-CFG-001 at it, no rule from it, and no
// document lost, so overlay additions still apply.
func TestNestedIgnoreNotRegular(t *testing.T) {
	m := overlayBundle()
	m["routes/real"] = &fstest.MapFile{Data: []byte("*.yaml\n")}
	symlink(m, "routes/"+IgnoreFile, "real")
	m["policies/"+IgnoreFile+"/x"] = &fstest.MapFile{Data: []byte("x")}
	m["routes/r.yaml"] = &fstest.MapFile{Data: []byte(route("r"))}
	m["overlays/prod/new.yaml"] = &fstest.MapFile{Data: []byte(upstream("added"))}
	r := loadEnv(t, newLoader(t, Options{}), m, envs, "prod")
	wantTexts(t, "not regular", r.Diagnostics,
		"policies/.ruralzignore error RZ-CFG-001: directory: .ruralzignore must be a regular file of patterns",
		"routes/.ruralzignore error RZ-CFG-001: symbolic link: a Bundle holds only regular files and directories (Ruralz Control reads the Bundle from Git, where a link has no file content; commit the file itself)",
	)
	if got := ids(r.Resources); !slices.Contains(got, "Route/r") || !slices.Contains(got, "Upstream/added") {
		t.Errorf("resources = %q, want the Route read without rules and the overlay addition kept", got)
	}
}
