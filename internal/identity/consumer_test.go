// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"reflect"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Tests for spec 06 requirement 21 (the compiled Consumer: name, tier,
// tags, labels, quota names, plus the by-name quota map QuotaByName that
// Consumers.Quota reads) and the Revoker
// hook of requirements 31 and 40 (M1 never revokes).

func TestReq21CompileConsumer(t *testing.T) {
	tests := []struct {
		name string
		in   *v1alpha1.Consumer
		want wantConsumer
	}{
		{
			name: "full",
			in: &v1alpha1.Consumer{
				Metadata: v1alpha1.ObjectMeta{Name: "partner-beta", Labels: map[string]string{"team": "b2b"}},
				Spec: v1alpha1.ConsumerSpec{
					Tier: "gold",
					Tags: []string{"partner", "eu", "partner"},
					Quotas: []v1alpha1.Quota{
						{Name: "monthly", Unit: v1alpha1.QuotaUnitRequests, Limit: 10},
						{Name: "daily-tokens", Unit: v1alpha1.QuotaUnitTokens, Limit: 20},
					},
				},
			},
			want: wantConsumer{
				name: "partner-beta", tier: "gold",
				tags: []string{"eu", "partner"}, labels: map[string]string{"team": "b2b"},
				quotas: []string{"daily-tokens", "monthly"},
				quotaByName: map[string]v1alpha1.Quota{
					"monthly":      {Name: "monthly", Unit: v1alpha1.QuotaUnitRequests, Limit: 10},
					"daily-tokens": {Name: "daily-tokens", Unit: v1alpha1.QuotaUnitTokens, Limit: 20},
				},
			},
		},
		{
			name: "duplicate quota name keeps the first",
			in: &v1alpha1.Consumer{
				Metadata: v1alpha1.ObjectMeta{Name: "dup"},
				Spec: v1alpha1.ConsumerSpec{Quotas: []v1alpha1.Quota{
					{Name: "monthly", Unit: v1alpha1.QuotaUnitRequests, Limit: 1},
					{Name: "monthly", Unit: v1alpha1.QuotaUnitTokens, Limit: 2},
				}},
			},
			want: wantConsumer{
				name: "dup", tags: []string{}, labels: map[string]string{}, quotas: []string{"monthly"},
				quotaByName: map[string]v1alpha1.Quota{"monthly": {Name: "monthly", Unit: v1alpha1.QuotaUnitRequests, Limit: 1}},
			},
		},
		{
			name: "empty collections are non-nil, no quota map",
			in:   &v1alpha1.Consumer{Metadata: v1alpha1.ObjectMeta{Name: "anon"}},
			want: wantConsumer{name: "anon", tags: []string{}, labels: map[string]string{}, quotas: []string{}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := CompileConsumer(tt.in)
			got := wantConsumer{name: c.Name, tier: c.Tier, tags: c.Tags, labels: c.Labels, quotas: c.Quotas, quotaByName: c.QuotaByName}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("CompileConsumer = %+v, want %+v", got, tt.want)
			}
			if c.Tags == nil || c.Labels == nil || c.Quotas == nil {
				t.Fatal("nil collection in the compiled view")
			}
		})
	}
}

type wantConsumer struct {
	name, tier  string
	tags        []string
	labels      map[string]string
	quotas      []string
	quotaByName map[string]v1alpha1.Quota
}

// The view never aliases the Bundle: editing the source after compile
// leaves it unchanged (snapshots are immutable).
func TestReq21CompileConsumerCopies(t *testing.T) {
	src := &v1alpha1.Consumer{
		Metadata: v1alpha1.ObjectMeta{Name: "a", Labels: map[string]string{"k": "v"}},
		Spec: v1alpha1.ConsumerSpec{
			Tags:   []string{"b", "a"},
			Quotas: []v1alpha1.Quota{{Name: "q", Unit: v1alpha1.QuotaUnitRequests, Limit: 5}},
		},
	}
	c := CompileConsumer(src)
	src.Metadata.Labels["k"] = "changed"
	src.Spec.Tags[0] = "z"
	src.Spec.Quotas[0].Limit = 6
	if c.Labels["k"] != "v" || c.Tags[0] != "a" || c.Tags[1] != "b" || c.QuotaByName["q"].Limit != 5 {
		t.Fatalf("view aliases its source: labels %v tags %v quotas %v", c.Labels, c.Tags, c.QuotaByName)
	}
}

