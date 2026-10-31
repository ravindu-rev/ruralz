// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/mockup"
	"github.com/ravindu-rev/ruralz/internal/testkit/topology"
)

// End-to-end cases of ruralz node dump and ruralz node drain, Planned
// (M1), each documented exit code and output form
// (docs/reference/01-cli-and-api-surface.md "Rollout, promotion and Node
// verbs"), against harness Nodes.

// TestNodeDump: ruralz node dump writes the /config/dump document exactly
// as received, to standard output or, with --output-file, to a file with
// one line on standard error naming its Revision, digest and
// Last-Known-Good digest; RURALZ_ADMIN_URL and RURALZ_ADMIN_TOKEN_FILE
// stand in for --admin and --admin-token-file; secrets are omitted, every
// secret field holding only its reference; a token the Node refuses and a
// Node without an active Revision (503 RZ-RT-024) exit 2 and write
// nothing.
func TestNodeDump(t *testing.T) {
	s := startShopRest(t, shopRestOptions{nodes: 1})
	n := s.nodes[0]
	creds := []string{"--admin-token-file", n.TokenFile(), "--ca-file", s.ca}
	// The Node writes Last-Known-Good after the activation, so the
	// document gains its lastKnownGood member then; the comparisons start
	// once it holds the active Revision.
	var resp topology.AdminResponse
	waitFor(t, "the Last-Known-Good write", func() bool {
		var err error
		if resp, err = n.Admin(t.Context(), "/config/dump"); err != nil || resp.Status != http.StatusOK {
			return false
		}
		var d struct {
			LastKnownGood string `json:"lastKnownGood"`
		}
		return json.Unmarshal(resp.Body, &d) == nil && d.LastKnownGood == n.TestDigest()
	})

	r := cli(t, s.top, cliOptions{}, append([]string{"node", "dump", "--admin", n.AdminURL()}, creds...)...)
	if r.code != 0 || r.stdout != string(resp.Body) || r.stderr != "" {
		t.Errorf("node dump to standard output: exit %d, stderr %q, %d bytes, want the %d bytes as received: %s",
			r.code, r.stderr, len(r.stdout), len(resp.Body), firstDifference(r.stdout, string(resp.Body)))
	}
	r = cli(t, s.top, cliOptions{env: []string{"RURALZ_ADMIN_URL=" + n.AdminURL(), "RURALZ_ADMIN_TOKEN_FILE=" + n.TokenFile()}},
		"node", "dump", "--ca-file", s.ca)
	if r.code != 0 || r.stdout != string(resp.Body) {
		t.Errorf("node dump through RURALZ_ADMIN_URL and RURALZ_ADMIN_TOKEN_FILE: exit %d, stderr %q", r.code, r.stderr)
	}

	file := filepath.Join(t.TempDir(), "dump.json")
	r = cli(t, s.top, cliOptions{}, append([]string{"node", "dump", "--admin", n.AdminURL(), "--output-file", file}, creds...)...)
	want := "saved " + displayRevision(t, n.TestDigest()) + " " + n.TestDigest() + ", Last-Known-Good " + n.TestDigest() + "\n"
	if r.code != 0 || r.stdout != "" || r.stderr != want {
		t.Errorf("node dump --output-file: %s\nwant only %q on standard error", r, want)
	}
	saved := readFile(t, file)
	s.top.Capture("node-dump-file", saved)
	if !bytes.Equal(saved, resp.Body) {
		t.Error("the saved dump is not the document as received")
	}
	var dump struct {
		Content struct {
			Resources []map[string]any `json:"resources"`
		} `json:"content"`
	}
	if err := json.Unmarshal(saved, &dump); err != nil {
		t.Fatal(err)
	}
	refs := 0
	for _, res := range dump.Content.Resources {
		refs += checkSecretRefs(t, res, "")
	}
	if refs < 5 {
		t.Errorf("the dump holds %d secret references, want the REST example Bundle's five (TLS certificate and key, State Store URL, client secret, API key)", refs)
	}

	wrong := writeFile(t, t.TempDir(), "wrong-token", []byte(s.top.Canary()+"\n"))
	out := filepath.Join(t.TempDir(), "refused.json")
	r = cli(t, s.top, cliOptions{}, "node", "dump", "--admin", n.AdminURL(), "--admin-token-file", wrong, "--ca-file", s.ca, "--output-file", out)
	if _, err := os.Stat(out); r.code != 2 || !regexp.MustCompile(`401 RZ-AUTH-00[12] `).MatchString(r.stderr) || err == nil {
		t.Errorf("a refused token: %s\nwant exit 2 with the 401 problem and no file", r)
	}

	// A Node whose TLS key is missing never activates its Revision.
	unready, err := s.top.AddNodes(t.Context(), 1, topology.NodeOptions{
		Bundle: s.rendered, NoWait: true, MissingSecrets: []string{refTLSKey},
		Secrets:        map[string][]byte{refPartnerKey: []byte(s.apiKey), refClientSecret: []byte(s.clientSecret)},
		StateStoreURLs: map[string]string{refStateStore: "main"},
	})
	if err != nil {
		t.Fatal(err)
	}
	u := unready[0]
	waitFor(t, "the unready Node's admin port", func() bool {
		_, status, err := u.Readyz(t.Context())
		return err == nil && status == http.StatusServiceUnavailable
	})
	r = cli(t, s.top, cliOptions{}, "node", "dump", "--admin", u.AdminURL(), "--admin-token-file", u.TokenFile(), "--ca-file", s.ca, "--output-file", out)
	if _, err := os.Stat(out); r.code != 2 || !strings.Contains(r.stderr, "503 RZ-RT-024 ") || err == nil {
		t.Errorf("a Node without an active Revision: %s\nwant exit 2 with 503 RZ-RT-024 and no file", r)
	}
}

