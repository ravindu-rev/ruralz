// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/errcode"
)

const modulePath = "github.com/ravindu-rev/ruralz"

// finding is one check failure at a repository-relative path.
type finding struct {
	path string
	line int
	msg  string
}

func (f finding) String() string {
	if f.line > 0 {
		return fmt.Sprintf("%s:%d: %s", f.path, f.line, f.msg)
	}
	return fmt.Sprintf("%s: %s", f.path, f.msg)
}

// repo is the file set the checks read.
type repo struct {
	root string
	// files are slash-separated paths relative to root, sorted.
	files []string
}

// skipDir reports directories never scanned.
func skipDir(rel string) bool {
	base := path.Base(rel)
	return base == ".git" || base == "node_modules" || base == "testdata" || base == "bin" || base == "dist" ||
		(strings.HasPrefix(base, ".") && rel != ".")
}

func loadRepo(root string) (*repo, error) {
	r := &repo{root: root}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if skipDir(rel) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() {
			r.files = append(r.files, rel)
		}
		return nil
	})
	slices.Sort(r.files)
	return r, err
}

func (r *repo) read(rel string) ([]byte, error) {
	return os.ReadFile(filepath.Join(r.root, filepath.FromSlash(rel)))
}

func (r *repo) goFiles() []string {
	var out []string
	for _, f := range r.files {
		if strings.HasSuffix(f, ".go") {
			out = append(out, f)
		}
	}
	return out
}

func run(root, allowlist string) ([]finding, error) {
	r, err := loadRepo(root)
	if err != nil {
		return nil, err
	}
	allow, err := loadAllowlist(filepath.Join(root, allowlist))
	if err != nil {
		return nil, err
	}
	var out []finding
	for _, check := range []func(*repo) ([]finding, error){
		checkGo,
		checkLicenseHeaders,
		func(r *repo) ([]finding, error) { return checkNoLicense(r, allow) },
	} {
		f, err := check(r)
		if err != nil {
			return nil, err
		}
		out = append(out, f...)
	}
	return out, nil
}

// checkGo runs the checks that parse Go files.
func checkGo(r *repo) ([]finding, error) {
	var out []finding
	fset := token.NewFileSet()
	cmdFiles := map[string][]string{}
	for _, rel := range r.goFiles() {
		src, err := r.read(rel)
		if err != nil {
			return nil, err
		}
		f, err := parser.ParseFile(fset, rel, src, parser.SkipObjectResolution)
		if err != nil {
			out = append(out, finding{path: rel, msg: "does not parse: " + err.Error()})
			continue
		}
		out = append(out, checkImports(fset, rel, f)...)
		if !strings.HasPrefix(rel, "internal/errcode/") {
			out = append(out, checkCodes(fset, rel, f)...)
		}
		if parts := strings.Split(rel, "/"); len(parts) >= 3 && parts[0] == "cmd" {
			dir := strings.Join(parts[:2], "/")
			cmdFiles[dir] = append(cmdFiles[dir], rel)
			out = append(out, checkWiring(fset, rel, f)...)
		}
	}
	for dir, files := range cmdFiles {
		if len(files) != 1 || files[0] != dir+"/main.go" {
			out = append(out, finding{path: dir, msg: "cmd/<binary> holds only main.go; move logic under internal/"})
		}
	}
	slices.SortFunc(out, func(a, b finding) int { return strings.Compare(a.String(), b.String()) })
	return out, nil
}

// checkImports forbids cgo, and third-party imports in public packages.
func checkImports(fset *token.FileSet, rel string, f *ast.File) []finding {
	var out []finding
	public := (strings.HasPrefix(rel, "pkg/") || strings.HasPrefix(rel, "api/schema/")) && !strings.HasSuffix(rel, "_test.go")
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		line := fset.Position(imp.Pos()).Line
		if p == "C" {
			out = append(out, finding{path: rel, line: line, msg: `import "C" is forbidden: binaries build with CGO_ENABLED=0 (ADR-0001)`})
		}
		if public && !isStdlib(p) && p != modulePath+"/pkg" && !strings.HasPrefix(p, modulePath+"/pkg/") {
			out = append(out, finding{path: rel, line: line, msg: fmt.Sprintf("public package imports %q; pkg/ and api/schema may import only the standard library and pkg/", p)})
		}
	}
	return out
}

// isStdlib reports whether an import path belongs to the standard library,
// whose first element has no dot.
func isStdlib(p string) bool {
	first, _, _ := strings.Cut(p, "/")
	return !strings.Contains(first, ".")
}

// checkCodes requires every RZ code in a string literal to be registered.
func checkCodes(fset *token.FileSet, rel string, f *ast.File) []finding {
	re := regexp.MustCompile(errcode.Pattern)
	var out []finding
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		for _, id := range re.FindAllString(lit.Value, -1) {
			if _, ok := errcode.Lookup(id); !ok {
				out = append(out, finding{
					path: rel, line: fset.Position(lit.Pos()).Line,
					msg: id + " is not registered in internal/errcode",
				})
			}
		}
		return true
	})
	return out
}

