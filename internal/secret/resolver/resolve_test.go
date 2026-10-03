// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package resolver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/secret"
)

// Tests for Resolve and ResolveWithRetry: spec 01 requirements 44 and 46
// (providers, root, caps, every failing use reported), spec 06
// requirements 87 to 89 (rule 5, fixed messages) and the RZ-CFG-026
// interim checks (spec 06 requirements 13 and 80).

func TestEnvProvider(t *testing.T) {
	// Spec 01 req 44 and spec 06 req 88: env resolves only
	// RURALZ_STATE_STORE_URL and RURALZ_SECRET_*; unset or empty is 026;
	// key is ignored.
	tests := []struct {
		name, env, value string
		set              bool
		key              string
		want             string // value, or "" for a failure
		reason           error
	}{
		{name: "prefix", env: "RURALZ_SECRET_DB", value: "db-pass", set: true, want: "db-pass"},
		{name: "state store url", env: EnvStateStoreURL, value: "redis://127.0.0.1:6379", set: true, want: "redis://127.0.0.1:6379"},
		{name: "key ignored", env: "RURALZ_SECRET_K", value: "v", set: true, key: "unused", want: "v"},
		{name: "exact bytes", env: "RURALZ_SECRET_SP", value: " v\n", set: true, want: " v\n"},
		{name: "HOME refused", env: "HOME", value: "/root", set: true, reason: errEnvName},
		{name: "prefix without underscore", env: "RURALZ_SECRETX", value: "hidden-value", set: true, reason: errEnvName},
		{name: "lower case", env: "ruralz_secret_x", value: "hidden-value", set: true, reason: errEnvName},
		{name: "other setting", env: "RURALZ_DATA_DIR", value: "/var/lib/ruralz", set: true, reason: errEnvName},
		{name: "unset", env: "RURALZ_SECRET_UNSET", reason: errEnvUnset},
		{name: "empty", env: "RURALZ_SECRET_EMPTY", set: true, reason: errEnvEmpty},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, nil)
			if tc.set {
				h.setEnv(tc.env, tc.value)
			}
			ref := secret.Ref{Provider: providerEnv, Name: tc.env, Key: tc.key}
			st, diags := h.r.Resolve(context.Background(), []secret.Use{use(ref, secret.KindOpaque)})
			if tc.reason == nil {
				if st == nil {
					t.Fatalf("diagnostics:\n%s", diagText(diags))
				}
				if got := get(t, st, ref); got != tc.want {
					t.Errorf("value = %q, want %q", got, tc.want)
				}
				return
			}
			if st != nil || len(diags) != 1 {
				t.Fatalf("store %v, diagnostics %d; want one failure", st != nil, len(diags))
			}
			if want := "secretRef " + ref.String() + ": " + tc.reason.Error(); diags[0].Message != want {
				t.Errorf("message = %q, want %q", diags[0].Message, want)
			}
			if tc.set && tc.value != "" && strings.Contains(diagText(diags), tc.value) {
				t.Errorf("diagnostic repeats the value: %s", diagText(diags))
			}
		})
	}
}

func TestPlannedAndUnknownProviders(t *testing.T) {
	// Spec 01 req 44: kubernetes and vault are 026 "provider Planned (M2),
	// not supported by this Node".
	h := newHarness(t, nil)
	k8s := secret.Ref{Provider: providerKubernetes, Name: "llm-keys", Key: "anthropic"}
	vault := secret.Ref{Provider: providerVault, Name: "kv/data/x", Key: "f"}
	other := secret.Ref{Provider: "aws", Name: "x"}
	diags := h.resolveDiags(use(k8s, secret.KindOpaque), use(vault, secret.KindOpaque), use(other, secret.KindOpaque))
	want := []string{
		"secretRef aws:x: unknown provider",
		"secretRef kubernetes:llm-keys#anthropic: provider Planned (M2), not supported by this Node",
		"secretRef vault:kv/data/x#f: provider Planned (M2), not supported by this Node",
	}
	var got []string
	for _, d := range diags {
		got = append(got, d.Message)
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("messages = %q, want %q", got, want)
	}
}

