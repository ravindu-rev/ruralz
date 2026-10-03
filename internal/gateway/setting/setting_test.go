// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package setting

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/egress"
	"github.com/ravindu-rev/ruralz/internal/nodedir"
)

// env is a fake environment for Load.
type env map[string]string

func (e env) lookup(name string) (string, bool) {
	v, ok := e[name]
	return v, ok
}

// fixture is a temporary layout: a secret root, a data dir beside it and
// valid key files.
type fixture struct {
	base, root, data, key string
}

// macKeyBytes is a 32-byte canary key.
const macKeyBytes = "CANARY-MAC-KEY-0123456789abcdef!"

func newFixture(t *testing.T) fixture {
	t.Helper()
	base := t.TempDir()
	f := fixture{
		base: base,
		root: filepath.Join(base, "etc", "ruralz"),
		data: filepath.Join(base, "var", "lib", "ruralz"),
		key:  filepath.Join(base, "keys", "mac.key"),
	}
	for _, d := range []string{f.root, f.data, filepath.Dir(f.key)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, f.key, macKeyBytes, 0o600)
	return f
}

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// base returns a valid environment for the fixture.
func (f fixture) env() env {
	return env{
		EnvConfig:     filepath.Join(f.base, "bundle"),
		EnvDataDir:    f.data,
		EnvSecretRoot: f.root,
	}
}

// with returns a copy of e with the given pairs set.
func (e env) with(kv ...string) env {
	out := env{}
	for k, v := range e {
		out[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i]] = kv[i+1]
	}
	return out
}

// without returns a copy of e without the given names.
func (e env) without(names ...string) env {
	out := e.with()
	for _, n := range names {
		delete(out, n)
	}
	return out
}

// TestLoadValid covers every setting's valid form (spec 04 requirement 2
// and test plan item 1 "every variable valid").
func TestLoadValid(t *testing.T) {
	f := newFixture(t)
	s, err := Load(nil, f.env().with(
		EnvLogLevel, "debug",
		EnvFetchAllow, " 127.0.0.1/32, ::1 ,env-proxy,10.0.0.0/8,::ffff:10.0.0.1",
		EnvAdminTokenFile, filepath.Join(f.base, "admin", "token"),
		EnvAdminMetricsTokenFile, filepath.Join(f.base, "admin", "metrics.token"),
		EnvAdminTLSDir, filepath.Join(f.base, "admin", "tls")+"/",
		EnvStateStoreMACKeyFile, f.key,
		EnvStateStoreURL, "redis://user:pw@127.0.0.1:6379",
	).lookup)
	if err != nil {
		t.Fatal(err)
	}
	want := Settings{
		Mode:                  ModeFile,
		Config:                filepath.Join(f.base, "bundle"),
		DataDir:               f.data,
		LogLevel:              LogDebug,
		SecretRoot:            f.root,
		FetchAllow:            " 127.0.0.1/32, ::1 ,env-proxy,10.0.0.0/8,::ffff:10.0.0.1",
		AdminTokenFile:        filepath.Join(f.base, "admin", "token"),
		AdminMetricsTokenFile: filepath.Join(f.base, "admin", "metrics.token"),
		AdminTLSDir:           filepath.Join(f.base, "admin", "tls"),
		MACKeyFile:            f.key,
	}
	key := s.MACKey
	s.MACKey = MACKey{}
	if fmt.Sprintf("%#v", s) != fmt.Sprintf("%#v", want) {
		t.Fatalf("Load =\n%#v\nwant\n%#v", s, want)
	}
	if string(key.Bytes()) != macKeyBytes || key.Len() != 32 || !key.IsSet() {
		t.Fatalf("MAC key not read (len %d)", key.Len())
	}
}

