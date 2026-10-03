// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package resilience

import (
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/errcode"
)

// req40Table is the code table of 05 req 40, verbatim.
const req40Table = `
| RZ-UP-001 | 502 | Connect failed or dial timed out, no retry |
| RZ-UP-002 | 502 | TLS handshake or verification failed or timed out, no retry |
| RZ-UP-003 | 504 | Deadline expired, or the final attempt timed out |
| RZ-UP-004 | 502 | Reset or protocol error before a response, no retry |
| RZ-UP-005 | 503 | Circuit breaker open, or half-open with its probe in flight |
| RZ-UP-006 | 503 | In-flight ceiling and pending queue full |
| RZ-UP-007 | 502 | Retries ran and the last attempt got no response |
| RZ-UP-008 | 503 | Endpoint set empty |
| RZ-UP-009 | n/a | Upstream failed after commit; the stream ended with an error |
| RZ-UP-010 | 502 | Buffered plain ` + "`upstreams`" + ` response over its cap |
`

// TestCodes_05Req40 holds Codes() equal to the table of 05 I req 40 and
// to the internal/errcode registry.
func TestCodes_05Req40(t *testing.T) {
	var rows []string
	for _, c := range Codes() {
		status := "n/a"
		if c.Status != 0 {
			status = strconv.Itoa(c.Status)
		}
		rows = append(rows, "| "+c.Code+" | "+status+" | "+c.Meaning+" |")
	}
	if got, want := strings.Join(rows, "\n"), strings.TrimSpace(req40Table); got != want {
		t.Fatalf("Codes() table:\n%s\nwant (05 req 40):\n%s", got, want)
	}
	for _, c := range Codes() {
		reg, ok := errcode.Lookup(c.Code)
		if !ok {
			t.Errorf("%s is not registered", c.Code)
			continue
		}
		if reg.Area != errcode.AreaUP || reg.Status != c.Status || !strings.HasPrefix(reg.Meaning, c.Meaning) {
			t.Errorf("%s registry row %+v differs from %+v", c.Code, reg, c)
		}
		if got := Status(c.Code); got != errcode.Status(c.Code) {
			t.Errorf("Status(%s) = %d, registry %d", c.Code, got, errcode.Status(c.Code))
		}
	}
	for _, code := range []string{CodeRouteTimeout, "bogus"} {
		if got := Status(code); got != errcode.Status(code) {
			t.Errorf("Status(%s) = %d, registry %d", code, got, errcode.Status(code))
		}
	}
}

// TestSelectCode_05Req40 is 05 section 6 test 2: {no retry, retry} ×
// {connect, tls, reset, timeout (per-try), timeout (leg), timeout (Route)}
// plus responses and gates.
func TestSelectCode_05Req40(t *testing.T) {
	type failure struct {
		name    string
		kind    Kind
		expired bool
	}
	failures := []failure{
		{"connect", KindConnect, false},
		{"tls", KindTLS, false},
		{"reset", KindReset, false},
		{"timeout per-try", KindTimeout, false},
		{"timeout leg", KindTimeout, true},
		{"timeout Route", KindTimeout, true},
	}
	want := map[string][2]string{ // [no retry, retry]
		"connect":         {"RZ-UP-001", "RZ-UP-007"},
		"tls":             {"RZ-UP-002", "RZ-UP-007"},
		"reset":           {"RZ-UP-004", "RZ-UP-007"},
		"timeout per-try": {"RZ-UP-003", "RZ-UP-003"},
		"timeout leg":     {"RZ-UP-003", "RZ-UP-003"},
		"timeout Route":   {"RZ-UP-003", "RZ-UP-003"},
	}
	for _, f := range failures {
		for i, attempts := range []int{1, 3} {
			o := Outcome{Kind: f.kind, Attempts: attempts, Expired: f.expired}
			if got := SelectCode(o); got != want[f.name][i] {
				t.Errorf("%s with %d attempts: %s, want %s", f.name, attempts, got, want[f.name][i])
			}
		}
	}
	tests := []struct {
		name string
		o    Outcome
		want string
	}{
		{"response after retries returned unchanged", Outcome{Responded: true, Attempts: 3}, ""},
		{"503 response passes through", Outcome{Responded: true, Attempts: 1}, ""},
		{"breaker open", Outcome{Gate: GateBreaker}, "RZ-UP-005"},
		{"bulkhead and waiters full", Outcome{Gate: GateBulkhead}, "RZ-UP-006"},
		{"empty Endpoint set", Outcome{Gate: GateNoEndpoint}, "RZ-UP-008"},
		{"retry that cannot start keeps the previous outcome", Outcome{Kind: KindReset, Attempts: 1, Gate: GateBulkhead}, "RZ-UP-004"},
		{"expired leg with a connect error", Outcome{Kind: KindConnect, Attempts: 2, Expired: true}, "RZ-UP-003"},
		{"leg expired before the first attempt", Outcome{Expired: true}, "RZ-UP-003"},
		{"no kind is a protocol error", Outcome{Attempts: 1}, "RZ-UP-004"},
	}
	for _, tt := range tests {
		if got := SelectCode(tt.o); got != tt.want {
			t.Errorf("%s: SelectCode(%+v) = %q, want %q", tt.name, tt.o, got, tt.want)
		}
	}
}