// checkWiring allows only imports and func main in a cmd/ main.go.
func checkWiring(fset *token.FileSet, rel string, f *ast.File) []finding {
	var out []finding
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.GenDecl:
			if d.Tok != token.IMPORT {
				out = append(out, finding{
					path: rel, line: fset.Position(d.Pos()).Line,
					msg: "cmd/ holds wiring only: move declarations under internal/",
				})
			}
		case *ast.FuncDecl:
			if d.Name.Name != "main" || d.Recv != nil {
				out = append(out, finding{
					path: rel, line: fset.Position(d.Pos()).Line,
					msg: "cmd/ holds wiring only: only func main is allowed",
				})
			}
		}
	}
	return out
}

// checkLicenseHeaders covers the files goheader does not: proto and TypeScript.
func checkLicenseHeaders(r *repo) ([]finding, error) {
	copyright := regexp.MustCompile(`Copyright [0-9]{4} \S`)
	var out []finding
	for _, rel := range r.files {
		switch path.Ext(rel) {
		case ".proto", ".ts", ".tsx", ".mts", ".cts":
		default:
			continue
		}
		data, err := r.read(rel)
		if err != nil {
			return nil, err
		}
		head := string(data[:min(len(data), 512)])
		if strings.HasPrefix(head, "// Code generated ") {
			continue
		}
		if !copyright.MatchString(head) || !strings.Contains(head, "SPDX-License-Identifier: Apache-2.0") {
			out = append(out, finding{
				path: rel, line: 1,
				msg: "missing license header: Copyright <year> <holder> and SPDX-License-Identifier: Apache-2.0",
			})
		}
	}
	return out, nil
}

// Scanned roots of the no-license-check scan (ADR-0002).
func scanRoots() []string {
	return []string{"cmd/", "internal/", "pkg/", "sdk/", "console/"}
}

// deniedTerms are identifiers and strings that suggest license or edition
// gating. Matching ignores case and the separators "-", "_" and spaces, so
// licenseKey, license_key and LICENSE-KEY all match "licensekey".
func deniedTerms() []string {
	return []string{"licensekey", "licensefile", "licenseserver", "licensetoken", "entitlement", "enterpriseedition", "enterpriseonly"}
}

// deniedHosts are hostnames of Revington-operated services, which no feature
// may depend on. Matching ignores case.
func deniedHosts() []string {
	return []string{"revington.co"}
}

// allowEntry permits one term under a path prefix.
type allowEntry struct {
	prefix, term string
}

// loadAllowlist reads "<path or dir/> <term> # <reason>" lines; the reason
// is required, because every entry needs maintainer review.
func loadAllowlist(p string) ([]allowEntry, error) {
	data, err := os.ReadFile(p) //nolint:gosec // The allowlist path comes from the repository layout.
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []allowEntry
	sc := bufio.NewScanner(bytes.NewReader(data))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		entry, reason, ok := strings.Cut(line, "#")
		fields := strings.Fields(entry)
		if !ok || strings.TrimSpace(reason) == "" || len(fields) != 2 {
			return nil, fmt.Errorf("%s:%d: want \"<path> <term> # <reason>\"", p, n)
		}
		term := fields[1]
		if !slices.Contains(deniedTerms(), term) && !slices.Contains(deniedHosts(), term) {
			return nil, fmt.Errorf("%s:%d: %q is not a denied term", p, n, term)
		}
		out = append(out, allowEntry{prefix: fields[0], term: term})
	}
	return out, sc.Err()
}

func allowed(allow []allowEntry, rel, term string) bool {
	for _, a := range allow {
		if a.term == term && (rel == a.prefix || (strings.HasSuffix(a.prefix, "/") && strings.HasPrefix(rel, a.prefix))) {
			return true
		}
	}
	return false
}

func normalize(line string) string {
	return strings.NewReplacer("-", "", "_", "", " ", "", "\t", "").Replace(strings.ToLower(line))
}

// checkNoLicense is the no-license-check scan (SM-1).
func checkNoLicense(r *repo, allow []allowEntry) ([]finding, error) {
	var out []finding
	for _, rel := range r.files {
		if !slices.ContainsFunc(scanRoots(), func(root string) bool { return strings.HasPrefix(rel, root) }) {
			continue
		}
		data, err := r.read(rel)
		if err != nil {
			return nil, err
		}
		if bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0 {
			continue // binary file
		}
		for n, line := range strings.Split(string(data), "\n") {
			norm, lower := normalize(line), strings.ToLower(line)
			for _, term := range deniedTerms() {
				if strings.Contains(norm, term) && !allowed(allow, rel, term) {
					out = append(out, finding{path: rel, line: n + 1, msg: fmt.Sprintf("no-license-check: %q suggests feature gating (ADR-0002)", term)})
				}
			}
			for _, host := range deniedHosts() {
				if strings.Contains(lower, host) && !allowed(allow, rel, host) {
					out = append(out, finding{path: rel, line: n + 1, msg: fmt.Sprintf("no-license-check: %q is a Revington-operated host (ADR-0002)", host)})
				}
			}
		}
	}
	return out, nil
}
