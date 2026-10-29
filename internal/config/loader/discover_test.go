// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package loader

import (
	"errors"
	"io/fs"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// TestLongNamesClipped covers the bound on authored text in diagnostics:
// a kind or name longer than 64 bytes is clipped, and one that holds a
// control character is quoted, so the text form stays one short line.
func TestLongNamesClipped(t *testing.T) {
	long := strings.Repeat("k", 100)
	m := mapFS(minimal())
	m["a.yaml"] = &fstest.MapFile{Data: []byte("apiVersion: ruralz/v1alpha1\nkind: " + long + "\nmetadata:\n  name: \"a\\tb\"\n")}
	got := texts(load(t, newLoader(t, Options{}), m).Diagnostics)
	want := `a.yaml:2:7 error RZ-CFG-007 ` + strings.Repeat("k", 64) + `.../"a\tb" kind: kind "` + strings.Repeat("k", 64) + `..." is not served by apiVersion "ruralz/v1alpha1"`
	if len(got) != 1 || got[0] != want {
		t.Errorf("diagnostics %q\nwant %q", got, want)
	}
}

// TestLoadMinimal covers the smallest Bundle: ruralz.yaml with one Gateway
// loads without a diagnostic.
func TestLoadMinimal(t *testing.T) {
	l := newLoader(t, Options{})
	r := load(t, l, mapFS(minimal()))
	wantClean(t, "minimal", r.Diagnostics)
	if got := ids(r.Resources); !slices.Equal(got, []string{"Gateway/edge"}) {
		t.Errorf("resources = %q, want the Gateway", got)
	}
	if r.Stopped || r.Overlay != "" || r.Environment != "" {
		t.Errorf("result = %+v", r)
	}
	if f := r.Files.File(r.Resources[0].Start.File); f.Path != RootFile || f.Role != tree.RoleBase {
		t.Errorf("file of the Gateway = %+v", f)
	}
}

// TestDiscoveryOrder covers stage A order: files are read in lexical byte
// order of their slash-separated root-relative paths, not in a per
// directory walk order ("a.yaml" before "a/b.yaml", since '.' < '/'), and
// documents keep their order within a file. The union starts with
// ruralz.yaml, the Bundle root.
func TestDiscoveryOrder(t *testing.T) {
	m := mapFS(minimal())
	m["a/b.yaml"] = &fstest.MapFile{Data: []byte(upstream("u-ab"))}
	m["a.yaml"] = &fstest.MapFile{Data: []byte(docs(upstream("u-a1"), upstream("u-a2")))}
	m["B.yaml"] = &fstest.MapFile{Data: []byte(upstream("u-upper"))}
	m["z/y/x.json"] = &fstest.MapFile{Data: []byte(`{"apiVersion":"ruralz/v1alpha1","kind":"Upstream","metadata":{"name":"u-json"},"spec":{"protocol":"http","endpoints":[{"address":"j:80"}]}}`)}
	m["routes/r.yml"] = &fstest.MapFile{Data: []byte(upstream("u-yml"))}
	r := load(t, newLoader(t, Options{}), m)
	wantClean(t, "order", r.Diagnostics)
	want := []string{"Gateway/edge", "Upstream/u-upper", "Upstream/u-a1", "Upstream/u-a2", "Upstream/u-ab", "Upstream/u-yml", "Upstream/u-json"}
	if got := ids(r.Resources); !slices.Equal(got, want) {
		t.Errorf("resources = %q\nwant %q", got, want)
	}
	var paths []string
	for _, f := range newBundle(t, m).Files() {
		paths = append(paths, f.Path)
	}
	if want := []string{"B.yaml", "a.yaml", "a/b.yaml", "routes/r.yml", RootFile, "z/y/x.json"}; !slices.Equal(paths, want) {
		t.Errorf("Files = %q, want %q", paths, want)
	}
}

// newBundle parses m.
func newBundle(t testing.TB, m fs.FS) *Bundle {
	t.Helper()
	b, err := newLoader(t, Options{}).Parse(t.Context(), Source{FS: m, Path: "."})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return b
}

// TestDiscoverySkips covers what stage A never reads: hidden paths at any
// depth, files without a configuration extension (case-sensitive), and
// overlays/ as a base directory. A broken file in any of them would
// otherwise be reported.
func TestDiscoverySkips(t *testing.T) {
	m := mapFS(minimal())
	for _, p := range []string{
		".hidden.yaml", ".github/workflows/ci.yaml", "routes/.draft.yaml", "routes/.d/x.yaml",
		"README.md", "routes/x.YAML", "routes/x.yaml.bak", "routes/x.txt", "routes/yaml",
		"overlays/prod/x.yaml",
	} {
		m[p] = &fstest.MapFile{Data: []byte("{ not yaml")}
	}
	b := newBundle(t, m)
	wantClean(t, "skips", b.Diagnostics())
	var base []string
	for _, f := range b.Files() {
		if f.Overlay == "" {
			base = append(base, f.Path)
		}
	}
	if !slices.Equal(base, []string{RootFile}) {
		t.Errorf("base files = %q, want only ruralz.yaml", base)
	}
	if got := b.Overlays(); !slices.Equal(got, []string{"prod"}) {
		t.Errorf("Overlays = %q, want [prod]", got)
	}
}

// TestNoRootFile covers a directory without ruralz.yaml: it is not a
// Bundle, RZ-CFG-016, and nothing else is read, so a broken file there is
// not reported.
func TestNoRootFile(t *testing.T) {
	l := newLoader(t, Options{})
	m := mapFS(map[string]string{"routes/x.yaml": "{ not yaml"})
	r := load(t, l, m)
	wantTexts(t, "no ruralz.yaml", r.Diagnostics,
		"ruralz.yaml error RZ-CFG-016: the directory holds no ruralz.yaml, so it is not a Bundle: ruralz.yaml is the Bundle root and declares its Gateway")
	if !r.Stopped || r.Resources != nil {
		t.Errorf("result = %+v, want a stopped load", r)
	}
	m = mapFS(map[string]string{"ruralz.yaml/x.yaml": gateway})
	wantTexts(t, "ruralz.yaml directory", load(t, l, m).Diagnostics,
		"ruralz.yaml error RZ-CFG-016: ruralz.yaml is a directory: it must be the file that declares the Bundle's Gateway")
}

// TestSymlinkRejected covers symbolic links: RZ-CFG-001 wherever discovery
// meets one, whatever its name, and its target is never read.
func TestSymlinkRejected(t *testing.T) {
	m := mapFS(minimal())
	m["shared/u.yaml"] = &fstest.MapFile{Data: []byte(upstream("orders"))}
	symlink(m, "routes/u.yaml", "../shared/u.yaml")
	symlink(m, "LICENSE", "../LICENSE")
	symlink(m, "linked", "shared")
	r := load(t, newLoader(t, Options{}), m)
	const msg = ": symbolic link: a Bundle holds only regular files and directories (Ruralz Control reads the Bundle from Git, where a link has no file content; commit the file itself)"
	wantTexts(t, "symlinks", r.Diagnostics,
		"LICENSE error RZ-CFG-001"+msg,
		"linked error RZ-CFG-001"+msg,
		"routes/u.yaml error RZ-CFG-001"+msg,
	)
	if got := ids(r.Resources); !slices.Equal(got, []string{"Gateway/edge", "Upstream/orders"}) {
		t.Errorf("resources = %q, want the Gateway and the one real Upstream", got)
	}
}

// TestIrregularRejected covers an entry that is neither a regular file
// nor a directory, such as a named pipe: RZ-CFG-001, never opened.
func TestIrregularRejected(t *testing.T) {
	m := mapFS(minimal())
	m["routes/pipe.yaml"] = &fstest.MapFile{Mode: fs.ModeNamedPipe | 0o600}
	wantTexts(t, "named pipe", load(t, newLoader(t, Options{}), m).Diagnostics,
		"routes/pipe.yaml error RZ-CFG-001: not a regular file or directory: a Bundle holds only regular files and directories")
}

// TestSubmoduleRejected covers a Git submodule: a directory below the root
// that holds .git is RZ-CFG-001 and none of its files is read; .git at the
// root is the repository itself.
func TestSubmoduleRejected(t *testing.T) {
	m := mapFS(minimal())
	m[".git/HEAD"] = &fstest.MapFile{Data: []byte("ref: refs/heads/main\n")}
	m["vendor/shared/.git"] = &fstest.MapFile{Data: []byte("gitdir: ../../.git/modules/shared\n")}
	m["vendor/shared/u.yaml"] = &fstest.MapFile{Data: []byte(upstream("shared"))}
	m["overlays/prod/sub/.git/HEAD"] = &fstest.MapFile{Data: []byte("x")}
	r := load(t, newLoader(t, Options{}), m)
	wantTexts(t, "submodule", r.Diagnostics,
		"vendor/shared error RZ-CFG-001: Git submodule (the directory holds .git): a Bundle holds only regular files and directories (Ruralz Control reads the Bundle from one Git tree, where a submodule has no files)")
	if got := ids(r.Resources); !slices.Equal(got, []string{"Gateway/edge"}) {
		t.Errorf("resources = %q, want only the Gateway", got)
	}
}

// TestLFSPointerRejected covers a Git LFS pointer in place of a file's
// content: RZ-CFG-001, not a parse error.
func TestLFSPointerRejected(t *testing.T) {
	m := mapFS(minimal())
	m["routes/big.yaml"] = &fstest.MapFile{Data: []byte("version https://git-lfs.github.com/spec/v1\noid sha256:4d7a214614ab2935c943f9e0ff69d22eadbb8f32b1258daaa5e2ca24d17e2393\nsize 12345\n")}
	m["routes/old.yaml"] = &fstest.MapFile{Data: []byte("version https://hawser.github.com/spec/v1\noid sha256:4d7a\nsize 1\n")}
	const msg = ": Git LFS pointer, not the file's content: a Bundle holds the content itself (fetch the content with git lfs pull, or commit the file without LFS)"
	wantTexts(t, "LFS", load(t, newLoader(t, Options{}), m).Diagnostics,
		"routes/big.yaml error RZ-CFG-001"+msg, "routes/old.yaml error RZ-CFG-001"+msg)
}

// TestSourceErrors covers sources that cannot be read: ErrSource, never a
// diagnostic.
func TestSourceErrors(t *testing.T) {
	l := newLoader(t, Options{})
	m := mapFS(minimal())
	m["pipe"] = &fstest.MapFile{Mode: fs.ModeNamedPipe}
	for name, src := range map[string]Source{
		"missing FS path": {FS: m, Path: "nope"},
		"irregular":       {FS: m, Path: "pipe"},
		"missing OS path": {Path: t.TempDir() + "/nope"},
		"empty OS path":   {},
	} {
		if _, err := l.Parse(t.Context(), src); !errors.Is(err, ErrSource) {
			t.Errorf("%s: Parse error = %v, want ErrSource", name, err)
		}
	}
}

// TestFileSource covers a one-file Bundle, such as a rendered Bundle: the
// file plays ruralz.yaml, whatever its name; diagnostics name it as
// given; a name without a configuration extension is read as YAML.
func TestFileSource(t *testing.T) {
	l := newLoader(t, Options{})
	m := fstest.MapFS{
		"out/rendered.yaml": {Data: []byte(docs(gateway, upstream("orders")))},
		"out/rendered":      {Data: []byte(docs(gateway, upstream("orders")))},
		"out/r.json":        {Data: []byte(`{"apiVersion":"ruralz/v1alpha1","kind":"Gateway","metadata":{"name":"edge"},"spec":{"listeners":[{"name":"http","protocol":"http","port":8080}]}}`)},
		"out/two.yaml":      {Data: []byte(docs(gateway, upstream("orders"), upstream("orders")))},
	}
	for _, p := range []string{"out/rendered.yaml", "out/rendered", "out/r.json"} {
		r, err := l.Load(t.Context(), Request{Source: Source{FS: m, Path: p}})
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		wantClean(t, p, r.Diagnostics)
		if r.Resources[0].ID.String() != "Gateway/edge" || r.Files.File(r.Resources[0].Start.File).Path != p {
			t.Errorf("%s: resources %q in %q", p, ids(r.Resources), r.Files.File(0).Path)
		}
	}
	r, err := l.Load(t.Context(), Request{Source: Source{FS: m, Path: "out/two.yaml"}})
	if err != nil {
		t.Fatal(err)
	}
	wantTexts(t, "duplicate in a file", r.Diagnostics,
		"out/two.yaml:23:9 error RZ-CFG-008 Upstream/orders metadata.name: Upstream/orders is declared twice: a resource lives in exactly one place (first declared in out/two.yaml:14:9)")
}

// TestOverlayDirectoryEntries covers what discovery takes from overlays/:
// each subdirectory with a valid overlay name is an overlay; hidden ones,
// invalid names and files directly in overlays/ are not read; a symbolic
// link there is the overlay's own RZ-CFG-001, reported when it is
// selected; a .git in overlays/ makes it a submodule of the base.
func TestOverlayDirectoryEntries(t *testing.T) {
	m := mapFS(minimal())
	broken := []byte("{")
	m["overlays/.hidden/x.yaml"] = &fstest.MapFile{Data: broken}
	m["overlays/bad name/x.yaml"] = &fstest.MapFile{Data: broken}
	m["overlays/common.yaml"] = &fstest.MapFile{Data: broken}
	m["overlays/prod/x.yaml"] = &fstest.MapFile{Data: []byte(upstream("p"))}
	m["shared/qa/x.yaml"] = &fstest.MapFile{Data: []byte(upstream("shared"))}
	symlink(m, "overlays/qa", "../shared/qa")
	b := newBundle(t, m)
	if got := b.Overlays(); !slices.Equal(got, []string{"prod", "qa"}) {
		t.Errorf("Overlays = %q", got)
	}
	wantClean(t, "Bundle", b.Diagnostics())
	file := docs(environment("prod", " {}"), environment("qa", "\n  overlay: qa"))
	l := newLoader(t, Options{})
	r := loadEnv(t, l, m, file, "prod")
	wantClean(t, "prod", r.Diagnostics)
	if got := ids(r.Resources); !slices.Contains(got, "Upstream/p") {
		t.Errorf("prod resources = %q", got)
	}
	r = loadEnv(t, l, m, file, "qa")
	wantTexts(t, "qa", r.Diagnostics,
		"overlays/qa error RZ-CFG-001: symbolic link: a Bundle holds only regular files and directories (Ruralz Control reads the Bundle from Git, where a link has no file content; commit the file itself)")

	m = mapFS(minimal())
	m["overlays/.git"] = &fstest.MapFile{Data: []byte("gitdir: x")}
	m["overlays/prod/x.yaml"] = &fstest.MapFile{Data: broken}
	b = newBundle(t, m)
	wantTexts(t, "overlays as a submodule", b.Diagnostics(),
		"overlays error RZ-CFG-001: Git submodule (the directory holds .git): a Bundle holds only regular files and directories (Ruralz Control reads the Bundle from one Git tree, where a submodule has no files)")
	if len(b.Overlays()) != 0 {
		t.Errorf("Overlays = %q", b.Overlays())
	}
}