// checkSecretRefs fails t for a secret field of a canonical resource that
// holds anything but a reference, and returns the references it found: a
// member named secretRef must hold provider and name, with key at most.
func checkSecretRefs(t testing.TB, v any, path string) int {
	t.Helper()
	n := 0
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			if k == "secretRef" {
				ref, ok := child.(map[string]any)
				_, hasProvider := ref["provider"]
				_, hasName := ref["name"]
				if !ok || !hasProvider || !hasName || len(ref) > 3 {
					t.Errorf("%s.secretRef = %v, want a reference", path, child)
				}
				n++
				continue
			}
			n += checkSecretRefs(t, child, path+"."+k)
		}
	case []any:
		for i, child := range x {
			n += checkSecretRefs(t, child, path+"["+strconv.Itoa(i)+"]")
		}
	}
	return n
}

// drainBundle is a one-Route Bundle to the mock Upstream at address.
func drainBundle(address string) []byte {
	return []byte(`apiVersion: ruralz/v1alpha1
kind: Gateway
metadata:
  name: edge
spec:
  listeners:
    - name: http
      protocol: http
      port: 8080
---
apiVersion: ruralz/v1alpha1
kind: Upstream
metadata:
  name: backend
spec:
  protocol: http
  endpoints:
    - address: "` + address + `"
---
apiVersion: ruralz/v1alpha1
kind: Route
metadata:
  name: plain
spec:
  match:
    path: {prefix: /}
  upstreams:
    - name: backend
`)
}

// drainOutput is ruralz node drain --output json's object.
type drainOutput struct {
	NodeID  string  `json:"nodeId"`
	PID     int     `json:"pid"`
	Seconds float64 `json:"seconds"`
	Result  string  `json:"result"`
}

