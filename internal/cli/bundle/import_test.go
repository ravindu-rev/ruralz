// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package bundle

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/cli/command"
	"github.com/ravindu-rev/ruralz/internal/config/loader"
	"github.com/ravindu-rev/ruralz/internal/config/pipeline"
)

// importPetstore is the OpenAPI document of the import goldens: a root
// server with a base path, a path item parameter through components, an
// operation with servers of its own, a partial-segment parameter and an
// operation without operationId.
const importPetstore = `openapi: 3.0.3
info: {title: Petstore, version: 1.0.0}
servers:
  - url: https://petstore.example/v1
paths:
  /pets:
    get: {operationId: listPets}
    post: {operationId: createPets}
  /pets/{pet-id}:
    parameters: [{$ref: "#/components/parameters/PetId"}]
    get: {operationId: showPetById}
    delete:
      servers: [{url: "http://admin.petstore.example:8080"}]
  /pets/{petId}/photo.{format}:
    get: {operationId: getPhoto}
components:
  parameters:
    PetId: {name: pet-id, in: path, required: true, schema: {type: string}}
`

// TestImportSpec: ruralz bundle import takes FORMAT, openapi alone, and
// FILE, and the one flag --output-dir.
func TestImportSpec(t *testing.T) {
	s := ImportSpec(Options{})
	if s.Name() != "bundle import" || s.Summary != "Routes and Upstreams from OpenAPI 3.x" || s.Planned != "" || s.New == nil {
		t.Fatalf("spec = %+v", s)
	}
	if len(s.Args) != 2 || s.Args[0].Name != "FORMAT" || !slices.Equal(s.Args[0].Values, []string{"openapi"}) ||
		s.Args[1].Name != "FILE" || s.Args[1].Optional || s.Args[1].Complete != command.CompleteFile {
		t.Errorf("args = %+v", s.Args)
	}
	var names []string
	for _, f := range command.FlagSpecsOf(s) {
		names = append(names, f.Name)
	}
	if !slices.Equal(names, []string{"output-dir"}) {
		t.Errorf("flags = %q, want output-dir", names)
	}
}

// TestImportStdout: without --output-dir the Bundle goes to standard
// output as one YAML stream, a one-file Bundle that ruralz bundle
// validate accepts; nothing else is printed.
func TestImportStdout(t *testing.T) {
	doc := writeFile(t, "openapi.yaml", importPetstore)
	r := run(t, "bundle", "import", "openapi", doc)
	r.wantCode(t, "import", command.ExitOK)
	if r.stderr != "" {
		t.Errorf("stderr = %q, want none", r.stderr)
	}
	golden(t, "import/petstore.yaml", r.stdout)
	file := writeFile(t, "imported.yaml", r.stdout)
	run(t, "bundle", "validate", file).want(t, "validate the imported Bundle", command.ExitOK, "", "")
}

// TestImportOutputDir: --output-dir writes ruralz.yaml, upstreams/ and
// routes/, one file per resource, the same documents as the stream; the
// directory is a Bundle ruralz bundle validate and render accept.
func TestImportOutputDir(t *testing.T) {
	doc := writeFile(t, "openapi.yaml", importPetstore)
	out := filepath.Join(t.TempDir(), "imported")
	run(t, "bundle", "import", "openapi", doc, "--output-dir", out).want(t, "import", command.ExitOK, "", "")
	var listing strings.Builder
	err := filepath.WalkDir(out, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(out, p)
		listing.WriteString("== " + filepath.ToSlash(rel) + "\n" + readFile(t, p))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "import/petstore-dir.txt", listing.String())
	run(t, "bundle", "validate", out).want(t, "validate the imported directory", command.ExitOK, "", "")
	if r := run(t, "bundle", "render", out); r.code != command.ExitOK || r.stderr != "" {
		t.Errorf("render the imported directory = exit %d, stderr %q", r.code, r.stderr)
	}
	stream := run(t, "bundle", "import", "openapi", doc).stdout
	if got := run(t, "bundle", "render", out).stdout; got != run(t, "bundle", "render", writeFile(t, "one.yaml", stream)).stdout {
		t.Errorf("the directory renders otherwise than the stream:\n%s", got)
	}
}

