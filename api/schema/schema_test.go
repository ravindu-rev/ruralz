// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package schema

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"testing"
)

func TestViews(t *testing.T) {
	views := map[string][]byte{
		"ruralz/v1alpha1/authoring.schema.json": AuthoringV1alpha1(),
		"ruralz/v1alpha1/rendered.schema.json":  RenderedV1alpha1(),
	}
	for path, data := range views {
		var doc struct {
			Schema string         `json:"$schema"`
			Defs   map[string]any `json:"$defs"`
		}
		if err := json.Unmarshal(data, &doc); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if doc.Schema != "https://json-schema.org/draft/2020-12/schema" {
			t.Errorf("%s: $schema = %q", path, doc.Schema)
		}
		for _, kind := range []string{"Gateway", "Route", "Upstream", "Policy", "Plugin", "Consumer", "AIProvider", "AIModel", "Environment", "Cluster"} {
			if _, ok := doc.Defs[kind]; !ok {
				t.Errorf("%s: no definition for %s", path, kind)
			}
		}
		fromFS, err := fs.ReadFile(FS(), path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(fromFS, data) {
			t.Errorf("%s: FS and accessor differ", path)
		}
	}
}

func TestAccessorsReturnCopies(t *testing.T) {
	a := RenderedV1alpha1()
	a[0] = 'x'
	if RenderedV1alpha1()[0] == 'x' {
		t.Fatal("RenderedV1alpha1 exposes the embedded bytes")
	}
}