// TestOutcomeErr covers the leg errors carrying the selected code and, for
// RZ-UP-007, the kind (05 req 40).
func TestOutcomeErr(t *testing.T) {
	cause := io.EOF
	tests := []struct {
		name     string
		o        Outcome
		code     string
		kind     Kind
		sentinel error
	}{
		{"responded", Outcome{Responded: true, Attempts: 1}, "", KindNone, nil},
		{"connect", Outcome{Kind: KindConnect, Attempts: 1}, "RZ-UP-001", KindConnect, cause},
		{"retries names the kind", Outcome{Kind: KindTLS, Attempts: 2}, "RZ-UP-007", KindTLS, cause},
		{"expired without kind", Outcome{Expired: true, Attempts: 1}, "RZ-UP-003", KindTimeout, cause},
		{"no kind", Outcome{Attempts: 1}, "RZ-UP-004", KindNone, cause},
		{"breaker gate", Outcome{Gate: GateBreaker}, "RZ-UP-005", KindNone, ErrBreakerOpen},
		{"bulkhead gate", Outcome{Gate: GateBulkhead}, "RZ-UP-006", KindNone, ErrBulkheadFull},
		{"no Endpoint gate", Outcome{Gate: GateNoEndpoint}, "RZ-UP-008", KindNone, ErrNoEndpoint},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.o.Err(cause)
			if tt.code == "" {
				if err != nil {
					t.Fatalf("Err() = %v, want nil", err)
				}
				return
			}
			code, ok := errcode.CodeOf(err)
			if !ok || code != tt.code {
				t.Fatalf("CodeOf(%v) = %q, want %q", err, code, tt.code)
			}
			if KindOf(err) != tt.kind {
				t.Fatalf("KindOf(%v) = %v, want %v", err, KindOf(err), tt.kind)
			}
			if !errors.Is(err, tt.sentinel) {
				t.Fatalf("%v does not wrap %v", err, tt.sentinel)
			}
		})
	}
}

// TestGate covers the gate codes and errors (05 req 11 steps a and e).
func TestGate(t *testing.T) {
	for g, want := range map[Gate]string{GateNone: "", GateBreaker: "RZ-UP-005", GateBulkhead: "RZ-UP-006", GateNoEndpoint: "RZ-UP-008"} {
		if got := g.Code(); got != want {
			t.Errorf("Gate(%d).Code() = %q, want %q", g, got, want)
		}
		err := g.Err()
		if want == "" {
			if err != nil {
				t.Errorf("GateNone.Err() = %v", err)
			}
			continue
		}
		if code, _ := errcode.CodeOf(err); code != want {
			t.Errorf("Gate(%d).Err() code %q", g, code)
		}
	}
}

// TestExpiryCode_05Req26 maps Route timeout expiry to its code.
func TestExpiryCode_05Req26(t *testing.T) {
	tests := []struct {
		legStarted, committed bool
		want                  string
	}{
		{false, false, "RZ-RT-007"},
		{true, false, "RZ-UP-003"},
		{true, true, "RZ-UP-009"},
		{false, true, "RZ-UP-009"},
	}
	for _, tt := range tests {
		if got := ExpiryCode(tt.legStarted, tt.committed); got != tt.want {
			t.Errorf("ExpiryCode(%v, %v) = %s, want %s", tt.legStarted, tt.committed, got, tt.want)
		}
	}
}
