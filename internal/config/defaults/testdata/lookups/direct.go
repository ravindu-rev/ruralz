// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package lookups is a fixture of TestDirectLookupsFound: every line
// marked "found" uses a scanning tree.Node or schemaidx method or
// function, in every form the detector must see, and the other lines use
// the same names on other types, which the detector must not report.
package lookups

import (
	"reflect"

	"github.com/ravindu-rev/ruralz/internal/config/registry"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// direct calls each scanning method and function once.
func direct(n *tree.Node, s *schemaidx.Node, x *schemaidx.Index) {
	n.Get("a")                       // found
	n.Set("a", tree.Pos{}, nil)      // found
	n.Delete("a")                    // found
	n.At(nil)                        // found
	n.Clone()                        // found
	n.JSONValue()                    // found
	s.Select(n)                      // found
	s.SelectValue("a", "b")          // found
	x.Lookup("Route", n, nil)        // found
	x.SchemaPath("Route", n, nil)    // found
	_ = x.Walk("Route", n, nil)      // found
	schemaidx.ItemElem(s, n, 0)      // found
	schemaidx.AppendItemJSON(nil, n) // found
}

// getter has the Get method of tree.Node, which a *tree.Node satisfies.
type getter interface {
	Get(key string) (*tree.Node, bool)
}

// wrapped embeds a *tree.Node, whose methods it promotes.
type wrapped struct{ *tree.Node }

// indirect uses scanning methods in the forms a spelling check misses.
func indirect(n *tree.Node, v *tree.Node, into *tree.Node) {
	get := n.Get // found
	_, _ = get("a")
	v.Set("x", tree.Pos{}, nil)     // found
	into.Set("x", tree.Pos{}, nil)  // found
	_, _ = (*tree.Node).Get(n, "a") // found
	w := wrapped{n}
	w.Delete("a") // found
	var g getter = n
	_, _ = g.Get("a")          // found
	elem := schemaidx.ItemElem // found
	_ = elem
}

// allowed calls the same names on types whose methods scan nothing.
func allowed(v, into reflect.Value, sf, f reflect.StructField, reg *registry.Registry) {
	v.Set(into)
	into.Set(v)
	_ = sf.Tag.Get("json")
	_, _ = f.Tag.Lookup("json")
	_, _ = reg.Lookup("ratelimit")
}
