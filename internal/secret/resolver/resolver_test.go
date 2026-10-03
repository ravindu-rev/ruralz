// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package resolver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// Tests for NewResolver, Activate, Current and Run: spec 01 requirements
// 45 and 46, spec 04 requirement 3, spec 06 requirement 88, architecture
// 2.8 and R-9.

func TestCodeIsRegistered(t *testing.T) {
	c, ok := errcode.Lookup(CodeUnresolvable)
	if !ok || c.Status != 0 {
		t.Fatalf("%s: registered %v, status %d; want a configuration code without status", CodeUnresolvable, ok, c.Status)
	}
}

func TestNewResolverDefaults(t *testing.T) {
	r, err := NewResolver(Config{})
	if err != nil {
		t.Fatal(err)
	}
	// Req 44: RURALZ_SECRET_ROOT defaults to /etc/ruralz; R-9: 2 s poll.
	if r.Root() != DefaultRoot {
		t.Errorf("Root = %q, want %q", r.Root(), DefaultRoot)
	}
	if r.interval != 2*time.Second || r.retryInitial != DefaultRetryInitial || r.retryMax != DefaultRetryMax {
		t.Errorf("interval %v, retry %v..%v", r.interval, r.retryInitial, r.retryMax)
	}
	if len(r.owners) != 0 {
		t.Errorf("owners = %v, want any owner", r.owners)
	}
	if _, ok := r.Current().Get(envRef("RURALZ_SECRET_X")); ok {
		t.Error("the Store before Activate holds a reference")
	}
	// The counters exist for env and file even without RotationFailures.
	if r.failureCounter(providerEnv) == nil || r.failureCounter(providerFile) == nil || r.failureCounter("vault") == nil {
		t.Error("nil failure counter")
	}
	r.failureCounter("vault").Add(0, 1) // nop
}

