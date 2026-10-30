// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package bundle

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/cli/command"
	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/problem"
)

// Tests of ruralz bundle diff (docs/reference/01-cli-and-api-surface.md
// "Bundle verbs in detail", "Diff JSON compatibility";
// docs/architecture/02-configuration-model.md "Diff semantics").

// fourSeconds is simpleBundle with the Route timeout 4s instead of 5s.
func fourSeconds() map[string]string {
	files := simpleBundle()
	files["routes/orders.yaml"] = routeDoc("orders", "orders", "  timeout: 4s\n")
	return files
}

// timeoutDiff is the human diff of a Route timeout changed from 5s to 4s
// between from and to, with the header's suffix env.
func timeoutDiff(from, to revision.Digest, env string) string {
	return "ruralz bundle diff: " + from.Short() + " -> " + to.Short() + env + "\n\n" +
		"~ Route/orders                                  routes/orders.yaml           [traffic]\n" +
		"    ~ spec.timeout: 5s -> 4s\n\n" +
		"1 resource changed: 0 added, 0 removed, 1 modified; 0 Route chains changed; impact: traffic\n"
}

// diffJSON is the part of ruralz.diff.v1 the tests read.
type diffJSON struct {
	Format string            `json:"format"`
	From   map[string]string `json:"from"`
	To     map[string]string `json:"to"`
	Env    string            `json:"environment"`
	Res    []map[string]any  `json:"resources"`
}

// decodeDiff decodes a ruralz.diff.v1 document.
func decodeDiff(t *testing.T, s string) diffJSON {
	t.Helper()
	var d diffJSON
	if err := json.Unmarshal([]byte(s), &d); err != nil {
		t.Fatalf("not a ruralz.diff.v1 document: %v\n%s", err, s)
	}
	if d.Format != "ruralz.diff.v1" {
		t.Fatalf("format = %q", d.Format)
	}
	return d
}

// TestDiffDirectories: two Bundle directories compare as validated
// states; a change exits 1 with the human diff, or ruralz.diff.v1 whose
// sides carry the member bundle and the full digest.
func TestDiffDirectories(t *testing.T) {
	a, b := writeBundle(t, simpleBundle()), writeBundle(t, fourSeconds())
	ra, rb := build(t, a, "", "").Digest, build(t, b, "", "").Digest
	run(t, "bundle", "diff", a, b).want(t, "diff", command.ExitNegative, timeoutDiff(ra, rb, ""), "")
	r := run(t, "bundle", "diff", "--output", "json", a, b)
	r.wantCode(t, "diff --output json", command.ExitNegative)
	d := decodeDiff(t, r.stdout)
	if d.From["bundle"] != a || d.From["digest"] != ra.String() || d.From["revision"] != ra.Short() ||
		d.To["bundle"] != b || d.To["digest"] != rb.String() || d.Env != "" || len(d.Res) != 1 {
		t.Errorf("diff JSON = %+v", d)
	}
	if !strings.HasPrefix(r.stdout, "{\n  \"format\": \"ruralz.diff.v1\",\n") {
		t.Errorf("JSON is not indented by two spaces:\n%s", r.stdout)
	}
}

// TestDiffNoChanges: equal states exit 0 with the header and a summary
// whose impact reads none.
func TestDiffNoChanges(t *testing.T) {
	a := writeBundle(t, simpleBundle())
	d := build(t, a, "", "").Digest
	run(t, "bundle", "diff", a, a).want(t, "diff", command.ExitOK, "ruralz bundle diff: "+d.Short()+" -> "+d.Short()+"\n\n"+
		"0 resources changed: 0 added, 0 removed, 0 modified; 0 Route chains changed; impact: none\n", "")
}

