// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package response

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/cel/celtest"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/filtertest"
	"github.com/ravindu-rev/ruralz/internal/filter/transform"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// env returns the BuildEnv of cfg at scope with a real CEL builder.
func env(tb testing.TB, cfg any, scope phase.Scope) filter.BuildEnv {
	tb.Helper()
	return filter.BuildEnv{
		Policy: &v1alpha1.Policy{Metadata: v1alpha1.ObjectMeta{Name: "t"}, Spec: v1alpha1.PolicySpec{Type: Type}},
		Config: cfg,
		Scope:  scope,
		CEL:    celtest.Compiler(tb).NewBuilder(nil, celtest.Caps(nil)),
		Limits: filter.Limits{MaxRequestBodyBytes: 1 << 20, MaxResponseBodyBytes: 1 << 20},
	}
}

// TestFactory covers Build: a transform.response config compiles to a
// Filter acting in onResponse, or onUpstreamResponseBody at Upstream
// scope; a config of another type or a nil one is RZ-CFG-005, a refused
// config is never a non-nil Filter, and an ended context is its error.
func TestFactory(t *testing.T) {
	cfg := &v1alpha1.TransformResponseConfig{Set: []v1alpha1.TransformResponseSet{{Target: v1alpha1.TransformResponseTargetHeader, Name: "x-status", ValueExpression: "string(response.status)"}}}
	for _, tc := range []struct {
		scope phase.Scope
		p     phase.Phase
	}{{phase.ScopeRoute, phase.OnResponse}, {phase.ScopeUpstream, phase.OnUpstreamResponseBody}} {
		f, err := Factory{}.Build(context.Background(), env(t, cfg, tc.scope))
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		h := http.Header{}
		x := &filtertest.Exchange{Msg: &filtertest.Message{Hdr: h, Code: 201}, VarsV: &expr.Vars{Response: &expr.Response{Status: 201, Header: h}}}
		if res := f.Handle(context.Background(), tc.p, x); res.Outcome != filter.Continue || h.Get("X-Status") != "201" {
			t.Errorf("%v: Handle = %+v, header %v", tc.p, res, h)
		}
	}
	for _, bad := range []any{&v1alpha1.TransformRequestConfig{}, (*v1alpha1.TransformResponseConfig)(nil), nil} {
		f, err := Factory{}.Build(context.Background(), env(t, bad, phase.ScopeRoute))
		if code, _ := errcode.CodeOf(err); code != transform.CodeConfig || f != nil {
			t.Errorf("Build(%T) = %v, %v; want nil and %s", bad, f, err, transform.CodeConfig)
		}
	}
	refused := &v1alpha1.TransformResponseConfig{Set: []v1alpha1.TransformResponseSet{{Target: v1alpha1.TransformResponseTargetHeader, Name: "x-a", ValueExpression: "response.status"}}}
	if f, err := (Factory{}).Build(context.Background(), env(t, refused, phase.ScopeRoute)); f != nil || err == nil {
		t.Errorf("Build of an int header value = %v, %v", f, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (Factory{}).Build(ctx, env(t, cfg, phase.ScopeRoute)); !errors.Is(err, context.Canceled) {
		t.Errorf("Build with an ended context = %v", err)
	}
}

// TestCheck covers the offline check: the findings of transform.Report at
// paths under spec.config, nothing for a resource without a decoded
// transform.response config.
func TestCheck(t *testing.T) {
	cfg := &v1alpha1.TransformResponseConfig{
		Remove:  []v1alpha1.TransformResponseRemove{{Target: v1alpha1.TransformResponseTargetHeader, Name: "content-encoding"}},
		Replace: []v1alpha1.Replace{{Pattern: "a("}},
	}
	r := &hub.Resource{ID: hub.ID{Kind: v1alpha1.KindPolicy, Name: "t"}, Config: cfg}
	var got []string
	Check(r, nil, func(d diag.Diagnostic) { got = append(got, d.Code+" "+d.Path.String()) })
	want := []string{transform.CodeConfig + " spec.config.remove[0].name", transform.CodeConfig + " spec.config.replace[0].pattern"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("Check = %q, want %q", got, want)
	}
	for _, other := range []*hub.Resource{nil, {Config: &v1alpha1.TransformRequestConfig{}}, {Config: (*v1alpha1.TransformResponseConfig)(nil)}} {
		Check(other, nil, func(d diag.Diagnostic) { t.Errorf("Check(%+v) reported %+v", other, d) })
	}
}
