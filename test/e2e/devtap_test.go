// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"encoding/json"
	"net/http"
	"os"
	"regexp"
	"strings"
	"syscall"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/adminapi"
	"github.com/ravindu-rev/ruralz/internal/testkit/proc"
)

// End-to-end cases of ruralz dev tap, Planned (M1)
// (docs/reference/01-cli-and-api-surface.md "Plugin, AI and development
// verbs"): against a harness Node serving the REST example Bundle over
// admin TLS, four subscribers print the Node's redacted events, in text
// and in JSON, a fifth exits 2 with 503 RZ-RT-019, a token the Node
// refuses exits 2, an out-of-range --sample is a usage error, and Ctrl-C
// ends a subscriber with 130 and SIGTERM with 143.

// TestDevTap runs the dev tap cases.
func TestDevTap(t *testing.T) {
	s := startShopRest(t, shopRestOptions{nodes: 1})
	n := s.nodes[0]
	creds := []string{"--admin", n.AdminURL(), "--admin-token-file", n.TokenFile(), "--ca-file", s.ca}
	tap := func(args ...string) *proc.Process {
		return startCLI(t, cliOptions{}, append(append([]string{"dev", "tap"}, creds...), args...)...)
	}
	taps := []*proc.Process{tap(), tap("--route", "partner-orders"), tap("--output", "json"), tap()}

	// The token is a credential field /tap redacts; apikey-partner strips
	// the partner's key before the exchange is recorded, and the key is a
	// canary, so the leak scan catches it anywhere in the subscribers'
	// output.
	client := s.client("")
	token := "Bearer " + s.jwt("orders:read", nil)
	partnerLine := regexp.MustCompile(`(?m)^\S+ 200 GET ` + shopHost + `/partner/v1/orders/42 route=partner-orders upstream=orders [0-9.]+ms\n(  [<>] .*\n)*  < x-mockup-server: orders\n`)
	summaryLine := regexp.MustCompile(`(?m)^\S+ 200 GET ` + shopHost + `/v1/orders/42/summary route=orders-summary upstream=\S+ [0-9.]+ms\n(  [<>] .*\n)*  > authorization: \[REDACTED\]\n`)
	seen := func(p *proc.Process, re *regexp.Regexp) bool { out, _ := p.Output(); return re.Match(out) }
	defer func() {
		if t.Failed() {
			for i, p := range taps {
				out, errOut := p.Output()
				t.Logf("subscriber %d:\n--- stdout ---\n%.3000s\n--- stderr ---\n%.2000s", i+1, out, errOut)
			}
		}
	}()
	waitFor(t, "every subscriber to print the exchanges", func() bool {
		shopGet(t, client, s.listener(n, "http"), "/partner/v1/orders/"+orderID, map[string]string{"X-Api-Key": s.apiKey})
		shopGet(t, client, s.listener(n, "http"), "/v1/orders/"+orderID+"/summary", map[string]string{"Authorization": token})
		jsonOut, _ := taps[2].Output()
		return seen(taps[0], partnerLine) && seen(taps[0], summaryLine) && seen(taps[1], partnerLine) && seen(taps[3], summaryLine) &&
			strings.Contains(string(jsonOut), `"route":"orders-summary"`) && strings.Contains(string(jsonOut), `"route":"partner-orders"`)
	})
	if seen(taps[1], summaryLine) {
		t.Error("dev tap --route partner-orders printed an orders-summary event")
	}
	for i, p := range []*proc.Process{taps[0], taps[1], taps[3]} {
		out, _ := p.Output()
		for _, l := range lines(string(out)) {
			for _, field := range []string{"  > authorization: ", "  > x-api-key: "} {
				if v, ok := strings.CutPrefix(l, field); ok && v != "[REDACTED]" {
					t.Errorf("subscriber %d printed %q, want the credential redacted", i+1, l)
				}
			}
		}
	}
	jsonOut, _ := taps[2].Output()
	for _, l := range jsonLines(jsonOut) {
		var ev adminapi.TapEvent
		if err := json.Unmarshal(l, &ev); err != nil {
			t.Fatalf("dev tap --output json line %q: %v", l, err)
		}
		for name, values := range ev.RequestHeaders {
			if name == "authorization" || name == "x-api-key" {
				if len(values) != 1 || values[0] != "[REDACTED]" {
					t.Errorf("dev tap --output json: %s %q, want [REDACTED]", name, values)
				}
			}
		}
	}

	fifth := cli(t, s.top, cliOptions{}, append([]string{"dev", "tap"}, creds...)...)
	if fifth.code != 2 || fifth.stdout != "" || !strings.Contains(fifth.stderr, "ruralz dev tap: 503 RZ-RT-019 ") {
		t.Errorf("the fifth subscriber: %s\nwant exit 2 with 503 RZ-RT-019", fifth)
	}

	for i, p := range taps {
		sig, want := os.Signal(os.Interrupt), 130
		if i == len(taps)-1 {
			sig, want = syscall.SIGTERM, 143
		}
		if err := p.Signal(sig); err != nil {
			t.Fatal(err)
		}
		if r := finish(t, s.top, p, []string{"dev", "tap"}, waitTimeout); r.code != want {
			t.Errorf("dev tap after %v: %s\nwant exit %d", sig, r, want)
		}
	}

	wrong := writeFile(t, t.TempDir(), "wrong-token", []byte(s.top.Canary()+"\n"))
	r := cli(t, s.top, cliOptions{}, "dev", "tap", "--admin", n.AdminURL(), "--admin-token-file", wrong, "--ca-file", s.ca)
	if r.code != 2 || !regexp.MustCompile(`401 RZ-AUTH-00[12] `).MatchString(r.stderr) {
		t.Errorf("a refused token: %s\nwant exit 2 with the 401 problem", r)
	}
	if r := cli(t, s.top, cliOptions{}, append(append([]string{"dev", "tap"}, creds...), "--sample", "0")...); r.code != 2 {
		t.Errorf("--sample 0: %s\nwant exit 2, a usage error", r)
	}
	if resp, err := n.Admin(t.Context(), "/readyz"); err != nil || resp.Status != http.StatusOK {
		t.Errorf("the Node after the subscribers: %d, %v", resp.Status, err)
	}
}
