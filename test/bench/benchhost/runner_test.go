// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package benchhost

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The fixed clock of the runner tests: every stand-in date reads it.
const (
	epoch     = "1800000000"
	appID     = "123456"
	repo      = "ravindu-rev/ruralz"
	runnerDir = "/opt/ruralz-bench/actions-runner"
	// runsDir holds the runners' copies of the install; hookPath is their
	// job-started hook.
	runsDir  = "/var/lib/ruralz-bench-runner"
	hookPath = "/usr/local/lib/ruralz-bench/job-started.sh"
)

// appKey writes a GitHub App private key, PKCS #1 as GitHub issues it,
// and returns its path and public key.
func appKey(t *testing.T) (string, *rsa.PublicKey) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}), 0o600); err != nil {
		t.Fatal(err)
	}
	return p, &k.PublicKey
}

// runnerStandins answer the GitHub API as GitHub does for an App
// installed on the repository, recording each header curl reads from a
// file as a curl-header line, run commands as runuser does, and fix the
// clock.
func runnerStandins() map[string]string {
	return map[string]string{
		"date": `echo ` + epoch + "\n",
		"id":   `case "$1" in -u) echo ` + userUID + ` ;; -gn) echo "$2" ;; *) exit 1 ;; esac` + "\n",
		"curl": `for a in "$@"; do
  if [[ $a == @* ]]; then
    while IFS= read -r h; do printf 'curl-header\t%s\n' "$h" >>"$STUB_STATE/calls"; done <"${a#@}"
  fi
done
url=${*: -1}
case "$url" in
  */repos/` + repo + `/installation | */orgs/ravindu-rev/installation) echo '{"id":42}' ;;
  */app/installations/42/access_tokens) echo '{"token":"ghs_installation"}' ;;
  */actions/runners/generate-jitconfig) echo '{"runner":{"id":7},"encoded_jit_config":"JITCONFIG"}' ;;
  *) exit 22 ;;
esac
`,
		"runuser": runuserStandin,
		"chown":   "",
		"kill":    "",
		"logger":  "",
	}
}

// fakeRunner is the run.sh of root's actions/runner install: it records
// where it runs from, its arguments and the environment it runs with.
const fakeRunner = `#!/usr/bin/env bash
{
  printf 'dir\t%s\n' "${0%/run.sh}"
  printf 'args\t%s\n' "$*"
  printf 'jitconfig\t%s\n' "${ACTIONS_RUNNER_INPUT_JITCONFIG:-}"
  printf 'hook\t%s\n' "${ACTIONS_RUNNER_HOOK_JOB_STARTED:-}"
  printf 'xdg\t%s\n' "${XDG_RUNTIME_DIR:-}"
  printf 'bus\t%s\n' "${DBUS_SESSION_BUS_ADDRESS:-}"
} >"$STUB_STATE/runner"
`

// runRig returns a rig of RH-1 for runner.sh run, with the stand-ins of
// extra too: root's install at runnerDir, and each runner's copy under
// runsDir its user's.
func runRig(t *testing.T, extra map[string]string) *rig {
	t.Helper()
	r := newRig(t, with(runnerStandins(), extra))
	p := r.path(runnerDir + "/run.sh")
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	//nolint:gosec // G306: the runner's run.sh is executable.
	if err := os.WriteFile(p, []byte(fakeRunner), 0o700); err != nil {
		t.Fatal(err)
	}
	r.userDirs(r.path(runsDir) + "/*")
	return r
}

// runnerRecord returns what the runner recorded of its run.
func (r *rig) runnerRecord() map[string]string {
	r.t.Helper()
	out := map[string]string{}
	b, err := os.ReadFile(filepath.Join(r.state, "runner"))
	if err != nil {
		return out
	}
	for line := range strings.SplitSeq(strings.TrimSuffix(string(b), "\n"), "\n") {
		k, v, _ := strings.Cut(line, "\t")
		out[k] = v
	}
	return out
}

