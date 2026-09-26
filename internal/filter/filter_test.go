// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package filter

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Tests for architecture section 2.13 (WP-01): ConnCache eviction order,
// Registry copy semantics and Components order (R-45), the Result helpers
// including Retry (R-44) and the SPI sentinels.

func TestConnCacheEvictionOrder(t *testing.T) {
	var c ConnCache
	for i := range 8 {
		c.Put(i, fmt.Sprint("v", i))
	}
	for i := range 8 {
		if v, ok := c.Get(i); !ok || v != fmt.Sprint("v", i) {
			t.Fatalf("Get(%d) = %v, %v with 8 entries", i, v, ok)
		}
	}
	// The ninth entry evicts the oldest, the tenth the next oldest.
	c.Put(8, "v8")
	if _, ok := c.Get(0); ok {
		t.Fatal("the oldest entry survived a ninth Put")
	}
	c.Put(9, "v9")
	if _, ok := c.Get(1); ok {
		t.Fatal("the second-oldest entry survived a tenth Put")
	}
	for i := 2; i < 10; i++ {
		if v, ok := c.Get(i); !ok || v != fmt.Sprint("v", i) {
			t.Fatalf("Get(%d) = %v, %v after eviction", i, v, ok)
		}
	}
	// A full cycle later every original entry is gone.
	for i := 10; i < 18; i++ {
		c.Put(i, i)
	}
	for i := range 10 {
		if _, ok := c.Get(i); ok {
			t.Fatalf("entry %d survived a full cycle", i)
		}
	}
}

func TestConnCachePutReplaces(t *testing.T) {
	var c ConnCache
	type key struct{ subject string }
	c.Put(key{"cn=a"}, "first")
	c.Put(key{"cn=b"}, "other")
	c.Put(key{"cn=a"}, "second")
	if v, ok := c.Get(key{"cn=a"}); !ok || v != "second" {
		t.Fatalf("Get after a second Put = %v, %v; want the latest value", v, ok)
	}
	// Replacing does not use a slot: six more keys fill the cache and keep both.
	for i := range 6 {
		c.Put(i, i)
	}
	if _, ok := c.Get(key{"cn=a"}); !ok {
		t.Fatal("replacing a key consumed a slot")
	}
	if _, ok := c.Get(key{"cn=b"}); !ok {
		t.Fatal("replacing a key consumed a slot")
	}
}

func TestConnCacheMisses(t *testing.T) {
	var c ConnCache
	if _, ok := c.Get("x"); ok {
		t.Fatal("empty cache hit")
	}
	if _, ok := c.Get(nil); ok {
		t.Fatal("Get(nil) hit an empty slot")
	}
	c.Put("k", nil)
	if v, ok := c.Get("k"); !ok || v != nil {
		t.Fatalf("a nil value is not stored: %v, %v", v, ok)
	}
}

// TestConnCacheKeys pins the documented key contract: keys are non-nil
// comparable values; a nil or uncomparable key is never stored and never
// found, and neither Put nor Get panics on one.
func TestConnCacheKeys(t *testing.T) {
	type digest [32]byte
	type withSlice struct{ b []byte }
	type withAny struct{ v any }
	tests := []struct {
		name      string
		key, same any
		cached    bool
	}{
		{"string", "cn=a", "cn=a", true},
		{"digest array", digest{1, 2}, digest{1, 2}, true},
		{"struct of comparable fields", struct{ a, b string }{"x", "y"}, struct{ a, b string }{"x", "y"}, true},
		{"interface field holding a string", withAny{"x"}, withAny{"x"}, true},
		{"nil", nil, nil, false},
		{"byte slice", []byte("k"), []byte("k"), false},
		{"map", map[string]int{"k": 1}, map[string]int{"k": 1}, false},
		{"func", func() {}, func() {}, false},
		{"struct with a slice", withSlice{[]byte("k")}, withSlice{[]byte("k")}, false},
		{"interface field holding a slice", withAny{[]byte("k")}, withAny{[]byte("k")}, false},
		{"array of interfaces holding a map", [1]any{map[string]int{}}, [1]any{map[string]int{}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var c ConnCache
			// Fill every slot first, so a stored uncomparable key would be
			// compared (and panic) on the second Put or Get.
			for i := range 8 {
				c.Put(i, i)
			}
			c.Put(tt.key, "v")
			c.Put(tt.key, "v2")
			v, ok := c.Get(tt.same)
			if ok != tt.cached {
				t.Fatalf("Get = %v, %v; cached %v", v, ok, tt.cached)
			}
			if tt.cached && v != "v2" {
				t.Fatalf("Get = %v, want the latest value", v)
			}
			// An uncached key uses no slot: all eight integer keys remain.
			hits := 0
			for i := range 8 {
				if _, ok := c.Get(i); ok {
					hits++
				}
			}
			want := 8
			if tt.cached {
				want = 7 // the key evicted the oldest entry once
			}
			if hits != want {
				t.Fatalf("%d integer keys remain, want %d", hits, want)
			}
		})
	}
}

func TestConnCacheConcurrent(t *testing.T) {
	// Concurrent HTTP/2 streams of one connection share it.
	var c ConnCache
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 200 {
				c.Put(w*1000+i%10, i)
				c.Get(w*1000 + i%10)
			}
		}()
	}
	wg.Wait()
	hits := 0
	for w := range 8 {
		for i := range 10 {
			if _, ok := c.Get(w*1000 + i); ok {
				hits++
			}
		}
	}
	if hits == 0 || hits > 8 {
		t.Fatalf("%d keys cached, want 1 to 8", hits)
	}
}