// TestDiffEnvironments: --env renders both sides for one Environment,
// which the header and the JSON member environment name; --from-env and
// --to-env stand in for it per side, reviewing a promotion.
func TestDiffEnvironments(t *testing.T) {
	dir := writeBundle(t, simpleBundle())
	envs := envFile(t)
	staging, prod := build(t, dir, envs, "staging").Digest, build(t, dir, envs, "prod").Digest
	same := run(t, "bundle", "diff", "--env", "prod", "--environments", envs, dir, dir)
	same.want(t, "diff --env prod", command.ExitOK, "ruralz bundle diff: "+prod.Short()+" -> "+prod.Short()+" (environment: prod)\n\n"+
		"0 resources changed: 0 added, 0 removed, 0 modified; 0 Route chains changed; impact: none\n", "")
	r := run(t, "bundle", "diff", "--from-env", "staging", "--to-env", "prod", "--environments", envs, dir, dir)
	r.wantCode(t, "diff --from-env staging --to-env prod", command.ExitNegative)
	if !strings.HasPrefix(r.stdout, "ruralz bundle diff: "+staging.Short()+" -> "+prod.Short()+" (environment: staging -> prod)\n") ||
		!strings.Contains(r.stdout, "    ~ spec.timeout: 3s -> 4s\n") {
		t.Errorf("promotion diff:\n%s", r.stdout)
	}
	r = run(t, "bundle", "diff", "--output", "json", "--from-env", "staging", "--to-env", "prod", "--environments", envs, dir, dir)
	d := decodeDiff(t, r.stdout)
	if d.Env != "" || d.From["environment"] != "staging" || d.To["environment"] != "prod" {
		t.Errorf("promotion JSON = %+v", d)
	}
	r = run(t, "bundle", "diff", "--output", "json", "--env", "prod", "--environments", envs, dir, dir)
	if d := decodeDiff(t, r.stdout); d.Env != "prod" || d.From["environment"] != "" {
		t.Errorf("one-Environment JSON = %+v", d)
	}
}

// TestDiffEnvironmentRules: --env with --from-env or --to-env is a usage
// error, an Environment flag needs --environments, Environment flags are a
// usage error on a dump or admin side, and with --environments every
// Bundle side needs an Environment.
func TestDiffEnvironmentRules(t *testing.T) {
	dir := writeBundle(t, simpleBundle())
	envs := envFile(t)
	dump := writeFile(t, "dump.json", string(dumpOf(t, build(t, dir, "", ""), revision.Digest{})))
	const admin = "http://127.0.0.1:1"
	for _, tc := range []struct {
		args []string
		msg  string
	}{
		{[]string{"--env", "prod", "--from-env", "staging", "--environments", envs, dir, dir},
			"--env names the Environment of both sides; give it or --from-env and --to-env, not both"},
		{[]string{"--from-env", "prod", dir, dir}, "--from-env needs --environments: fetching Environments from Ruralz Control is Planned (M2)"},
		{[]string{"--to-env", "", "--environments", envs, dir, dir}, "--to-env needs an Environment name"},
		{[]string{"--environments", envs, dir, dir}, "--environments needs an Environment for FROM: give --env or --from-env"},
		{[]string{"--from-env", "prod", "--environments", envs, dir, dir}, "--environments needs an Environment for TO: give --env or --to-env"},
		{[]string{"--from-env", "prod", "--environments", envs, dump, dir},
			"--from-env names the Environment of a Bundle side, and FROM " + dump + " is a /config/dump document"},
		{[]string{"--env", "prod", "--environments", envs, admin, dir}, "--env names the Environment of a Bundle side, and FROM is an admin URL"},
		{[]string{"--to-env", "prod", "--environments", envs, dir, admin}, "--to-env names the Environment of a Bundle side, and TO is an admin URL"},
	} {
		r := run(t, append([]string{"bundle", "diff"}, tc.args...)...)
		r.want(t, strings.Join(tc.args, " "), command.ExitNoResult, "",
			"ruralz bundle diff: "+tc.msg+"\nRun 'ruralz bundle diff --help' for usage.\n")
	}
	run(t, "bundle", "diff", "--to-env", "prod", "--environments", envs, dump, dir).wantCode(t, "--to-env on the Bundle side of a dump", command.ExitNegative)
}