// copies returns the runners' copies left under runsDir.
func (r *rig) copies() []string {
	r.t.Helper()
	entries, err := os.ReadDir(r.path(runsDir))
	if err != nil {
		r.t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// curlCall is one recorded GitHub API call.
type curlCall struct {
	method, url, auth string
	body              map[string]any
}

// curlCalls returns the recorded curl calls, each with the token of the
// authorization header curl read from a file.
func curlCalls(t *testing.T, calls [][]string) []curlCall {
	t.Helper()
	var out []curlCall
	for _, c := range calls {
		switch c[0] {
		case "curl":
			cc := curlCall{url: c[len(c)-1]}
			for i := 1; i < len(c)-1; i++ {
				switch c[i] {
				case "-X":
					cc.method = c[i+1]
				case "--data":
					if err := json.Unmarshal([]byte(c[i+1]), &cc.body); err != nil {
						t.Fatalf("curl --data %q: %v", c[i+1], err)
					}
				}
			}
			out = append(out, cc)
		case "curl-header":
			if v, ok := strings.CutPrefix(c[1], "Authorization: Bearer "); ok && len(out) > 0 {
				out[len(out)-1].auth = v
			}
		}
	}
	return out
}

// decodeSegment decodes one base64url segment of a JWT.
func decodeSegment(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("JWT segment %q: %v", s, err)
	}
	return b
}

// checkJWT checks an App JWT: RS256 over the App's key, issued by the App
// 60 s before the fixed clock and expiring 9 minutes after it.
func checkJWT(t *testing.T, jwt string, pub *rsa.PublicKey) {
	t.Helper()
	parts := strings.Split(strings.TrimSpace(jwt), ".")
	if len(parts) != 3 {
		t.Fatalf("JWT %q has %d parts", jwt, len(parts))
	}
	var header, claims map[string]any
	if err := json.Unmarshal(decodeSegment(t, parts[0]), &header); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(decodeSegment(t, parts[1]), &claims); err != nil {
		t.Fatal(err)
	}
	if want := map[string]any{"alg": "RS256", "typ": "JWT"}; !reflect.DeepEqual(header, want) {
		t.Errorf("JWT header %v, want %v", header, want)
	}
	if want := map[string]any{"iat": 1799999940.0, "exp": 1800000540.0, "iss": appID}; !reflect.DeepEqual(claims, want) {
		t.Errorf("JWT claims %v, want %v", claims, want)
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], decodeSegment(t, parts[2])); err != nil {
		t.Errorf("JWT signature: %v", err)
	}
}

// runnerEnv returns the runner service's environment.
func runnerEnv(key string, extra ...string) []string {
	return append([]string{
		"RUNNER_APP_ID=" + appID, "RUNNER_APP_KEY=" + key, "RUNNER_REPO=" + repo,
		"RUNNER_USER=ruralz-bench", "RUNNER_DIR=" + runnerDir, "RUNNER_API_URL=https://api.github.test",
	}, extra...)
}

// TestRunnerJWT: the service signs its App JWT with the App's key,
// RS256, issued 60 s in the past and expiring within GitHub's 10 minutes.
func TestRunnerJWT(t *testing.T) {
	r := newRig(t, runnerStandins())
	key, pub := appKey(t)
	jwt, err := r.source("runner.sh", "app_jwt", runnerEnv(key))
	if err != nil {
		t.Fatal(err)
	}
	checkJWT(t, jwt, pub)
}

