// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package metered is a fixture of TestDirectUsesFound: every line marked
// "found" uses a scanning tree.Node, schemaidx or diag method or function,
// or a function or method of a scanning standard library package, in
// every form the detector must see, and the other lines use the same
// names on other types, which the detector must not report.
package metered

import (
	"bytes"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
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
	s.DispatchValues("type")         // found
	s.PolicyConfig("cors")           // found
	x.Lookup("Route", n, nil)        // found
	x.SchemaPath("Route", n, nil)    // found
	_ = x.Walk("Route", n, nil)      // found
	_ = x.Fields()                   // found
	_ = x.FieldsSeq()                // found
	schemaidx.ItemElem(s, n, 0)      // found
	schemaidx.AppendItemJSON(nil, n) // found
	diag.Nearest("a", nil)           // found
}

// packages calls a function or method of each scanning package.
func packages(ss []string, m map[string]int) {
	_ = slices.Contains(ss, "a")   // found
	_ = maps.Clone(m)              // found
	_ = strings.Contains("a", "b") // found
	sort.Strings(ss)               // found
	var b strings.Builder
	b.WriteString("x") // found
	b.Grow(1)
	_ = b.String()
	_ = b.Len()
	_ = regexp.MustCompile("a").MatchString("a") // found
	_ = utf8.ValidString("a")                    // found
	_ = bytes.Equal(nil, nil)                    // found
}

// getter has the Get method of tree.Node, which a *tree.Node satisfies.
type getter interface {
	Get(key string) (*tree.Node, bool)
}

// wrapped embeds a *tree.Node, whose methods it promotes.
type wrapped struct{ *tree.Node }

// indirect uses scanning methods in the forms a spelling check misses.
func indirect(n *tree.Node, v *tree.Node) {
	get := n.Get // found
	_, _ = get("a")
	v.Set("x", tree.Pos{}, nil)     // found
	_, _ = (*tree.Node).Get(n, "a") // found
	w := wrapped{n}
	w.Delete("a") // found
	var g getter = n
	_, _ = g.Get("a")          // found
	elem := schemaidx.ItemElem // found
	_ = elem
}

// allowed calls the same names on types whose methods scan nothing in
// the sense of the detector, and cheap schemaidx methods.
func allowed(v, into reflect.Value, s *schemaidx.Node) {
	v.Set(into)
	_, _ = s.Property("a")
	_, _ = s.Items()
	_ = s.Def()
}
