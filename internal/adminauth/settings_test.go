// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package adminauth

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// Tests for spec 06 requirement 93 and spec 04 requirement 70: the three
// RURALZ_ADMIN_* process settings, token files read at start (trailing
// whitespace trimmed; empty or shorter than 22 bytes refuses start), the
// TLS directory (missing tls.crt or tls.key refuses start; optional
// ca.crt), and requirement 88 (the settings are guarded paths).

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadSettingsReq93(t *testing.T) {
	s, err := LoadSettings(env(nil))
	if err != nil || s != (Settings{}) || s.TLS() || len(s.Guarded()) != 0 {
		t.Fatalf("unset: %+v, %v", s, err)
	}
	const opPath, metricsPath, tlsPath = "/etc/ruralz-admin/op", "/run/secrets/../secrets/metrics", "/etc/ruralz-admin/tls/"
	vars := map[string]string{}
	vars[EnvTokenFile], vars[EnvMetricsTokenFile], vars[EnvTLSDir] = opPath, metricsPath, tlsPath
	s, err = LoadSettings(env(vars))
	if err != nil {
		t.Fatal(err)
	}
	want := Settings{TokenFile: opPath, MetricsTokenFile: "/run/secrets/metrics", TLSDir: "/etc/ruralz-admin/tls"}
	if s != want {
		t.Fatalf("LoadSettings = %+v, want %+v", s, want)
	}
	if !s.TLS() || !slices.Equal(s.Guarded(), []string{want.TokenFile, want.MetricsTokenFile, want.TLSDir}) {
		t.Fatalf("TLS %v, Guarded %v", s.TLS(), s.Guarded())
	}

	_, err = LoadSettings(env(map[string]string{EnvTokenFile: "token", EnvTLSDir: "./tls", EnvMetricsTokenFile: "/ok"}))
	if !errors.Is(err, ErrRelativePath) || !strings.Contains(err.Error(), EnvTokenFile) || !strings.Contains(err.Error(), EnvTLSDir) {
		t.Fatalf("relative paths: %v", err)
	}
	if strings.Contains(err.Error(), EnvMetricsTokenFile) {
		t.Fatalf("a valid setting is reported: %v", err)
	}
}

