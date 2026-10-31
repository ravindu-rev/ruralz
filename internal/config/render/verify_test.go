// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package render

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
)

// fakeBuild returns a BuildFunc answering from per-Environment results and
// recording the Environments it was asked for.
func fakeBuild(results map[string]Outcome, asked *[]string) BuildFunc {
	return func(_ context.Context, env string) (revision.Digest, diag.List, error) {
		*asked = append(*asked, env)
		o := results[env]
		return o.Digest, o.Diagnostics, nil
	}
}

// TestVerifyConversion checks the comparison of render --api-version:
// one Comparison per Environment in the given order, Equal exactly when
// both trees built one Revision, and AllEqual only when every one is.
func TestVerifyConversion(t *testing.T) {
	a, b := revision.Sum([]byte("a")), revision.Sum([]byte("b"))
	failed := diag.List{{Code: "RZ-CFG-005", Severity: diag.SeverityError}}
	warned := diag.List{{Code: "RZ-CFG-013", Severity: diag.SeverityWarning}}
	var srcAsked, dstAsked []string
	src := fakeBuild(map[string]Outcome{
		"same": {Digest: a}, "differ": {Digest: a}, "src-fails": {Diagnostics: failed},
		"dst-fails": {Digest: a}, "warning": {Digest: a, Diagnostics: warned}, "error-with-digest": {Digest: a, Diagnostics: failed},
	}, &srcAsked)
	dst := fakeBuild(map[string]Outcome{
		"same": {Digest: a}, "differ": {Digest: b}, "src-fails": {Digest: a},
		"dst-fails": {Diagnostics: failed}, "warning": {Digest: a}, "error-with-digest": {Digest: a},
	}, &dstAsked)
	envs := []string{"same", "differ", "src-fails", "dst-fails", "warning", "error-with-digest"}
	cs, err := VerifyConversion(t.Context(), envs, src, dst)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"same": true, "differ": false, "src-fails": false, "dst-fails": false, "warning": true, "error-with-digest": false}
	var order []string
	for _, c := range cs {
		order = append(order, c.Environment)
		if c.Equal() != want[c.Environment] {
			t.Errorf("%s: Equal() = %v, want %v", c.Environment, c.Equal(), want[c.Environment])
		}
	}
	if !slices.Equal(order, envs) || !slices.Equal(srcAsked, envs) || !slices.Equal(dstAsked, envs) {
		t.Errorf("order = %q, source asked %q, converted asked %q; want %q", order, srcAsked, dstAsked, envs)
	}
	if AllEqual(cs) {
		t.Error("AllEqual with differing Environments")
	}
	if !AllEqual(cs[:1]) || !AllEqual([]Comparison{cs[0], cs[4]}) {
		t.Error("AllEqual false for equal Environments")
	}
	if AllEqual(nil) {
		t.Error("AllEqual(nil) is true, so a check of nothing would pass")
	}
}

// TestVerifyConversionErrors checks the errors of the comparison: no
// Environment, a nil BuildFunc, a build error from either tree, and an
// ended context.
func TestVerifyConversionErrors(t *testing.T) {
	ok := func(context.Context, string) (revision.Digest, diag.List, error) {
		return revision.Sum([]byte("x")), nil, nil
	}
	boom := errors.New("unreadable Bundle")
	broken := func(context.Context, string) (revision.Digest, diag.List, error) {
		return revision.Digest{}, nil, boom
	}
	if _, err := VerifyConversion(t.Context(), nil, ok, ok); !errors.Is(err, ErrNoEnvironment) {
		t.Errorf("no Environment: error = %v, want ErrNoEnvironment", err)
	}
	if _, err := VerifyConversion(t.Context(), []string{"prod"}, nil, ok); !errors.Is(err, ErrOptions) {
		t.Errorf("nil BuildFunc: error = %v, want ErrOptions", err)
	}
	for name, fns := range map[string][2]BuildFunc{"source": {broken, ok}, "converted": {ok, broken}} {
		if _, err := VerifyConversion(t.Context(), []string{"prod"}, fns[0], fns[1]); !errors.Is(err, boom) {
			t.Errorf("%s build error: error = %v, want it wrapped", name, err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := VerifyConversion(ctx, []string{"prod"}, ok, ok); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled: error = %v, want context.Canceled", err)
	}
}
