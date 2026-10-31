// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/proc"
)

// End-to-end cases of ruralz dev run, Planned (M1)
// (docs/reference/01-cli-and-api-surface.md "Local ruralzd launched by the
// CLI"): the built ruralz launches the ruralzd beside it, the
// controllable-clock build, on the REST example Bundle rendered for the
// e2e Environment, with --ephemeral-ports and --secret-overrides naming
// every secret, the TLS files, the OAuth client secret, the partner's API
// key and the State Store URL, each value a canary; the Node serves
// through the mocks, a source edit Hot Reloads it, and Ctrl-C drains it:
// exit 130. --output json prints the NDJSON events and SIGTERM ends it
// with 143; a busy port without --ephemeral-ports exits 2 before the
// launch.

// devReady is the readiness block ruralz dev run prints.
type devReady struct {
	revision, digest, testDigest, admin, tokenFile, tap string
	listeners                                           map[string]string
	mapping                                             []string
}

// parseDevReady parses the readiness block of ruralz dev run's output.
func parseDevReady(out string) devReady {
	r := devReady{listeners: map[string]string{}}
	for _, l := range lines(out) {
		key, value, _ := strings.Cut(l, ": ")
		switch {
		case key == "revision":
			r.revision, r.digest, _ = strings.Cut(value, " ")
		case key == "testDigest":
			r.testDigest = value
		case key == "admin" && strings.HasPrefix(value, "http"):
			r.admin = value
		case key == "adminTokenFile":
			r.tokenFile = value
		case strings.HasPrefix(key, "listener "):
			r.listeners[strings.TrimPrefix(key, "listener ")] = value
		case strings.HasPrefix(l, "ruralz dev tap "):
			r.tap = l
		case strings.Contains(value, " -> "):
			r.mapping = append(r.mapping, l)
		}
	}
	return r
}

// devRunSetup is a ruralz dev run of the REST example Bundle.
type devRunSetup struct {
	s         *shopRest
	clk       *childClock
	child     *os.File
	tmp       string // the CLI's TMPDIR, which holds its private directory
	overrides string
	env       []string
}

// newDevRun prepares the REST example Bundle, the secret overrides and
// the clock of the ruralzd ruralz dev run launches.
func newDevRun(t *testing.T) *devRunSetup {
	t.Helper()
	requireTestClock(t)
	var clk atomic.Pointer[childClock]
	s := newShopRest(t, shopRestOptions{now: func() time.Time {
		if c := clk.Load(); c != nil {
			return c.Now()
		}
		return time.Now()
	}})
	d := &devRunSetup{s: s, tmp: t.TempDir()}
	d.clk, d.child = newChildClock(t)
	clk.Store(d.clk)
	dir := t.TempDir()
	for name, value := range map[string][]byte{
		"tls.crt": s.certPEM, "tls.key": s.keyPEM, "client-secret": []byte(s.clientSecret),
		"partner-key": []byte(s.apiKey), "state-store-url": []byte(s.store.URL()),
	} {
		writeFile(t, dir, "local/"+name, value)
	}
	d.overrides = writeFile(t, dir, "overrides.yaml", []byte(
		refTLSCert+": local/tls.crt\n"+refTLSKey+": local/tls.key\n"+refClientSecret+": local/client-secret\n"+
			refPartnerKey+": local/partner-key\n"+refStateStore+": local/state-store-url\n"))
	// ruralzd trusts the mock IdP, the OTLP sink and the State Store
	// through the environment ruralz passes it.
	bundle := append(append([]byte(nil), s.top.CA().CertPEM...), s.store.CAPEM()...)
	d.env = []string{"SSL_CERT_FILE=" + writeFile(t, dir, "ca-bundle.pem", bundle), "TMPDIR=" + d.tmp}
	return d
}

