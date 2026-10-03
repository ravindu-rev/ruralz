// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package proc

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
)

func TestRunPattern(t *testing.T) {
	tests := map[string]string{
		"TestA":            "^TestA$",
		"TestA/sub_case":   "^TestA$/^sub_case$",
		"TestA/x.y+(z)/#1": `^TestA$/^x\.y\+\(z\)$/^#1$`,
	}
	for in, want := range tests {
		if got := RunPattern(in); got != want {
			t.Errorf("RunPattern(%q) = %q, want %q", in, got, want)
		}
		for i, part := range regexpParts(RunPattern(in)) {
			if _, err := regexp.Compile(part); err != nil {
				t.Errorf("part %d of %q: %v", i, in, err)
			}
		}
	}
}

func regexpParts(p string) []string {
	var out []string
	start := 0
	for i := 0; i < len(p); i++ {
		if p[i] == '/' && i > 0 && p[i-1] == '$' {
			out = append(out, p[start:i])
			start = i + 1
		}
	}
	return append(out, p[start:])
}

func TestSysctlEncoding(t *testing.T) {
	enc, err := encodeSysctls(map[string]string{"net.ipv4.ip_local_port_range": "1024 65535"})
	if err != nil {
		t.Fatal(err)
	}
	m, err := decodeSysctls(enc)
	if err != nil || m["net.ipv4.ip_local_port_range"] != "1024 65535" {
		t.Fatalf("round trip = %v, %v", m, err)
	}
	if enc, err := encodeSysctls(nil); err != nil || enc != "{}" {
		t.Fatalf("nil = %q, %v", enc, err)
	}
	if m, err := decodeSysctls(""); err != nil || len(m) != 0 {
		t.Fatalf("empty = %v, %v", m, err)
	}
	if _, err := decodeSysctls("{"); err == nil {
		t.Fatal("bad JSON: want error")
	}
	if _, err := encodeSysctls(map[string]string{"a/b": "1"}); err == nil {
		t.Fatal("bad name: want error")
	}
}

func TestNetNSChildOutsideChild(t *testing.T) {
	t.Setenv(EnvInNetNS, "")
	if NetNSChild(t) {
		t.Fatal("NetNSChild outside a child")
	}
}

func TestTestResult(t *testing.T) { // 11 req 40, 44: a skipped InNetNS child is not a pass
	skipOut := "=== RUN   TestA\n    a_test.go:9: redis-server not found\n--- SKIP: TestA (0.00s)\nPASS\n"
	subOut := "=== RUN   TestA\n=== RUN   TestA/sub\n    a_test.go:9: why\n" +
		"    --- SKIP: TestA/sub (0.00s)\n--- PASS: TestA (0.00s)\nPASS\n"
	var many strings.Builder
	many.WriteString("=== RUN   TestA\n")
	for i := range 20 {
		fmt.Fprintf(&many, "    a_test.go:%d: line %d\n", i, i)
	}
	many.WriteString("--- SKIP: TestA (0.00s)\n")
	tests := []struct {
		name, test, out string
		runErr          error
		wantCode        int
		wantErr         error // nil, ErrChildFailed or ErrChildSkipped
		wantMsg         string
	}{
		{"pass", "TestA", "=== RUN   TestA\n--- PASS: TestA (0.01s)\nPASS\n", nil, 0, nil, ""},
		{"pass without newline", "TestA", "--- PASS: TestA (0.01s)", nil, 0, nil, ""},
		{"pass with CRLF", "TestA", "--- PASS: TestA (0.01s)\r\nPASS\r\n", nil, 0, nil, ""},
		{"skip with reason", "TestA", skipOut, nil, 0, ErrChildSkipped, "TestA: a_test.go:9: redis-server not found"},
		{"skip without reason", "TestA", "--- SKIP: TestA (0.00s)\n", nil, 0, ErrChildSkipped, "no reason given"},
		{"skipped subtest", "TestA/sub", subOut, nil, 0, ErrChildSkipped, "why"},
		{"parent of a skipped subtest", "TestA", subOut, nil, 0, nil, ""},
		{"reason keeps the last lines", "TestA", many.String(), nil, 0, ErrChildSkipped, "line 12\na_test.go:13"},
		{"no tests", "TestA", noTests + "\nPASS\n", nil, 0, ErrChildFailed, "no test matched TestA"},
		{"no result line", "TestA", "=== RUN   TestA\nPASS\n", nil, 0, ErrChildFailed, "printed no PASS line"},
		{"another test's pass", "TestA", "--- PASS: TestAB (0.00s)\n", nil, 0, ErrChildFailed, "printed no PASS line"},
		{"run error", "TestA", "", errors.New("context deadline exceeded"), -1, nil, "context deadline exceeded"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, chunk := range []int{1, 7, 1 << 20} { // line splitting is independent of write sizes
				r := newTestResult(tt.test)
				for b := []byte(tt.out); len(b) > 0; {
					n := min(chunk, len(b))
					if w, err := r.Write(b[:n]); w != n || err != nil {
						t.Fatalf("Write = %d, %v", w, err)
					}
					b = b[n:]
				}
				r.finish()
				code, err := r.outcome(tt.runErr, []byte(tt.out))
				if code != tt.wantCode {
					t.Errorf("chunk %d: code = %d, want %d", chunk, code, tt.wantCode)
				}
				switch {
				case tt.wantMsg == "" && err != nil:
					t.Errorf("chunk %d: err = %v, want nil", chunk, err)
				case tt.wantMsg != "" && (err == nil || !strings.Contains(err.Error(), tt.wantMsg)):
					t.Errorf("chunk %d: err = %v, want it to contain %q", chunk, err, tt.wantMsg)
				case tt.wantErr != nil && !errors.Is(err, tt.wantErr):
					t.Errorf("chunk %d: err = %v, want %v", chunk, err, tt.wantErr)
				case tt.wantErr == nil && (errors.Is(err, ErrChildFailed) || errors.Is(err, ErrChildSkipped)):
					t.Errorf("chunk %d: err = %v, want neither sentinel", chunk, err)
				}
			}
		})
	}
	// Reason lines are bounded in number; lines are cut at maxResultLine.
	r := newTestResult("TestA")
	_, _ = r.Write([]byte(many.String()))
	if len(r.reason) != maxReasonLines {
		t.Fatalf("kept %d reason lines, want %d", len(r.reason), maxReasonLines)
	}
	_, _ = r.Write([]byte("    " + strings.Repeat("x", 3*maxResultLine) + "\n"))
	if last := r.reason[len(r.reason)-1]; len(last) > maxResultLine {
		t.Fatalf("kept a %d-byte line", len(last))
	}
}