// TestRunnerRunOnce: one turn of the service registers one just-in-time
// runner labeled self-hosted and rh-1, at repository level in the
// Default group or in the organization runner group RUNNER_GROUP_ID
// names, with an installation token of the App limited to what that
// takes. The dedicated user copies root's install into a fresh directory,
// whose copy runs as that user with the user manager's bus, its
// configuration in its environment and the job-started hook set, and the
// copy is deleted once the runner exits.
func TestRunnerRunOnce(t *testing.T) {
	cases := []struct {
		name, group, install, jit string
		tokenBody                 map[string]any
		groupID                   float64
	}{
		{
			"repository", "", "https://api.github.test/repos/" + repo + "/installation",
			"https://api.github.test/repos/" + repo + "/actions/runners/generate-jitconfig",
			map[string]any{"repositories": []any{"ruralz"}, "permissions": map[string]any{"administration": "write"}},
			1,
		},
		{
			"organization group", "7", "https://api.github.test/orgs/ravindu-rev/installation",
			"https://api.github.test/orgs/ravindu-rev/actions/runners/generate-jitconfig",
			map[string]any{"permissions": map[string]any{"organization_self_hosted_runners": "write"}},
			7,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := runRig(t, nil)
			key, pub := appKey(t)
			r.mustRun("runner.sh", runnerEnv(key, "RUNNER_GROUP_ID="+c.group), "run", "--once")
			calls := r.calls()
			api := curlCalls(t, calls)
			if len(api) != 3 {
				t.Fatalf("GitHub API calls %+v, want installation, token and configuration", api)
			}
			if api[0].method != http.MethodGet || api[0].url != c.install {
				t.Errorf("installation call %s %s, want GET %s", api[0].method, api[0].url, c.install)
			}
			checkJWT(t, api[0].auth, pub)
			checkJWT(t, api[1].auth, pub)
			if api[1].method != http.MethodPost || api[1].url != "https://api.github.test/app/installations/42/access_tokens" || !reflect.DeepEqual(api[1].body, c.tokenBody) {
				t.Errorf("token call %s %s %v, want POST of %v", api[1].method, api[1].url, api[1].body, c.tokenBody)
			}
			want := map[string]any{"name": "rh-1-" + epoch, "runner_group_id": c.groupID, "labels": []any{"self-hosted", "rh-1"}, "work_folder": "_work"}
			if api[2].method != http.MethodPost || api[2].url != c.jit || api[2].auth != "ghs_installation" || !reflect.DeepEqual(api[2].body, want) {
				t.Errorf("configuration call %s %s with %q: %v, want POST %s with the installation token: %v", api[2].method, api[2].url, api[2].auth, api[2].body, c.jit, want)
			}

			got := r.runnerRecord()
			dir := got["dir"]
			if !strings.HasPrefix(dir, r.path(runsDir+"/rh-1-"+epoch+".")) {
				t.Fatalf("the runner ran from %q, want a fresh directory of %s", dir, runsDir)
			}
			for k, want := range map[string]string{
				"args": "", "jitconfig": "JITCONFIG", "hook": hookPath,
				"xdg": "/run/user/" + userUID, "bus": "unix:path=/run/user/" + userUID + "/bus",
			} {
				if got[k] != want {
					t.Errorf("the runner's %s = %q, want %q", k, got[k], want)
				}
			}
			for _, want := range [][]string{
				{"chown", "ruralz-bench:ruralz-bench", dir},
				{"runuser", "-u", "ruralz-bench", "--", "cp", "-R", r.path(runnerDir) + "/.", dir},
				{
					"runuser", "-u", "ruralz-bench", "--", "env", "XDG_RUNTIME_DIR=/run/user/" + userUID,
					"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/" + userUID + "/bus", "ACTIONS_RUNNER_HOOK_JOB_STARTED=" + hookPath, dir + "/run.sh",
				},
			} {
				if !slices.ContainsFunc(calls, func(c []string) bool { return slices.Equal(c, want) }) {
					t.Errorf("calls %q lack %q", calls, want)
				}
			}
			if left := r.copies(); len(left) != 0 {
				t.Errorf("copies %q left under %s, want the runner's deleted", left, runsDir)
			}
		})
	}
}