// TestDiffRevisionSources: a bare rev-<12 hex> or sha256:<64 hex> is a
// Revision and an oci:// source an OCI Revision, each Planned (M2), exit
// 2; anything else is a path, so ./rev-<12 hex> names a directory.
func TestDiffRevisionSources(t *testing.T) {
	dir := writeBundle(t, simpleBundle())
	digest := "sha256:68f782530463d5c1f0e2b9a7c4d8e6f1a3b5c7d9e0f2a4b6c8d0e2f4a6b8c0d2"
	for _, tc := range []struct{ from, msg string }{
		{"rev-162af81f5de4", "FROM rev-162af81f5de4: a Revision source is Planned (M2) and not in this build"},
		{digest, "FROM " + digest + ": a Revision source is Planned (M2) and not in this build"},
		{"oci://registry.example/shop@" + digest, "FROM oci://registry.example/shop@" + digest + ": an OCI Revision source is Planned (M2) and not in this build"},
	} {
		run(t, "bundle", "diff", tc.from, dir).want(t, tc.from, command.ExitNoResult, "", "ruralz bundle diff: "+tc.msg+"\n")
	}
	run(t, "bundle", "diff", dir, "rev-162af81f5de4").want(t, "TO rev", command.ExitNoResult, "",
		"ruralz bundle diff: TO rev-162af81f5de4: a Revision source is Planned (M2) and not in this build\n")
	parent := t.TempDir()
	revDir := filepath.Join(parent, "rev-162af81f5de4")
	writeFiles(t, revDir, simpleBundle())
	t.Chdir(parent)
	run(t, "bundle", "diff", "./rev-162af81f5de4", dir).wantCode(t, "./rev- directory", command.ExitOK)
	for _, p := range []string{"rev-162AF81F5DE4", "rev-123", "sha256:abc"} {
		r := run(t, "bundle", "diff", p, dir)
		if r.code != command.ExitNoResult || !strings.HasPrefix(r.stderr, "ruralz bundle diff: cannot read FROM: ") {
			t.Errorf("%s = exit %d, stderr %q; want a path that cannot be read", p, r.code, r.stderr)
		}
	}
}

// TestClassify: the source forms of FROM and TO.
func TestClassify(t *testing.T) {
	for arg, want := range map[string]sourceKind{
		"http://127.0.0.1:9901":             kindAdmin,
		"HTTPS://node-a:9901":               kindAdmin,
		"oci://r.example/x@sha":             kindOCI,
		"rev-162af81f5de4":                  kindRevision,
		"rev-162af81f5de":                   kindPath,
		"./rev-162af81f5de4":                kindPath,
		"sha256:" + strings.Repeat("a", 64): kindRevision,
		"sha256:" + strings.Repeat("A", 64): kindPath,
		"-":                                 kindPath,
		"bundle":                            kindPath,
	} {
		if got := classify(arg); got != want {
			t.Errorf("classify(%q) = %d, want %d", arg, got, want)
		}
	}
}

// TestDiffStandardInput: one side "-" reads standard input as a one-file
// Bundle or a saved dump; both sides "-" is a usage error.
func TestDiffStandardInput(t *testing.T) {
	dir := writeBundle(t, simpleBundle())
	rendered := run(t, "bundle", "render", dir).stdout
	execute(t, withStdin(testOptions(), rendered), nil, "bundle", "diff", "-", dir).wantCode(t, "rendered Bundle on standard input", command.ExitOK)
	dump := string(dumpOf(t, build(t, dir, "", ""), revision.Digest{}))
	r := execute(t, withStdin(testOptions(), dump), nil, "bundle", "diff", "--output", "json", dir, "-")
	r.wantCode(t, "dump on standard input", command.ExitOK)
	if d := decodeDiff(t, r.stdout); d.To["dump"] != "-" || d.From["bundle"] != dir {
		t.Errorf("diff JSON sides = %v, %v", d.From, d.To)
	}
	execute(t, withStdin(testOptions(), dump), nil, "bundle", "diff", "-", "-").want(t, "- -", command.ExitNoResult, "",
		"ruralz bundle diff: FROM and TO both read standard input; at most one side may be -\nRun 'ruralz bundle diff --help' for usage.\n")
}