// start runs ruralz dev run on the Bundle with args added.
func (d *devRunSetup) start(t *testing.T, args ...string) (*proc.Process, []string) {
	t.Helper()
	args = append([]string{
		"dev", "run", "--env", shopRestEnv, "--environments", d.s.envFile, "--ephemeral-ports",
		"--secret-overrides", d.overrides,
	}, append(args, d.s.bundleDir)...)
	p := startCLI(t, cliOptions{dir: d.s.work, env: d.env, extraFiles: []*os.File{d.child}}, args...)
	// ruralz and the ruralzd it launches hold their own copies now;
	// closing the suite's makes the socket end once both exited, which
	// tells an exited ruralzd from a slow one.
	_ = d.child.Close()
	return p, args
}

// stdout returns what p wrote to standard output.
func stdout(p *proc.Process) string { out, _ := p.Output(); return string(out) }

// TestDevRun runs the text form through a Hot Reload and Ctrl-C.
func TestDevRun(t *testing.T) {
	d := newDevRun(t)
	s := d.s
	p, args := d.start(t)
	advancingChild(t, d.clk, 100*time.Millisecond, func() bool { return strings.Contains(stdout(p), "ruralz dev tap --admin ") || p.State() != nil })
	r := parseDevReady(stdout(p))
	want, err := revisionDigest(s.rendered)
	if err != nil {
		t.Fatal(err)
	}
	if r.revision != displayRevision(t, want) || r.digest != want || !digestRE.MatchString(r.testDigest) || r.testDigest == r.digest {
		t.Errorf("readiness block %+v, want the e2e render's Revision %s and a test digest of its own\n%s", r, want, stdout(p))
	}
	if r.tap != "ruralz dev tap --admin "+r.admin+" --admin-token-file "+r.tokenFile || !strings.HasPrefix(r.tokenFile, d.tmp) ||
		!strings.HasPrefix(r.admin, "http://127.0.0.1:") {
		t.Errorf("admin %q, token file %q, tap line %q, want a loopback admin URL and a token file in the private directory under %s", r.admin, r.tokenFile, r.tap, d.tmp)
	}
	mapping := regexp.MustCompile(`^(http: 8080|https: 8443|admin: 9901) -> \d+$`)
	if len(r.mapping) != 3 || !mapping.MatchString(r.mapping[0]) || !mapping.MatchString(r.mapping[1]) || !mapping.MatchString(r.mapping[2]) ||
		strings.HasSuffix(r.mapping[0], "-> 8080") {
		t.Errorf("port mapping %q, want the three rewritten ports", r.mapping)
	}
	httpURL, httpsURL := r.listeners["http"], r.listeners["https"]
	if !strings.HasPrefix(httpURL, "http://127.0.0.1:") || !strings.HasPrefix(httpsURL, "https://127.0.0.1:") {
		t.Fatalf("listeners %v", r.listeners)
	}

	// The overridden API key, client secret, State Store URL and TLS
	// files serve: the partner's key binds its Consumer and charges its
	// quota, the token route reaches the mock IdP and both Upstreams.
	client := s.client("")
	partner := shopGet(t, client, httpURL, "/partner/v1/orders/"+orderID, map[string]string{"X-Api-Key": s.apiKey})
	if partner.status != http.StatusOK || rateLimit(t, partner.header, "quota-partner") < 0 {
		t.Errorf("partner request = %d %s, RateLimit %q; want 200 charging quota-partner", partner.status, partner.code, partner.header.Values("RateLimit"))
	}
	token := map[string]string{"Authorization": "Bearer " + s.jwt("orders:read", nil)}
	summary := shopGet(t, client, httpsURL, "/v1/orders/"+orderID+"/summary", token)
	if summary.status != http.StatusOK || summary.header.Get("Strict-Transport-Security") != "max-age=31536000" {
		t.Fatalf("orders-summary over TLS = %d %s, HSTS %q", summary.status, summary.code, summary.header.Get("Strict-Transport-Security"))
	}

	common := filepath.Join(s.bundleDir, "policies", "common.yaml")
	edited := strings.Replace(string(readFile(t, common)), "max-age=31536000", "max-age=63072000", 1)
	before := stdout(p)
	writeFile(t, s.bundleDir, "policies/common.yaml", []byte(edited))
	advancingChild(t, d.clk, time.Second, func() bool { return strings.Contains(strings.TrimPrefix(stdout(p), before), "active rev-") })
	if added := strings.TrimPrefix(stdout(p), before); !regexp.MustCompile(`^active rev-[0-9a-f]{12}\n$`).MatchString(added) {
		t.Errorf("after the source edit: %q, want one active line and no new mapping", added)
	}
	if got := shopGet(t, client, httpsURL, "/v1/orders/"+orderID+"/summary", token); got.status != http.StatusOK ||
		got.header.Get("Strict-Transport-Security") != "max-age=63072000" {
		t.Errorf("after the Hot Reload: %d %s, HSTS %q, want the edited max-age=63072000 on the same port",
			got.status, got.code, got.header.Get("Strict-Transport-Security"))
	}

	client.CloseIdleConnections()
	if err := p.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	advancingChild(t, d.clk, time.Second, func() bool { return p.State() != nil })
	res := finish(t, s.top, p, args, waitTimeout)
	if res.code != 130 {
		t.Errorf("after Ctrl-C: %s\nwant exit 130", res)
	}
	if entries, _ := os.ReadDir(d.tmp); len(entries) != 0 {
		t.Errorf("the private directory stays: %v", entries)
	}
}