// TestRunnerKeepsSecretsOffCommandLines: the App's JWT and the
// installation token reach curl only in a header file, deleted after each
// call, and the runner's configuration reaches the runner only in its
// environment: no command line, which every local user can read in /proc,
// holds any of them.
func TestRunnerKeepsSecretsOffCommandLines(t *testing.T) {
	r := runRig(t, nil)
	key, _ := appKey(t)
	r.mustRun("runner.sh", runnerEnv(key), "run", "--once")
	jwtHeader := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	headers := 0
	for _, c := range r.calls() {
		if c[0] == "curl-header" {
			if strings.HasPrefix(c[1], "Authorization: Bearer ") {
				headers++
			}
			continue
		}
		for _, w := range c[1:] {
			for _, secret := range []string{jwtHeader, "ghs_installation", "JITCONFIG", "Bearer"} {
				if strings.Contains(w, secret) {
					t.Errorf("the command line %q holds %q", c, secret)
				}
			}
		}
	}
	if headers != 3 {
		t.Errorf("curl read %d authorization headers from a file, want one for each GitHub API call", headers)
	}
	if got := r.runnerRecord()["jitconfig"]; got != "JITCONFIG" {
		t.Errorf("the runner's environment holds the configuration %q, want JITCONFIG", got)
	}
	if entries, err := os.ReadDir(r.tmp); err != nil || len(entries) != 0 {
		t.Errorf("TMPDIR holds %v (%v) after the run, want every header file deleted", entries, err)
	}
}

// TestRunnerRunOnceWithoutConfiguration: when GitHub gives no runner
// configuration, no runner starts, no copy is made and a single turn
// fails.
func TestRunnerRunOnceWithoutConfiguration(t *testing.T) {
	r := runRig(t, map[string]string{"curl": "exit 22\n"})
	key, _ := appKey(t)
	if out, err := r.run("runner.sh", runnerEnv(key), "run", "--once"); err == nil {
		t.Errorf("runner.sh run --once = nil error:\n%s", out)
	}
	if called(r.calls(), "runuser") {
		t.Error("a runner started without a configuration")
	}
	if left := r.copies(); len(left) != 0 {
		t.Errorf("copies %q under %s, want none", left, runsDir)
	}
}

// runnerProcesses writes the fake /proc of a runner that runs the hook:
// the test's process stands for the worker, the hook's parent, below the
// listener and run.sh, all of the test's user, and runuser, of another
// user. It returns the runner's processes, nearest first.
func (r *rig) runnerProcesses() []string {
	r.t.Helper()
	uid, other := strconv.Itoa(os.Getuid()), strconv.Itoa(os.Getuid()+1)
	worker := strconv.Itoa(os.Getpid())
	for _, p := range [][3]string{{worker, "4001", uid}, {"4001", "4000", uid}, {"4000", "3999", uid}, {"3999", "1", other}} {
		r.write("/proc/"+p[0]+"/status", "Name:\tstand-in\nPPid:\t"+p[1]+"\nUid:\t"+p[2]+"\t"+p[2]+"\t"+p[2]+"\t"+p[2]+"\n")
	}
	return []string{worker, "4001", "4000"}
}

// admitEnv returns the job facts GitHub gives the hook for a scheduled
// nightly run on main, with changes.
func admitEnv(changes map[string]string) []string {
	facts := map[string]string{
		"GITHUB_REPOSITORY":    repo,
		"GITHUB_WORKFLOW_REF":  repo + "/.github/workflows/nightly.yml@refs/heads/main",
		"GITHUB_REF":           "refs/heads/main",
		"GITHUB_REF_PROTECTED": "true",
		"GITHUB_EVENT_NAME":    "schedule",
		"GITHUB_JOB":           "latency",
	}
	maps.Copy(facts, changes)
	var env []string
	for _, k := range slices.Sorted(maps.Keys(facts)) {
		env = append(env, k+"="+facts[k])
	}
	return env
}