// TestDiffFiles: a file that holds a /config/dump document is a saved
// dump, JSON member dump; any other file a rendered Bundle, member bundle.
func TestDiffFiles(t *testing.T) {
	a, b := writeBundle(t, simpleBundle()), writeBundle(t, fourSeconds())
	revA, rb := build(t, a, "", ""), build(t, b, "", "").Digest
	dump := writeFile(t, "node-a.json", "\n "+string(dumpOf(t, revA, revision.Sum([]byte("lkg")))))
	rendered := writeFile(t, "rendered.yaml", run(t, "bundle", "render", a).stdout)
	for _, tc := range []struct{ file, member string }{{dump, "dump"}, {rendered, "bundle"}} {
		r := run(t, "bundle", "diff", "--output", "json", tc.file, b)
		r.wantCode(t, tc.member, command.ExitNegative)
		d := decodeDiff(t, r.stdout)
		if d.From[tc.member] != tc.file || d.From["digest"] != revA.Digest.String() || d.To["digest"] != rb.String() {
			t.Errorf("%s side JSON = %v", tc.member, d.From)
		}
	}
	run(t, "bundle", "diff", dump, b).want(t, "dump vs directory", command.ExitNegative, timeoutDiff(revA.Digest, rb, ""), "")
	jsonBundle := writeFile(t, "bundle.json", `{"apiVersion": "ruralz/v1alpha1", "kind": "Gateway", "metadata": {"name": "edge"},
"spec": {"listeners": [{"name": "http", "protocol": "http", "port": 8080}]}}`)
	r := run(t, "bundle", "diff", "--output", "json", jsonBundle, jsonBundle)
	r.wantCode(t, "JSON one-file Bundle", command.ExitOK)
	if d := decodeDiff(t, r.stdout); d.From["bundle"] != jsonBundle {
		t.Errorf("a JSON one-file Bundle is not a bundle side: %v", d.From)
	}
}

// TestDiffRenderedFileNamesEnvironment: on a rendered-file side an
// Environment flag substitutes nothing new but names its Environment: the
// prod render compared under staging keeps its Revision.
func TestDiffRenderedFileNamesEnvironment(t *testing.T) {
	dir := writeBundle(t, simpleBundle())
	envs := envFile(t)
	prod := build(t, dir, envs, "prod").Digest
	rendered := writeFile(t, "prod.yaml", run(t, "bundle", "render", "--env", "prod", "--environments", envs, dir).stdout)
	r := run(t, "bundle", "diff", "--from-env", "staging", "--to-env", "prod", "--environments", envs, rendered, dir)
	r.want(t, "rendered prod file under staging", command.ExitOK, "ruralz bundle diff: "+prod.Short()+" -> "+prod.Short()+
		" (environment: staging -> prod)\n\n0 resources changed: 0 added, 0 removed, 0 modified; 0 Route chains changed; impact: none\n", "")
}

// TestDiffTamperedDump: a dump whose content does not hash to its digest
// fails its side with RZ-CFG-027: the diagnostic prints after "from: ",
// with --output json also in one array whose entries name their side, and
// the diff exits 2.
func TestDiffTamperedDump(t *testing.T) {
	dir := writeBundle(t, simpleBundle())
	good := string(dumpOf(t, build(t, dir, "", ""), revision.Digest{}))
	dump := writeFile(t, "tampered.json", strings.Replace(good, `"port":8080`, `"port":8081`, 1))
	r := run(t, "bundle", "diff", dump, dir)
	r.wantCode(t, "tampered dump", command.ExitNoResult)
	if !strings.HasPrefix(r.stderr, "from: "+filepath.ToSlash(dump)+" error RZ-CFG-027: content hashes to ") || r.stdout != "" {
		t.Errorf("stdout %q, stderr %q", r.stdout, r.stderr)
	}
	r = run(t, "bundle", "diff", "--output", "json", dump, dir)
	r.wantCode(t, "tampered dump, JSON", command.ExitNoResult)
	var ds []map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &ds); err != nil || len(ds) != 1 || ds[0]["code"] != "RZ-CFG-027" || ds[0]["side"] != "from" {
		t.Errorf("stdout = %q (%v), want one RZ-CFG-027 entry with side from", r.stdout, err)
	}
}