// TestImportOutputDirRefused: --output-dir refuses a directory that holds
// a file, and names a missing parent's creation; an empty value is a
// usage error. Nothing is written into a refused directory.
func TestImportOutputDirRefused(t *testing.T) {
	doc := writeFile(t, "openapi.yaml", importPetstore)
	full := t.TempDir()
	writeFiles(t, full, map[string]string{"keep.txt": "x"})
	r := run(t, "bundle", "import", "openapi", doc, "--output-dir", full)
	r.wantCode(t, "import into a directory that holds a file", command.ExitNoResult)
	if !strings.Contains(scrub(r.stderr, full), "ruralz bundle import: --output-dir $DIR: unusable output directory: $DIR is not empty") {
		t.Errorf("stderr = %q", scrub(r.stderr, full))
	}
	if entries, _ := os.ReadDir(full); len(entries) != 1 {
		t.Errorf("the refused directory holds %d entries", len(entries))
	}
	nested := filepath.Join(t.TempDir(), "a", "b")
	run(t, "bundle", "import", "openapi", doc, "--output-dir", nested).want(t, "import under missing parents", command.ExitOK, "", "")
	if _, err := os.Stat(filepath.Join(nested, "ruralz.yaml")); err != nil {
		t.Errorf("ruralz.yaml under missing parents: %v", err)
	}
	r = run(t, "bundle", "import", "openapi", doc, "--output-dir", "")
	r.want(t, "an empty --output-dir", command.ExitNoResult, "",
		"ruralz bundle import: --output-dir needs a directory\nRun 'ruralz bundle import --help' for usage.\n")
}

// TestImportWarnings: what the import leaves out prints one warning per
// line on standard error, at its place in the document: a servers list
// with other servers, an operation without an absolute server, an
// operation whose path the Router cannot match and the webhooks; the
// rest is imported and the exit is 0.
func TestImportWarnings(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"openapi.yaml": `openapi: 3.1.0
servers:
  - url: https://api.example
  - url: https://sandbox.api.example
paths:
  /ok:
    get: {operationId: ok}
  /relative:
    servers: [{url: /v2}]
    get: {operationId: relative}
  /broken/{id:
    get: {operationId: broken}
webhooks:
  event: {post: {operationId: event}}
`})
	r := run(t, "bundle", "import", "openapi", filepath.Join(dir, "openapi.yaml"))
	r.wantCode(t, "import with warnings", command.ExitOK)
	golden(t, "import/warnings.txt", scrub(r.stderr, dir))
	if !strings.Contains(r.stdout, "name: ok\n") || strings.Contains(r.stdout, "name: relative\n") {
		t.Errorf("stdout =\n%s\nwant Route ok alone", r.stdout)
	}
}

// TestImportProblems: a document that is not a readable OpenAPI 3.0 or
// 3.1 document prints each problem as one line, then names the file, and
// exits 2 with nothing on standard output.
func TestImportProblems(t *testing.T) {
	tests := []struct {
		name, src string
	}{
		{name: "swagger", src: "swagger: \"2.0\"\ninfo: {title: t, version: \"1\"}\npaths: {}\n"},
		{name: "types", src: "openapi: 3.0.3\npaths:\n  /a: {get: {operationId: 1, tags: x}}\n  /b: {$ref: \"https://example.com/b.yaml\"}\n"},
		{name: "yaml", src: "openapi: 3.0.3\npaths: &p {}\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFiles(t, dir, map[string]string{"openapi.yaml": tc.src})
			r := run(t, "bundle", "import", "openapi", filepath.Join(dir, "openapi.yaml"))
			r.wantCode(t, "import", command.ExitNoResult)
			if r.stdout != "" {
				t.Errorf("stdout = %q, want none", r.stdout)
			}
			golden(t, "import/problems-"+tc.name+".txt", scrub(r.stderr, dir))
		})
	}
}

// TestImportReadError: a FILE that does not exist, or that is a
// directory, exits 2 naming it.
func TestImportReadError(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.yaml")
	r := run(t, "bundle", "import", "openapi", missing)
	r.wantCode(t, "import a missing file", command.ExitNoResult)
	if want := "ruralz bundle import: cannot read $DIR/missing.yaml: "; !strings.HasPrefix(scrub(r.stderr, dir), want) || r.stdout != "" {
		t.Errorf("stderr = %q, want it to start with %q", scrub(r.stderr, dir), want)
	}
	r = run(t, "bundle", "import", "openapi", dir)
	r.want(t, "import a directory", command.ExitNoResult, "", "ruralz bundle import: cannot read "+filepath.ToSlash(dir)+": not a regular file\n")
}