// TestRunnerAdmit: the job-started hook lets a job run only when it
// belongs to nightly.yml, chaos-scale.yml or release.yml of the
// repository, on a protected branch or tag, scheduled, dispatched or
// pushed. It refuses any other job, a pull request's above all, in the
// host's journal and stops it by killing the runner, the hook's ancestors
// that run as the runner's user, so not even an if: always() step runs.
func TestRunnerAdmit(t *testing.T) {
	wf := func(file, ref string) string { return repo + "/.github/workflows/" + file + "@" + ref }
	cases := []struct {
		name    string
		changes map[string]string
		admit   bool
	}{
		{"nightly scheduled on main", nil, true},
		{"nightly dispatched on a release branch", map[string]string{
			"GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_REF": "refs/heads/release-0.1", "GITHUB_WORKFLOW_REF": wf("nightly.yml", "refs/heads/release-0.1"),
		}, true},
		{"chaos-scale scheduled", map[string]string{"GITHUB_WORKFLOW_REF": wf("chaos-scale.yml", "refs/heads/main"), "GITHUB_JOB": "chaos"}, true},
		{"release on its tag", map[string]string{
			"GITHUB_EVENT_NAME": "push", "GITHUB_REF": "refs/tags/v0.1.0", "GITHUB_WORKFLOW_REF": wf("release.yml", "refs/tags/v0.1.0"), "GITHUB_JOB": "soak",
		}, true},
		{"pull request", map[string]string{
			"GITHUB_EVENT_NAME": "pull_request", "GITHUB_REF": "refs/pull/7/merge", "GITHUB_REF_PROTECTED": "false", "GITHUB_WORKFLOW_REF": wf("nightly.yml", "refs/pull/7/merge"),
		}, false},
		{"pull request target", map[string]string{"GITHUB_EVENT_NAME": "pull_request_target"}, false},
		{"workflow run", map[string]string{"GITHUB_EVENT_NAME": "workflow_run"}, false},
		{"unprotected branch", map[string]string{
			"GITHUB_EVENT_NAME": "push", "GITHUB_REF": "refs/heads/topic", "GITHUB_REF_PROTECTED": "false", "GITHUB_WORKFLOW_REF": wf("nightly.yml", "refs/heads/topic"),
		}, false},
		{"other workflow", map[string]string{"GITHUB_WORKFLOW_REF": wf("pr-fast.yml", "refs/heads/main")}, false},
		{"workflow of another ref", map[string]string{"GITHUB_WORKFLOW_REF": wf("nightly.yml", "refs/heads/topic")}, false},
		{"fork", map[string]string{"GITHUB_REPOSITORY": "someone/ruralz", "GITHUB_WORKFLOW_REF": "someone/ruralz/.github/workflows/nightly.yml@refs/heads/main"}, false},
		{"no job facts", map[string]string{"GITHUB_REPOSITORY": "", "GITHUB_WORKFLOW_REF": "", "GITHUB_REF": "", "GITHUB_REF_PROTECTED": "", "GITHUB_EVENT_NAME": ""}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t, runnerStandins())
			runner := r.runnerProcesses()
			out, err := r.run("runner.sh", admitEnv(c.changes), "admit", repo)
			calls := r.calls()
			if c.admit {
				if err != nil || !strings.Contains(out, "admitted the job") || called(calls, "kill") || called(calls, "logger") {
					t.Errorf("runner.sh admit = %v, calls %q:\n%s\nwant the job admitted", err, calls, out)
				}
				return
			}
			kill := append([]string{"kill", "-KILL"}, runner...)
			killed := slices.ContainsFunc(calls, func(c []string) bool { return slices.Equal(c, kill) })
			if err == nil || !strings.Contains(out, "refused this job") || !killed || !called(calls, "logger", "-t", "ruralz-bench-runner", "-p", "auth.warning", "--") {
				t.Errorf("runner.sh admit = %v, calls %q:\n%s\nwant the job refused in the journal and the runner %q killed", err, calls, out, runner)
			}
		})
	}
}

