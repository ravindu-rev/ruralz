// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build t5 && linux

package t5

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/loadgen"
)

// These tests check the live suite's own parts, which run only where
// systemd is PID 1, on inputs built here.

// TestParseShow checks the reading of `systemctl show` output.
func TestParseShow(t *testing.T) {
	got := parseShow("MainPID=4242\nStatusText=not ready: no_revision RZ-CFG-002 a=b\nWatchdogTimestamp=\n")
	want := map[string]string{"MainPID": "4242", "StatusText": "not ready: no_revision RZ-CFG-002 a=b", "WatchdogTimestamp": ""}
	if len(got) != len(want) {
		t.Errorf("parseShow() = %q, want %q", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("parseShow()[%q] = %q, want %q", k, got[k], v)
		}
	}
}

// TestParseStat checks the state and start time read from
// /proc/<pid>/stat, whose command name may hold spaces and parentheses.
func TestParseStat(t *testing.T) {
	fields := "S 1 4242 4242 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 9 0 987654 1000000 200"
	for _, tc := range []struct {
		name, stat, state string
		start             uint64
		bad               bool
	}{
		{"plain", "4242 (ruralzd) " + fields, "S", 987654, false},
		{"odd name", "4242 (rz (a) b) " + fields, "S", 987654, false},
		{"zombie", "4242 (ruralzd) Z" + fields[1:], "Z", 987654, false},
		{"no name", "4242 ruralzd S 1", "", 0, true},
		{"short", "4242 (ruralzd) S 1 2", "", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, start, err := parseStat([]byte(tc.stat))
			if (err != nil) != tc.bad || state != tc.state || start != tc.start {
				t.Errorf("parseStat(%q) = %q, %d, %v; want %q, %d, error %t", tc.stat, state, start, err, tc.state, tc.start, tc.bad)
			}
		})
	}
}

// TestLookupProc checks process identity: this process runs, and a PID
// with another start time is another process.
func TestLookupProc(t *testing.T) {
	p, ok := lookupProc(os.Getpid())
	if !ok || !p.alive() {
		t.Fatalf("lookupProc(self) = %v, %t; want this running process", p, ok)
	}
	if (proc{PID: p.PID, Start: p.Start + 1}).alive() {
		t.Error("a process with another start time counts as this one")
	}
}

// TestCapProblems checks the capability rule: CAP_NET_BIND_SERVICE and
// nothing else in every set, present in all but the inheritable one.
func TestCapProblems(t *testing.T) {
	only := "0000000000000400"
	base := map[string]string{"CapInh": only, "CapPrm": only, "CapEff": only, "CapBnd": only, "CapAmb": only}
	with := func(k, v string) map[string]string {
		m := map[string]string{}
		for kk, vv := range base {
			m[kk] = vv
		}
		m[k] = v
		return m
	}
	for _, tc := range []struct {
		name   string
		status map[string]string
		want   string
	}{
		{"only CAP_NET_BIND_SERVICE", base, ""},
		{"empty inheritable", with("CapInh", "0000000000000000"), ""},
		{"extra effective", with("CapEff", "0000000000000401"), "CapEff"},
		{"full bounding", with("CapBnd", "000001ffffffffff"), "CapBnd"},
		{"no ambient", with("CapAmb", "0000000000000000"), "lacks"},
		{"unreadable", with("CapPrm", "zz"), "CapPrm"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := capProblems(tc.status)
			switch {
			case tc.want == "" && len(got) > 0:
				t.Errorf("capProblems() = %q, want none", got)
			case tc.want != "" && (len(got) != 1 || !strings.Contains(got[0], tc.want)):
				t.Errorf("capProblems() = %q, want one with %q", got, tc.want)
			}
		})
	}
	if got := parseStatus([]byte("Name:\truralzd\nCapEff:\t0000000000000400\nNoNewPrivs:\t1\n")); got["CapEff"] != only || got["NoNewPrivs"] != "1" {
		t.Errorf("parseStatus() = %q", got)
	}
}

// tarball returns a gzip-compressed tar of the members, each a name with a
// type flag and content.
func tarball(t *testing.T, members ...tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, h := range members {
		body := []byte(h.Linkname)
		h.Linkname = ""
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(body))
		}
		h.Mode = 0o644
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write(body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestReadArchive checks the archive reader: the binary and the shipped
// files are found, and a member that is not a local, unique directory or
// regular file refuses the archive.
func TestReadArchive(t *testing.T) {
	// A regular member's content travels in Linkname, which tarball moves
	// into the body.
	reg := func(name, content string) tar.Header {
		return tar.Header{Name: name, Typeflag: tar.TypeReg, Linkname: content}
	}
	good := []tar.Header{
		reg("LICENSE", "license"), reg("ruralzd", "binary"),
		{Name: "deploy/", Typeflag: tar.TypeDir},
		{Name: "deploy/systemd/", Typeflag: tar.TypeDir},
		reg("deploy/systemd/99-ruralzd.conf", "sysctl"), reg("deploy/systemd/ruralzd-handover", "helper"),
		reg("deploy/systemd/ruralzd.env", "env"), reg("deploy/systemd/ruralzd.service", "unit"),
		reg("deploy/systemd/ruralzd.sysusers", "sysusers"),
	}
	a, err := readArchive(tarball(t, good...))
	if err != nil {
		t.Fatal(err)
	}
	if string(a.binary) != "binary" || string(a.files[unitFile]) != "unit" || string(a.files[helperFile]) != "helper" || len(a.files) != 5 {
		t.Errorf("readArchive() = binary %q, files %q", a.binary, a.files)
	}
	for _, tc := range []struct {
		name   string
		member tar.Header
	}{
		{"parent path", reg("../ruralzd", "x")},
		{"absolute path", reg("/etc/passwd", "x")},
		{"unclean path", reg("deploy//systemd/x", "x")},
		{"symbolic link", tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}},
		{"duplicate", reg("ruralzd", "again")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := readArchive(tarball(t, append(append([]tar.Header(nil), good...), tc.member)...)); err == nil {
				t.Errorf("readArchive() accepted a member %q", tc.member.Name)
			}
		})
	}
	t.Run("missing file", func(t *testing.T) {
		if _, err := readArchive(tarball(t, good[:len(good)-1]...)); err == nil {
			t.Error("readArchive() accepted an archive without ruralzd.sysusers")
		}
	})
}