// TestImportUsage: a FORMAT other than openapi, a missing FILE and an
// extra argument are usage errors.
func TestImportUsage(t *testing.T) {
	tests := []struct {
		name   string
		args   []string
		stderr string
	}{
		{"another format", []string{"swagger", "a.yaml"}, "ruralz bundle import: unsupported FORMAT \"swagger\" (want openapi)\n"},
		{"no FILE", []string{"openapi"}, "ruralz bundle import: missing argument FILE\n"},
		{"no arguments", nil, "ruralz bundle import: missing arguments FORMAT and FILE\n"},
		{"an extra argument", []string{"openapi", "a.yaml", "b.yaml"}, "ruralz bundle import: unexpected argument \"b.yaml\"\n"},
		{"an unknown flag", []string{"openapi", "a.yaml", "--output-file", "x"}, "ruralz bundle import: unknown flag --output-file\n"},
		{"a misspelled flag", []string{"openapi", "a.yaml", "--output-dri", "x"}, "ruralz bundle import: unknown flag --output-dri (did you mean --output-dir?)\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := run(t, append([]string{"bundle", "import"}, tc.args...)...)
			r.want(t, "import", command.ExitNoResult, "", tc.stderr+"Run 'ruralz bundle import --help' for usage.\n")
		})
	}
}

// TestImportMultiFile: path items and parameters in other files, below
// and above the document's directory, import as the document's own.
func TestImportMultiFile(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"api/openapi.yaml": `openapi: 3.0.3
servers: [{url: "https://multi.example"}]
paths:
  /items: {$ref: "paths/items.yaml"}
  /shared: {$ref: "../shared/paths.yaml#/Shared"}
`,
		"api/paths/items.yaml": "get: {operationId: listItems}\n",
		"shared/paths.yaml":    "Shared: {get: {operationId: getShared}}\n",
	})
	r := run(t, "bundle", "import", "openapi", filepath.Join(dir, "api", "openapi.yaml"))
	r.wantCode(t, "import", command.ExitOK)
	for _, want := range []string{"name: list-items\n", "exact: /items\n", "name: get-shared\n", "exact: /shared\n"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, r.stdout)
		}
	}
}

// TestImportCheck: the import runs the Bundle it built through every
// offline stage before writing; a Bundle that does not validate, here
// one past the pipeline's resource limit, prints its diagnostics and exits
// 2 without writing anything.
func TestImportCheck(t *testing.T) {
	o := testOptions()
	o.pipelineFunc = func(Options) (*pipeline.Pipeline, error) {
		return newTestPipeline(pipeline.Options{Limits: loader.Limits{MaxResources: 2}})
	}
	doc := writeFile(t, "openapi.yaml", importPetstore)
	out := filepath.Join(t.TempDir(), "never")
	r := execute(t, o, nil, "bundle", "import", "openapi", doc, "--output-dir", out)
	r.wantCode(t, "import past the resource limit", command.ExitNoResult)
	if !strings.Contains(r.stderr, "imported.yaml") || !strings.Contains(r.stderr, " error RZ-CFG-001: ") ||
		!strings.HasSuffix(r.stderr, "ruralz bundle import: the imported Bundle does not validate (RZ-CFG-001)\n") {
		t.Errorf("stderr = %q", r.stderr)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("the output directory exists: %v", err)
	}
}

// TestImportHelp: the help names the arguments, the flag and what the
// command writes, on every platform.
func TestImportHelp(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "windows"} {
		var out, errOut bytes.Buffer
		sio := command.NewIO(&out, &errOut, nil, epoch, goos)
		code := command.Execute(t.Context(), sio, "ruralz", Specs(Options{}), []string{"bundle", "import", "--help"})
		if code != command.ExitOK || errOut.Len() != 0 {
			t.Fatalf("%s: exit %d, stderr %q", goos, code, errOut.String())
		}
		golden(t, "import/help.txt", out.String())
	}
}
