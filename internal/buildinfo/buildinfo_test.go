// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package buildinfo

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestGetDefaults(t *testing.T) {
	info := Get()
	if info.Version == "" || info.Commit == "" || info.Flavor == "" {
		t.Fatalf("Get() has empty fields: %+v", info)
	}
	if len(info.APIVersions) != 1 || info.APIVersions[0] != "ruralz/v1alpha1" {
		t.Fatalf("APIVersions = %v, want [ruralz/v1alpha1]", info.APIVersions)
	}
}

func TestWriteText(t *testing.T) {
	info := Info{Version: "0.1.0", Commit: "abc", Flavor: "default", APIVersions: []string{"ruralz/v1alpha1"}}
	var buf bytes.Buffer
	if err := info.WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	want := "version:     0.1.0\ncommit:      abc\nflavor:      default\napiVersions: ruralz/v1alpha1\n"
	if got := buf.String(); got != want {
		t.Fatalf("WriteText =\n%s\nwant\n%s", got, want)
	}
}

func TestWriteJSON(t *testing.T) {
	info := Info{Version: "0.1.0", Commit: "abc", Flavor: "default", APIVersions: []string{"ruralz/v1alpha1"}}
	var buf bytes.Buffer
	if err := info.WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	for _, key := range []string{"version", "commit", "flavor", "apiVersions"} {
		if _, ok := got[key]; !ok {
			t.Errorf("JSON output lacks %q", key)
		}
	}
	if !strings.HasSuffix(buf.String(), "}\n") {
		t.Errorf("JSON output should end with a newline")
	}
}
