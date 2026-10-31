// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package topology

import (
	"slices"
	"strings"
	"testing"
)

// TestSetenv checks Node.Setenv: it sets or replaces one variable of the
// environment the Node's next process starts with, after Config.Env and
// keeping the Node's other variables, and refuses a malformed name and a
// secret variable whose value carries no canary, as NodeOptions.Env does.
func TestSetenv(t *testing.T) {
	topo := standinTopology(t)
	n := &Node{t: topo, name: "node-1", opts: NodeOptions{Env: []string{"A=1", "RURALZ_ZONE=a"}}}
	for _, kv := range [][2]string{{"A", "2"}, {"B", "3"}} {
		if err := n.Setenv(kv[0], kv[1]); err != nil {
			t.Fatalf("Setenv(%q, %q) = %v", kv[0], kv[1], err)
		}
	}
	env := n.env()
	tail := env[len(env)-3:]
	if want := []string{"RURALZ_ZONE=a", "A=2", "B=3"}; !slices.Equal(tail, want) {
		t.Errorf("env() ends %q, want %q", tail, want)
	}
	if slices.Contains(env, "A=1") {
		t.Errorf("env() kept the replaced A=1: %q", env)
	}
	if i, j := slices.Index(env, topo.cfg.Env[0]), slices.Index(env, "A=2"); i < 0 || i > j {
		t.Errorf("env() = %q: Config.Env must precede the Node's variables", env)
	}
	for _, tc := range []struct{ key, value, want string }{
		{"", "x", "variable name"},
		{"A=B", "x", "variable name"},
		{"RURALZ_STATE_STORE_URL", "redis://u:plain@127.0.0.1:1", "carries no canary"},
	} {
		if err := n.Setenv(tc.key, tc.value); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Setenv(%q, %q) = %v, want an error naming %q", tc.key, tc.value, err, tc.want)
		}
	}
	url := "redis://u:" + topo.Canary() + "@127.0.0.1:1"
	if err := n.Setenv("RURALZ_STATE_STORE_URL", url); err != nil {
		t.Errorf("Setenv of a secret variable carrying a canary = %v", err)
	}
	if env := n.env(); env[len(env)-1] != "RURALZ_STATE_STORE_URL="+url {
		t.Errorf("env() ends %q, want the State Store URL", env[len(env)-1])
	}
}
