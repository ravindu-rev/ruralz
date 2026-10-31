// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package proc

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCheckResolvConf checks the NetNSOptions.ResolvConf rule: the
// absolute path of a regular file, refused otherwise.
func TestCheckResolvConf(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "resolv.conf")
	if err := os.WriteFile(conf, []byte("nameserver 127.0.0.1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkResolvConf(conf); err != nil {
		t.Errorf("checkResolvConf(%q) = %v, want nil", conf, err)
	}
	for _, p := range []string{"resolv.conf", filepath.Join(dir, "missing"), dir, ""} {
		if err := checkResolvConf(p); err == nil {
			t.Errorf("checkResolvConf(%q) = nil, want a refusal", p)
		}
	}
}