// TestDiffSideDiagnostics: a side that fails its pipeline run prints its
// diagnostics after its prefix and exits 2, with --output json also as one
// array whose entries end with side; warnings print and the diff goes on.
func TestDiffSideDiagnostics(t *testing.T) {
	good, bad := writeBundle(t, simpleBundle()), writeBundle(t, typoBundle())
	run(t, "bundle", "diff", good, bad).want(t, "invalid TO", command.ExitNoResult, "", "to: "+typoLine+"\n")
	r := run(t, "bundle", "diff", "--output", "json", bad, bad)
	r.wantCode(t, "invalid sides, JSON", command.ExitNoResult)
	if r.stderr != "from: "+typoLine+"\nto: "+typoLine+"\n" {
		t.Errorf("stderr = %q", r.stderr)
	}
	var ds []map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &ds); err != nil || len(ds) != 2 || ds[0]["side"] != "from" || ds[1]["side"] != "to" {
		t.Fatalf("stdout = %q (%v)", r.stdout, err)
	}
	if !strings.Contains(r.stdout, "\n    \"side\": \"from\"\n  },\n") || !strings.HasSuffix(r.stdout, "\n    \"side\": \"to\"\n  }\n]\n") {
		t.Errorf("side is not the last member of each entry:\n%s", r.stdout)
	}
	warn := writeBundle(t, warningBundle())
	r = run(t, "bundle", "diff", warn, good)
	r.wantCode(t, "warnings on FROM", command.ExitOK)
	if !strings.HasPrefix(r.stderr, "from: routes/orders.yaml:") || !strings.Contains(r.stderr, " warning RZ-CFG-013 ") ||
		!strings.HasPrefix(r.stdout, "ruralz bundle diff: ") {
		t.Errorf("stdout %q, stderr %q", r.stdout, r.stderr)
	}
}

// TestDiffUnreadableSource: a missing path, or one neither a directory nor
// a regular file, exits 2.
func TestDiffUnreadableSource(t *testing.T) {
	dir := writeBundle(t, simpleBundle())
	missing := filepath.Join(t.TempDir(), "missing")
	r := run(t, "bundle", "diff", dir, missing)
	if r.code != command.ExitNoResult || !strings.HasPrefix(r.stderr, "ruralz bundle diff: cannot read TO: ") {
		t.Errorf("missing TO = exit %d, stderr %q", r.code, r.stderr)
	}
	o := testOptions()
	o.Stdin = failingReader{}
	execute(t, o, nil, "bundle", "diff", "-", dir).want(t, "failing standard input", command.ExitNoResult, "",
		"ruralz bundle diff: FROM: read standard input: unexpected EOF\n")
}

// TestDiffColors: on a terminal that takes color each changed line takes
// the color of its change, and NO_COLOR leaves the diff plain.
func TestDiffColors(t *testing.T) {
	a, b := writeBundle(t, simpleBundle()), writeBundle(t, fourSeconds())
	ra, rb := build(t, a, "", "").Digest, build(t, b, "", "").Digest
	o := testOptions()
	o.terminal = func(io.Writer) bool { return true }
	want := "ruralz bundle diff: " + ra.Short() + " -> " + rb.Short() + "\n\n" +
		ansiYellow + "~ Route/orders                                  routes/orders.yaml           [traffic]" + ansiReset + "\n" +
		ansiYellow + "    ~ spec.timeout: 5s -> 4s" + ansiReset + "\n\n" +
		"1 resource changed: 0 added, 0 removed, 1 modified; 0 Route chains changed; impact: traffic\n"
	execute(t, o, nil, "bundle", "diff", a, b).want(t, "colored", command.ExitNegative, want, "")
	execute(t, o, []string{"NO_COLOR=yes"}, "bundle", "diff", a, b).want(t, "NO_COLOR", command.ExitNegative, timeoutDiff(ra, rb, ""), "")
	if r := execute(t, o, nil, "bundle", "diff", "--output", "json", a, b); strings.Contains(r.stdout, "\x1b") {
		t.Error("JSON is colored")
	}
}

// testToken is an operator token canary of RFC 6750 b64token characters.
//
//nolint:gosec // G101: a test canary, not a credential.
const testToken = "op-canary-bundle-7Hq2Lx9Zp4Wm1Rt8"

// otherToken is a second operator token canary, another Node's.
//
//nolint:gosec // G101: a test canary, not a credential.
const otherToken = "op-canary-other-Zp4Wm1Rt87Hq2Lx9"

// adminServer is an admin endpoint that answers /config/dump with body,
// or with a problem document of code when code is set, and refuses a
// request without the bearer token when token is set.
type adminServer struct {
	*httptest.Server
	mu    sync.Mutex
	paths []string
}

