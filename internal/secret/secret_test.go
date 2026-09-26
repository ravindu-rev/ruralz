// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package secret

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"text/template"

	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Tests for architecture section 2.8 (WP-01) and spec 06 section 2.14
// (requirements 87 to 92, the redacting value): no formatting, logging or
// encoding path prints the bytes; Reveal returns a copy; an empty resolved
// value is set and distinct from unset (R-55).

const plaintext = "hunter2-s3cr3t-value"

func TestFormattingRedacts(t *testing.T) {
	v := NewValue([]byte(plaintext))
	type holder struct {
		Name string
		V    Value
		P    *Value
	}
	h := holder{Name: "n", V: v, P: &v}
	outputs := map[string]string{
		"%v":           fmt.Sprintf("%v", v),
		"%+v":          fmt.Sprintf("%+v", v),
		"%#v":          fmt.Sprintf("%#v", v),
		"%s":           fmt.Sprintf("%s", v),
		"%q":           fmt.Sprintf("%q", v),
		"%x":           fmt.Sprintf("%x", v),
		"%X":           fmt.Sprintf("%X", v),
		"%d":           fmt.Sprintf("%d", v),
		"%20v":         fmt.Sprintf("%20v", v),
		"pointer %v":   fmt.Sprintf("%v", &v),
		"Sprint":       fmt.Sprint(v),
		"Sprintln":     strings.TrimSuffix(fmt.Sprintln(v), "\n"),
		"String":       v.String(),
		"GoString":     v.GoString(),
		"Errorf":       strings.TrimPrefix(fmt.Errorf("resolve: %v", v).Error(), "resolve: "),
		"struct %v":    fmt.Sprintf("%v", h),
		"struct %+v":   fmt.Sprintf("%+v", h),
		"struct %#v":   fmt.Sprintf("%#v", h),
		"slice":        fmt.Sprintf("%v", []Value{v}),
		"map":          fmt.Sprintf("%v", map[string]Value{"k": v}),
		"struct %s":    fmt.Sprintf("%s", h),
		"struct %x":    fmt.Sprintf("%x", h),
		"slice %q":     fmt.Sprintf("%q", []Value{v, v}),
		"pointer %+v":  fmt.Sprintf("%+v", &h),
		"interface %v": fmt.Sprintf("%v", any(v)),
	}
	for name, out := range outputs {
		if strings.Contains(out, plaintext) || strings.Contains(out, fmt.Sprintf("%x", []byte(plaintext))) {
			t.Errorf("%s printed the secret: %q", name, out)
		}
		if !strings.Contains(out, Redacted) {
			t.Errorf("%s = %q, want %s", name, out, Redacted)
		}
	}
	if got := fmt.Sprintf("%v", v); got != Redacted {
		t.Errorf("%%v = %q, want exactly %s", got, Redacted)
	}
}