func TestStartRefusalReq93(t *testing.T) {
	ca := newCA(t, "operators")
	dir := t.TempDir()
	good := writeFile(t, dir, "good", operatorToken+"\n")
	longFile := strings.Repeat("a", maxTokenFileBytes+1)

	tests := []struct {
		name    string
		content string // token file content; "" with dirAsFile uses a directory
		err     error
	}{
		{name: "empty", content: "", err: ErrTokenEmpty},
		{name: "whitespace only", content: " \n\t\r\n", err: ErrTokenEmpty},
		{name: "21 bytes", content: strings.Repeat("x", 21), err: ErrTokenShort},
		{name: "21 bytes and a newline", content: strings.Repeat("x", 21) + "\n", err: ErrTokenShort},
		{name: "4097 bytes", content: strings.Repeat("x", MaxTokenBytes+1), err: ErrTokenLong},
		{name: "file over 64 KiB", content: longFile, err: ErrTokenLong},
		{name: "leading space", content: " " + operatorToken, err: ErrTokenSyntax},
		{name: "inner space", content: "op-canary 4fQ9x2LmZ7rT1vB8nK3sW6yH", err: ErrTokenSyntax},
		{name: "two lines", content: operatorToken + "\n" + operatorToken, err: ErrTokenSyntax},
		{name: "symbol", content: operatorToken + "!", err: ErrTokenSyntax},
		{name: "padding then text", content: "abcdefghijklmnopqrstuvwxyz==x", err: ErrTokenSyntax},
		{name: "non-ASCII", content: operatorToken + "é", err: ErrTokenSyntax},
		{name: "22 bytes", content: strings.Repeat("x", 22)},
		{name: "4096 bytes", content: strings.Repeat("y", MaxTokenBytes)},
		{name: "trailing whitespace trimmed", content: operatorToken + " \t\r\n\n"},
		{name: "base64 padding", content: "c2VjcmV0LXRva2VuLWZvci1hZG1pbg=="},
		{name: "all b64token characters", content: "azAZ09-._~+/azAZ09-._~+/=="},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := writeFile(t, t.TempDir(), "token", tt.content)
			for _, s := range []Settings{{TokenFile: p}, {MetricsTokenFile: p}} {
				a, err := New(s, Options{})
				if tt.err == nil {
					if err != nil || a == nil {
						t.Fatalf("New(%+v): %v", s, err)
					}
					continue
				}
				if !errors.Is(err, tt.err) || a != nil {
					t.Fatalf("New(%+v) = %v, want %v", s, err, tt.err)
				}
				if tok := strings.TrimSpace(tt.content); tok != "" && len(tok) < 100 && strings.Contains(err.Error(), tok) {
					t.Fatalf("the error quotes the token: %v", err)
				}
			}
		})
	}

	t.Run("missing file", func(t *testing.T) {
		_, err := New(Settings{TokenFile: filepath.Join(dir, "absent")}, Options{})
		if !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), EnvTokenFile) {
			t.Fatalf("New = %v", err)
		}
	})
	t.Run("directory", func(t *testing.T) {
		_, err := New(Settings{MetricsTokenFile: dir}, Options{})
		if !errors.Is(err, ErrNotRegular) || !strings.Contains(err.Error(), EnvMetricsTokenFile) {
			t.Fatalf("New = %v", err)
		}
	})
	t.Run("TLS directory", func(t *testing.T) {
		full := tlsDir(t, ca, true)
		a, err := New(Settings{TLSDir: full}, Options{})
		if err != nil || !a.ClientCertificates() || !a.Configured() {
			t.Fatalf("with ca.crt: %v", err)
		}
		noCA := tlsDir(t, ca, false)
		a, err = New(Settings{TLSDir: noCA}, Options{})
		if err != nil || a.ClientCertificates() || a.Configured() {
			t.Fatalf("without ca.crt: %v", err)
		}
		for _, missing := range []string{TLSCertFile, TLSKeyFile} {
			d := tlsDir(t, ca, true)
			if err := os.Remove(filepath.Join(d, missing)); err != nil {
				t.Fatal(err)
			}
			if _, err := New(Settings{TLSDir: d}, Options{}); !errors.Is(err, ErrTLSFileMissing) {
				t.Fatalf("without %s: %v", missing, err)
			}
		}
		d := tlsDir(t, ca, false)
		if err := os.Mkdir(filepath.Join(d, ClientCAFile), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := New(Settings{TLSDir: d}, Options{}); !errors.Is(err, ErrNotRegular) {
			t.Fatalf("ca.crt directory: %v", err)
		}
		d = tlsDir(t, ca, false)
		if err := os.Remove(filepath.Join(d, TLSKeyFile)); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(d, TLSKeyFile), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := New(Settings{TLSDir: d}, Options{}); !errors.Is(err, ErrTLSFileMissing) || !errors.Is(err, ErrNotRegular) {
			t.Fatalf("tls.key directory: %v", err)
		}
		// ca.crt is parsed as internal/tlsconf.Admin parses it.
		for _, bad := range []struct {
			name, content string
			err           error
		}{
			{"no PEM", "not a certificate\n", ErrClientCA},
			{"no CERTIFICATE block", "-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n", ErrClientCA},
			{"a CERTIFICATE that does not parse", string(ca.pem) + "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n", ErrClientCA},
			{"over 1 MiB", string(ca.pem) + strings.Repeat("#", maxClientCABytes), ErrClientCALarge},
		} {
			d := tlsDir(t, ca, false)
			writeFile(t, d, ClientCAFile, bad.content)
			if _, err := New(Settings{TLSDir: d}, Options{}); !errors.Is(err, bad.err) || !strings.Contains(err.Error(), EnvTLSDir) {
				t.Fatalf("ca.crt %s: %v", bad.name, err)
			}
		}
		other := newCA(t, "other")
		d = tlsDir(t, ca, false)
		writeFile(t, d, ClientCAFile, "leading text\n"+string(other.pem)+"-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n"+string(ca.pem))
		a, err = New(Settings{TLSDir: d}, Options{})
		if err != nil || !a.ClientCertificates() || len(a.anchors) != 2 {
			t.Fatalf("bundle of two: %v", err)
		}
		if runtime.GOOS != "windows" && os.Getuid() != 0 { // mode 0 stops only a non-root unix reader
			if err := os.Chmod(filepath.Join(d, ClientCAFile), 0); err != nil {
				t.Fatal(err)
			}
			if _, err := New(Settings{TLSDir: d}, Options{}); !errors.Is(err, os.ErrPermission) {
				t.Fatalf("unreadable ca.crt: %v", err)
			}
		}
		if _, err := New(Settings{TLSDir: good}, Options{}); !errors.Is(err, ErrNotDirectory) {
			t.Fatalf("TLS dir is a file: %v", err)
		}
		if _, err := New(Settings{TLSDir: filepath.Join(dir, "absent")}, Options{}); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("TLS dir missing: %v", err)
		}
	})
	t.Run("every error reported", func(t *testing.T) {
		empty := writeFile(t, t.TempDir(), "empty", "")
		short := writeFile(t, t.TempDir(), "short", "short")
		_, err := New(Settings{TokenFile: empty, MetricsTokenFile: short, TLSDir: tlsDir(t, ca, false) + "-absent"}, Options{})
		if !errors.Is(err, ErrTokenEmpty) || !errors.Is(err, ErrTokenShort) || !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("New = %v", err)
		}
	})
	t.Run("settings kept", func(t *testing.T) {
		s := Settings{TokenFile: good}
		a, err := New(s, Options{})
		if err != nil || a.Settings() != s || !a.Configured() || a.ClientCertificates() {
			t.Fatalf("New = %v", err)
		}
		a, err = New(Settings{}, Options{})
		if err != nil || a.Configured() {
			t.Fatalf("no credential: %v", err)
		}
	})
}