func newAdminServer(t *testing.T, body []byte, code, token string) *adminServer {
	t.Helper()
	a := &adminServer{}
	a.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.paths = append(a.paths, r.URL.Path)
		a.mu.Unlock()
		id := traceID(r.Header.Get("Traceparent"))
		switch {
		case token != "" && r.Header.Get("Authorization") != "Bearer "+token:
			problem.Write(w, problem.New("RZ-AUTH-001", id), nil)
		case code != "":
			problem.Write(w, problem.New(code, id), nil)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(body)
		}
	}))
	t.Cleanup(a.Close)
	return a
}

// requests returns the paths the server saw.
func (a *adminServer) requests() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.paths...)
}

// traceID returns the trace ID of a version 00 traceparent, the requestId
// of a Node's problem documents.
func traceID(tp string) string {
	m := regexp.MustCompile(`^00-([0-9a-f]{32})-[0-9a-f]{16}-[0-9a-f]{2}$`).FindStringSubmatch(tp)
	if m == nil {
		return ""
	}
	return m[1]
}

// TestDiffAdmin: an admin URL is a live Node's /config/dump, JSON member
// admin, read with per-side credentials that fall back to the shared
// --admin-token-file.
func TestDiffAdmin(t *testing.T) {
	a, b := writeBundle(t, simpleBundle()), writeBundle(t, fourSeconds())
	revA, revB := build(t, a, "", ""), build(t, b, "", "")
	nodeA := newAdminServer(t, dumpOf(t, revA, revision.Digest{}), "", testToken)
	nodeB := newAdminServer(t, dumpOf(t, revB, revision.Digest{}), "", otherToken)
	tokA, tokB := writeFile(t, "a.token", testToken+"\n"), writeFile(t, "b.token", otherToken+"\n")

	r := run(t, "bundle", "diff", "--output", "json", "--from-admin-token-file", tokA, "--to-admin-token-file", tokB, nodeA.URL, nodeB.URL)
	r.wantCode(t, "two Nodes", command.ExitNegative)
	d := decodeDiff(t, r.stdout)
	if d.From["admin"] != nodeA.URL || d.To["admin"] != nodeB.URL || d.From["digest"] != revA.Digest.String() || d.To["digest"] != revB.Digest.String() {
		t.Errorf("Node sides = %v, %v", d.From, d.To)
	}
	if got := nodeA.requests(); len(got) != 1 || got[0] != "/config/dump" {
		t.Errorf("node A saw %q", got)
	}
	run(t, "bundle", "diff", "--admin-token-file", tokA, nodeA.URL, a).wantCode(t, "shared token", command.ExitOK)
	run(t, "bundle", "diff", "--admin-token-file", tokB, "--from-admin-token-file", tokA, nodeA.URL, b).
		want(t, "side token over shared", command.ExitNegative, timeoutDiff(revA.Digest, revB.Digest, ""), "")
	r = run(t, "bundle", "diff", "--admin-token-file", tokB, nodeA.URL, a)
	if r.code != command.ExitNoResult || !strings.Contains(r.stderr, "401 RZ-AUTH-001") {
		t.Errorf("wrong token = exit %d, stderr %q", r.code, r.stderr)
	}
	r = execute(t, testOptions(), []string{"RURALZ_ADMIN_TOKEN_FILE=" + tokA}, "bundle", "diff", nodeA.URL, a)
	r.wantCode(t, "token from RURALZ_ADMIN_TOKEN_FILE", command.ExitOK)
}

