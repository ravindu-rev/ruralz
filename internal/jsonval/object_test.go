// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonval

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// Tests for the mutable tree (07 req 56, 62, 63, 68; WP-02 scope "mutable
// tree (object member order kept)").

func names(o *Object) string {
	var b strings.Builder
	for name := range o.All() {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(name)
	}
	return b.String()
}

func TestObjectSmallAndIndexed(t *testing.T) {
	// The same operations through the linear path (few members) and the
	// indexed path (more than indexedMembers).
	for _, n := range []int{3, indexedMembers, indexedMembers + 1, 40} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			o := NewObject(n)
			for i := range n {
				o.Set(fmt.Sprintf("k%d", i), i)
			}
			if o.Len() != n {
				t.Fatalf("Len = %d", o.Len())
			}
			for i := range n {
				name := fmt.Sprintf("k%d", i)
				if o.Index(name) != i || !o.Has(name) {
					t.Fatalf("Index(%s) = %d", name, o.Index(name))
				}
				if v, ok := o.Get(name); !ok || v != i {
					t.Fatalf("Get(%s) = %v, %v", name, v, ok)
				}
			}
			if o.Has("missing") || o.Index("missing") != -1 {
				t.Fatal("missing member found")
			}
			// Set replaces in place.
			o.Set("k1", "x")
			if o.At(1).Name != "k1" || o.At(1).Value != "x" || o.Len() != n {
				t.Fatalf("Set replaced out of place: %v", o.At(1))
			}
			// Appending after an indexed lookup keeps the index right.
			o.Set("new", true)
			if o.Index("new") != n {
				t.Fatalf("Index(new) = %d", o.Index("new"))
			}
			// Delete keeps the order of the rest and the index right.
			if !o.Delete("k0") || o.Delete("k0") {
				t.Fatal("Delete(k0)")
			}
			for i := 1; i < n; i++ {
				if got := o.Index(fmt.Sprintf("k%d", i)); got != i-1 {
					t.Fatalf("after Delete: Index(k%d) = %d", i, got)
				}
			}
			if o.Index("new") != n-1 {
				t.Fatalf("after Delete: Index(new) = %d", o.Index("new"))
			}
			o.SetAt(0, "y")
			if v, _ := o.Get("k1"); v != "y" {
				t.Fatalf("SetAt: %v", v)
			}
			// Deleting down through the threshold drops the index.
			for o.Len() > 0 {
				o.DeleteAt(o.Len() - 1)
				if o.Len() > 0 {
					last := o.At(o.Len() - 1).Name
					if o.Index(last) != o.Len()-1 {
						t.Fatalf("Index(%s) = %d with %d members", last, o.Index(last), o.Len())
					}
				}
			}
		})
	}
}

func TestObjectNilAndZero(t *testing.T) {
	var o *Object
	if o.Len() != 0 || o.Index("a") != -1 || o.Has("a") || o.Clone() != nil {
		t.Fatal("nil *Object is not empty")
	}
	if _, ok := o.Get("a"); ok {
		t.Fatal("nil Get found a member")
	}
	for range o.All() {
		t.Fatal("nil All yielded")
	}
	o.Sort()
	var z Object
	z.Set("a", 1)
	if z.Len() != 1 {
		t.Fatal("zero Object unusable")
	}
	if NewObject(-1).Len() != 0 {
		t.Fatal("NewObject(-1)")
	}
}

func TestObjectSortAndAll(t *testing.T) {
	o := &Object{}
	for _, n := range []string{"b", "é", "a", "B", "aa", "k1", "k0", "k3", "k2", "z"} {
		o.Set(n, n)
	}
	_ = o.Index("a") // build the index before sorting
	o.Sort()
	if got := names(o); got != "B,a,aa,b,k0,k1,k2,k3,z,é" {
		t.Fatalf("Sort = %s", got)
	}
	for i := range o.Len() {
		if o.Index(o.At(i).Name) != i {
			t.Fatalf("index stale after Sort at %d", i)
		}
	}
	// Early break stops iteration.
	count := 0
	for range o.All() {
		count++
		if count == 2 {
			break
		}
	}
	if count != 2 {
		t.Fatal("All ignored break")
	}
}

func TestClone(t *testing.T) {
	// 07 req 54, 55, 68: the working copy is separate from the original.
	v, _, err := Decode([]byte(`{"a":[1,{"b":"c"}],"d":{"e":null}}`), Options{})
	if err != nil {
		t.Fatal(err)
	}
	c := Clone(v)
	if !Equal(c, v) {
		t.Fatal("clone differs")
	}
	co := c.(*Object)
	arr, _ := co.Get("a")
	inner := arr.([]any)[1].(*Object)
	inner.Set("b", "changed")
	arr.([]any)[0] = json.Number("9")
	co.Delete("d")
	want, _, _ := Decode([]byte(`{"a":[1,{"b":"c"}],"d":{"e":null}}`), Options{})
	if !Equal(v, want) {
		t.Fatal("changing the clone changed the original")
	}
	m, _, _ := Decode([]byte(`{"a":[1,{"b":"c"}]}`), Options{MapObjects: true})
	mc := Clone(m).(map[string]any)
	mc["a"].([]any)[1].(map[string]any)["b"] = "changed"
	if m.(map[string]any)["a"].([]any)[1].(map[string]any)["b"] != "c" {
		t.Fatal("map clone aliases the original")
	}
	for _, v := range []any{nil, "s", json.Number("1"), true, map[string]any(nil), []any(nil), 3} {
		if !Equal(Clone(v), v) && v != 3 {
			t.Fatalf("Clone(%#v) = %#v", v, Clone(v))
		}
	}
}