// TestLoadDefaults covers the defaults of spec 04 requirement 2 and spec
// 06 requirement 88: only RURALZ_CONFIG set; relative paths of
// RURALZ_CONFIG and RURALZ_DATA_DIR are made absolute.
func TestLoadDefaults(t *testing.T) {
	s, err := Load(nil, env{EnvConfig: "/srv/bundle"}.lookup)
	if err != nil {
		t.Fatal(err)
	}
	if s.Mode != ModeFile || s.Config != "/srv/bundle" || s.DataDir != DefaultDataDir ||
		s.LogLevel != LogInfo || s.SecretRoot != DefaultSecretRoot || s.MACKey.IsSet() ||
		s.FetchAllow != "" || s.Unused != nil || s.AdminTokenFile != "" {
		t.Fatalf("Load = %+v", s)
	}
	if DefaultDataDir != nodedir.DefaultRoot {
		t.Errorf("DefaultDataDir %q != nodedir.DefaultRoot %q", DefaultDataDir, nodedir.DefaultRoot)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	s, err = Load([]string{}, env{EnvConfig: "bundle/./x", EnvDataDir: "data", EnvLogLevel: ""}.lookup)
	if err != nil {
		t.Fatal(err)
	}
	if s.Config != filepath.Join(wd, "bundle", "x") || s.DataDir != filepath.Join(wd, "data") || s.LogLevel != LogInfo {
		t.Fatalf("Load = %+v", s)
	}
}

// TestLoadExit2 is the settings table of every exit-2 case (spec 04
// requirements 1 to 4, spec 06 requirements 85, 88 and 93, spec 08
// requirement 68, R-49; test plan item 1).
func TestLoadExit2(t *testing.T) {
	f := newFixture(t)
	good := f.env()
	short := filepath.Join(f.base, "keys", "short.key")
	writeFile(t, short, macKeyBytes[:31], 0o600)
	empty := filepath.Join(f.base, "keys", "empty.key")
	writeFile(t, empty, "", 0o600)
	long := filepath.Join(f.base, "keys", "long.key")
	writeFile(t, long, strings.Repeat("k", MaxMACKeyBytes+1), 0o600)
	maxKey := filepath.Join(f.base, "keys", "max.key")
	writeFile(t, maxKey, strings.Repeat("k", MaxMACKeyBytes), 0o600)
	groupReadable := filepath.Join(f.base, "keys", "group.key")
	writeFile(t, groupReadable, macKeyBytes, 0o640)
	otherReadable := filepath.Join(f.base, "keys", "other.key")
	writeFile(t, otherReadable, macKeyBytes, 0o604)
	groupExec := filepath.Join(f.base, "keys", "gx.key")
	writeFile(t, groupExec, macKeyBytes, 0o610)
	readOnly := filepath.Join(f.base, "keys", "ro.key")
	writeFile(t, readOnly, macKeyBytes, 0o400)
	insideKey := filepath.Join(f.root, "mac.key")
	writeFile(t, insideKey, macKeyBytes, 0o600)
	linkToGroup := filepath.Join(f.base, "keys", "link.key")
	if err := os.Symlink(groupReadable, linkToGroup); err != nil {
		t.Fatal(err)
	}
	linkToGood := filepath.Join(f.base, "keys", "goodlink.key")
	if err := os.Symlink(f.key, linkToGood); err != nil {
		t.Fatal(err)
	}
	unix := runtime.GOOS != "windows"
	// A FIFO without a writer: a blocking open would hang the start.
	fifo := filepath.Join(f.base, "keys", "fifo.key")
	hasFIFO := unix && mkfifo(t, fifo)
	// A 0600 key owned by another account, which could rewrite it.
	foreign := filepath.Join(f.base, "keys", "foreign.key")
	writeFile(t, foreign, macKeyBytes, 0o600)
	asRoot := unix && os.Geteuid() == 0
	if asRoot {
		if err := os.Chown(foreign, 65534, 65534); err != nil {
			t.Fatal(err)
		}
	}
	// A data dir reached through a link into the secret root.
	linkedData := filepath.Join(f.base, "linked-data")
	if err := os.Symlink(filepath.Join(f.root, "data"), linkedData); err != nil {
		t.Fatal(err)
	}
	// The same with an existing target, and a relative dangling chain.
	if err := os.Mkdir(filepath.Join(f.root, "data2"), 0o700); err != nil {
		t.Fatal(err)
	}
	linkedData2 := filepath.Join(f.base, "linked-data2")
	if err := os.Symlink(filepath.Join(f.root, "data2"), linkedData2); err != nil {
		t.Fatal(err)
	}
	chain := filepath.Join(f.base, "chain")
	if err := os.Symlink("linked-data", chain); err != nil {
		t.Fatal(err)
	}
	loop := filepath.Join(f.base, "loop")
	if err := os.Symlink("loop", loop); err != nil {
		t.Fatal(err)
	}
	// A secret root reached through a link that contains the data dir.
	linkedRoot := filepath.Join(f.base, "linked-root")
	if err := os.Symlink(filepath.Dir(f.data), linkedRoot); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		args    []string
		env     env
		setting string // "" for a usage error or when ok
		want    error  // nil: Load succeeds
		msg     string // substring of the error text
		skip    bool
	}{
		// Spec 04 requirement 4: any argument.
		{name: "positional argument", args: []string{"serve"}, env: good, want: ErrUsage, msg: `got "serve"`},
		{name: "flag", args: []string{"-h"}, env: good, want: ErrUsage, msg: "no arguments or flags"},
		// Spec 04 requirements 1 and 2: RURALZ_CONFIG.
		{name: "config unset", env: good.without(EnvConfig), setting: EnvConfig, want: ErrRequired},
		{name: "config empty", env: good.with(EnvConfig, ""), setting: EnvConfig, want: ErrRequired},
		{name: "config oci", env: good.with(EnvConfig, "oci://registry.example/bundles/shop:1"), setting: EnvConfig, want: ErrPlanned, msg: "Planned (M2): oci:// selects OCI pull mode"},
		{name: "config OCI uppercase", env: good.with(EnvConfig, "OCI://registry.example/x"), setting: EnvConfig, want: ErrPlanned, msg: "Planned (M2)"},
		{name: "config control", env: good.with(EnvConfig, "ruralz-control://control.example:8091"), setting: EnvConfig, want: ErrPlanned, msg: "Planned (M2): ruralz-control:// selects Control mode"},
		{name: "config https", env: good.with(EnvConfig, "https://example.com/bundle"), setting: EnvConfig, want: ErrSource, msg: `scheme "https"`},
		{name: "config file URL", env: good.with(EnvConfig, "file:///srv/bundle"), setting: EnvConfig, want: ErrSource},
		{name: "config path with ://", env: good.with(EnvConfig, "/srv/a://b"), want: nil},
		{name: "config odd scheme chars", env: good.with(EnvConfig, "1x://b"), want: nil},
		// Spec 04 requirement 2: RURALZ_LOG_LEVEL.
		{name: "log level trace", env: good.with(EnvLogLevel, "trace"), setting: EnvLogLevel, want: ErrLogLevel, msg: `got "trace"`},
		{name: "log level uppercase", env: good.with(EnvLogLevel, "INFO"), setting: EnvLogLevel, want: ErrLogLevel},
		{name: "log level warning", env: good.with(EnvLogLevel, "warning"), setting: EnvLogLevel, want: ErrLogLevel},
		{name: "log level error", env: good.with(EnvLogLevel, "error"), want: nil},
		{name: "log level warn", env: good.with(EnvLogLevel, "warn"), want: nil},
		// Spec 01 requirement 45: a relative secret root.
		{name: "secret root relative", env: good.with(EnvSecretRoot, "etc/ruralz"), setting: EnvSecretRoot, want: ErrNotAbsolute},
		// Spec 06 requirement 93: admin settings are absolute paths.
		{name: "admin token relative", env: good.with(EnvAdminTokenFile, "token"), setting: EnvAdminTokenFile, want: ErrNotAbsolute},
		{name: "metrics token relative", env: good.with(EnvAdminMetricsTokenFile, "./m"), setting: EnvAdminMetricsTokenFile, want: ErrNotAbsolute},
		{name: "admin TLS dir relative", env: good.with(EnvAdminTLSDir, "tls"), setting: EnvAdminTLSDir, want: ErrNotAbsolute},
		// Spec 04 requirement 3, spec 06 requirement 88: the secret root
		// never contains a protected path.
		{name: "data dir inside root", env: good.with(EnvDataDir, filepath.Join(f.root, "data")), setting: EnvDataDir, want: ErrInsideSecretRoot, msg: EnvSecretRoot + "=" + f.root},
		{name: "data dir is root", env: good.with(EnvDataDir, f.root), setting: EnvDataDir, want: ErrInsideSecretRoot},
		{name: "relative data dir inside root", env: good.with(EnvDataDir, "x").with(EnvSecretRoot, mustAbs(t, ".")), setting: EnvDataDir, want: ErrInsideSecretRoot},
		{name: "data dir via symlink into root", env: good.with(EnvDataDir, linkedData), setting: EnvDataDir, want: ErrInsideSecretRoot, skip: !unix},
		{name: "data dir via symlink to existing dir in root", env: good.with(EnvDataDir, linkedData2), setting: EnvDataDir, want: ErrInsideSecretRoot, skip: !unix},
		{name: "data dir via relative link chain", env: good.with(EnvDataDir, filepath.Join(chain, "sub")), setting: EnvDataDir, want: ErrInsideSecretRoot, skip: !unix},
		{name: "data dir via link loop", env: good.with(EnvDataDir, loop), want: nil, skip: !unix},
		{name: "root via symlink over data dir", env: good.with(EnvSecretRoot, linkedRoot), setting: EnvDataDir, want: ErrInsideSecretRoot, skip: !unix},
		{name: "default data dir inside root", env: good.without(EnvDataDir).with(EnvSecretRoot, "/var/lib"), setting: EnvDataDir, want: ErrInsideSecretRoot},
		{name: "default root contains data dir", env: good.without(EnvSecretRoot).with(EnvDataDir, "/etc/ruralz/data"), setting: EnvDataDir, want: ErrInsideSecretRoot},
		{name: "prefix sibling is outside", env: good.with(EnvDataDir, f.root+"2"), want: nil},
		{name: "parent of root is outside", env: good.with(EnvDataDir, filepath.Dir(f.root)), want: nil},
		{name: "admin TLS dir inside root", env: good.with(EnvAdminTLSDir, filepath.Join(f.root, "admin-tls")), setting: EnvAdminTLSDir, want: ErrInsideSecretRoot},
		{name: "admin token inside root", env: good.with(EnvAdminTokenFile, filepath.Join(f.root, "admin.token")), setting: EnvAdminTokenFile, want: ErrInsideSecretRoot},
		{name: "metrics token inside root", env: good.with(EnvAdminMetricsTokenFile, filepath.Join(f.root, "sub", "..", "m.token")), setting: EnvAdminMetricsTokenFile, want: ErrInsideSecretRoot},
		{name: "enrollment token inside root", env: good.with(EnvEnrollmentTokenFile, filepath.Join(f.root, "enroll")), setting: EnvEnrollmentTokenFile, want: ErrInsideSecretRoot},
		// Spec 06 requirement 85: RURALZ_FETCH_ALLOW.
		{name: "fetch allow host name", env: good.with(EnvFetchAllow, "localhost"), setting: EnvFetchAllow, want: ErrFetchAllow, msg: `entry 1 ("localhost")`},
		{name: "fetch allow bad prefix", env: good.with(EnvFetchAllow, "127.0.0.1/8,10.0.0.0/33"), setting: EnvFetchAllow, want: ErrFetchAllow, msg: "entry 2"},
		{name: "fetch allow zone", env: good.with(EnvFetchAllow, "fe80::1%eth0"), setting: EnvFetchAllow, want: ErrFetchAllow, msg: "zone"},
		{name: "fetch allow zone prefix", env: good.with(EnvFetchAllow, "fe80::1%eth0/64"), setting: EnvFetchAllow, want: ErrFetchAllow, msg: "zone"},
		{name: "fetch allow empty entry", env: good.with(EnvFetchAllow, "127.0.0.1,,::1"), setting: EnvFetchAllow, want: ErrFetchAllow, msg: "empty entry"},
		{name: "fetch allow trailing comma", env: good.with(EnvFetchAllow, "127.0.0.1,"), setting: EnvFetchAllow, want: ErrFetchAllow},
		{name: "fetch allow proxy typo", env: good.with(EnvFetchAllow, "env_proxy"), setting: EnvFetchAllow, want: ErrFetchAllow},
		{name: "fetch allow long entry echo bounded", env: good.with(EnvFetchAllow, strings.Repeat("x", 200)), setting: EnvFetchAllow, want: ErrFetchAllow, msg: `..."`},
		{name: "fetch allow blanks", env: good.with(EnvFetchAllow, "   "), want: nil},
		// R-49, spec 08 requirement 68: the MAC key file.
		{name: "mac key ok", env: good.with(EnvStateStoreMACKeyFile, f.key), want: nil},
		{name: "mac key read-only owner", env: good.with(EnvStateStoreMACKeyFile, readOnly), want: nil},
		{name: "mac key max size", env: good.with(EnvStateStoreMACKeyFile, maxKey), want: nil},
		{name: "mac key via symlink", env: good.with(EnvStateStoreMACKeyFile, linkToGood), want: nil},
		{name: "mac key relative", env: good.with(EnvStateStoreMACKeyFile, "mac.key"), setting: EnvStateStoreMACKeyFile, want: ErrNotAbsolute},
		{name: "mac key missing", env: good.with(EnvStateStoreMACKeyFile, filepath.Join(f.base, "none")), setting: EnvStateStoreMACKeyFile, want: ErrMACKeyFile},
		{name: "mac key directory", env: good.with(EnvStateStoreMACKeyFile, filepath.Dir(f.key)), setting: EnvStateStoreMACKeyFile, want: ErrMACKeyFile, msg: "has mode d"},
		{name: "mac key FIFO does not block", env: good.with(EnvStateStoreMACKeyFile, fifo), setting: EnvStateStoreMACKeyFile, want: ErrMACKeyFile, msg: "has mode p", skip: !hasFIFO},
		{name: "mac key owned by another user", env: good.with(EnvStateStoreMACKeyFile, foreign), setting: EnvStateStoreMACKeyFile, want: ErrMACKeyOwner, msg: "owned by uid 65534", skip: !asRoot},
		{name: "mac key group readable", env: good.with(EnvStateStoreMACKeyFile, groupReadable), setting: EnvStateStoreMACKeyFile, want: ErrMACKeyMode, msg: "chmod 0600", skip: !unix},
		{name: "mac key other readable", env: good.with(EnvStateStoreMACKeyFile, otherReadable), setting: EnvStateStoreMACKeyFile, want: ErrMACKeyMode, skip: !unix},
		{name: "mac key group executable", env: good.with(EnvStateStoreMACKeyFile, groupExec), setting: EnvStateStoreMACKeyFile, want: ErrMACKeyMode, skip: !unix},
		{name: "mac key symlink to group readable", env: good.with(EnvStateStoreMACKeyFile, linkToGroup), setting: EnvStateStoreMACKeyFile, want: ErrMACKeyMode, skip: !unix},
		{name: "mac key 31 bytes", env: good.with(EnvStateStoreMACKeyFile, short), setting: EnvStateStoreMACKeyFile, want: ErrMACKeyShort, msg: "holds 31 bytes"},
		{name: "mac key empty", env: good.with(EnvStateStoreMACKeyFile, empty), setting: EnvStateStoreMACKeyFile, want: ErrMACKeyShort},
		{name: "mac key too large", env: good.with(EnvStateStoreMACKeyFile, long), setting: EnvStateStoreMACKeyFile, want: ErrMACKeyLong},
		{name: "mac key inside root", env: good.with(EnvStateStoreMACKeyFile, insideKey), setting: EnvStateStoreMACKeyFile, want: ErrInsideSecretRoot},
		// Spec 04 requirement 2: M2 settings only warn.
		{name: "M2 settings set", env: good.with(EnvTrustPolicyFile, "p", EnvEnrollmentTokenFile, "/run/enroll", EnvRegistryAuthFile, "a"), want: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.skip {
				t.Skip("needs Unix modes, links or FIFOs, or root to chown")
			}
			s, err := loadWithin(t, tc.args, tc.env.lookup)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("Load: %v", err)
				}
				if ExitCode(err) != ExitOK {
					t.Fatal("ExitCode(nil) != 0")
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("Load err = %v, want %v", err, tc.want)
			}
			if got := ExitCode(err); got != ExitUsage {
				t.Errorf("ExitCode = %d, want 2", got)
			}
			var se *Error
			if !errors.As(err, &se) || se.Setting != tc.setting {
				t.Errorf("error setting = %+v, want %q", se, tc.setting)
			}
			if tc.setting != "" && !strings.HasPrefix(err.Error(), tc.setting+": ") {
				t.Errorf("message %q does not start with the setting name", err)
			}
			if !strings.Contains(err.Error(), tc.msg) {
				t.Errorf("message %q lacks %q", err, tc.msg)
			}
			if strings.Contains(err.Error(), macKeyBytes[:16]) {
				t.Errorf("message quotes the key: %q", err)
			}
			if s.Mode != 0 || s.MACKey.IsSet() {
				t.Errorf("Load returned settings with an error: %+v", s)
			}
		})
	}
}