// TestRunnerAdmitsTheRH1Workflows: the hook admits exactly the workflows
// whose runs can put a job on rh-1: each with an rh-1 job and each that
// calls one of those.
func TestRunnerAdmitsTheRH1Workflows(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join(repoRoot(t), ".github", "workflows"))
	if err != nil {
		t.Fatal(err)
	}
	texts, onRH1 := map[string]string{}, map[string]bool{}
	for _, e := range entries {
		if name := e.Name(); strings.HasSuffix(name, ".yml") || strings.HasSuffix(name, ".yaml") {
			texts[name] = workflow(t, name)
			onRH1[name] = strings.Contains(texts[name], `"rh-1"]`)
		}
	}
	for {
		var callers []string
		for name, text := range texts {
			if !onRH1[name] && slices.ContainsFunc(slices.Collect(maps.Keys(onRH1)), func(callee string) bool {
				return onRH1[callee] && strings.Contains(text, "uses: ./.github/workflows/"+callee+"\n")
			}) {
				callers = append(callers, name)
			}
		}
		if len(callers) == 0 {
			break
		}
		for _, name := range callers {
			onRH1[name] = true
		}
	}
	for _, name := range []string{"nightly.yml", "chaos-scale.yml", "release.yml"} {
		if !onRH1[name] {
			t.Errorf("%s puts no job on rh-1", name)
		}
	}
	r := newRig(t, runnerStandins())
	for _, name := range slices.Sorted(maps.Keys(texts)) {
		_, err := r.source("runner.sh", "rh1_workflow "+name, nil)
		if got := err == nil; got != onRH1[name] {
			t.Errorf("rh1_workflow %s admits: %t, want %t", name, got, onRH1[name])
		}
	}
}