// TestVerifySum checks the archive against SHA256SUMS, text and binary
// mode lines alike.
func TestVerifySum(t *testing.T) {
	dir := t.TempDir()
	data := []byte("archive")
	sum := sha256.Sum256(data)
	p := filepath.Join(dir, "ruralzd_0.1.0_linux_amd64.tar.gz")
	for _, tc := range []struct {
		name, sums string
		ok         bool
	}{
		{"text mode", hex.EncodeToString(sum[:]) + "  ruralzd_0.1.0_linux_amd64.tar.gz\n", true},
		{"binary mode", hex.EncodeToString(sum[:]) + " *ruralzd_0.1.0_linux_amd64.tar.gz\n", true},
		{"other digest", strings.Repeat("0", 64) + "  ruralzd_0.1.0_linux_amd64.tar.gz\n", false},
		{"not listed", hex.EncodeToString(sum[:]) + "  other.tar.gz\n", false},
		{"malformed", "abc ruralzd_0.1.0_linux_amd64.tar.gz\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(tc.sums), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := verifySum(p, data); (err == nil) != tc.ok {
				t.Errorf("verifySum() = %v, want ok %t", err, tc.ok)
			}
		})
	}
}

// TestEnvAndUnitValues checks the reading of the shipped EnvironmentFile
// and unit, which tell the suite where RURALZ_CONFIG and the admin token
// file are.
func TestEnvAndUnitValues(t *testing.T) {
	root := repoRoot(t)
	if v := envValue(shipped(t, root, envFile), "RURALZ_CONFIG"); v != "/etc/ruralzd/bundle" {
		t.Errorf("RURALZ_CONFIG of the shipped %s = %q", envFile, v)
	}
	if v := envValue([]byte("A=1\n#A=2\nA=3\n"), "A"); v != "3" {
		t.Errorf("envValue() = %q, want the last assignment 3", v)
	}
	if v := adminTokenFile(shipped(t, root, unitFile)); v != "/etc/ruralzd/admin-token" {
		t.Errorf("RURALZ_ADMIN_TOKEN_FILE of the shipped %s = %q", unitFile, v)
	}
}

// TestCheckedTarget checks the load target's response check: the Route's
// 200 passes, anything else fails with its reason.
func TestCheckedTarget(t *testing.T) {
	var mode atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch mode.Load() {
		case 0:
			w.Header().Set(policyHeader, policyValue)
			_, _ = io.WriteString(w, upstreamBody)
		case 1:
			_, _ = io.WriteString(w, upstreamBody)
		case 2:
			w.Header().Set(policyHeader, policyValue)
			_, _ = io.WriteString(w, "other\n")
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	srv.EnableHTTP2 = true
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	srv.Config.Protocols = &protocols
	srv.Start()
	defer srv.Close()
	for _, proto := range []loadgen.Protocol{loadgen.HTTP1, loadgen.H2C} {
		tg := newCheckedTarget(srv.URL+"/", proto, 2)
		var seq uint64
		for m, want := range []loadgen.Outcome{loadgen.Success, loadgen.Other, loadgen.Other, loadgen.ServerError} {
			mode.Store(int64(m))
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			r := tg.Do(ctx, loadgen.Request{Seq: seq})
			cancel()
			seq++
			if r.Outcome != want {
				t.Errorf("%v, response mode %d: outcome %v (%v), want %v", proto, m, r.Outcome, r.Err, want)
			}
		}
		tg.close()
	}
}

// TestReadyzChecker checks that the keep-alive checker counts each probe
// that does not answer 200.
func TestReadyzChecker(t *testing.T) {
	var fail atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()
	c := startChecker(t.Context(), t, srv.URL)
	wait := func(cond func(probes, failed int) bool) {
		t.Helper()
		err := waitFor(t.Context(), 10*time.Second, func() (bool, string) {
			c.mu.Lock()
			defer c.mu.Unlock()
			return cond(c.probes, c.failed), "probes not seen"
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	wait(func(probes, _ int) bool { return probes >= 2 })
	fail.Store(true)
	wait(func(_, failed int) bool { return failed >= 1 })
	probes, failed, failures := c.stop()
	if failed < 1 || probes <= failed || len(failures) == 0 || failures[0] != "status 503" {
		t.Errorf("checker.stop() = %d probes, %d failed, %q", probes, failed, failures)
	}
}
