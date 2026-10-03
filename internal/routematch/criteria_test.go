// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package routematch

import (
	"reflect"
	"slices"
	"testing"

	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// fieldNames returns the exported field names of struct type t, in order.
func fieldNames(t reflect.Type) []string {
	names := make([]string, t.NumField())
	for i := range names {
		names[i] = t.Field(i).Name
	}
	return names
}

// TestCriteriaMirrorsRouteMatch pins R-68: Criteria and its nested types
// have the field names of v1alpha1.RouteMatch and its nested types, so a
// field added to Route.spec.match is never silently left out of MatchKey.
// The nested types are also pinned by the struct conversions in CriteriaOf.
func TestCriteriaMirrorsRouteMatch(t *testing.T) {
	pairs := []struct{ api, mirror reflect.Type }{
		{reflect.TypeFor[v1alpha1.RouteMatch](), reflect.TypeFor[Criteria]()},
		{reflect.TypeFor[v1alpha1.PathMatch](), reflect.TypeFor[PathCriteria]()},
		{reflect.TypeFor[v1alpha1.HeaderMatch](), reflect.TypeFor[HeaderCriteria]()},
		{reflect.TypeFor[v1alpha1.GRPCMatch](), reflect.TypeFor[GRPCCriteria]()},
		{reflect.TypeFor[v1alpha1.GraphQLMatch](), reflect.TypeFor[GraphQLCriteria]()},
	}
	for _, p := range pairs {
		if got, want := fieldNames(p.mirror), fieldNames(p.api); !slices.Equal(got, want) {
			t.Errorf("%s fields = %v, want those of %s: %v", p.mirror, got, p.api, want)
		}
	}
}

// TestCriteriaOf: every field is carried over, the result shares no memory
// with the input, and nil gives the zero Criteria.
func TestCriteriaOf(t *testing.T) {
	m := &v1alpha1.RouteMatch{
		Hosts:   []string{"api.shop.example", "*.shop.example"},
		Path:    &v1alpha1.PathMatch{Template: "/v1/orders/{orderId}"},
		Methods: []string{"GET", "HEAD"},
		Headers: []v1alpha1.HeaderMatch{
			{Name: "X-Tenant", Exact: "eu"},
			{Name: "x-debug", Present: boolPtr(false)},
		},
		GRPC:    &v1alpha1.GRPCMatch{Service: "shop.v1.Orders", Method: "Get"},
		GraphQL: &v1alpha1.GraphQLMatch{OperationType: "query", OperationName: "Orders"},
		Topic:   "orders",
		When:    `request.query["v"] == "2"`,
	}
	want := Criteria{
		Hosts:   []string{"api.shop.example", "*.shop.example"},
		Path:    &PathCriteria{Template: "/v1/orders/{orderId}"},
		Methods: []string{"GET", "HEAD"},
		Headers: []HeaderCriteria{
			{Name: "X-Tenant", Exact: "eu"},
			{Name: "x-debug", Present: boolPtr(false)},
		},
		GRPC:    &GRPCCriteria{Service: "shop.v1.Orders", Method: "Get"},
		GraphQL: &GraphQLCriteria{OperationType: "query", OperationName: "Orders"},
		Topic:   "orders",
		When:    `request.query["v"] == "2"`,
	}
	got := CriteriaOf(m)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CriteriaOf = %+v, want %+v", got, want)
	}
	if MatchKey(&got) != MatchKey(&want) {
		t.Errorf("MatchKey(CriteriaOf(m)) = %s, want %s", MatchKey(&got), MatchKey(&want))
	}

	// Mutating the input leaves the result unchanged.
	m.Hosts[0], m.Methods[0], m.Path.Template = "x.example", "POST", "/x"
	m.Headers[0].Exact, *m.Headers[1].Present = "us", true
	m.GRPC.Method, m.GraphQL.OperationName = "List", "Other"
	if !reflect.DeepEqual(got, want) {
		t.Errorf("CriteriaOf result changed with its input: %+v", got)
	}

	if got := CriteriaOf(nil); !reflect.DeepEqual(got, Criteria{}) {
		t.Errorf("CriteriaOf(nil) = %+v, want the zero Criteria", got)
	}
	if got := CriteriaOf(&v1alpha1.RouteMatch{}); !reflect.DeepEqual(got, Criteria{}) {
		t.Errorf("CriteriaOf(empty) = %+v, want the zero Criteria", got)
	}
}