// loadDeadline bounds a Load in tests, so a blocking open fails the test
// instead of hanging it.
const loadDeadline = 30 * time.Second

// loadWithin runs Load and fails the test when it does not return within
// loadDeadline (a FIFO opened without O_NONBLOCK waits for a writer).
func loadWithin(t *testing.T, args []string, lookup func(string) (string, bool)) (Settings, error) {
	t.Helper()
	type result struct {
		s   Settings
		err error
	}
	done := make(chan result, 1)
	go func() {
		s, err := Load(args, lookup)
		done <- result{s, err}
	}()
	timer := time.NewTimer(loadDeadline)
	defer timer.Stop()
	select {
	case r := <-done:
		return r.s, r.err
	case <-timer.C:
		t.Fatalf("Load still blocked after %v", loadDeadline)
		return Settings{}, nil
	}
}

// TestMACKeyFinalNewline covers the key bytes (R-49, spec 08 requirement
// 68): one final "\n" or "\r\n" is not part of the key, so a key file
// written by a shell tool and the same key without a newline give the same
// HMAC on every Node; nothing else is trimmed, and the 32-byte minimum
// counts the key without that newline.
func TestMACKeyFinalNewline(t *testing.T) {
	f := newFixture(t)
	cases := []struct {
		name    string
		content string
		key     string // "" when Load fails
		want    error
		msg     string
	}{
		{name: "no newline", content: macKeyBytes, key: macKeyBytes},
		{name: "LF", content: macKeyBytes + "\n", key: macKeyBytes},
		{name: "CRLF", content: macKeyBytes + "\r\n", key: macKeyBytes},
		{name: "only one LF removed", content: macKeyBytes + "\n\n", key: macKeyBytes + "\n"},
		{name: "CR alone kept", content: macKeyBytes + "\r", key: macKeyBytes + "\r"},
		{name: "spaces kept", content: macKeyBytes + " \n", key: macKeyBytes + " "},
		{name: "leading newline kept", content: "\n" + macKeyBytes[:31], key: "\n" + macKeyBytes[:31]},
		{name: "hex key from openssl rand -hex 32", content: strings.Repeat("ab", 32) + "\n", key: strings.Repeat("ab", 32)},
		{name: "31 bytes and LF", content: macKeyBytes[:31] + "\n", want: ErrMACKeyShort, msg: "holds 31 bytes besides its final newline"},
		{name: "30 bytes and two LF", content: macKeyBytes[:30] + "\n\n", want: ErrMACKeyShort, msg: "holds 31 bytes"},
		{name: "only a newline", content: "\n", want: ErrMACKeyShort, msg: "holds 0 bytes"},
		{name: "max size counts the newline", content: strings.Repeat("k", MaxMACKeyBytes) + "\n", want: ErrMACKeyLong},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(f.base, "keys", fmt.Sprintf("nl%d.key", i))
			writeFile(t, path, tc.content, 0o600)
			s, err := Load(nil, f.env().with(EnvStateStoreMACKeyFile, path).lookup)
			if tc.want != nil {
				if !errors.Is(err, tc.want) || !strings.Contains(fmt.Sprint(err), tc.msg) {
					t.Fatalf("Load err = %v, want %v with %q", err, tc.want, tc.msg)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := string(s.MACKey.Bytes()); got != tc.key {
				t.Fatalf("key = %q, want %q", got, tc.key)
			}
		})
	}
}