func TestNewResolverRetryMaxAtLeastInitial(t *testing.T) {
	r, err := NewResolver(Config{Root: t.TempDir(), RetryInitial: time.Minute, RetryMax: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if r.retryMax != time.Minute {
		t.Errorf("retryMax = %v, want %v", r.retryMax, time.Minute)
	}
}

func TestNewResolverPrecreatesRotationSeries(t *testing.T) {
	// Spec 01 requirement 56: env and file series exist from the start.
	var asked []string
	_, err := NewResolver(Config{Root: t.TempDir(), RotationFailures: func(p string) emit.Counter {
		asked = append(asked, p)
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(asked, ",") != "env,file" {
		t.Errorf("RotationFailures called for %v, want env and file", asked)
	}
}

func TestNewResolverRefusesRelativeRoot(t *testing.T) {
	// Req 45: a relative RURALZ_SECRET_ROOT refuses start, naming the setting.
	_, err := NewResolver(Config{Root: "etc/ruralz"})
	if !errors.Is(err, ErrRelativeRoot) || !strings.Contains(err.Error(), SettingSecretRoot) {
		t.Fatalf("err = %v, want ErrRelativeRoot naming %s", err, SettingSecretRoot)
	}
	if _, ok := errcode.CodeOf(err); ok {
		t.Error("a startup refusal carries no RZ code")
	}
}

func TestNewResolverProtectedPaths(t *testing.T) {
	// Spec 01 req 45, spec 04 req 3, spec 06 req 88 (rule 5): the root must
	// not be or contain the data dir, the admin TLS dir, the admin token
	// files, the MAC key file or the Enrollment token file.
	base := t.TempDir()
	realDir := filepath.Join(base, "realDir")
	if err := os.MkdirAll(filepath.Join(realDir, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatal(err)
	}
	// A path outside the root that points into it through a symbolic link.
	intoRoot := filepath.Join(base, "into")
	if err := os.Symlink(filepath.Join(realDir, "data"), intoRoot); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		root    string
		prot    []Protected
		refused []string // settings named in the error
	}{
		{"unset paths", realDir, []Protected{{SettingDataDir, ""}, {SettingAdminTLSDir, ""}}, nil},
		{"outside", realDir, []Protected{{SettingDataDir, filepath.Join(base, "var")}}, nil},
		{"prefix sibling", realDir, []Protected{{SettingDataDir, realDir + "2"}}, nil},
		{"parent of root", realDir, []Protected{{SettingDataDir, base}}, nil},
		{"data dir inside", realDir, []Protected{{SettingDataDir, filepath.Join(realDir, "data")}}, []string{SettingDataDir}},
		{"root itself", realDir, []Protected{{SettingAdminTLSDir, realDir}}, []string{SettingAdminTLSDir}},
		{"unclean path", realDir, []Protected{{SettingAdminTokenFile, realDir + "/x/../token"}}, []string{SettingAdminTokenFile}},
		{
			"several named", realDir,
			[]Protected{
				{SettingAdminTokenFile, filepath.Join(realDir, "admin.token")},
				{SettingAdminMetricsTokenFile, filepath.Join(realDir, "metrics.token")},
				{SettingMACKeyFile, filepath.Join(realDir, "mac.key")},
				{SettingEnrollmentTokenFile, filepath.Join(realDir, "enroll.token")},
				{SettingDataDir, filepath.Join(base, "var")},
			},
			[]string{SettingAdminTokenFile, SettingAdminMetricsTokenFile, SettingMACKeyFile, SettingEnrollmentTokenFile},
		},
		{"root through a symlink", link, []Protected{{SettingDataDir, filepath.Join(realDir, "data")}}, []string{SettingDataDir}},
		{"protected through a symlink", realDir, []Protected{{SettingDataDir, intoRoot}}, []string{SettingDataDir}},
		{"missing path under the resolved root", link, []Protected{{SettingDataDir, filepath.Join(realDir, "new", "dir")}}, []string{SettingDataDir}},
		{"missing path under a symlinked parent", realDir, []Protected{{SettingDataDir, filepath.Join(link, "new", "dir")}}, []string{SettingDataDir}},
		{"missing root", filepath.Join(base, "none"), []Protected{{SettingDataDir, filepath.Join(base, "none", "d")}}, []string{SettingDataDir}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewResolver(Config{Root: tc.root, Protected: tc.prot})
			if len(tc.refused) == 0 {
				if err != nil {
					t.Fatalf("NewResolver: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrProtectedInRoot) {
				t.Fatalf("err = %v, want ErrProtectedInRoot", err)
			}
			for _, s := range tc.refused {
				if !strings.Contains(err.Error(), s+"=") {
					t.Errorf("error %q does not name %s", err, s)
				}
			}
			if strings.Contains(err.Error(), SettingDataDir+"="+filepath.Join(base, "var")) {
				t.Errorf("error %q names an allowed setting", err)
			}
		})
	}
}

func TestProtectedRelativePath(t *testing.T) {
	// A relative protected path is compared from the working directory.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewResolver(Config{Root: wd, Protected: []Protected{{SettingDataDir, "data"}}}); !errors.Is(err, ErrProtectedInRoot) {
		t.Fatalf("err = %v, want ErrProtectedInRoot", err)
	}
}

func TestActivateForeignStorePanics(t *testing.T) {
	h := newHarness(t, nil)
	other := newHarness(t, nil)
	st := other.resolve()
	for name, s := range map[string]secret.Store{"other resolver": st, "foreign type": foreignStore{}} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("Activate did not panic")
				}
			}()
			h.r.Activate(s)
		})
	}
}

type foreignStore struct{}

func (foreignStore) Get(secret.Ref) (secret.Value, bool)               { return secret.Value{}, false }
func (foreignStore) Watch(secret.Ref, func(secret.Value) error) func() { return func() {} }

func TestActivateNilIsEmpty(t *testing.T) {
	h := newHarness(t, nil)
	h.setEnv("RURALZ_SECRET_A", "a")
	st := h.resolve(use(envRef("RURALZ_SECRET_A"), secret.KindOpaque))
	h.r.Activate(st)
	if h.r.Current() != st {
		t.Fatal("Current is not the activated Store")
	}
	h.r.Activate(nil)
	if _, ok := h.r.Current().Get(envRef("RURALZ_SECRET_A")); ok {
		t.Error("Activate(nil) kept the references")
	}
}

func TestRunSecondRunRefused(t *testing.T) {
	h := newHarness(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cycles := make(chan struct{}, 16)
	h.r.cycleHook = func() { cycles <- struct{}{} }
	done := make(chan error, 1)
	go func() { done <- h.r.Run(ctx) }()
	// Activate kicks an immediate cycle, which proves Run is running.
	h.r.Activate(nil)
	waitCycle(t, cycles)
	if err := h.r.Run(ctx); !errors.Is(err, ErrRunning) {
		t.Errorf("second Run = %v, want ErrRunning", err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run = %v, want nil after cancel", err)
	}
	// Run may start again once the first returned.
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	if err := h.r.Run(ctx2); err != nil {
		t.Errorf("Run after return = %v", err)
	}
}

// waitCycle waits for one poll cycle with a realDir-time bound.
func waitCycle(t *testing.T, cycles <-chan struct{}) {
	t.Helper()
	select {
	case <-cycles:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for a poll cycle")
	}
}