// runnerArchive returns an actions/runner archive stand-in: run.sh and
// bin/installdependencies.sh, which records its call.
func runnerArchive(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range map[string]string{
		"run.sh":                     "#!/usr/bin/env bash\nexit 0\n",
		"bin/installdependencies.sh": "#!/usr/bin/env bash\nprintf 'installdependencies.sh\\n' >>\"$STUB_STATE/calls\"\n",
	} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// installRig returns a rig of RH-1 for runner.sh install: 16 online cores,
// a curl stand-in that downloads archive, and chmod recorded.
func installRig(t *testing.T, archive []byte) *rig {
	t.Helper()
	r := newRig(t, with(hostStandins(), map[string]string{
		"curl": `while (($#)); do if [[ $1 == -o ]]; then out=$2; fi; shift; done
cp "$STUB_STATE/runner.tar.gz" "$out"
`,
		"chmod": `exec "$STUB_SYSTEM/chmod" "$@"` + "\n",
	}))
	r.write("/sys/devices/system/cpu/online", "0-15\n")
	if err := os.WriteFile(filepath.Join(r.state, "runner.tar.gz"), archive, 0o600); err != nil {
		t.Fatal(err)
	}
	r.writeState("installed", "base-files\njq\n")
	r.writeState("held", "base-files\n")
	return r
}

// TestRunnerInstall: install verifies the actions/runner archive against
// its SHA-256 and installs it with its dependencies as root's alone, which
// no job can change, each runner running a copy; it writes the runners'
// job-started hook, keeps the App key root's alone, and runs
// ruralz-bench-runner.service on the cores outside ruralzd's cpuset with
// the benchmark open-file limit.
func TestRunnerInstall(t *testing.T) {
	archive := runnerArchive(t)
	sum := sha256.Sum256(archive)
	r := installRig(t, archive)
	key, _ := appKey(t)
	const version = "2.330.0"
	r.mustRun("runner.sh", nil, "install", "--inventory", script(t, "rh-1.json"), "--app-id", appID, "--app-key", key,
		"--repo", repo, "--runner-version", version, "--runner-sha256", hex.EncodeToString(sum[:]))
	calls := r.calls()
	if !called(calls, "curl", "-fsSL", "-o") || !slices.ContainsFunc(calls, func(c []string) bool {
		return c[0] == "curl" && c[len(c)-1] == "https://github.com/actions/runner/releases/download/v"+version+"/actions-runner-linux-x64-"+version+".tar.gz"
	}) {
		t.Errorf("calls %q, want the linux-x64 archive of actions/runner %s", calls, version)
	}
	if !called(calls, "installdependencies.sh") {
		t.Error("the runner's dependencies were not installed")
	}
	dir := runnerDir + "-" + version
	if r.read(dir+"/run.sh") == "" {
		t.Errorf("no %s/run.sh", dir)
	}
	if !called(calls, "chown", "-R", "root:root", r.path(dir)) || !called(calls, "chmod", "-R", "go-w,a+rX", r.path(dir)) {
		t.Error("the runner's install is not root's alone, readable by the user who copies it")
	}
	if called(calls, "chown", "-R", "ruralz-bench:ruralz-bench") {
		t.Error("the runner's install is the dedicated user's, whose jobs could change it")
	}
	if link, err := os.Readlink(r.path(runnerDir)); err != nil || link != dir {
		t.Errorf("%s links to %q (%v), want %s", runnerDir, link, err, dir)
	}
	hook := "#!/usr/bin/env bash\n# deploy/benchhost runner.sh install: the rh-1 runners' job-started hook.\n" +
		"exec /usr/local/lib/ruralz-bench/runner.sh admit " + repo + "\n"
	if r.read(hookPath) != hook || r.mode(hookPath) != 0o755 {
		t.Errorf("job-started hook =\n%s\nwant an executable\n%s", r.read(hookPath), hook)
	}
	if r.read("/etc/ruralz-bench/github-app.pem") != readFile(t, key) || r.mode("/etc/ruralz-bench/github-app.pem") != 0o600 {
		t.Error("the App key is not installed root's with mode 0600")
	}
	env := "RUNNER_APP_ID=" + appID + "\nRUNNER_APP_KEY=/etc/ruralz-bench/github-app.pem\nRUNNER_REPO=" + repo +
		"\nRUNNER_GROUP_ID=\nRUNNER_USER=ruralz-bench\nRUNNER_DIR=" + runnerDir + "\n"
	if got := r.read("/etc/ruralz-bench/runner.env"); !strings.HasSuffix(got, env) {
		t.Errorf("runner.env =\n%s\nwant\n%s", got, env)
	}
	if r.read("/usr/local/lib/ruralz-bench/runner.sh") != readFile(t, script(t, "runner.sh")) || r.mode("/usr/local/lib/ruralz-bench/runner.sh") != 0o755 {
		t.Error("runner.sh is not installed as an executable copy")
	}
	unit := r.read("/etc/systemd/system/ruralz-bench-runner.service")
	for _, want := range []string{
		"\nAllowedCPUs=0-1,6-15\n", "\nLimitNOFILE=1048576\n", "\nExecStart=/usr/local/lib/ruralz-bench/runner.sh run\n",
		"\nEnvironmentFile=/etc/ruralz-bench/runner.env\n", "\nRestart=always\n", "user@" + userUID + ".service",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("ruralz-bench-runner.service lacks %q:\n%s", want, unit)
		}
	}
	for _, c := range [][]string{{"systemctl", "enable", "ruralz-bench-runner.service"}, {"systemctl", "restart", "ruralz-bench-runner.service"}, {"apt-mark", "hold", "jq"}} {
		if !called(calls, c...) {
			t.Errorf("calls %q lack %q", calls, c)
		}
	}
}

// TestRunnerInstallRefuses: install stops on an archive whose SHA-256
// differs, writing no unit, and off the rh-1 profile.
func TestRunnerInstallRefuses(t *testing.T) {
	archive := runnerArchive(t)
	key, _ := appKey(t)
	cases := []struct{ name, inventory, sha, want string }{
		{"checksum", "rh-1.json", strings.Repeat("0", 64), "does not match --runner-sha256"},
		{"profile", "rh-3.json", strings.Repeat("0", 64), "runs on the rh-1 profile's gateway host"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := installRig(t, archive)
			out, err := r.run("runner.sh", nil, "install", "--inventory", script(t, c.inventory), "--app-id", appID, "--app-key", key,
				"--repo", repo, "--runner-version", "2.330.0", "--runner-sha256", c.sha)
			if err == nil || !strings.Contains(out, c.want) {
				t.Errorf("runner.sh install = %v:\n%s\nwant a failure naming %q", err, out, c.want)
			}
			if r.read("/etc/systemd/system/ruralz-bench-runner.service") != "" {
				t.Error("a refused install wrote the unit")
			}
		})
	}
}