// TestDiffAdminProblem: a Node with no active Revision answers 503
// RZ-RT-024: exit 2 with the problem line, and with --output json the
// problem document on standard output.
func TestDiffAdminProblem(t *testing.T) {
	dir := writeBundle(t, simpleBundle())
	node := newAdminServer(t, nil, "RZ-RT-024", "")
	r := run(t, "bundle", "diff", node.URL, dir)
	if r.code != command.ExitNoResult || !strings.HasPrefix(r.stderr, "ruralz bundle diff: 503 RZ-RT-024 ") || r.stdout != "" {
		t.Errorf("diff = exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	r = run(t, "bundle", "diff", "--output", "json", node.URL, dir)
	if r.code != command.ExitNoResult || !strings.Contains(r.stdout, `"code":"RZ-RT-024"`) {
		t.Errorf("diff --output json = exit %d, stdout %q", r.code, r.stdout)
	}
}

// TestDiffAdminNotDump: a Node that answers with something other than a
// /config/dump document exits 2; one whose dump fails its check fails its
// side with the check's findings.
func TestDiffAdminNotDump(t *testing.T) {
	dir := writeBundle(t, simpleBundle())
	node := newAdminServer(t, []byte(`{"content": 1}`), "", "")
	r := run(t, "bundle", "diff", node.URL, dir)
	want := "ruralz bundle diff: FROM " + node.URL + ": GET " + node.URL + "/config/dump answered with no /config/dump document\n"
	r.want(t, "not a dump", command.ExitNoResult, "", want)
	good := string(dumpOf(t, build(t, dir, "", ""), revision.Digest{}))
	tampered := newAdminServer(t, []byte(strings.Replace(good, `"port":8080`, `"port":8081`, 1)), "", "")
	r = run(t, "bundle", "diff", dir, tampered.URL)
	if r.code != command.ExitNoResult || !strings.HasPrefix(r.stderr, "to: "+tampered.URL+"/config/dump error RZ-CFG-027: content hashes to ") {
		t.Errorf("tampered dump = exit %d, stderr %q", r.code, r.stderr)
	}
}

// TestDiffAdminUsage: an admin URL or credentials the admin client refuses
// exit 2 before anything is read, standard input included.
func TestDiffAdminUsage(t *testing.T) {
	o := testOptions()
	o.Stdin = failingReader{}
	r := execute(t, o, nil, "bundle", "diff", "-", "http://127.0.0.1:9901/?x=1")
	r.want(t, "admin URL with a query", command.ExitNoResult, "",
		"ruralz bundle diff: TO \"http://127.0.0.1:9901/?x=1\": an admin URL takes no query\nRun 'ruralz bundle diff --help' for usage.\n")
	r = execute(t, o, nil, "bundle", "diff", "--from-client-cert", "c.pem", "http://127.0.0.1:9901", "-")
	r.want(t, "client certificate without key", command.ExitNoResult, "",
		"ruralz bundle diff: --from-client-cert and --from-client-key come together\nRun 'ruralz bundle diff --help' for usage.\n")
}

// TestDiffAdminTimeout: --timeout bounds each admin call on the injected
// clock, and an expired deadline exits 2 naming it.
func TestDiffAdminTimeout(t *testing.T) {
	arrived := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	dir := writeBundle(t, simpleBundle())
	clk := clocktest.New(epoch())
	o := testOptions()
	o.Clock = clk
	done := make(chan result, 1)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { done <- executeCtx(ctx, o, nil, "bundle", "diff", "--timeout", "1s", srv.URL, dir) }()
	select {
	case <-arrived:
	case <-time.After(30 * time.Second):
		t.Fatal("the request did not arrive")
	}
	clk.Advance(time.Second)
	select {
	case r := <-done:
		r.want(t, "diff --timeout 1s", command.ExitNoResult, "",
			"ruralz bundle diff: GET "+srv.URL+"/config/dump: not complete within 1s (--timeout)\n")
	case <-time.After(30 * time.Second):
		t.Fatal("the command did not return")
	}
}

// TestDiffDumpFileLimit: a dump file is read to the /config/dump bound,
// and a file of another kind only to its first byte, the loader reading
// it.
func TestDiffDumpFileLimit(t *testing.T) {
	yaml := writeFile(t, "r.yaml", "# not JSON\n"+gatewayDoc)
	if _, err := readObjectFile(yaml, 1<<20); !errors.Is(err, errNotObject) {
		t.Errorf("readObjectFile of YAML = %v, want errNotObject", err)
	}
	obj := writeFile(t, "o.json", "  {\"a\": 1}")
	got, err := readObjectFile(obj, 4)
	if err != nil || string(got) != "  {\"a" {
		t.Errorf("readObjectFile at limit 4 = %q, %v; want the first 5 bytes", got, err)
	}
	if _, err := readObjectFile(filepath.Join(t.TempDir(), "missing"), 4); !os.IsNotExist(err) {
		t.Errorf("readObjectFile of a missing file = %v", err)
	}
}
