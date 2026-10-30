// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package request

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/cel/celtest"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/filtertest"
	"github.com/ravindu-rev/ruralz/internal/filter/transform"
	"github.com/ravindu-rev/ruralz/internal/httpfield"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// env returns the BuildEnv of cfg at Route scope with a real CEL builder.
func env(tb testing.TB, cfg any) filter.BuildEnv {
	tb.Helper()
	return filter.BuildEnv{
		Policy: &v1alpha1.Policy{Metadata: v1alpha1.ObjectMeta{Name: "t"}, Spec: v1alpha1.PolicySpec{Type: Type}},
		Config: cfg,
		Scope:  phase.ScopeRoute,
		CEL:    celtest.Compiler(tb).NewBuilder(nil, celtest.Caps(nil)),
		Limits: filter.Limits{MaxRequestBodyBytes: 1 << 20, MaxResponseBodyBytes: 1 << 20},
	}
}

// TestFactory covers Build: a transform.request config compiles to a
// Filter acting in onRequestBody, a config of another type or a nil one
// is RZ-CFG-005, a refused config is never a non-nil Filter, and an ended
// context is its error.
func TestFactory(t *testing.T) {
	cfg := &v1alpha1.TransformRequestConfig{Set: []v1alpha1.TransformRequestSet{{Target: v1alpha1.TransformRequestTargetHeader, Name: "x-route", ValueExpression: "route.name"}}}
	f, err := Factory{}.Build(context.Background(), env(t, cfg))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	h := http.Header{}
	x := &filtertest.Exchange{Msg: &filtertest.Message{Hdr: h}, VarsV: &expr.Vars{Route: celtest.Route("orders", nil)}}
	if res := f.Handle(context.Background(), phase.OnRequestBody, x); res.Outcome != filter.Continue || h.Get("X-Route") != "orders" {
		t.Errorf("Handle = %+v, header %v", res, h)
	}
	for _, bad := range []any{&v1alpha1.TransformResponseConfig{}, (*v1alpha1.TransformRequestConfig)(nil), nil} {
		f, err := Factory{}.Build(context.Background(), env(t, bad))
		if code, _ := errcode.CodeOf(err); code != transform.CodeConfig || f != nil {
			t.Errorf("Build(%T) = %v, %v; want nil and %s", bad, f, err, transform.CodeConfig)
		}
	}
	refused := &v1alpha1.TransformRequestConfig{Remove: []v1alpha1.TransformRequestRemove{{Target: v1alpha1.TransformRequestTargetPath}}}
	if f, err := (Factory{}).Build(context.Background(), env(t, refused)); f != nil || err == nil {
		t.Errorf("Build of a refused config = %v, %v", f, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (Factory{}).Build(ctx, env(t, cfg)); !errors.Is(err, context.Canceled) {
		t.Errorf("Build with an ended context = %v", err)
	}
}

// TestCheck covers the offline check: the findings of transform.Report at
// paths under spec.config, nothing for a resource without a decoded
// transform.request config.
func TestCheck(t *testing.T) {
	cfg := &v1alpha1.TransformRequestConfig{Set: []v1alpha1.TransformRequestSet{{Target: v1alpha1.TransformRequestTargetHeader, Name: "host", ValueExpression: `"a"`}}}
	r := &hub.Resource{ID: hub.ID{Kind: v1alpha1.KindPolicy, Name: "t"}, Config: cfg}
	var got []diag.Diagnostic
	Check(r, nil, func(d diag.Diagnostic) { got = append(got, d) })
	if len(got) != 1 || got[0].Code != transform.CodeConfig || got[0].Path.String() != "spec.config.set[0].name" {
		t.Errorf("Check = %+v", got)
	}
	for _, other := range []*hub.Resource{nil, {Config: &v1alpha1.TransformResponseConfig{}}, {Config: (*v1alpha1.TransformRequestConfig)(nil)}} {
		Check(other, nil, func(d diag.Diagnostic) { t.Errorf("Check(%+v) reported %+v", other, d) })
	}
}

// TestAddedRequestBytes covers the bytes a config adds to the request CEL
// reads: each header or query set entry its name, its value cap and 4
// bytes, a path entry the path cap and 4 bytes, a body rewrite its
// Content-Type and Content-Length lines, and a remove entry nothing.
func TestAddedRequestBytes(t *testing.T) {
	set := func(target v1alpha1.TransformRequestTarget, name string) v1alpha1.TransformRequestSet {
		return v1alpha1.TransformRequestSet{Target: target, Name: name, ValueExpression: `"v"`}
	}
	lines := int64(len("Content-Type")+len("application/json")+4) + int64(len("Content-Length")+19+4)
	long := "application/vnd.example.order-v2+json"
	tests := []struct {
		name string
		cfg  *v1alpha1.TransformRequestConfig
		want int64
	}{
		{"nil", nil, 0},
		{"empty", &v1alpha1.TransformRequestConfig{}, 0},
		{"removes", &v1alpha1.TransformRequestConfig{Remove: []v1alpha1.TransformRequestRemove{{Target: v1alpha1.TransformRequestTargetHeader, Name: "x-a"}, {Target: v1alpha1.TransformRequestTargetQuery, Name: "q"}}}, 0},
		{"header", &v1alpha1.TransformRequestConfig{Set: []v1alpha1.TransformRequestSet{set(v1alpha1.TransformRequestTargetHeader, "x-ab")}}, 4 + httpfield.MaxComputedValueBytes + 4},
		{"query", &v1alpha1.TransformRequestConfig{Set: []v1alpha1.TransformRequestSet{set(v1alpha1.TransformRequestTargetQuery, "page")}}, 4 + httpfield.MaxQueryValueBytes + 4},
		{"path", &v1alpha1.TransformRequestConfig{Set: []v1alpha1.TransformRequestSet{set(v1alpha1.TransformRequestTargetPath, "")}}, transform.MaxPathBytes + 4},
		{"body entry", &v1alpha1.TransformRequestConfig{Set: []v1alpha1.TransformRequestSet{set(v1alpha1.TransformRequestTargetBody, "a")}}, lines},
		{"body remove", &v1alpha1.TransformRequestConfig{Remove: []v1alpha1.TransformRequestRemove{{Target: v1alpha1.TransformRequestTargetBody, Name: "a"}}}, lines},
		{"array op", &v1alpha1.TransformRequestConfig{ArrayOps: []v1alpha1.ArrayOp{{Op: v1alpha1.ArrayOpDelete, From: "a.0"}}}, lines},
		{"replace", &v1alpha1.TransformRequestConfig{Replace: []v1alpha1.Replace{{Pattern: "a"}}}, lines},
		{"body with a long contentType", &v1alpha1.TransformRequestConfig{Body: "request.body", ContentType: &long},
			int64(len("Content-Type")+len(long)+4) + int64(len("Content-Length")+19+4)},
		{"everything", &v1alpha1.TransformRequestConfig{Body: "request.body", Set: []v1alpha1.TransformRequestSet{
			set(v1alpha1.TransformRequestTargetHeader, "x"), set(v1alpha1.TransformRequestTargetQuery, "y"), set(v1alpha1.TransformRequestTargetPath, ""),
		}}, 1 + httpfield.MaxComputedValueBytes + 4 + 1 + httpfield.MaxQueryValueBytes + 4 + transform.MaxPathBytes + 4 + lines},
	}
	for _, tc := range tests {
		if got := AddedRequestBytes(tc.cfg); got != tc.want {
			t.Errorf("%s: AddedRequestBytes = %d, want %d", tc.name, got, tc.want)
		}
	}
	if !strings.HasPrefix(string(Type), "transform.") {
		t.Errorf("Type = %q", Type)
	}
}