func TestDiagnosticShape(t *testing.T) {
	// Spec 01 req 44: RZ-CFG-026 names provider, reference and path; the
	// resource is kept; nothing of a value.
	h := newHarness(t, nil)
	u := use(envRef("HOME"), secret.KindStateStoreURL)
	diags := h.resolveDiags(u)
	d := diags[0]
	if d.Code != "RZ-CFG-026" || d.Severity != diag.SeverityError {
		t.Errorf("code %s severity %v", d.Code, d.Severity)
	}
	if d.Resource == nil || *d.Resource != u.Resource {
		t.Errorf("resource = %v", d.Resource)
	}
	if d.Path.String() != "spec.stateStore.url" {
		t.Errorf("path = %s", d.Path)
	}
	if got := diagText(diags); got != "- error RZ-CFG-026 Gateway/edge spec.stateStore.url: secretRef env:HOME: env name must be RURALZ_STATE_STORE_URL or start with RURALZ_SECRET_\n" {
		t.Errorf("text = %q", got)
	}
	// A use without a resource gets none.
	u.Resource = diag.ResourceID{}
	if d := h.resolveDiags(u)[0]; d.Resource != nil {
		t.Errorf("resource = %v, want nil", d.Resource)
	}
}

func TestEveryFailingUseReported(t *testing.T) {
	// Spec 01 req 44: every failing use is reported, not only the first;
	// two uses of one reference with different kinds are checked apart.
	h := newHarness(t, nil)
	h.setEnv("RURALZ_SECRET_KEY", "short")
	h.write("tls/tls.crt", "not a certificate")
	shared := envRef("RURALZ_SECRET_KEY")
	uses := []secret.Use{
		use(envRef("HOME"), secret.KindOpaque),
		use(fileRef(h.path("tls/tls.crt"), ""), secret.KindPEMCertificate),
		use(fileRef(h.path("missing"), ""), secret.KindOpaque),
		use(shared, secret.KindAPIKey),
		use(shared, secret.KindOpaque), // passes
		use(fileRef("/outside/root", ""), secret.KindOpaque),
	}
	uses[4].Path = diag.Path{diag.Field("other")}
	diags := h.resolveDiags(uses...)
	if len(diags) != 5 {
		t.Fatalf("got %d diagnostics, want 5:\n%s", len(diags), diagText(diags))
	}
	text := diagText(diags)
	for _, want := range []string{
		errEnvName.Error(), errNoCertificate.Error(), errNotExist.Error(), errShortAPIKey.Error(), errOutsideRoot.Error(),
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	sorted := slices.Clone(diags)
	sorted.Sort()
	if !slices.EqualFunc(diags, sorted, func(a, b diag.Diagnostic) bool { return a.Message == b.Message }) {
		t.Error("diagnostics are not sorted")
	}
}

func TestResolveEmpty(t *testing.T) {
	h := newHarness(t, nil)
	st := h.resolve()
	if _, ok := st.Get(envRef("RURALZ_SECRET_X")); ok {
		t.Error("empty Store holds a reference")
	}
	h.r.Activate(st)
	h.cycle()
}

func TestResolveCanceled(t *testing.T) {
	h := newHarness(t, nil)
	h.setEnv("RURALZ_SECRET_A", "a")
	h.write("a", "a")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	st, diags := h.r.Resolve(ctx, []secret.Use{
		use(envRef("RURALZ_SECRET_A"), secret.KindOpaque),
		use(fileRef(h.path("a"), ""), secret.KindOpaque),
	})
	if st != nil || len(diags) != 2 {
		t.Fatalf("store %v, %d diagnostics; want 2 cancellations", st != nil, len(diags))
	}
	for _, d := range diags {
		if !strings.HasSuffix(d.Message, errCanceled.Error()) {
			t.Errorf("message = %q", d.Message)
		}
	}
	// Canceled while file groups are pending, fresh and stale references
	// alike.
	a, b := fileRef(h.path("a"), "a"), fileRef(h.path("a"), "b")
	rs := &resolution{r: h.r, byRef: map[secret.Ref][]secret.Use{
		a: {use(a, secret.KindOpaque)},
		b: {use(b, secret.KindOpaque)},
	}}
	p := &pendingRead{fresh: []secret.Ref{a}, stale: []*cell{newCell(b, []byte("b"), 1)}}
	got := &store{cells: map[secret.Ref]*cell{}, checked: map[secret.Ref]uint64{}}
	pending := map[secret.Ref]*pendingValue{}
	rs.readGroup(ctx, h.path("a"), p, got, pending)
	if len(got.cells) != 0 || len(pending) != 0 || len(rs.diags) != 2 {
		t.Errorf("readGroup after cancel: %d cells, %d pending, %d diagnostics", len(got.cells), len(pending), len(rs.diags))
	}
}

func TestFileProviderRule5(t *testing.T) {
	// Spec 01 req 44, spec 06 req 88 (rule 5): only paths that lie under
	// the root before and after symlink resolution, read through os.Root,
	// so ".." and symbolic links cannot leave it; Kubernetes-style ..data
	// links and absolute links resolving inside the root are allowed.
	h := newHarness(t, nil)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.write("plain", "plain-value")
	// Kubernetes projected volume layout.
	h.write("..2026_09_26_12_00_00.1/tls.crt", "k8s-value")
	mustSymlink(t, "..2026_09_26_12_00_00.1", filepath.Join(h.root, "..data"))
	mustSymlink(t, "..data/tls.crt", filepath.Join(h.root, "tls.crt"))
	// Escapes.
	mustSymlink(t, filepath.Join(outside, "secret"), filepath.Join(h.root, "abs-link"))
	mustSymlink(t, "../"+filepath.Base(outside)+"/secret", filepath.Join(h.root, "rel-escape"))
	mustSymlink(t, filepath.Join(h.root, "plain"), filepath.Join(h.root, "abs-inside"))
	mustSymlink(t, filepath.Join(h.root, "tls.crt"), filepath.Join(h.root, "abs-to-k8s"))
	mustSymlink(t, filepath.Join(h.root, "rel-escape"), filepath.Join(h.root, "abs-to-escape"))
	mustSymlink(t, filepath.Join(h.root, "none"), filepath.Join(h.root, "abs-dangling"))
	mustSymlink(t, filepath.Join(h.root, "..gone", "tls.crt"), filepath.Join(h.root, "abs-dangling-dir"))
	mustSymlink(t, "none", filepath.Join(h.root, "rel-dangling"))
	mustSymlink(t, filepath.Join(h.root, "rel-dangling"), filepath.Join(h.root, "abs-to-dangling"))
	mustSymlink(t, filepath.Join(outside, "none"), filepath.Join(h.root, "abs-dangling-out"))
	mustSymlink(t, filepath.Join(h.root, "abs-loop"), filepath.Join(h.root, "abs-loop"))
	mustSymlink(t, filepath.Join(outside, "secret"), filepath.Join(h.root, "abs-link-2"))
	mustSymlink(t, h.root, filepath.Join(h.root, "abs-root"))
	if err := os.Mkdir(filepath.Join(h.root, "dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, outside, filepath.Join(h.root, "dir", "abs-dir-out"))

	tests := []struct {
		name   string
		file   string
		want   string
		reason error
	}{
		{"plain", h.path("plain"), "plain-value", nil},
		{"..data symlinks", h.path("tls.crt"), "k8s-value", nil},
		{"cleaned inside", h.path("dir/../plain"), "plain-value", nil},
		{"relative name", "plain", "", errNotAbsolute},
		{"outside", filepath.ToSlash(filepath.Join(outside, "secret")), "", errOutsideRoot},
		{"dot-dot traversal", h.path("../" + filepath.Base(outside) + "/secret"), "", errOutsideRoot},
		{"root itself", filepath.ToSlash(h.root), "", errOutsideRoot},
		{"prefix sibling", filepath.ToSlash(h.root) + "2/x", "", errOutsideRoot},
		{"absolute symlink out", h.path("abs-link"), "", errEscapes},
		{"relative symlink out", h.path("rel-escape"), "", errEscapes},
		{"absolute symlink in", h.path("abs-inside"), "plain-value", nil},
		{"absolute symlink to ..data links", h.path("abs-to-k8s"), "k8s-value", nil},
		{"absolute symlinked directory in", h.path("abs-root/plain"), "plain-value", nil},
		{"absolute symlink to an escaping link", h.path("abs-to-escape"), "", errEscapes},
		{"absolute symlink dangling", h.path("abs-dangling"), "", errNotExist},
		{"absolute symlink into a missing directory", h.path("abs-dangling-dir"), "", errNotExist},
		{"absolute symlink to a dangling link", h.path("abs-to-dangling"), "", errNotExist},
		{"relative symlink dangling", h.path("rel-dangling"), "", errNotExist},
		{"absolute symlink dangling out", h.path("abs-dangling-out"), "", errEscapes},
		{"absolute symlink loop", h.path("abs-loop"), "", errEscapes},
		{"absolute symlinked directory out", h.path("dir/abs-dir-out/secret"), "", errEscapes},
		{"absolute symlink to the root", h.path("abs-root"), "", errEscapes},
		{"missing", h.path("none"), "", errNotExist},
		{"directory", h.path("dir"), "", errNotRegular},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ref := fileRef(tc.file, "")
			st, diags := h.r.Resolve(context.Background(), []secret.Use{use(ref, secret.KindOpaque)})
			if tc.reason == nil {
				if st == nil {
					t.Fatalf("diagnostics:\n%s", diagText(diags))
				}
				if got := get(t, st, ref); got != tc.want {
					t.Errorf("value = %q, want %q", got, tc.want)
				}
				return
			}
			if st != nil || len(diags) != 1 {
				t.Fatalf("store %v, %d diagnostics; want one failure", st != nil, len(diags))
			}
			if !strings.HasSuffix(diags[0].Message, tc.reason.Error()) {
				t.Errorf("message = %q, want reason %q", diags[0].Message, tc.reason)
			}
			rootRule := errors.Is(tc.reason, errOutsideRoot) || errors.Is(tc.reason, errEscapes)
			if strings.Contains(diagText(diags), "outside") && !rootRule {
				t.Errorf("diagnostic repeats file content: %s", diagText(diags))
			}
			if rootRule && diags[0].Hint != SettingSecretRoot+" is "+h.r.Root() {
				t.Errorf("hint = %q", diags[0].Hint)
			}
		})
	}
}

func TestFileRootThroughSymlink(t *testing.T) {
	// Spec 06 req 88: the root is itself symlink-resolved; a name under
	// either form resolves.
	base := t.TempDir()
	realRoot := filepath.Join(base, "real")
	if err := os.Mkdir(realRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realRoot, "k"), []byte("v"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	mustSymlink(t, realRoot, link)
	h := newHarness(t, func(c *Config) { c.Root = link })
	for _, name := range []string{filepath.ToSlash(filepath.Join(link, "k")), filepath.ToSlash(filepath.Join(realRoot, "k"))} {
		if got := get(t, h.resolve(use(fileRef(name, ""), secret.KindOpaque)), fileRef(name, "")); got != "v" {
			t.Errorf("%s = %q", name, got)
		}
	}
}

func TestFileRootMissing(t *testing.T) {
	// The root need not exist at start; file references then fail.
	h := newHarness(t, func(c *Config) { c.Root = filepath.Join(t.TempDir(), "absent") })
	diags := h.resolveDiags(use(fileRef(h.r.Root()+"/x", ""), secret.KindOpaque))
	if !strings.HasSuffix(diags[0].Message, errRootOpen.Error()) || diags[0].Hint == "" {
		t.Errorf("diagnostic = %+v", diags[0])
	}
}

func TestFileExactBytesAndKeys(t *testing.T) {
	// Spec 01 req 44: without key the exact file bytes (no trimming); with
	// key the JSON string member's UTF-8 bytes. One file serves several
	// keys.
	h := newHarness(t, nil)
	h.write("exact", "line\n")
	doc, _ := json.Marshal(map[string]any{"user": "u", "password": "pé\n", "port": 6379, "empty": ""})
	h.write("creds.json", string(doc))
	cases := map[secret.Ref]string{
		fileRef(h.path("exact"), ""):              "line\n",
		fileRef(h.path("creds.json"), "user"):     "u",
		fileRef(h.path("creds.json"), "password"): "pé\n",
		fileRef(h.path("creds.json"), "empty"):    "",
	}
	var uses []secret.Use
	for ref := range cases {
		uses = append(uses, use(ref, secret.KindOpaque))
	}
	st := h.resolve(uses...)
	for ref, want := range cases {
		v, _ := st.Get(ref)
		if got := string(v.Reveal()); got != want || v.IsZero() {
			t.Errorf("%s = %q (zero %v), want %q", ref, got, v.IsZero(), want)
		}
	}
	// Key errors are fixed messages (spec 06 req 89).
	diags := h.resolveDiags(
		use(fileRef(h.path("creds.json"), "port"), secret.KindOpaque),
		use(fileRef(h.path("creds.json"), "absent"), secret.KindOpaque),
		use(fileRef(h.path("exact"), "k"), secret.KindOpaque),
	)
	text := diagText(diags)
	for _, want := range []error{errJSONNotString, errJSONMissing, errJSONNotObject} {
		if !strings.Contains(text, want.Error()) {
			t.Errorf("missing %q in\n%s", want, text)
		}
	}
	// The messages name the file; its path (a random temporary directory)
	// may contain the probed content by chance, so it is removed first.
	bare := strings.ReplaceAll(text, h.path(""), "<root>")
	bare = strings.ReplaceAll(bare, h.root, "<root>")
	if strings.Contains(bare, "line") || strings.Contains(bare, "6379") {
		t.Errorf("diagnostics repeat file content:\n%s", text)
	}
}

func TestFileSizeCaps(t *testing.T) {
	// Architecture R-9: 4 MiB per value, 16 MiB for CRLs.
	h := newHarness(t, nil)
	h.write("max", strings.Repeat("a", MaxValueBytes))
	h.write("over", strings.Repeat("a", MaxValueBytes+1))
	st := h.resolve(use(fileRef(h.path("max"), ""), secret.KindOpaque))
	if v, _ := st.Get(fileRef(h.path("max"), "")); v.Len() != MaxValueBytes {
		t.Errorf("len = %d", v.Len())
	}
	diags := h.resolveDiags(use(fileRef(h.path("over"), ""), secret.KindOpaque))
	if !strings.Contains(diags[0].Message, errTooLargeValue.Error()) {
		t.Errorf("message = %q", diags[0].Message)
	}
	// A file shared by an opaque and a CRL use is read up to the larger
	// cap: the CRL use is under its 16 MiB cap and fails only its PEM
	// check, while the opaque use still fails its own 4 MiB cap.
	crlUse := use(fileRef(h.path("over"), ""), secret.KindPEMCRL)
	crlUse.Path = diag.Path{diag.Field("crl")}
	diags = h.resolveDiags(crlUse, use(fileRef(h.path("over"), ""), secret.KindOpaque))
	if len(diags) != 2 {
		t.Fatalf("got %d diagnostics:\n%s", len(diags), diagText(diags))
	}
	byPath := map[string]string{}
	for _, d := range diags {
		byPath[d.Path.String()] = d.Message
	}
	if !strings.HasSuffix(byPath["crl"], errNoCRL.Error()) || !strings.Contains(byPath["spec.stateStore.url"], errTooLargeValue.Error()) {
		t.Errorf("messages = %q", byPath)
	}
	h.write("big-crl", strings.Repeat("a", MaxCRLBytes+1))
	diags = h.resolveDiags(use(fileRef(h.path("big-crl"), ""), secret.KindPEMCRL))
	if !strings.Contains(diags[0].Message, "16777216 bytes") {
		t.Errorf("message = %q", diags[0].Message)
	}
	// An env value has the same cap.
	h.setEnv("RURALZ_SECRET_BIG", strings.Repeat("b", MaxValueBytes+1))
	diags = h.resolveDiags(use(envRef("RURALZ_SECRET_BIG"), secret.KindOpaque))
	if !strings.Contains(diags[0].Message, errTooLargeValue.Error()) {
		t.Errorf("env message = %q", diags[0].Message)
	}
}

func TestFileOwnerAndMode(t *testing.T) {
	// WP-07 scope: owner and mode checks. Group- or world-writable files
	// are refused; the owner is checked only against a configured
	// FileOwners list, so files keeping a host owner (Docker Compose
	// bind-mounted secrets) resolve by default.
	h := newHarness(t, nil)
	h.write("ok", "v")
	h.write("writable", "v")
	if err := os.Chmod(filepath.Join(h.root, "writable"), 0o666); err != nil { //nolint:gosec // G302: the test makes the file writable on purpose.
		t.Fatal(err)
	}
	if !ownerModeChecked {
		t.Skip("owner and mode bits are not checked on this platform")
	}
	diags := h.resolveDiags(use(fileRef(h.path("writable"), ""), secret.KindOpaque))
	if !strings.HasSuffix(diags[0].Message, errWritable.Error()) {
		t.Errorf("message = %q", diags[0].Message)
	}
	// Another owner list refuses the test user's file.
	other := newHarness(t, func(c *Config) { c.FileOwners = []int{os.Geteuid() + 1} })
	other.write("f", "v")
	diags = other.resolveDiags(use(fileRef(other.path("f"), ""), secret.KindOpaque))
	if !strings.HasSuffix(diags[0].Message, errOwner.Error()) {
		t.Errorf("message = %q", diags[0].Message)
	}
	// A negative entry never matches.
	neg := newHarness(t, func(c *Config) { c.FileOwners = []int{-1} })
	neg.write("f", "v")
	neg.resolveDiags(use(fileRef(neg.path("f"), ""), secret.KindOpaque))
	// root may hand a file to another user: the default accepts it, a list
	// naming root and the Node's user refuses it.
	if os.Geteuid() == 0 {
		h.write("foreign", "v")
		if err := os.Chown(filepath.Join(h.root, "foreign"), 4242, 4242); err != nil {
			t.Fatal(err)
		}
		if got := get(t, h.resolve(use(fileRef(h.path("foreign"), ""), secret.KindOpaque)), fileRef(h.path("foreign"), "")); got != "v" {
			t.Errorf("value = %q", got)
		}
		strict := newHarness(t, func(c *Config) { c.Root = h.root; c.FileOwners = []int{0, os.Geteuid()} })
		diags = strict.resolveDiags(use(fileRef(h.path("foreign"), ""), secret.KindOpaque))
		if !strings.HasSuffix(diags[0].Message, errOwner.Error()) {
			t.Errorf("message = %q", diags[0].Message)
		}
	}
	h.resolve(use(fileRef(h.path("ok"), ""), secret.KindOpaque))
	// An owner list naming the test user accepts its file.
	own := newHarness(t, func(c *Config) { c.FileOwners = []int{os.Geteuid()} })
	own.write("f", "v")
	own.resolve(use(fileRef(own.path("f"), ""), secret.KindOpaque))
}

func TestFilePermissionDenied(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every file")
	}
	h := newHarness(t, nil)
	h.write("locked", "v")
	if err := os.Chmod(filepath.Join(h.root, "locked"), 0o000); err != nil {
		t.Fatal(err)
	}
	diags := h.resolveDiags(use(fileRef(h.path("locked"), ""), secret.KindOpaque))
	if !strings.HasSuffix(diags[0].Message, errPermission.Error()) {
		t.Errorf("message = %q", diags[0].Message)
	}
}

func TestReuseActiveCell(t *testing.T) {
	// Spec 01 req 46: an unchanged reference keeps its value across Hot
	// Reloads (no re-read) and shares its cell, while a reference the
	// active Store does not hold is read again.
	h := newHarness(t, nil)
	h.write("a", "a1")
	h.write("b", "b1")
	a, b := fileRef(h.path("a"), ""), fileRef(h.path("b"), "")
	s1 := h.resolve(use(a, secret.KindOpaque), use(b, secret.KindOpaque))
	h.r.Activate(s1)
	s1b := h.resolve(use(b, secret.KindOpaque)) // not activated
	h.write("a", "a2")
	h.write("b", "b2")
	h.r.Activate(h.resolve(use(a, secret.KindOpaque)))
	s2 := h.resolve(use(a, secret.KindOpaque), use(b, secret.KindOpaque))
	if got := get(t, s2, a); got != "a1" {
		t.Errorf("active reference re-read: %q", got)
	}
	if got := get(t, s2, b); got != "b2" {
		t.Errorf("inactive reference not re-read: %q", got)
	}
	if s2.(*store).cells[a] != s1.(*store).cells[a] {
		t.Error("active cell not shared")
	}
	if s2.(*store).cells[b] == s1b.(*store).cells[b] {
		t.Error("inactive cell reused")
	}
	// A reused value is checked against the new uses.
	diags := h.resolveDiags(use(a, secret.KindAPIKey))
	if !strings.HasSuffix(diags[0].Message, errShortAPIKey.Error()) {
		t.Errorf("message = %q", diags[0].Message)
	}
	// Env references are reused the same way.
	h.setEnv("RURALZ_SECRET_E", "e1")
	e := envRef("RURALZ_SECRET_E")
	h.r.Activate(h.resolve(use(e, secret.KindOpaque)))
	h.setEnv("RURALZ_SECRET_E", "e2")
	if got := get(t, h.resolve(use(e, secret.KindOpaque)), e); got != "e1" {
		t.Errorf("env re-read: %q", got)
	}
}

func TestConsumerCheck(t *testing.T) {
	// Spec 01 req 44: a failed consumer check is 026; its reason is kept
	// unless it repeats the value; the check sees a copy.
	h := newHarness(t, nil)
	h.setEnv(EnvStateStoreURL, "redis://user:hunter2-password@cache.internal:6379")
	ref := envRef(EnvStateStoreURL)
	topology := use(ref, secret.KindStateStoreURL)
	topology.Check = func(b []byte) error {
		b[0] = 'X' // a copy: the stored value is untouched
		return errors.New("addr is not allowed under topology standalone")
	}
	leaky := use(ref, secret.KindOpaque)
	leaky.Path = diag.Path{diag.Field("leaky")}
	leaky.Check = func(b []byte) error { return errors.New("bad url " + string(b)) }
	diags := h.resolveDiags(topology, leaky)
	text := diagText(diags)
	if !strings.Contains(text, "addr is not allowed under topology standalone") {
		t.Errorf("consumer reason dropped:\n%s", text)
	}
	if !strings.Contains(text, errWithheld.Error()) || strings.Contains(text, "hunter2") {
		t.Errorf("leaking reason not withheld:\n%s", text)
	}
	ok := use(ref, secret.KindStateStoreURL)
	ok.Check = func([]byte) error { return nil }
	if got := get(t, h.resolve(ok), ref); !strings.HasPrefix(got, "redis://") {
		t.Errorf("value = %q", got)
	}
}

func TestResolveWithRetry(t *testing.T) {
	// CM "secretRef" and spec 06 req 87 rule 4: a cold start retries with
	// backoff until every reference resolves.
	h := newHarness(t, func(c *Config) { c.RetryInitial = time.Second; c.RetryMax = 4 * time.Second })
	ref := envRef("RURALZ_SECRET_LATE")
	type attempt struct {
		n     int
		diags int
		next  time.Duration
	}
	attempts := make(chan attempt, 16)
	type result struct {
		st  secret.Store
		err error
	}
	done := make(chan result, 1)
	go func() {
		st, _, err := h.r.ResolveWithRetry(context.Background(), []secret.Use{use(ref, secret.KindOpaque)},
			func(n int, d diag.List, next time.Duration) { attempts <- attempt{n, len(d), next} })
		done <- result{st, err}
	}()
	wantNext := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second}
	for i, want := range wantNext {
		a := <-attempts
		if a.n != i+1 || a.diags != 1 || a.next != want {
			t.Fatalf("attempt %+v, want n=%d next=%v", a, i+1, want)
		}
		waitTimer(t, h)
		if i == len(wantNext)-1 {
			h.setEnv("RURALZ_SECRET_LATE", "arrived")
		}
		h.clock.Advance(a.next)
	}
	res := <-done
	if res.err != nil || res.st == nil {
		t.Fatalf("ResolveWithRetry = %v, %v", res.st, res.err)
	}
	if got := get(t, res.st, ref); got != "arrived" {
		t.Errorf("value = %q", got)
	}
}

