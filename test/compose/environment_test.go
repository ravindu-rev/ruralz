// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package compose

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/cli"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// restSource copies examples/shop-rest-bundle into dir with the compose
// Environment's overlay beside its own, and returns the copy's path and
// the environments file: the source the CLI renders the REST project's
// Bundle from. The example itself stays unchanged.
func restSource(t testing.TB, root, dir string) (bundle, environments string) {
	t.Helper()
	bundle = filepath.Join(dir, "bundle-src")
	if err := copyTree(filepath.Join(root, filepath.FromSlash(restBundleDir)), bundle); err != nil {
		t.Fatal(err)
	}
	if err := copyTree(suitePath(root, "testdata/overlays/"+composeEnv), filepath.Join(bundle, "overlays", composeEnv)); err != nil {
		t.Fatal(err)
	}
	environments = filepath.Join(dir, "environments.yaml")
	if err := copyFile(suitePath(root, "testdata/environments.yaml"), environments, 0o600); err != nil {
		t.Fatal(err)
	}
	return bundle, environments
}

// copyTree copies the regular files and directories under src to dst.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o750)
		}
		return copyFile(path, target, 0o600)
	})
}

// copyFile copies src to dst with mode.
func copyFile(src, dst string, mode os.FileMode) error {
	data, err := os.ReadFile(src) //nolint:gosec // G304: a file of the repository or of the test's directory
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, mode)
}

// runCLI runs the ruralz CLI in process and returns its exit code and
// outputs.
func runCLI(t testing.TB, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := cli.Run(context.Background(), args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestComposeEnvironmentRendersTheExample(t *testing.T) {
	root := repoRoot(t)
	bundle, envs := restSource(t, root, t.TempDir())
	out := filepath.Join(t.TempDir(), "ruralz.yaml")
	if code, _, stderr := runCLI(t, "bundle", "render", "--env", composeEnv, "--environments", envs, "--output-file", out, bundle); code != cli.ExitOK {
		t.Fatalf("ruralz bundle render --env %s: exit %d: %s", composeEnv, code, stderr)
	}
	docs := parseYAMLDocs(t, out)
	byName := map[string]*tree.Node{}
	for _, d := range docs {
		byName[scalar(node(d, "kind"))+"/"+scalar(node(d, "metadata", "name"))] = d
	}
	t.Run("the overlay turns the Consumer's key hash into a file secretRef", func(t *testing.T) {
		keys := node(byName["Consumer/partner-acme"], "spec", "credentials", "apiKeys")
		if keys == nil || len(keys.Items) != 1 {
			t.Fatalf("partner-acme apiKeys %v, want one key", keys)
		}
		key := keys.Items[0]
		if node(key, "hash") != nil || scalar(node(key, "secretRef", "provider")) != "file" || scalar(node(key, "secretRef", "name")) != consumerKeyRef {
			t.Errorf("partner-acme key %v, want only secretRef {provider: file, name: %s}", keys.Items[0], consumerKeyRef)
		}
	})
	t.Run("the variables point the OTLP endpoint at the Collector", func(t *testing.T) {
		got := scalar(node(byName["Gateway/edge"], "spec", "telemetry", "otlp", "endpoint"))
		if got != "https://"+svcCollector+":4317" {
			t.Errorf("OTLP endpoint %q, want https://%s:4317", got, svcCollector)
		}
	})
	t.Run("the secrets stay where the project mounts them", func(t *testing.T) {
		certs := node(byName["Gateway/edge"], "spec", "listeners")
		var refs []string
		for _, l := range certs.Items {
			for _, c := range nodeItems(node(l, "tls", "certificates")) {
				refs = append(refs, scalar(node(c, "certificate", "secretRef", "name")), scalar(node(c, "privateKey", "secretRef", "name")))
			}
		}
		refs = append(refs, scalar(node(byName["Policy/upstream-oauth"], "spec", "config", "clientSecret", "secretRef", "name")))
		want := []string{tlsCertRef, tlsKeyRef, clientSecretRef}
		if len(refs) != len(want) {
			t.Fatalf("secret references %v, want %v", refs, want)
		}
		for i := range want {
			if refs[i] != want[i] {
				t.Errorf("secret references %v, want %v", refs, want)
			}
		}
		if got := scalar(node(byName["Gateway/edge"], "spec", "stateStore", "url", "secretRef", "name")); got != "RURALZ_STATE_STORE_URL" {
			t.Errorf("State Store URL reference %q, want RURALZ_STATE_STORE_URL", got)
		}
	})
	t.Run("the effective chains are the example's", func(t *testing.T) {
		code, got, stderr := runCLI(t, "bundle", "render", "--env", composeEnv, "--environments", envs, "--effective", "--output", "json", bundle)
		if code != cli.ExitOK {
			t.Fatalf("render --effective --env %s: exit %d: %s", composeEnv, code, stderr)
		}
		code, want, stderr := runCLI(t, "bundle", "render", "--effective", "--output", "json", filepath.Join(root, filepath.FromSlash(restBundleDir)))
		if code != cli.ExitOK {
			t.Fatalf("render --effective of the example: exit %d: %s", code, stderr)
		}
		if got != want {
			t.Errorf("the compose Environment changes the effective chains:\n%s\nwant the example's:\n%s", got, want)
		}
		for _, route := range []string{routeSummary, routePartner} {
			rows := decodeRows(t, runEffective(t, envs, bundle, route))
			if len(rows) == 0 {
				t.Errorf("route %s runs no Policy", route)
			}
		}
	})
}

// runEffective prints one Route's effective chain as JSON with the
// in-process CLI.
func runEffective(t testing.TB, envs, bundle, route string) string {
	t.Helper()
	code, out, stderr := runCLI(t, "bundle", "render", "--env", composeEnv, "--environments", envs, "--effective", "--route", route, "--output", "json", bundle)
	if code != cli.ExitOK {
		t.Fatalf("render --effective --route %s: exit %d: %s", route, code, stderr)
	}
	return out
}

// nodeItems returns the items of a list node.
func nodeItems(n *tree.Node) []*tree.Node {
	if n == nil || n.Kind != tree.KindList {
		return nil
	}
	return n.Items
}