// devEvent is one NDJSON event of ruralz dev run --output json.
type devEvent struct {
	Event          string `json:"event"`
	Revision       string `json:"revision"`
	Digest         string `json:"digest"`
	TestDigest     string `json:"testDigest"`
	Admin          string `json:"admin"`
	AdminTokenFile string `json:"adminTokenFile"`
	Listeners      []struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	} `json:"listeners"`
	Ports []struct {
		Name     string `json:"name"`
		Port     int    `json:"port"`
		TestPort int    `json:"testPort"`
	} `json:"ports"`
}

// TestDevRunJSON runs the JSON form: one ready event describing the test
// render, then SIGTERM drains the Node: exit 143.
func TestDevRunJSON(t *testing.T) {
	d := newDevRun(t)
	p, args := d.start(t, "--output", "json")
	advancingChild(t, d.clk, 100*time.Millisecond, func() bool { return strings.Contains(stdout(p), `"event":"ready"`) || p.State() != nil })
	evs := jsonLines([]byte(stdout(p)))
	var ev devEvent
	if len(evs) != 1 || json.Unmarshal(evs[0], &ev) != nil {
		t.Fatalf("events:\n%s", stdout(p))
	}
	want, err := revisionDigest(d.s.rendered)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Event != "ready" || ev.Digest != want || ev.Revision != displayRevision(t, want) || !digestRE.MatchString(ev.TestDigest) ||
		len(ev.Listeners) != 2 || len(ev.Ports) != 3 || ev.Ports[2].Name != "admin" || ev.Ports[2].Port != 9901 || ev.Ports[2].TestPort == 9901 ||
		!strings.HasPrefix(ev.AdminTokenFile, d.tmp) {
		t.Errorf("ready event %+v", ev)
	}
	if err := p.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if t.Failed() {
			out, errOut := p.Output()
			t.Logf("ruralz dev run --output json:\n--- stdout ---\n%s\n--- stderr ---\n%.6000s\nstate %v", out, errOut, p.State())
		}
	}()
	advancingChild(t, d.clk, time.Second, func() bool { return p.State() != nil })
	if res := finish(t, d.s.top, p, args, waitTimeout); res.code != 143 {
		t.Errorf("after SIGTERM: %s\nwant exit 143", res)
	}
}

// TestDevRunBusyPort: without --ephemeral-ports, a listener port another
// process holds exits 2 before the launch, naming the listener.
func TestDevRunBusyPort(t *testing.T) {
	busy, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = busy.Close() }()
	port := strconv.Itoa(busy.Addr().(*net.TCPAddr).Port)
	src := writeFile(t, t.TempDir(), "bundle.yaml", []byte(strings.Replace(string(drainBundle("127.0.0.1:9")), "port: 8080", "port: "+port, 1)))
	r := cli(t, nil, cliOptions{}, "dev", "run", src)
	if r.code != 2 || !strings.Contains(r.stderr, "listener http: port "+port+" is busy") {
		t.Errorf("%s\nwant exit 2 naming the busy listener", r)
	}
}