func TestConnTLSPeerCertificates(t *testing.T) {
	var nilTLS *ConnTLS
	if nilTLS.PeerCertificates() != nil {
		t.Fatal("nil ConnTLS has certificates")
	}
	if (&ConnTLS{}).PeerCertificates() != nil {
		t.Fatal("ConnTLS without a state has certificates")
	}
	leaf := &x509.Certificate{}
	c := &ConnTLS{State: &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}}
	if got := c.PeerCertificates(); len(got) != 1 || got[0] != leaf {
		t.Fatalf("PeerCertificates = %v", got)
	}
}

type stubFactory struct{ name string }

func (stubFactory) Build(context.Context, BuildEnv) (Filter, error) { return nil, errors.New("stub") }

type stubComponent struct{ name string }

func (c stubComponent) Name() string                { return c.name }
func (stubComponent) Run(ctx context.Context) error { <-ctx.Done(); return nil }

func TestRegistry(t *testing.T) {
	jwt, rl := stubFactory{"jwt"}, stubFactory{"ratelimit"}
	factories := map[v1alpha1.PolicyType]Factory{v1alpha1.PolicyTypeAuthJWT: jwt, v1alpha1.PolicyTypeRateLimit: rl}
	comps := []Component{stubComponent{"jwks"}, stubComponent{"ratelimit-keys"}, stubComponent{"cache-revalidate"}}
	r := NewRegistry(factories, comps...)

	// Copy semantics: later changes to the arguments do not reach it.
	delete(factories, v1alpha1.PolicyTypeAuthJWT)
	factories[v1alpha1.PolicyTypeCache] = stubFactory{"cache"}
	comps[0] = stubComponent{"replaced"}

	if f, ok := r.Factory(v1alpha1.PolicyTypeAuthJWT); !ok || f != jwt {
		t.Fatalf("Factory(auth.jwt) = %v, %v", f, ok)
	}
	if f, ok := r.Factory(v1alpha1.PolicyTypeRateLimit); !ok || f != rl {
		t.Fatalf("Factory(ratelimit) = %v, %v", f, ok)
	}
	if _, ok := r.Factory(v1alpha1.PolicyTypeCache); ok {
		t.Fatal("a Factory added to the argument map reached the Registry")
	}
	if _, ok := r.Factory(v1alpha1.PolicyTypePlugin); ok {
		t.Fatal("an unserved type has a Factory")
	}

	names := func() []string {
		var out []string
		for _, c := range r.Components() {
			out = append(out, c.Name())
		}
		return out
	}
	want := []string{"jwks", "ratelimit-keys", "cache-revalidate"}
	if got := names(); !slices.Equal(got, want) {
		t.Fatalf("Components = %v, want construction order %v", got, want)
	}
	// Components returns a copy.
	cs := r.Components()
	cs[0] = stubComponent{"mutated"}
	if got := names(); !slices.Equal(got, want) {
		t.Fatalf("mutating the result changed the Registry: %v", got)
	}

	empty := NewRegistry(nil)
	if _, ok := empty.Factory(v1alpha1.PolicyTypeCORS); ok || len(empty.Components()) != 0 {
		t.Fatal("an empty Registry is not empty")
	}
}

func TestResultHelpers(t *testing.T) {
	if r := Next(); r.Outcome != Continue || r.Response != nil || r.Code != "" || r.Err != nil {
		t.Fatalf("Next = %+v", r)
	}
	resp := &Response{Status: 204, Header: http.Header{"Access-Control-Allow-Origin": {"*"}}}
	if r := Reply(resp); r.Outcome != Respond || r.Response != resp {
		t.Fatalf("Reply = %+v", r)
	}
	h := http.Header{"Www-Authenticate": {"Bearer"}}
	r := Deny(401, "RZ-AUTH-001", h)
	if r.Outcome != Respond || r.Response.Status != 401 || r.Response.Code != "RZ-AUTH-001" || r.Response.Body != nil || r.Response.Header.Get("Www-Authenticate") != "Bearer" {
		t.Fatalf("Deny = %+v", r.Response)
	}
	cause := errors.New("state store timeout")
	if r := Undecided("RZ-RL-005", cause); r.Outcome != CannotDecide || r.Code != "RZ-RL-005" || !errors.Is(r.Err, cause) {
		t.Fatalf("Undecided = %+v", r)
	}
	if r := RetryAttempt(); r.Outcome != Retry || r.Response != nil {
		t.Fatalf("RetryAttempt = %+v", r)
	}
	// Continue is the zero Outcome, so a zero Result continues.
	if (Result{}).Outcome != Continue {
		t.Fatal("the zero Result does not continue")
	}
}

func TestSentinels(t *testing.T) {
	all := []error{ErrBudget, ErrTooLarge, ErrSecondBinding, ErrNotAvailable}
	for i, a := range all {
		for j, b := range all {
			if (i == j) != errors.Is(a, b) {
				t.Errorf("errors.Is(%v, %v) = %v", a, b, errors.Is(a, b))
			}
		}
		if !errors.Is(fmt.Errorf("wrapped: %w", a), a) {
			t.Errorf("%v is lost through wrapping", a)
		}
	}
}

func TestIdentityMethods(t *testing.T) {
	// auth.method values of the CEL auth variable (spec 06).
	got := []string{MethodJWT, MethodAPIKey, MethodBasic, MethodMTLS}
	if !slices.Equal(got, []string{"jwt", "api-key", "basic", "mtls"}) {
		t.Fatalf("methods = %v", got)
	}
}