func mustAbs(t *testing.T, p string) string {
	t.Helper()
	a, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// TestLoadReportsEverySetting: all invalid settings are reported at once,
// each as its own *Error naming its setting.
func TestLoadReportsEverySetting(t *testing.T) {
	f := newFixture(t)
	_, err := Load(nil, env{
		EnvLogLevel:             "loud",
		EnvSecretRoot:           f.root,
		EnvDataDir:              filepath.Join(f.root, "d"),
		EnvFetchAllow:           "nope",
		EnvStateStoreMACKeyFile: "rel",
		EnvAdminTLSDir:          "tls",
	}.lookup)
	var got []string
	for _, e := range err.(interface{ Unwrap() []error }).Unwrap() {
		var se *Error
		if !errors.As(e, &se) {
			t.Fatalf("%v is not an *Error", e)
		}
		got = append(got, se.Setting)
	}
	want := []string{EnvConfig, EnvLogLevel, EnvFetchAllow, EnvAdminTLSDir, EnvStateStoreMACKeyFile, EnvDataDir}
	if !slices.Equal(got, want) {
		t.Fatalf("settings reported %v, want %v", got, want)
	}
}

// TestUnusedM2Settings covers spec 04 requirement 2: a set M2 setting is
// reported for a warn log, by name only.
func TestUnusedM2Settings(t *testing.T) {
	s, err := Load(nil, env{
		EnvConfig:              "/srv/b",
		EnvTrustPolicyFile:     "/etc/trust.json",
		EnvEnrollmentTokenFile: "enroll.token",
		EnvRegistryAuthFile:    "",
	}.lookup)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{EnvTrustPolicyFile, EnvEnrollmentTokenFile}; !slices.Equal(s.Unused, want) {
		t.Fatalf("Unused = %v, want %v", s.Unused, want)
	}
	if s.EnrollmentTokenFile != mustAbs(t, "enroll.token") {
		t.Errorf("EnrollmentTokenFile = %q", s.EnrollmentTokenFile)
	}
}

// TestProtected covers the protected path list handed to the secret
// resolver (spec 01 requirement 45).
func TestProtected(t *testing.T) {
	s := Settings{DataDir: "/d", AdminTLSDir: "/t", AdminTokenFile: "/a", AdminMetricsTokenFile: "/m", MACKeyFile: "/k", EnrollmentTokenFile: "/e"}
	got := s.Protected()
	want := []ProtectedPath{
		{EnvDataDir, "/d"},
		{EnvAdminTLSDir, "/t"},
		{EnvAdminTokenFile, "/a"},
		{EnvAdminMetricsTokenFile, "/m"},
		{EnvStateStoreMACKeyFile, "/k"},
		{EnvEnrollmentTokenFile, "/e"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Protected = %v", got)
	}
	if got := (&Settings{DataDir: "/d"}).Protected(); !slices.Equal(got, want[:1]) {
		t.Fatalf("Protected = %v", got)
	}
}

// TestMACKeyRedacted: the key never appears in fmt, JSON or slog output
// of the key or of Settings (R-9 marker "[REDACTED]").
func TestMACKeyRedacted(t *testing.T) {
	f := newFixture(t)
	s, err := Load(nil, f.env().with(EnvStateStoreMACKeyFile, f.key).lookup)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d"} {
		fmt.Fprintf(&out, verb+"\n", s.MACKey)
		fmt.Fprintf(&out, verb+"\n", s)
		fmt.Fprintf(&out, verb+"\n", &s)
	}
	fmt.Fprintln(&out, s.MACKey.String(), s.MACKey.GoString())
	j, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	out.Write(j)
	text, err := s.MACKey.MarshalText()
	if err != nil {
		t.Fatal(err)
	}
	out.Write(text)
	logger := slog.New(slog.NewJSONHandler(&out, nil))
	logger.Info("settings", slog.Any("key", s.MACKey), slog.Any("settings", s))
	for _, form := range []string{macKeyBytes, fmt.Sprintf("%x", macKeyBytes), "CANARY"} {
		if strings.Contains(out.String(), form) {
			t.Fatalf("key leaked as %q in:\n%s", form, out.String())
		}
	}
	if !strings.Contains(out.String(), `"MACKey":"[REDACTED]"`) {
		t.Errorf("JSON lacks the redaction marker: %s", j)
	}
	// Bytes returns a copy.
	b := s.MACKey.Bytes()
	b[0] = 'X'
	if s.MACKey.Bytes()[0] != 'C' {
		t.Error("Bytes exposes the internal buffer")
	}
	if (MACKey{}).Bytes() != nil || (MACKey{}).IsSet() {
		t.Error("zero MACKey is set")
	}
}

// TestExitCode covers spec 04 requirement 4's codes.
func TestExitCode(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{
		{nil, 0},
		{errors.New("runtime"), 1},
		{&Error{Setting: EnvConfig, Err: ErrRequired}, 2},
		{fmt.Errorf("wrapped: %w", &Error{Err: ErrUsage}), 2},
		{errors.Join(errors.New("x"), &Error{Err: ErrUsage}), 2},
	} {
		if got := ExitCode(tc.err); got != tc.want {
			t.Errorf("ExitCode(%v) = %d, want %d", tc.err, got, tc.want)
		}
	}
	if (&Error{Err: ErrUsage}).Error() != ErrUsage.Error() {
		t.Error("usage error text")
	}
	if !strings.HasPrefix(Usage, "usage: ruralzd\n") || !strings.Contains(Usage, EnvStateStoreMACKeyFile) {
		t.Error("Usage text")
	}
}

// TestModeAndLevelStrings covers the enum helpers.
func TestModeAndLevelStrings(t *testing.T) {
	for m, want := range map[Mode]string{ModeFile: "file", ModeOCI: "oci", ModeControl: "control", 0: "Mode(0)"} {
		if got := m.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", m, got, want)
		}
	}
	for l, want := range map[LogLevel]slog.Level{LogDebug: slog.LevelDebug, LogInfo: slog.LevelInfo, LogWarn: slog.LevelWarn, LogError: slog.LevelError, "": slog.LevelInfo} {
		if got := l.Slog(); got != want {
			t.Errorf("%q.Slog() = %v, want %v", l, got, want)
		}
	}
}