// indexed reports whether o has its name index exactly when it has more than
// indexedMembers members, with every member at its position.
func indexed(o *Object) bool {
	if len(o.members) <= indexedMembers {
		return o.index == nil
	}
	if len(o.index) != len(o.members) {
		return false
	}
	for i, m := range o.members {
		if o.index[m.Name] != i {
			return false
		}
	}
	return true
}

func TestObjectIndexInvariant(t *testing.T) {
	// 03 req 25, 07 req 56: reads never write, so Decode, Clone and every
	// mutation leave an object of more than 8 members with its index built.
	for _, n := range []int{indexedMembers, indexedMembers + 1, 40} {
		in := manyMembers(n)
		for _, o := range []Options{{}, {AllowDuplicateNames: true}} {
			v, _, err := Decode(in, o)
			if err != nil {
				t.Fatal(err)
			}
			obj := v.(*Object)
			if !indexed(obj) || !indexed(obj.Clone()) {
				t.Fatalf("%d members %+v: index missing after Decode or Clone", n, o)
			}
		}
		// Duplicates resolved last wins keep the index right.
		dup := strings.TrimSuffix(string(in), "}") + `,"k0":"last"}`
		v, _, err := Decode([]byte(dup), Options{AllowDuplicateNames: true})
		if err != nil {
			t.Fatal(err)
		}
		if obj := v.(*Object); !indexed(obj) || obj.Len() != n || obj.At(0).Value != "last" {
			t.Fatalf("%d members with a duplicate: %v", n, obj.At(0))
		}
	}
	// Set grows an index at the ninth member; Delete drops it at eight.
	o := NewObject(0)
	for i := range indexedMembers + 1 {
		o.Set(fmt.Sprint(i), i)
		if !indexed(o) {
			t.Fatalf("Set to %d members", o.Len())
		}
	}
	o.Sort()
	if !indexed(o) {
		t.Fatal("Sort")
	}
	o.DeleteAt(0)
	if !indexed(o) {
		t.Fatal("DeleteAt to 8 members")
	}
	// An object built without its index (inside the package only) still
	// reads correctly and gets one on its next mutation.
	raw := &Object{}
	for i := range 12 {
		raw.members = append(raw.members, Member{fmt.Sprint(i), i})
	}
	if raw.Index("11") != 11 || raw.Index("x") != -1 || raw.index != nil {
		t.Fatal("unindexed lookup")
	}
	raw.Sort()
	if !indexed(raw) {
		t.Fatal("Sort of an unindexed object")
	}
	raw.index = nil
	raw.DeleteAt(0)
	if !indexed(raw) {
		t.Fatal("DeleteAt of an unindexed object")
	}
	raw.index = nil
	if c := raw.Clone(); !indexed(c) {
		t.Fatal("Clone of an unindexed object")
	}
}

func TestObjectConcurrentReads(t *testing.T) {
	// 03 req 25: a decoded body is shared by every reader. Reads of a
	// decoded tree from many goroutines at once are race-free (run with
	// -race), objects of exactly 9 members and objects decoded last wins
	// included.
	inputs := [][]byte{manyMembers(indexedMembers + 1), manyMembers(40), readOrders(t)}
	for _, in := range inputs {
		for _, o := range []Options{{}, {AllowDuplicateNames: true}} {
			v, _, err := Decode(in, o)
			if err != nil {
				t.Fatal(err)
			}
			want, err := Append(nil, v)
			if err != nil {
				t.Fatal(err)
			}
			var wg sync.WaitGroup
			errs := make(chan string, 8)
			for g := range 8 {
				wg.Go(func() {
					obj := v.(*Object)
					for i := range obj.Len() {
						name := obj.At((i + g) % obj.Len()).Name
						if got, ok := obj.Get(name); !ok || !Equal(got, obj.At((i+g)%obj.Len()).Value) || !obj.Has(name) {
							errs <- "Get(" + name + ")"
							return
						}
					}
					if obj.Index("missing") != -1 || !Equal(v, v) || Cost(v) <= 0 {
						errs <- "lookup"
						return
					}
					if got, err := Append(nil, v); err != nil || string(got) != string(want) {
						errs <- "Append"
					}
				})
			}
			wg.Wait()
			close(errs)
			for e := range errs {
				t.Fatalf("%d bytes %+v: concurrent %s failed", len(in), o, e)
			}
		}
	}
}