func TestResolveWithRetryCanceled(t *testing.T) {
	h := newHarness(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	u := use(envRef("RURALZ_SECRET_NEVER"), secret.KindOpaque)
	done := make(chan error, 1)
	var last diag.List
	go func() {
		var err error
		_, last, err = h.r.ResolveWithRetry(ctx, []secret.Use{u}, nil)
		done <- err
	}()
	waitTimer(t, h)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if len(last) != 1 {
		t.Errorf("last diagnostics = %d", len(last))
	}
	// Already canceled: no attempt sleeps.
	if _, d, err := h.r.ResolveWithRetry(ctx, []secret.Use{u}, nil); !errors.Is(err, context.Canceled) || len(d) != 1 {
		t.Errorf("canceled = %v, %d", err, len(d))
	}
}

// waitTimer waits until the fake clock has an armed timer.
func waitTimer(t *testing.T, h *harness) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for h.clock.Pending() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for a timer")
		}
		time.Sleep(time.Millisecond)
	}
}

// mustSymlink creates a symbolic link or fails.
func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func TestReadLimited(t *testing.T) {
	// The buffer grows by copying; more than limit bytes fails.
	for _, n := range []int{0, 1, 511, 512, 513, 5000} {
		got, err := readLimited(bytes.NewReader(bytes.Repeat([]byte{'x'}, n)), 0, 1<<20)
		if err != nil || len(got) != n {
			t.Errorf("n=%d: len %d err %v", n, len(got), err)
		}
	}
	if _, err := readLimited(bytes.NewReader(make([]byte, 101)), 100, 100); !errors.Is(err, errTooLargeValue) {
		t.Errorf("over limit = %v", err)
	}
	if _, err := readLimited(bytes.NewReader(make([]byte, 5000)), 10, 1000); !errors.Is(err, errTooLargeValue) {
		t.Errorf("growing over limit = %v", err)
	}
	if _, err := readLimited(errReader{}, 10, 100); !errors.Is(err, errReadFailed) {
		t.Errorf("read error = %v", err)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, syscall.EIO }

func TestFileErrorMapping(t *testing.T) {
	tests := []struct {
		in   error
		want error
	}{
		{os.ErrNotExist, errNotExist},
		{os.ErrPermission, errPermission},
		{&os.PathError{Op: "statat", Path: "x", Err: errors.New(escapeText)}, errEscapes},
		{&os.PathError{Op: "open", Path: "x", Err: syscall.ELOOP}, errReadFailed},
		{errors.New("other"), errReadFailed},
	}
	for _, tc := range tests {
		if got := fileError(tc.in); !errors.Is(got, tc.want) {
			t.Errorf("fileError(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
