// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"errors"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/cel/celtest"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Tests for TransformBody: which transform attachments gate the body they
// act on (docs/architecture/03-data-plane.md "Transform Policies") and
// which run while it streams on.

// fakeRefs answers References from a table of sources and records every
// site it was asked at.
type fakeRefs struct {
	refs  map[string]expr.Refs
	errs  map[string]error
	sites []expr.Site
}

func (f *fakeRefs) References(site expr.Site, src string) (expr.Refs, error) {
	f.sites = append(f.sites, site)
	if err := f.errs[src]; err != nil {
		return expr.Refs{}, err
	}
	return f.refs[src], nil
}

// bodyRefs returns a fakeRefs where readsReq reads request.body and
// readsResp reads response.body.
func bodyRefs() *fakeRefs {
	return &fakeRefs{
		refs: map[string]expr.Refs{
			"readsReq":  {Vars: expr.VarRequest, RequestBody: true},
			"readsResp": {Vars: expr.VarResponse, ResponseBody: true},
			"plain":     {Vars: expr.VarRequest},
		},
		errs: map[string]error{"broken": errors.New("does not compile")},
	}
}

// TestTransformBodyNeeds checks every gate trigger of a transform config
// and the transforms that run ungated.
func TestTransformBodyNeeds(t *testing.T) {
	req, resp := v1alpha1.PolicyTypeTransformRequest, v1alpha1.PolicyTypeTransformResponse
	cases := []struct {
		name  string
		typ   v1alpha1.PolicyType
		scope phase.Scope
		js    string
		want  bool
	}{
		{"request body", req, rt, `{"body":"plain"}`, true},
		{"request arrayOps", req, gw, `{"arrayOps":[{"op":"delete","from":"a"}]}`, true},
		{"request replace without path", req, ups, `{"replace":[{"pattern":"a"}]}`, true},
		{"request replace with path", req, rt, `{"replace":[{"path":"a","pattern":"a"}]}`, true},
		{"request set with target body", req, rt, `{"set":[{"target":"body","name":"a","valueExpression":"plain"}]}`, true},
		{"request remove with target body", req, rt, `{"remove":[{"target":"body","name":"a"}]}`, true},
		{"request header set reading request.body", req, rt, `{"set":[{"target":"header","name":"x","valueExpression":"readsReq"}]}`, true},
		{"request query set reading request.body", req, gw, `{"set":[{"target":"query","name":"q","valueExpression":"readsReq"}]}`, true},
		{"request path set reading request.body", req, rt, `{"set":[{"target":"path","valueExpression":"readsReq"}]}`, true},
		{"request header set reading request.body at Upstream", req, ups, `{"set":[{"target":"header","name":"x","valueExpression":"readsReq"}]}`, true},
		{"request header, query and path without a body read", req, rt, `{"set":[{"target":"header","name":"x","valueExpression":"plain"},{"target":"query","name":"q","valueExpression":"plain"},{"target":"path","valueExpression":"plain"}],"remove":[{"target":"header","name":"y"},{"target":"query","name":"z"}]}`, false},
		{"request contentType only", req, rt, `{"contentType":"text/plain"}`, false},
		{"request empty", req, rt, `{}`, false},
		{"request CEL that does not compile", req, rt, `{"set":[{"target":"header","name":"x","valueExpression":"broken"}]}`, false},
		{"response body", resp, rt, `{"body":"plain"}`, true},
		{"response arrayOps at Upstream", resp, ups, `{"arrayOps":[{"op":"move","from":"a","to":"b"}]}`, true},
		{"response set with target body", resp, gw, `{"set":[{"target":"body","name":"a","valueExpression":"plain"}]}`, true},
		{"response remove with target body", resp, rt, `{"remove":[{"target":"body","name":"a"}]}`, true},
		{"response replace", resp, rt, `{"replace":[{"pattern":"a","literal":true}]}`, true},
		{"response header set reading response.body", resp, rt, `{"set":[{"target":"header","name":"x","valueExpression":"readsResp"}]}`, true},
		{"response header set reading only request.body", resp, rt, `{"set":[{"target":"header","name":"x","valueExpression":"readsReq"}]}`, false},
		{"response header operations without a body read", resp, ups, `{"set":[{"target":"header","name":"x","valueExpression":"plain"}],"remove":[{"target":"header","name":"y"}]}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := New().TransformBody(Attachment{Policy: policy("tr", tc.typ), Config: decode(t, tc.typ, tc.js), Scope: tc.scope}, bodyRefs())
			if err != nil || got != tc.want {
				t.Errorf("TransformBody = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

// TestTransformBodySites checks the site each valueExpression is asked
// at: its type's value place, the attachment's scope and the string result
// of a header, query or path entry.
func TestTransformBodySites(t *testing.T) {
	refs := bodyRefs()
	cfg := decode(t, v1alpha1.PolicyTypeTransformRequest, `{"set":[{"target":"query","name":"q","valueExpression":"plain"}]}`)
	if _, err := New().TransformBody(Attachment{Policy: policy("tr", v1alpha1.PolicyTypeTransformRequest), Config: cfg, Scope: ups}, refs); err != nil {
		t.Fatal(err)
	}
	want := expr.Site{Place: expr.PlaceTransformRequestValue, Scope: ups, PolicyType: v1alpha1.PolicyTypeTransformRequest, Result: expr.ResultString}
	if len(refs.sites) != 1 || refs.sites[0].Place != want.Place || refs.sites[0].Scope != want.Scope ||
		refs.sites[0].PolicyType != want.PolicyType || refs.sites[0].Result != want.Result {
		t.Errorf("sites = %+v, want one %+v", refs.sites, want)
	}
	refs = bodyRefs()
	cfg = decode(t, v1alpha1.PolicyTypeTransformResponse, `{"set":[{"target":"header","name":"x","valueExpression":"plain"}]}`)
	if _, err := New().TransformBody(Attachment{Policy: policy("tr", v1alpha1.PolicyTypeTransformResponse), Config: cfg, Scope: rt}, refs); err != nil {
		t.Fatal(err)
	}
	if len(refs.sites) != 1 || refs.sites[0].Place != expr.PlaceTransformResponseValue || refs.sites[0].Scope != rt {
		t.Errorf("response sites = %+v", refs.sites)
	}
}

// TestTransformBodyWithCompiler checks TransformBody against the CEL
// compiler itself, which implements CELRefs.
func TestTransformBodyWithCompiler(t *testing.T) {
	c := celtest.Compiler(t)
	for js, want := range map[string]bool{
		`{"set":[{"target":"header","name":"x","valueExpression":"string(request.body.total)"}]}`:       true,
		`{"set":[{"target":"header","name":"x","valueExpression":"request.headers[\"x-a\"]"}]}`:         false,
		`{"set":[{"target":"path","valueExpression":"\"/v2\" + request.path"}]}`:                        false,
		`{"set":[{"target":"query","name":"u","valueExpression":"upstream.name + request.body.kind"}]}`: true,
	} {
		cfg := decode(t, v1alpha1.PolicyTypeTransformRequest, js)
		got, err := New().TransformBody(Attachment{Policy: policy("tr", v1alpha1.PolicyTypeTransformRequest), Config: cfg, Scope: ups}, c)
		if err != nil || got != want {
			t.Errorf("TransformBody(%s) at Upstream scope = %v, %v; want %v", js, got, err, want)
		}
	}
}

// TestTransformBodyErrors checks the programming errors: a CEL field to
// ask without CELRefs, a type that is not a transform, an unregistered
// type, a config of another type, a nil Policy and the zero scope.
func TestTransformBodyErrors(t *testing.T) {
	reg := New()
	withCEL := decode(t, v1alpha1.PolicyTypeTransformRequest, `{"set":[{"target":"header","name":"x","valueExpression":"plain"}]}`)
	if _, err := reg.TransformBody(Attachment{Policy: policy("tr", v1alpha1.PolicyTypeTransformRequest), Config: withCEL, Scope: rt}, nil); !errors.Is(err, ErrNoCELRefs) {
		t.Errorf("TransformBody without CELRefs = %v, want ErrNoCELRefs", err)
	}
	// Without a CEL field to ask, no CELRefs is needed.
	rewrites := decode(t, v1alpha1.PolicyTypeTransformRequest, `{"remove":[{"target":"body","name":"a"}]}`)
	if got, err := reg.TransformBody(Attachment{Policy: policy("tr", v1alpha1.PolicyTypeTransformRequest), Config: rewrites, Scope: rt}, nil); err != nil || !got {
		t.Errorf("TransformBody of a body rewrite without CELRefs = %v, %v; want true", got, err)
	}
	for name, a := range map[string]Attachment{
		"headers type":     {Policy: policy("h", v1alpha1.PolicyTypeHeaders), Scope: rt},
		"unknown type":     {Policy: policy("u", "transform.bogus"), Scope: rt},
		"other config":     {Policy: policy("tr", v1alpha1.PolicyTypeTransformResponse), Config: withCEL, Scope: rt},
		"nil Policy":       {Scope: rt},
		"no scope":         {Policy: policy("tr", v1alpha1.PolicyTypeTransformRequest), Config: withCEL},
		"typed nil config": {Policy: policy("tr", v1alpha1.PolicyTypeTransformRequest), Config: (*v1alpha1.TransformResponseConfig)(nil), Scope: rt},
	} {
		if _, err := reg.TransformBody(a, bodyRefs()); err == nil {
			t.Errorf("%s: TransformBody succeeded", name)
		}
	}
	if _, err := reg.TransformBody(Attachment{Policy: policy("u", "transform.bogus"), Scope: rt}, bodyRefs()); !errors.Is(err, ErrUnknownType) {
		t.Errorf("unknown type: %v, want ErrUnknownType", err)
	}
	// A nil config reads as an empty one.
	if got, err := reg.TransformBody(Attachment{Policy: policy("tr", v1alpha1.PolicyTypeTransformRequest), Scope: rt}, nil); err != nil || got {
		t.Errorf("TransformBody of a nil config = %v, %v; want false", got, err)
	}
}