// TestNodeDrain: ruralz node drain signals the lock holder of a harness
// Node's data directory, which drains in controlled time while the test
// advances the Node clock, and exits 0 once the process exited: one line
// on standard error naming the Node, its PID and the seconds, nothing on
// standard output, or with --output json the object with result drained.
// A --timeout that passes first exits 2 naming the PID still running, with
// result timeout in JSON; a directory without a lock holder exits 2.
func TestNodeDrain(t *testing.T) {
	ctx := t.Context()
	top := newTopology(t, topology.Config{})
	mock, err := top.StartMockUpstream(mockup.Config{Name: "backend"})
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := top.AddNodes(ctx, 1, topology.NodeOptions{Bundle: drainBundle(mock.Addr())})
	if err != nil {
		t.Fatal(err)
	}
	n := nodes[0]
	drain := func(t *testing.T, args ...string) (cliRun, int, string) {
		t.Helper()
		h, err := n.Holder()
		if err != nil {
			t.Fatal(err)
		}
		args = append([]string{"node", "drain", "--data-dir", n.DataDir()}, args...)
		p := startCLI(t, cliOptions{}, args...) //nolint:contextcheck // the CLI runs on its subtest's t.Context(), not the parent test's ctx
		var r cliRun
		err = advancing(ctx, top, time.Second, func() error { //nolint:contextcheck // finish waits on its subtest's t.Context(), not the parent test's ctx
			r = finish(t, top, p, args, waitTimeout)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return r, h.PID, h.NodeID.String()
	}

	t.Run("Text", func(t *testing.T) {
		r, pid, nodeID := drain(t)
		line := regexp.MustCompile(`^drained Node ` + regexp.QuoteMeta(nodeID) + ` \(pid ` + strconv.Itoa(pid) + `\) in \d+\.\d s\n$`)
		if r.code != 0 || r.stdout != "" || !line.MatchString(r.stderr) {
			t.Errorf("%s\nwant exit 0 and only the drained line on standard error", r)
		}
		if st := n.Process().State(); st == nil || st.ExitCode() != 0 {
			t.Errorf("the drained Node's process: %v, want exit 0", st)
		}
	})
	t.Run("JSON", func(t *testing.T) {
		if err := n.Start(ctx); err != nil {
			t.Fatal(err)
		}
		r, pid, nodeID := drain(t, "--output", "json")
		var out drainOutput
		if r.code != 0 || json.Unmarshal([]byte(r.stdout), &out) != nil || out.NodeID != nodeID || out.PID != pid || out.Result != "drained" || out.Seconds < 0 {
			t.Errorf("%s\nwant exit 0 and {nodeId %s, pid %d, result drained}", r, nodeID, pid)
		}
	})
	t.Run("Timeout", func(t *testing.T) {
		if err := n.Start(ctx); err != nil {
			t.Fatal(err)
		}
		h, err := n.Holder()
		if err != nil {
			t.Fatal(err)
		}
		// The Node clock stands still, so the Drain cannot end before the
		// CLI's own --timeout, which runs in real time.
		r := cli(t, top, cliOptions{}, "node", "drain", "--data-dir", n.DataDir(), "--timeout", "1s", "--output", "json") //nolint:contextcheck // the CLI runs on its subtest's t.Context(), not the parent test's ctx
		var out drainOutput
		if r.code != 2 || json.Unmarshal([]byte(r.stdout), &out) != nil || out.Result != "timeout" || out.PID != h.PID ||
			!strings.Contains(r.stderr, strconv.Itoa(h.PID)) {
			t.Errorf("%s\nwant exit 2, result timeout and pid %d", r, h.PID)
		}
		if n.Process().State() != nil {
			t.Fatal("the Node exited without the clock moving through its Drain")
		}
		if err := advancing(ctx, top, time.Second, func() error {
			wctx, cancel := context.WithTimeout(ctx, waitTimeout)
			defer cancel()
			_, err := n.Process().Wait(wctx)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("NoHolder", func(t *testing.T) {
		empty := t.TempDir()
		r := cli(t, top, cliOptions{}, "node", "drain", "--data-dir", empty)
		if r.code != 2 || r.stdout != "" {
			t.Errorf("%s\nwant exit 2 for a directory without a lock holder", r)
		}
	})
}
