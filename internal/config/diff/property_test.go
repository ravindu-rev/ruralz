// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package diff

import (
	"bytes"
	"context"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/canonical"
	"github.com/ravindu-rev/ruralz/internal/config/defaults"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/profile"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// FuzzDiffProperties checks the diff properties of the testing strategy on
// random edits of the example Bundle: diff(A, A) is empty; diff(A, B) is
// empty exactly when the Revisions are equal; added and removed identities
// are set differences, mirrored in diff(B, A), as are the effective
// changes; the field operations turn A's canonical form into B's, secrets
// by reference; and reordering map or set lists yields no operation.
// Edits that make an invalid Bundle are skipped: the negative corpus covers
// them.
func FuzzDiffProperties(f *testing.F) {
	for i := range uint8(24) {
		f.Add(uint64(i), i*37)
	}
	f.Fuzz(func(t *testing.T, seed uint64, n uint8) {
		r := rand.New(rand.NewPCG(seed, uint64(n))) //nolint:gosec // G404: a reproducible fuzz input, not a secret.
		files := &tree.FileTable{}
		base := parse(t, files, shop(t, "to"))
		a := mutate(r, base, 1+int(n%4))
		b := mutate(r, a, 1+int(n/4%5))
		va, err := build(files, a)
		if err != nil {
			t.Skip(err)
		}
		vb, err := build(files, b)
		if err != nil {
			t.Skip(err)
		}
		checkProperties(t, va, vb, 1+int(n%3))
		checkReordering(t, r, va, vb)
	})
}

// parse parses sources into resources recorded in files.
func parse(t *testing.T, files *tree.FileTable, srcs []source) []*tree.Resource {
	t.Helper()
	var out []*tree.Resource
	for _, s := range srcs {
		id := files.Add(tree.File{Path: s.path, Role: tree.RoleBase})
		docs, ds, err := profile.Parse(context.Background(), []byte(s.src), id, s.path, profile.FormatYAML, profile.Options{})
		if err != nil || len(ds) > 0 {
			t.Fatalf("parse %s: %v %s", s.path, err, texts(ds))
		}
		for _, d := range docs {
			out = append(out, resource(t, d.Root, d.Start))
		}
	}
	return out
}

// build runs stages F to L on copies of rs, reporting a finding as an error.
func build(files *tree.FileTable, rs []*tree.Resource) (*hub.Validated, error) {
	s, err := testStages()
	if err != nil {
		return nil, err
	}
	cp := clones(rs)
	ctx := context.Background()
	for _, r := range cp {
		if ds, ok := s.views.Validate(r, files); !ok || len(ds) > 0 {
			return nil, fmt.Errorf("stage F: %s", texts(ds))
		}
	}
	bundle, ds, err := s.g.Run(ctx, cp, files, 2)
	if err != nil {
		return nil, fmt.Errorf("stage G: %w", err)
	}
	if ds.HasErrors() {
		return nil, fmt.Errorf("stage G: %s", texts(ds))
	}
	chains, ds, err := s.i.Run(ctx, bundle, files, 2)
	if err != nil {
		return nil, fmt.Errorf("stage I: %w", err)
	}
	if ds.HasErrors() {
		return nil, fmt.Errorf("stage I: %s", texts(ds))
	}
	rev, err := canonical.NewEncoder(s.idx).Encode(ctx, bundle, canonical.Options{})
	if err != nil {
		return nil, err
	}
	return &hub.Validated{Bundle: bundle, Chains: chains, Revision: rev}, nil
}

// clones deep-copies resources.
func clones(rs []*tree.Resource) []*tree.Resource {
	out := make([]*tree.Resource, len(rs))
	for i, r := range rs {
		cp := *r
		cp.Root = r.Root.Clone()
		out[i] = &cp
	}
	return out
}

// pick returns a random element of xs.
func pick[T any](r *rand.Rand, xs []T) T { return xs[r.IntN(len(xs))] }

// str returns a string node.
func str(s string) *tree.Node { return &tree.Node{Kind: tree.KindString, Text: s} }

// at returns the node at the field path below a resource root, or nil.
func at(root *tree.Node, fields ...string) *tree.Node {
	n := root
	for _, f := range fields {
		var ok bool
		if n, ok = n.Get(f); !ok {
			return nil
		}
	}
	return n
}

// ofKind returns the resources of kind k.
func ofKind(rs []*tree.Resource, k v1alpha1.Kind) []*tree.Resource {
	var out []*tree.Resource
	for _, r := range rs {
		if r.ID.Kind == k {
			out = append(out, r)
		}
	}
	return out
}

// named returns the resource id, or nil.
func named(rs []*tree.Resource, k v1alpha1.Kind, name string) *tree.Resource {
	for _, r := range rs {
		if r.ID.Kind == k && r.ID.Name == name {
			return r
		}
	}
	return nil
}

// mutate returns a copy of rs with count random edits applied.
func mutate(r *rand.Rand, rs []*tree.Resource, count int) []*tree.Resource {
	out := clones(rs)
	edits := edits()
	for range count {
		out = pick(r, edits)(r, out)
	}
	return out
}

// edit is one random change to a parsed Bundle.
type editFunc func(r *rand.Rand, rs []*tree.Resource) []*tree.Resource

// edits returns the random changes FuzzDiffProperties draws from.
func edits() []editFunc {
	durations := []string{"1s", "2s", "3s", "250ms", "1m", "90s"}
	return []editFunc{
		func(r *rand.Rand, rs []*tree.Resource) []*tree.Resource { // Route timeout
			route := pick(r, ofKind(rs, v1alpha1.KindRoute))
			at(route.Root, "spec").Set("timeout", tree.Pos{}, str(pick(r, durations)))
			return rs
		},
		func(r *rand.Rand, rs []*tree.Resource) []*tree.Resource { // labels, keys that need quoting included
			res := pick(r, rs)
			meta := at(res.Root, "metadata")
			labels, ok := meta.Get("labels")
			if !ok {
				labels = &tree.Node{Kind: tree.KindMap}
				meta.Set("labels", tree.Pos{}, labels)
			}
			key := pick(r, []string{"team", "tier", "shop.example/owner", "cost-center"})
			if r.IntN(3) == 0 {
				labels.Delete(key)
			} else {
				labels.Set(key, tree.Pos{}, str(pick(r, []string{"a", "platform", "commerce", "x-1"})))
			}
			return rs
		},
		func(r *rand.Rand, rs []*tree.Resource) []*tree.Resource { // orderedMap order
			m := named(rs, v1alpha1.KindAIModel, "support-chat")
			c := at(m.Root, "spec", "candidates")
			r.Shuffle(len(c.Items), func(i, j int) { c.Items[i], c.Items[j] = c.Items[j], c.Items[i] })
			return rs
		},
		func(r *rand.Rand, rs []*tree.Resource) []*tree.Resource { // attach a Policy
			route := pick(r, ofKind(rs, v1alpha1.KindRoute))
			spec := at(route.Root, "spec")
			ps, ok := spec.Get("policies")
			if !ok {
				ps = &tree.Node{Kind: tree.KindList}
				spec.Set("policies", tree.Pos{}, ps)
			}
			name := pick(r, []string{"headers-internal", "authz-orders", "ratelimit-orders", "cors-partner"})
			if slices.ContainsFunc(ps.Items, func(n *tree.Node) bool { k, _ := n.Get("name"); return k.Text == name }) {
				return rs
			}
			i := r.IntN(len(ps.Items) + 1)
			ref := &tree.Node{Kind: tree.KindMap, Members: []tree.Member{{Key: "name", Value: str(name)}}}
			ps.Items = slices.Insert(ps.Items, i, ref)
			return rs
		},
		func(r *rand.Rand, rs []*tree.Resource) []*tree.Resource { // detach a Policy
			holders := append(ofKind(rs, v1alpha1.KindRoute), ofKind(rs, v1alpha1.KindUpstream)...)
			holder := pick(r, holders)
			ps := at(holder.Root, "spec", "policies")
			if ps == nil || len(ps.Items) == 0 {
				return rs
			}
			i := r.IntN(len(ps.Items))
			ps.Items = slices.Delete(ps.Items, i, i+1)
			return rs
		},
		func(r *rand.Rand, rs []*tree.Resource) []*tree.Resource { // add or remove a Policy
			if r.IntN(2) == 0 {
				return slices.DeleteFunc(rs, func(x *tree.Resource) bool {
					return x.ID == tree.ID{Kind: v1alpha1.KindPolicy, Name: "semantic-cache-support"}
				})
			}
			name := fmt.Sprintf("rl-%d", r.IntN(4))
			if named(rs, v1alpha1.KindPolicy, name) != nil {
				return rs
			}
			src := fmt.Sprintf("apiVersion: ruralz/v1alpha1\nkind: Policy\nmetadata: {name: %s}\nspec:\n  type: ratelimit\n  config: {key: source.ip, limits: [{requests: %d, window: 1s}]}\n", name, 1+r.IntN(100))
			docs, _, _ := profile.Parse(context.Background(), []byte(src), 0, "x.yaml", profile.FormatYAML, profile.Options{})
			return append(rs, &tree.Resource{ID: tree.ID{Kind: v1alpha1.KindPolicy, Name: name}, APIVersion: v1alpha1.APIVersion, Root: docs[0].Root})
		},
		func(r *rand.Rand, rs []*tree.Resource) []*tree.Resource { // secret reference
			gw := ofKind(rs, v1alpha1.KindGateway)[0]
			ls := at(gw.Root, "spec", "listeners")
			for _, l := range ls.Items {
				if key := at(l, "tls", "certificates"); key != nil && len(key.Items) > 0 {
					at(key.Items[0], "privateKey", "secretRef").Set("name", tree.Pos{}, str(pick(r, []string{"/etc/ruralz/tls/a.key", "/etc/ruralz/tls/b.key"})))
				}
			}
			return rs
		},
		func(r *rand.Rand, rs []*tree.Resource) []*tree.Resource { // Capability set
			p := named(rs, v1alpha1.KindPlugin, "geo-block")
			caps := at(p.Root, "spec", "capabilities")
			c := pick(r, []string{"log.write", "response.send", "request.body.read", "clock.read"})
			if i := slices.IndexFunc(caps.Items, func(n *tree.Node) bool { return n.Text == c }); i >= 0 {
				caps.Items = slices.Delete(caps.Items, i, i+1)
			} else {
				caps.Items = append(caps.Items, str(c))
			}
			return rs
		},
		func(r *rand.Rand, rs []*tree.Resource) []*tree.Resource { // Filter class of a plugin Policy
			p := named(rs, v1alpha1.KindPolicy, "geo-block-default")
			at(p.Root, "spec").Set("filterClass", tree.Pos{}, str(pick(r, []string{"authz", "custom", "transform", "validation"})))
			return rs
		},
		func(r *rand.Rand, rs []*tree.Resource) []*tree.Resource { // atomic list
			p := named(rs, v1alpha1.KindPolicy, "ratelimit-global")
			l := at(p.Root, "spec", "config", "limits")
			l.Items[0].Set("requests", tree.Pos{}, &tree.Node{Kind: tree.KindInt, Text: fmt.Sprint(1 + r.IntN(5000))})
			return rs
		},
		func(r *rand.Rand, rs []*tree.Resource) []*tree.Resource { // map list entries
			u := named(rs, v1alpha1.KindUpstream, "inventory")
			eps := at(u.Root, "spec", "endpoints")
			addr := pick(r, []string{"inventory-b.shop.svc:8080", "inventory-c.shop.svc:8080", "inventory.shop.svc:8080"})
			if i := slices.IndexFunc(eps.Items, func(n *tree.Node) bool { return at(n, "address").Text == addr }); i >= 0 && len(eps.Items) > 1 {
				eps.Items = slices.Delete(eps.Items, i, i+1)
			} else if i < 0 {
				eps.Items = slices.Insert(eps.Items, r.IntN(len(eps.Items)+1), &tree.Node{Kind: tree.KindMap, Members: []tree.Member{{Key: "address", Value: str(addr)}}})
			}
			return rs
		},
		func(r *rand.Rand, rs []*tree.Resource) []*tree.Resource { // rename an Upstream
			u := named(rs, v1alpha1.KindUpstream, "cart")
			route := named(rs, v1alpha1.KindRoute, "cart-grpc")
			if u == nil {
				return rs
			}
			name := fmt.Sprintf("cart-%d", r.IntN(3))
			at(u.Root, "metadata").Set("name", tree.Pos{}, str(name))
			u.ID.Name = name
			at(route.Root, "spec", "upstreams").Items[0].Set("name", tree.Pos{}, str(name))
			return rs
		},
		func(r *rand.Rand, rs []*tree.Resource) []*tree.Resource { // listeners
			gw := ofKind(rs, v1alpha1.KindGateway)[0]
			ls := at(gw.Root, "spec", "listeners")
			port := 9000 + r.IntN(3)
			name := fmt.Sprintf("l%d", port)
			if i := slices.IndexFunc(ls.Items, func(n *tree.Node) bool { return at(n, "name").Text == name }); i >= 0 {
				ls.Items = slices.Delete(ls.Items, i, i+1)
				return rs
			}
			ls.Items = append(ls.Items, &tree.Node{Kind: tree.KindMap, Members: []tree.Member{
				{Key: "name", Value: str(name)},
				{Key: "protocol", Value: str("http")},
				{Key: "port", Value: &tree.Node{Kind: tree.KindInt, Text: fmt.Sprint(port)}},
			}})
			return rs
		},
		func(r *rand.Rand, rs []*tree.Resource) []*tree.Resource { // set order only
			route := pick(r, ofKind(rs, v1alpha1.KindRoute))
			if m := at(route.Root, "spec", "match", "methods"); m != nil {
				m.Items = append(m.Items, str("HEAD"))
				slices.Reverse(m.Items)
			}
			return rs
		},
	}
}

// identities returns the identities of v's resources.
func identities(v *hub.Validated) map[hub.ID]bool {
	out := map[hub.ID]bool{}
	for _, r := range v.Bundle.Resources() {
		out[r.ID] = true
	}
	return out
}

// changed returns the identities d reports with change c.
func changed(d *Diff, c Change) map[hub.ID]bool {
	out := map[hub.ID]bool{}
	for _, r := range d.Resources {
		if r.Change == c {
			out[r.ID] = true
		}
	}
	return out
}

// minus returns the members of a not in b.
func minus(a, b map[hub.ID]bool) map[hub.ID]bool {
	out := map[hub.ID]bool{}
	for id := range a {
		if !b[id] {
			out[id] = true
		}
	}
	return out
}

// sameSet compares identity sets.
func sameSet(a, b map[hub.ID]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for id := range a {
		if !b[id] {
			return false
		}
	}
	return true
}

// checkProperties checks the properties of diff(A, B) and diff(B, A).
func checkProperties(t *testing.T, a, b *hub.Validated, workers int) {
	t.Helper()
	e := engine(t, workers)
	diffOf := func(x, y *hub.Validated) *Diff {
		d, err := e.Compare(context.Background(), Side{State: x}, Side{State: y})
		if err != nil {
			t.Fatalf("Compare() error = %v", err)
		}
		return d
	}
	for _, s := range []*hub.Validated{a, b} {
		if d := diffOf(s, s); d.Changed() {
			t.Fatalf("diff(A, A) changed:\n%s", d.AppendText(nil))
		}
	}
	ab, ba := diffOf(a, b), diffOf(b, a)
	equal := a.Revision.Digest == b.Revision.Digest
	if ab.Changed() == equal || ba.Changed() == equal {
		t.Fatalf("equal Revisions %v, but diff changed %v / %v:\n%s", equal, ab.Changed(), ba.Changed(), ab.AppendText(nil))
	}
	ia, ib := identities(a), identities(b)
	switch {
	case !sameSet(changed(ab, Added), minus(ib, ia)), !sameSet(changed(ab, Removed), minus(ia, ib)):
		t.Fatalf("added and removed are not the set differences:\n%s", ab.AppendText(nil))
	case !sameSet(changed(ba, Added), changed(ab, Removed)), !sameSet(changed(ba, Removed), changed(ab, Added)), !sameSet(changed(ba, Modified), changed(ab, Modified)):
		t.Fatalf("diff(B, A) does not mirror diff(A, B):\n%s\n%s", ab.AppendText(nil), ba.AppendText(nil))
	}
	checkEffectiveMirror(t, ab, ba)
	idx := hubIndex(t)
	for _, ra := range a.Bundle.Resources() {
		rb, ok := b.Bundle.Get(ra.ID)
		if !ok {
			continue
		}
		want := encodeOne(t, idx, rb.ID, rb.Tree)
		got := encodeOne(t, idx, ra.ID, ra.Tree)
		if r := resourceOf(ab, ra.String()); r != nil {
			got = encodeOne(t, idx, ra.ID, apply(t, idx, ra, r.Ops))
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%s: the operations do not turn A into B\n got %s\nwant %s\n%s", ra.ID, got, want, ab.AppendText(nil))
		}
	}
}

// chainKey names an effective change without its op and neighbor.
func chainKey(c ChainChange) string {
	return fmt.Sprintf("%s %s %s %s", c.Route, c.Phase, c.Leg, c.Policy)
}

// checkEffectiveMirror: the Policies diff(A, B) adds are those diff(B, A)
// removes and the other way round, and both move as many.
func checkEffectiveMirror(t *testing.T, ab, ba *Diff) {
	t.Helper()
	keys := func(d *Diff, op OpKind) []string {
		var out []string
		for _, c := range d.Effective {
			if c.Op == op {
				out = append(out, chainKey(c))
			}
		}
		slices.Sort(out)
		return out
	}
	if !slices.Equal(keys(ab, OpAdd), keys(ba, OpRemove)) || !slices.Equal(keys(ab, OpRemove), keys(ba, OpAdd)) || len(keys(ab, OpMove)) != len(keys(ba, OpMove)) {
		t.Fatalf("effective changes are not mirrored:\n%s\n%s", ab.AppendText(nil), ba.AppendText(nil))
	}
}

// encodeOne returns the canonical document of one resource.
func encodeOne(t *testing.T, idx *schemaidx.Index, id hub.ID, root *tree.Node) []byte {
	t.Helper()
	rev, err := canonical.NewEncoder(idx).EncodeResources(context.Background(), []tree.Resource{{ID: id, Root: root}}, canonical.Options{})
	if err != nil {
		t.Fatalf("encode %s: %v", id, err)
	}
	return rev.Content
}

// keyedList records an orderedMap or map list the operations touch: the
// positions its entries had, the moves and the entries added, in order.
type keyedList struct {
	list    *tree.Node
	field   string
	ordered bool
	orig    map[string]int
	moved   map[string]int
	added   []*tree.Node
}

// apply applies ops to a copy of r's tree the way the package
// documentation states, then normalizes it as stage G does: a path
// resolves by key; an orderedMap entry moves to its position, one neither
// moved nor added keeps its position and added entries fill the free ones
// in order; map entries and set elements take their sorted place.
func apply(t *testing.T, idx *schemaidx.Index, r *hub.Resource, ops []Op) *tree.Node {
	t.Helper()
	out := r.Tree.Clone()
	lists := map[*tree.Node]*keyedList{}
	var order []*keyedList
	touch := func(list *tree.Node, field string, p diag.Path) *keyedList {
		if k, ok := lists[list]; ok {
			return k
		}
		info, _ := idx.Lookup(string(r.Kind), out, p)
		lt, _ := info.Node.List()
		k := &keyedList{list: list, field: field, ordered: lt == schemaidx.ListOrderedMap, orig: map[string]int{}, moved: map[string]int{}}
		for i, it := range list.Items {
			k.orig[at(it, field).Text] = i
		}
		lists[list] = k
		order = append(order, k)
		return k
	}
	for _, op := range ops {
		parentPath, last := op.Path[:len(op.Path)-1], op.Path[len(op.Path)-1]
		parent, ok := out.At(parentPath)
		if !ok {
			t.Fatalf("%s: no parent at %s for %s", r.ID, parentPath, op.Kind)
		}
		switch last.Kind {
		case diag.ElemField:
			if op.Kind == OpRemove {
				parent.Delete(last.Name)
			} else {
				parent.Set(last.Name, tree.Pos{}, op.To.Clone())
			}
		case diag.ElemKeyed:
			k := touch(parent, last.KeyField, parentPath)
			i := slices.IndexFunc(parent.Items, func(n *tree.Node) bool { return at(n, last.KeyField).Text == last.Key })
			switch op.Kind {
			case OpRemove:
				parent.Items = slices.Delete(parent.Items, i, i+1)
			case OpAdd:
				v := op.To.Clone()
				parent.Items = append(parent.Items, v)
				k.added = append(k.added, v)
			case OpReplace:
				parent.Items[i] = op.To.Clone()
			case OpMove:
				k.moved[last.Key] = op.ToIndex
			}
		case diag.ElemItem:
			if op.Kind == OpAdd {
				parent.Items = append(parent.Items, op.To.Clone())
				continue
			}
			want := itemJSON(last.Item)
			i := slices.IndexFunc(parent.Items, func(n *tree.Node) bool { return string(schemaidx.AppendItemJSON(nil, n)) == want })
			if i < 0 {
				t.Fatalf("%s: no set element %s", r.ID, want)
			}
			parent.Items = slices.Delete(parent.Items, i, i+1)
		default:
			t.Fatalf("%s: operation at a list index %s", r.ID, op.Path)
		}
	}
	for _, k := range order {
		if !k.ordered {
			continue
		}
		slots := make([]*tree.Node, len(k.list.Items))
		for _, it := range k.list.Items {
			if slices.Contains(k.added, it) {
				continue
			}
			key := at(it, k.field).Text
			pos, ok := k.moved[key]
			if !ok {
				pos = k.orig[key]
			}
			if pos >= len(slots) || slots[pos] != nil {
				t.Fatalf("%s: entry %s cannot take position %d", r.ID, key, pos)
			}
			slots[pos] = it
		}
		next := 0
		for _, v := range k.added {
			for slots[next] != nil {
				next++
			}
			slots[next] = v
		}
		k.list.Items = slots
	}
	res := &tree.Resource{ID: r.ID, APIVersion: v1alpha1.APIVersion, Root: out}
	if ds := defaults.Normalize(idx, res, nil); len(ds) > 0 {
		t.Fatalf("%s: normalize: %s", r.ID, texts(ds))
	}
	return res.Root
}

// itemJSON returns the RFC 8785 text of a set element path value.
func itemJSON(v any) string {
	var n *tree.Node
	switch x := v.(type) {
	case string:
		n = str(x)
	case bool:
		n = &tree.Node{Kind: tree.KindBool, Bool: x}
	case fmt.Stringer:
		n = &tree.Node{Kind: tree.KindFloat, Text: x.String()}
	default:
		return fmt.Sprint(v)
	}
	return string(schemaidx.AppendItemJSON(nil, n))
}

// checkReordering: reordering B's map and set lists, which the canonical
// form sorts, yields no operation against B and the same operations
// against A.
func checkReordering(t *testing.T, r *rand.Rand, a, b *hub.Validated) {
	t.Helper()
	idx := hubIndex(t)
	var rs []*hub.Resource
	for _, res := range b.Bundle.Resources() {
		cp := *res
		cp.Tree = res.Tree.Clone()
		err := idx.Walk(string(cp.Kind), cp.Tree, func(c *schemaidx.Cursor) error {
			if c.Node.Kind != tree.KindList || c.Info.Node == nil {
				return nil
			}
			if lt, _ := c.Info.Node.List(); lt == schemaidx.ListMap || lt == schemaidx.ListSet {
				r.Shuffle(len(c.Node.Items), func(i, j int) { c.Node.Items[i], c.Node.Items[j] = c.Node.Items[j], c.Node.Items[i] })
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		rs = append(rs, &cp)
	}
	bundle := hub.NewBundle(rs)
	rev, err := canonical.NewEncoder(idx).Encode(context.Background(), bundle, canonical.Options{})
	if err != nil {
		t.Fatal(err)
	}
	shuffled := &hub.Validated{Bundle: bundle, Chains: b.Chains, Revision: rev}
	if d := compare(t, b, shuffled); d.Changed() {
		t.Fatalf("reordering map and set lists changed the diff:\n%s", d.AppendText(nil))
	}
	lines := func(d *Diff) []string {
		var out []string
		for _, res := range d.Resources {
			for i := range res.Ops {
				out = append(out, res.ID.String()+" "+strings.TrimSpace(string(appendOp(nil, &res.Ops[i]))))
			}
			out = append(out, res.Change.String()+" "+res.ID.String())
		}
		slices.Sort(out)
		return out
	}
	if a.Revision.Digest == b.Revision.Digest {
		return
	}
	if got, want := lines(compare(t, a, shuffled)), lines(compare(t, a, b)); !slices.Equal(got, want) {
		t.Fatalf("reordering B's lists changed diff(A, B):\n got %q\nwant %q", got, want)
	}
}
