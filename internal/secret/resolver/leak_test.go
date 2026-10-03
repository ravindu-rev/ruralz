// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package resolver

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/secret"
)

// Secret leak test (WP-07 "Done when"; spec 01 requirement 46 and section
// 6 "Secret leak tests"; spec 06 requirements 87, 89 and 91, section 6.6):
// a canary injected through env and file never appears, raw or in its
// base64, hex or URL-encoded forms, in diagnostics (text and JSON), errors,
// log records (JSON and text handlers) or the formatted Store, across
// resolution failures, consumer checks that try to repeat it, rotation
// failures and watchers that echo it.

const canary = "rz-canary-7d41c0e9a2b85f36:p@ss/w0rd+=&x"

// canaryForms returns the forms of the canary the test looks for.
func canaryForms() map[string]string {
	b := []byte(canary)
	return map[string]string{
		"raw":          canary,
		"base64":       base64.StdEncoding.EncodeToString(b),
		"base64url":    base64.URLEncoding.EncodeToString(b),
		"base64rawurl": base64.RawURLEncoding.EncodeToString(b),
		"hex":          hex.EncodeToString(b),
		"HEX":          strings.ToUpper(hex.EncodeToString(b)),
		"query":        url.QueryEscape(canary),
		"path":         url.PathEscape(canary),
		"distinctive":  "7d41c0e9a2b85f36",
	}
}

// assertNoCanary fails when out contains any form of the canary.
func assertNoCanary(t *testing.T, what, out string) {
	t.Helper()
	for form, s := range canaryForms() {
		if strings.Contains(out, s) {
			t.Errorf("%s contains the canary (%s form):\n%s", what, form, out)
		}
	}
}

// teeHandler sends every record to several handlers.
type teeHandler []slog.Handler

func (h teeHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h teeHandler) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	for _, x := range h {
		errs = append(errs, x.Handle(ctx, r.Clone()))
	}
	return errors.Join(errs...)
}

func (h teeHandler) WithAttrs(as []slog.Attr) slog.Handler {
	out := make(teeHandler, len(h))
	for i, x := range h {
		out[i] = x.WithAttrs(as)
	}
	return out
}

func (h teeHandler) WithGroup(name string) slog.Handler {
	out := make(teeHandler, len(h))
	for i, x := range h {
		out[i] = x.WithGroup(name)
	}
	return out
}

