// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package openapi

import (
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
)

// memFS is an in-memory FS of files by slash path, relative to no
// directory; it counts the reads of each file.
type memFS struct {
	mu    sync.Mutex
	files map[string]string
	reads map[string]int
}

// newMemFS returns a memFS of files, slash path to content.
func newMemFS(files map[string]string) *memFS {
	m := &memFS{files: map[string]string{}, reads: map[string]int{}}
	for p, c := range files {
		m.files[filepath.Clean(filepath.FromSlash(p))] = c
	}
	return m
}

// ReadFile returns at most limit+1 bytes of the file name.
func (m *memFS) ReadFile(name string, limit int64) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reads[name]++
	c, ok := m.files[name]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	if int64(len(c)) > limit+1 {
		c = c[:limit+1]
	}
	return []byte(c), nil
}

// readsOf returns the reads of the file at slash path p.
func (m *memFS) readsOf(p string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reads[filepath.Clean(filepath.FromSlash(p))]
}

// loadFiles runs Load on the root of files with o, its FS replaced by them.
func loadFiles(t *testing.T, root string, files map[string]string, o Options) (*Document, error) {
	t.Helper()
	o.FS = newMemFS(files)
	return Load(t.Context(), filepath.FromSlash(root), o)
}

// mustLoad loads a document that must load without problems.
func mustLoad(t *testing.T, root string, files map[string]string) *Document {
	t.Helper()
	doc, err := loadFiles(t, root, files, Options{})
	if err != nil {
		t.Fatalf("Load(%s) = %v", root, err)
	}
	return doc
}

// problems returns the problem lines of a load that must fail with an
// *Error.
func problems(t *testing.T, err error) []string {
	t.Helper()
	var le *Error
	if !errors.As(err, &le) {
		t.Fatalf("Load error = %v (%T), want an *Error", err, err)
	}
	out := make([]string, 0, len(le.Problems))
	for _, p := range le.Problems {
		out = append(out, p.String())
	}
	return out
}

// locationOf returns a location.
func locationOf(file string, line, column int) diag.Location {
	return diag.Location{File: file, Line: line, Column: column}
}

// hasProblem reports whether a problem line of ps contains want.
func hasProblem(ps []string, want string) bool {
	for _, p := range ps {
		if strings.Contains(p, want) {
			return true
		}
	}
	return false
}

// minimal30 is the smallest 3.0 document: openapi, info and paths.
const minimal30 = `openapi: 3.0.3
info: {title: t, version: "1"}
paths: {}
`

// petstore is a small 3.0 document with root servers, a parameterized
// path and three operations.
const petstore = `openapi: 3.0.3
info:
  title: Petstore
  version: 1.0.0
servers:
  - url: https://petstore.example/v1
paths:
  /pets:
    get:
      operationId: listPets
      responses: {"200": {description: ok}}
    post:
      operationId: createPets
      requestBody:
        required: true
        content:
          application/json:
            schema: {$ref: "#/components/schemas/Pet"}
      responses: {"201": {description: created}}
  /pets/{petId}:
    parameters:
      - $ref: "#/components/parameters/PetId"
    get:
      operationId: showPetById
      responses: {"200": {description: ok}}
components:
  parameters:
    PetId: {name: petId, in: path, required: true, schema: {type: string}}
  schemas:
    Pet: {type: object, required: [id], properties: {id: {type: integer}}}
`