// TestLoadOSEnvironment covers the nil lookup (os.LookupEnv).
func TestLoadOSEnvironment(t *testing.T) {
	for _, name := range []string{
		EnvConfig, EnvDataDir, EnvLogLevel, EnvSecretRoot, EnvFetchAllow,
		EnvAdminTokenFile, EnvAdminMetricsTokenFile, EnvAdminTLSDir, EnvStateStoreMACKeyFile,
		EnvTrustPolicyFile, EnvEnrollmentTokenFile, EnvRegistryAuthFile,
	} {
		t.Setenv(name, "")
	}
	t.Setenv(EnvConfig, "/srv/os-bundle")
	s, err := Load(nil, nil)
	if err != nil || s.Config != "/srv/os-bundle" {
		t.Fatalf("Load = %+v, %v", s, err)
	}
}

// FuzzLoadFetchAllow keeps Load's RURALZ_FETCH_ALLOW check aligned with
// egress.ParseAllow, which parses the value later (spec 06 requirement
// 85): both accept exactly the same values, so a value Load passes never
// fails the guard and Load refuses every value the guard would. Every
// refusal is an *Error naming the setting and wrapping ErrFetchAllow.
func FuzzLoadFetchAllow(f *testing.F) {
	for _, s := range []string{
		"", " ", " , ", ",", "127.0.0.1", "127.0.0.1/32,::1", " 127.0.0.1/32, ::1 ,env-proxy,10.0.0.0/8",
		"env-proxy", " env-proxy ", "env_proxy", "ENV-PROXY", "env-proxy,env-proxy",
		"localhost", "10.0.0.0/33", "1.2.3.4/0", "1.2.3.4/32/1", "::1/129", "0.0.0.0/0", "::/0",
		"fe80::1%eth0", "fe80::1%eth0/64", "1.2.3.4%x", "%", "fe80::1%", "[::1]", "::ffff:10.0.0.1",
		"::ffff:10.0.0.0/104", "::ffff:1.2.3.4/96", "127.1", "0x7f.0.0.1", "01.2.3.4", "1.2.3.4.5",
		"127.0.0.1,,::1", "127.0.0.1,", "\t127.0.0.1\n", "\u00a0127.0.0.1", "\u2028::1", "1.2.3.4/+8",
		"1.2.3.4/08", " / ", "/", "::ffff:1.2.3.4%eth0",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, v string) {
		l := loader{}
		l.fetchAllow(v)
		_, perr := egress.ParseAllow(v)
		if (len(l.errs) == 0) != (perr == nil) {
			t.Fatalf("RURALZ_FETCH_ALLOW=%q: Load errors %v, egress.ParseAllow error %v", v, l.errs, perr)
		}
		for _, err := range l.errs {
			var se *Error
			if !errors.As(err, &se) || se.Setting != EnvFetchAllow || !errors.Is(err, ErrFetchAllow) {
				t.Fatalf("error %v does not name %s with ErrFetchAllow", err, EnvFetchAllow)
			}
		}
	})
}