func TestSecretCanaryNeverLeaks(t *testing.T) {
	logs := &syncBuffer{}
	h := newHarness(t, func(c *Config) {
		opts := &slog.HandlerOptions{Level: slog.LevelDebug}
		c.Logger = slog.New(teeHandler{slog.NewJSONHandler(logs, opts), slog.NewTextHandler(logs, opts)})
	})
	h.logs = logs
	h.setEnv("RURALZ_SECRET_CANARY", canary)
	h.setEnv(EnvStateStoreURL, "redis://"+url.UserPassword("u", canary).String()+"@cache.internal:6379")
	h.write("canary", canary)
	h.write("canary.json", `{"password":"`+canary+`","port":6379}`)
	h.write("broken.json", `{"password": `+canary+`}`)
	h.write("canary.pem", "-----BEGIN CERTIFICATE-----\n"+base64.StdEncoding.EncodeToString([]byte(canary))+"\n-----END CERTIFICATE-----\n")

	echo := func(format string) func([]byte) error {
		return func(b []byte) error { return fmt.Errorf(format, b) }
	}
	withCheck := func(u secret.Use, fn func([]byte) error, field string) secret.Use {
		u.Check = fn
		u.Path = diag.Path{diag.Field(field)}
		return u
	}
	env := envRef("RURALZ_SECRET_CANARY")
	file := fileRef(h.path("canary"), "")
	var all diag.List
	collect := func(uses ...secret.Use) {
		st, diags := h.r.Resolve(context.Background(), uses)
		if st != nil {
			t.Fatal("Resolve succeeded; want failures")
		}
		all = append(all, diags...)
	}
	collect(
		use(env, secret.KindPEMCertificate),
		use(env, secret.KindPEMPrivateKey),
		use(env, secret.KindPEMCertPool),
		use(env, secret.KindPEMCRL),
		use(env, secret.KindStateStoreURL),
		withCheck(use(env, secret.KindOpaque), echo("raw %s"), "raw"),
		withCheck(use(env, secret.KindOpaque), echo("hex %x"), "hex"),
		withCheck(use(env, secret.KindOpaque), echo("HEX %X"), "HEX"),
		withCheck(use(env, secret.KindOpaque), func(b []byte) error {
			return errors.New("b64 " + base64.StdEncoding.EncodeToString(b))
		}, "b64"),
		withCheck(use(env, secret.KindOpaque), func(b []byte) error {
			return errors.New("query " + url.QueryEscape(string(b)))
		}, "query"),
		withCheck(use(env, secret.KindOpaque), func(b []byte) error { panic(fmt.Sprintf("%s %x", b, b)) }, "panic"),
		withCheck(use(envRef(EnvStateStoreURL), secret.KindStateStoreURL), func(b []byte) error {
			u, err := url.Parse(string(b))
			if err != nil {
				return err
			}
			return errors.New("does not fit topology: " + u.String())
		}, "url"),
		use(file, secret.KindPEMCertificate),
		withCheck(use(file, secret.KindOpaque), echo("%q"), "quoted"),
		use(fileRef(h.path("canary.json"), "port"), secret.KindOpaque),
		use(fileRef(h.path("canary.json"), "absent"), secret.KindOpaque),
		use(fileRef(h.path("broken.json"), "password"), secret.KindOpaque),
		use(fileRef(h.path("canary"), "password"), secret.KindOpaque),
		use(fileRef(h.path("canary.pem"), ""), secret.KindPEMCertificate),
		use(fileRef(h.path("canary.pem"), ""), secret.KindPEMCertPool),
	)
	if len(all) < 20 {
		t.Fatalf("got %d diagnostics, want one per failing use:\n%s", len(all), diagText(all))
	}

	// Rotation failures and watchers that echo the value.
	h.write("rot", strings.Repeat("r", 30))
	rot := fileRef(h.path("rot"), "")
	rotUse := use(rot, secret.KindAPIKey)
	rotUse.Check = func(b []byte) error {
		if bytes.Contains(b, []byte(canary)) {
			return fmt.Errorf("rejected %s (%x)", b, b)
		}
		return nil
	}
	st := h.resolve(rotUse, use(env, secret.KindOpaque), use(fileRef(h.path("canary.json"), "password"), secret.KindOpaque))
	h.r.Activate(st)
	// Registered on a Store without the env reference, these watchers have
	// seen no version, so the next cycle delivers the canary to them.
	other := h.resolve()
	for _, format := range []string{"echo %s", "hex %x", "go %#v", "quoted %q"} {
		defer other.Watch(env, func(v secret.Value) error {
			return fmt.Errorf(format, v.Reveal())
		})()
	}
	defer other.Watch(env, func(v secret.Value) error { panic(string(v.Reveal())) })()
	h.write("rot", canary)
	h.cycle()
	if err := os.WriteFile(filepath.Join(h.root, "canary.json"), []byte(`{"password":`+canary), 0o600); err != nil {
		t.Fatal(err)
	}
	h.cycle()
	if h.fileN.value() != 2 || h.envN.value() != 5 {
		t.Errorf("failures file %d env %d, want 2 and 5", h.fileN.value(), h.envN.value())
	}

	var text, js bytes.Buffer
	if err := diag.WriteText(&text, all); err != nil {
		t.Fatal(err)
	}
	if err := diag.WriteJSON(&js, all); err != nil {
		t.Fatal(err)
	}
	assertNoCanary(t, "diagnostics text", text.String())
	assertNoCanary(t, "diagnostics JSON", js.String())
	assertNoCanary(t, "logs", logs.String())
	if !strings.Contains(logs.String(), msgRotationFailed) || !strings.Contains(logs.String(), msgWatcherFailed) {
		t.Errorf("expected failure logs:\n%s", logs.String())
	}
	assertNoCanary(t, "formatted Store", fmt.Sprintf("%v %+v %#v", st, st, st))
	v, _ := st.Get(env)
	assertNoCanary(t, "formatted Value", fmt.Sprintf("%v %+v %#v %s %q %x", v, v, v, v, v, v))
	if got := string(v.Reveal()); got != canary {
		t.Errorf("Reveal = %q", got)
	}
	// Startup errors carry settings and paths only.
	_, err := NewResolver(Config{Root: h.root, Protected: []Protected{{SettingDataDir, filepath.Join(h.root, "data")}}})
	assertNoCanary(t, "NewResolver error", err.Error())
}