func TestReq21ConsumersQuotaMap(t *testing.T) {
	day := v1alpha1.Duration(24 * time.Hour)
	cs := CompileConsumers([]*v1alpha1.Consumer{
		{
			Metadata: v1alpha1.ObjectMeta{Name: "acme"},
			Spec: v1alpha1.ConsumerSpec{Quotas: []v1alpha1.Quota{
				{Name: "daily-tokens", Unit: v1alpha1.QuotaUnitTokens, Limit: 2000000, Window: day},
			}},
		},
		nil,
		{Metadata: v1alpha1.ObjectMeta{Name: "acme"}, Spec: v1alpha1.ConsumerSpec{Tier: "duplicate"}},
		{Metadata: v1alpha1.ObjectMeta{Name: "beta"}},
	})
	if cs.Len() != 2 {
		t.Fatalf("Len = %d, want 2", cs.Len())
	}
	if c, ok := cs.Get("acme"); !ok || c.Tier != "" {
		t.Fatalf("Get(acme) = %+v, %v; a duplicate name must keep the first", c, ok)
	}
	if len(cs.Map()) != 2 || cs.Map()["beta"] == nil {
		t.Fatalf("Map = %v", cs.Map())
	}
	tests := []struct {
		consumer, quota string
		ok              bool
		limit           int64
	}{
		{"acme", "daily-tokens", true, 2000000},
		{"acme", "monthly", false, 0},
		{"beta", "daily-tokens", false, 0},
		{"nobody", "daily-tokens", false, 0},
	}
	for _, tt := range tests {
		q, ok := cs.Quota(tt.consumer, tt.quota)
		if ok != tt.ok || q.Limit != tt.limit {
			t.Errorf("Quota(%s, %s) = %+v, %v; want limit %d, %v", tt.consumer, tt.quota, q, ok, tt.limit, tt.ok)
		}
		if ok && (q.Window != day || q.Unit != v1alpha1.QuotaUnitTokens) {
			t.Errorf("Quota(%s, %s) = %+v", tt.consumer, tt.quota, q)
		}
		// Quota reads the view's map, which the quota Policies read directly.
		if c, found := cs.Get(tt.consumer); found {
			if vq, vok := c.QuotaByName[tt.quota]; vok != ok || vq != q {
				t.Errorf("view QuotaByName[%s] = %+v, %v; Quota gave %+v, %v", tt.quota, vq, vok, q, ok)
			}
		}
	}
}

func TestReq21CompileBundle(t *testing.T) {
	if CompileBundle(nil).Len() != 0 {
		t.Fatal("nil Bundle compiled Consumers")
	}
	b := hub.NewBundle([]*hub.Resource{
		{ID: hub.ID{Kind: v1alpha1.KindConsumer, Name: "b"}, Object: consumer("b")},
		{ID: hub.ID{Kind: v1alpha1.KindConsumer, Name: "a"}, Object: consumer("a")},
		{ID: hub.ID{Kind: v1alpha1.KindRoute, Name: "r"}, Object: &v1alpha1.Route{}},
	})
	cs := CompileBundle(b)
	if cs.Len() != 2 {
		t.Fatalf("Len = %d, want 2", cs.Len())
	}
}

func TestReq31NopRevokerNeverRevokes(t *testing.T) {
	var r Revoker = NopRevoker{}
	if r.RevokedJWT("iss", "kid", "jti", "sub", time.Unix(1_700_000_000, 0)) || r.RevokedKey("iss", "kid") ||
		r.RevokedAPIKey([32]byte{1}) || r.RevokedBasic("u") || r.RevokedCert([]byte{1}, nil) {
		t.Fatal("NopRevoker revoked a credential")
	}
}