func TestEncodersRedact(t *testing.T) {
	v := NewValue([]byte(plaintext))
	type doc struct {
		V Value  `json:"v" xml:"v"`
		P *Value `json:"p" xml:"p,attr"`
	}
	d := doc{V: v, P: &v}

	var outputs []string
	b, err := json.Marshal(v)
	if err != nil || string(b) != `"[REDACTED]"` {
		t.Fatalf("json.Marshal = %s, %v", b, err)
	}
	b, err = json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	outputs = append(outputs, string(b))
	b, err = json.MarshalIndent(map[string]any{"v": v}, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	outputs = append(outputs, string(b))
	b, err = xml.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	outputs = append(outputs, string(b))
	text, err := v.MarshalText()
	if err != nil || string(text) != Redacted {
		t.Fatalf("MarshalText = %s, %v", text, err)
	}

	var logs bytes.Buffer
	for _, h := range []slog.Handler{slog.NewJSONHandler(&logs, nil), slog.NewTextHandler(&logs, nil)} {
		log := slog.New(h)
		log.Info("secret resolved", slog.Any("secret_value", v), slog.Any("secret_pointer", &v), slog.Any("holder", d))
		log.Info("secret group", slog.Group("g", slog.Any("value", v)))
	}
	outputs = append(outputs, logs.String())
	if got := v.LogValue(); got.Kind() != slog.KindString || got.String() != Redacted {
		t.Fatalf("LogValue = %v", got)
	}

	var tpl bytes.Buffer
	if err := template.Must(template.New("t").Parse("{{.V}} {{.P}} {{printf `%q` .V}}")).Execute(&tpl, d); err != nil {
		t.Fatal(err)
	}
	outputs = append(outputs, tpl.String())

	for _, out := range outputs {
		if strings.Contains(out, plaintext) {
			t.Errorf("encoder printed the secret: %s", out)
		}
		if !strings.Contains(out, Redacted) {
			t.Errorf("encoder output lacks %s: %s", Redacted, out)
		}
	}
}

func TestRevealReturnsACopy(t *testing.T) {
	in := []byte(plaintext)
	v := NewValue(in)
	in[0] = 'X' // NewValue copied its input
	got := v.Reveal()
	if string(got) != plaintext {
		t.Fatalf("Reveal = %q after the input changed", got)
	}
	got[0] = 'Y'
	if string(v.Reveal()) != plaintext {
		t.Fatal("mutating Reveal's result changed the Value")
	}
	if v.Len() != len(plaintext) || v.IsZero() {
		t.Fatalf("Len %d, IsZero %v", v.Len(), v.IsZero())
	}
}

func TestEmptyAndUnsetValues(t *testing.T) {
	// R-55 / architecture 2.8: a resolved empty secret is set, not zero.
	empty := NewValue([]byte{})
	if empty.IsZero() {
		t.Fatal("NewValue([]byte{}) is zero, want a set empty value")
	}
	if empty.Len() != 0 {
		t.Fatalf("Len = %d", empty.Len())
	}
	if r := empty.Reveal(); r == nil || len(r) != 0 {
		t.Fatalf("Reveal of an empty value = %#v, want a non-nil empty slice", r)
	}
	if empty.String() != Redacted {
		t.Fatal("an empty value is not redacted")
	}

	for name, v := range map[string]Value{"NewValue(nil)": NewValue(nil), "zero Value": {}} {
		if !v.IsZero() || v.Len() != 0 || v.Reveal() != nil {
			t.Errorf("%s: IsZero %v, Len %d, Reveal %v", name, v.IsZero(), v.Len(), v.Reveal())
		}
		if fmt.Sprint(v) != Redacted {
			t.Errorf("%s prints %q", name, fmt.Sprint(v))
		}
	}
}

func TestRef(t *testing.T) {
	tests := []struct {
		in   v1alpha1.SecretRef
		want string
	}{
		{v1alpha1.SecretRef{Provider: v1alpha1.SecretProviderEnv, Name: "API_TOKEN"}, "env:API_TOKEN"},
		{v1alpha1.SecretRef{Provider: v1alpha1.SecretProviderFile, Name: "/run/secrets/idp.json", Key: "clientSecret"}, "file:/run/secrets/idp.json#clientSecret"},
	}
	for _, tt := range tests {
		r := RefOf(tt.in)
		if r.Provider != tt.in.Provider || r.Name != tt.in.Name || r.Key != tt.in.Key {
			t.Errorf("RefOf(%+v) = %+v", tt.in, r)
		}
		if r.String() != tt.want {
			t.Errorf("String = %q, want %q", r.String(), tt.want)
		}
	}
	// Ref is comparable: the resolver keys its Node-wide table and watches by it.
	a := RefOf(tests[1].in)
	b := RefOf(tests[1].in)
	if a != b || map[Ref]int{a: 1}[b] != 1 {
		t.Fatal("equal references do not compare equal")
	}
}

func TestKinds(t *testing.T) {
	kinds := []Kind{KindOpaque, KindPEMCertificate, KindPEMPrivateKey, KindPEMCertPool, KindPEMCRL, KindAPIKey, KindStateStoreURL}
	seen := map[Kind]bool{}
	for _, k := range kinds {
		if seen[k] {
			t.Fatalf("Kind %d declared twice", k)
		}
		seen[k] = true
	}
	if KindOpaque != 0 {
		t.Fatal("the zero Kind is not opaque")
	}
}

// fakeStore shows the Store contract a carried-over Filter relies on
// (R-55): a Watch registration is keyed by Ref and its stop unregisters.
type fakeStore struct {
	vals    map[Ref]Value
	watches map[Ref][]func(Value) error
}

func (s *fakeStore) Get(r Ref) (Value, bool) { v, ok := s.vals[r]; return v, ok }

func (s *fakeStore) Watch(r Ref, fn func(Value) error) func() {
	s.watches[r] = append(s.watches[r], fn)
	i := len(s.watches[r]) - 1
	return func() { s.watches[r][i] = nil }
}

func TestStoreContract(t *testing.T) {
	ref := Ref{Provider: v1alpha1.SecretProviderFile, Name: "/run/secrets/ca.pem"}
	s := &fakeStore{vals: map[Ref]Value{ref: NewValue([]byte("old"))}, watches: map[Ref][]func(Value) error{}}
	var store Store = s
	var got []string
	stop := store.Watch(ref, func(v Value) error {
		if v.Len() == 0 {
			return errors.New("empty CA bundle")
		}
		got = append(got, string(v.Reveal()))
		return nil
	})
	rotate := func(v Value) {
		s.vals[ref] = v
		for _, fn := range s.watches[ref] {
			if fn != nil {
				_ = fn(v)
			}
		}
	}
	rotate(NewValue([]byte("new")))
	rotate(NewValue([]byte{}))
	stop()
	rotate(NewValue([]byte("after stop")))
	if len(got) != 1 || got[0] != "new" {
		t.Fatalf("watch saw %v", got)
	}
	if v, ok := store.Get(ref); !ok || string(v.Reveal()) != "after stop" {
		t.Fatal("Get does not follow rotations")
	}
	if _, ok := store.Get(Ref{Provider: v1alpha1.SecretProviderEnv, Name: "OTHER"}); ok {
		t.Fatal("Get of a reference outside the Store succeeded")
	}
}
