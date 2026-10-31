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
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// The fixed clock of the runner tests: every stand-in date reads it.
const (
	epoch     = "1800000000"
	appID     = "123456"
	repo      = "ravindu-rev/ruralz"
	runnerDir = "/opt/ruralz-bench/actions-runner"
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
// installed on the repository, run the runner as runuser would, and fix
// the clock.
func runnerStandins() map[string]string {
	return map[string]string{
		"date": `echo ` + epoch + "\n",
		"id":   `case "$1" in -u) echo ` + userUID + ` ;; -gn) echo "$2" ;; *) exit 1 ;; esac` + "\n",
		"curl": `url=${*: -1}
case "$url" in
  */repos/` + repo + `/installation | */orgs/ravindu-rev/installation) echo '{"id":42}' ;;
  */app/installations/42/access_tokens) echo '{"token":"ghs_installation"}' ;;
  */actions/runners/generate-jitconfig) echo '{"runner":{"id":7},"encoded_jit_config":"JITCONFIG"}' ;;
  *) exit 22 ;;
esac
`,
		"runuser": "",
	}
}

// curlCall is one recorded GitHub API call.
type curlCall struct {
	method, url, auth string
	body              map[string]any
}

// curlCalls returns the recorded curl calls.
func curlCalls(t *testing.T, calls [][]string) []curlCall {
	t.Helper()
	var out []curlCall
	for _, c := range calls {
		if c[0] != "curl" {
			continue
		}
		cc := curlCall{url: c[len(c)-1]}
		for i := 1; i < len(c)-1; i++ {
			switch {
			case c[i] == "-X":
				cc.method = c[i+1]
			case c[i] == "-H" && strings.HasPrefix(c[i+1], "Authorization: Bearer "):
				cc.auth = strings.TrimPrefix(c[i+1], "Authorization: Bearer ")
			case c[i] == "--data":
				if err := json.Unmarshal([]byte(c[i+1]), &cc.body); err != nil {
					t.Fatalf("curl --data %q: %v", c[i+1], err)
				}
			}
		}
		out = append(out, cc)
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
// takes, and runs it as the dedicated user with the user manager's bus.
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
			r := newRig(t, runnerStandins())
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
			run := []string{
				"runuser", "-u", "ruralz-bench", "--", "env", "XDG_RUNTIME_DIR=/run/user/" + userUID,
				"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/" + userUID + "/bus", runnerDir + "/run.sh", "--jitconfig", "JITCONFIG",
			}
			if !slices.ContainsFunc(calls, func(c []string) bool { return slices.Equal(c, run) }) {
				t.Errorf("calls %q, want the runner run as %q", calls, run)
			}
		})
	}
}

// TestRunnerRunOnceWithoutConfiguration: when GitHub gives no runner
// configuration, no runner starts and a single turn fails.
func TestRunnerRunOnceWithoutConfiguration(t *testing.T) {
	r := newRig(t, with(runnerStandins(), map[string]string{"curl": "exit 22\n"}))
	key, _ := appKey(t)
	if out, err := r.run("runner.sh", runnerEnv(key), "run", "--once"); err == nil {
		t.Errorf("runner.sh run --once = nil error:\n%s", out)
	}
	if called(r.calls(), "runuser") {
		t.Error("a runner started without a configuration")
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
// and a curl stand-in that downloads archive.
func installRig(t *testing.T, archive []byte) *rig {
	t.Helper()
	r := newRig(t, with(hostStandins(), map[string]string{
		"curl": `while (($#)); do if [[ $1 == -o ]]; then out=$2; fi; shift; done
cp "$STUB_STATE/runner.tar.gz" "$out"
`,
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
// its SHA-256, installs it for the dedicated user with its dependencies,
// keeps the App key root's alone, and runs ruralz-bench-runner.service on
// the cores outside ruralzd's cpuset with the benchmark open-file limit.
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
	if !called(calls, "chown", "-R", "ruralz-bench:ruralz-bench", r.path(dir)) {
		t.Error("the runner is not the dedicated user's")
	}
	if link, err := os.Readlink(r.path(runnerDir)); err != nil || link != dir {
		t.Errorf("%s links to %q (%v), want %s", runnerDir, link, err, dir)
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
