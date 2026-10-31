// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package adminclient

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
)

// Tests for the version skew warning of
// docs/reference/01-cli-and-api-surface.md "Admin API conventions": a
// Node's authenticated responses carry ruralz-version, and a CLI two or
// more minor releases newer warns once on standard error.

// TestOutsideSkew covers the comparison, each (major, minor) pair
// counting as a release.
func TestOutsideSkew(t *testing.T) {
	cases := []struct {
		cli, node string
		want      bool
	}{
		{"0.4.0", "0.4.0", false},
		{"0.4.0", "0.3.9", false},
		{"0.4.0", "0.2.0", true},
		{"0.9.1", "0.2.7", true},
		{"0.4.0-rc.1", "0.2.0", true},
		{"0.4.0+build.7", "0.2.0-dev", true},
		{"0.2.0", "0.4.0", false},
		{"1.0.0", "0.9.0", false},
		{"1.1.0", "0.9.0", true},
		{"2.0.0", "0.9.0", true},
		{"1.3.0", "2.0.0", false},
		{"0.0.0-dev", "0.4.0", false},
		{"dev", "0.1.0", false},
		{"0.4.0", "unknown", false},
		{"0.4.0", "v0.1.0", false},
		{"0.4.0", "0.1", false},
		{"0.4.0", "0.1.0.0", false},
		{"0.4.0", "0.-1.0", false},
		{"0.4.0", "0.1.x", false},
		{"0.40000000000.0", "0.1.0", false},
	}
	for _, c := range cases {
		if got := outsideSkew(c.cli, c.node); got != c.want {
			t.Errorf("outsideSkew(%q, %q) = %v, want %v", c.cli, c.node, got, c.want)
		}
	}
}

// TestSkewWarnsOnce checks the warning: once per client however many
// responses are skewed, naming both versions and the Node; none without
// the header, for a newer Node or a version that does not parse.
func TestSkewWarnsOnce(t *testing.T) {
	versions := map[string]string{
		"/old/config/dump":     "0.2.3",
		"/same/config/dump":    "0.4.0",
		"/newer/config/dump":   "0.6.0",
		"/garbled/config/dump": "0.2.x",
		"/absent/config/dump":  "",
	}
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if v := versions[r.URL.Path]; v != "" {
			w.Header().Set("Ruralz-Version", v)
		}
		_, _ = io.WriteString(w, "{}")
	})
	for prefix, want := range map[string]string{
		"/old": "warning: ruralz 0.4.0 is two or more minor releases newer than the Node at " + srv.URL +
			"/old (ruralz-version 0.2.3), outside the version skew policy\n",
		"/same": "", "/newer": "", "/garbled": "", "/absent": "",
	} {
		var stderr bytes.Buffer
		c, err := New(Config{URL: srv.URL + prefix, Timeout: DefaultTimeout},
			Options{Stderr: &stderr, Clock: clocktest.New(epoch()), Version: testVersion})
		if err != nil {
			t.Fatal(err)
		}
		for range 3 {
			if _, err := c.ConfigDump(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		c.Close()
		if got := stderr.String(); got != want {
			t.Errorf("%s: stderr = %q, want %q", prefix, got, want)
		}
	}
}

// TestSkewWithoutStderr checks that a client without Stderr checks the
// skew silently.
func TestSkewWithoutStderr(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Ruralz-Version", "0.1.0")
		_, _ = io.WriteString(w, "{}")
	})
	c, _ := newTestClient(t, Config{URL: srv.URL, Timeout: DefaultTimeout})
	if _, err := c.ConfigDump(t.Context()); err != nil || !c.warned.Load() {
		t.Errorf("ConfigDump = %v, warned %v", err, c.warned.Load())
	}
}

// TestSkewCleansVersions checks that a Node version prints on one
// printable line: obs-text bytes that are not UTF-8, which HTTP/1.1 lets
// through, and control characters both print as U+FFFD.
func TestSkewCleansVersions(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Ruralz-Version", "0.1.0-\xff\xfe")
		_, _ = io.WriteString(w, "{}")
	})
	var stderr bytes.Buffer
	c, err := New(Config{URL: srv.URL, Timeout: DefaultTimeout}, Options{Stderr: &stderr, Clock: clocktest.New(epoch()), Version: testVersion})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.ConfigDump(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := stderr.String(); !strings.Contains(got, "(ruralz-version 0.1.0-�)") {
		t.Errorf("stderr = %q", got)
	}

	stderr.Reset()
	c.warned.Store(false)
	c.checkSkew(http.Header{"Ruralz-Version": {"0.1.0-\x1b[2J"}})
	if got := stderr.String(); strings.ContainsRune(got, '\x1b') || !strings.Contains(got, "(ruralz-version 0.1.0-�[2J)") {
		t.Errorf("stderr = %q", got)
	}
}
