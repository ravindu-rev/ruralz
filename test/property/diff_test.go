// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package property

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/cli"
	"github.com/ravindu-rev/ruralz/internal/config/canonical"
	"github.com/ravindu-rev/ruralz/internal/config/defaults"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/profile"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// diffDoc is a ruralz.diff.v1 document (docs/reference/01-cli-and-api-
// surface.md "Diff JSON compatibility").
type diffDoc struct {
	Format    string         `json:"format"`
	From      diffSide       `json:"from"`
	To        diffSide       `json:"to"`
	Resources []diffResource `json:"resources"`
	Effective []diffChange   `json:"effective"`
}

// diffSide is the from or to member.
type diffSide struct {
	Revision string `json:"revision"`
	Digest   string `json:"digest"`
}

// diffResource is one changed resource.
type diffResource struct {
	Kind   string   `json:"kind"`
	Name   string   `json:"name"`
	Change string   `json:"change"`
	Ops    []diffOp `json:"ops"`
}

// diffOp is one field operation.
type diffOp struct {
	Op   string            `json:"op"`
	Path []json.RawMessage `json:"path"`
	From json.RawMessage   `json:"from"`
	To   json.RawMessage   `json:"to"`
}

// diffChange is one effective Filter Chain change.
type diffChange struct {
	Route       string `json:"route"`
	Phase       string `json:"phase"`
	Leg         string `json:"leg"`
	Op          string `json:"op"`
	Policy      string `json:"policy"`
	FilterClass string `json:"filterClass"`
}

// FuzzDiff is the Diff row of the Required properties: diff(A, A) is
// empty, exit 0; diff(A, B) is empty exactly when Revisions are equal;
// added and removed identities are set differences, mirrored in diff(B,
// A); field ops turn A's canonical form into B's (SecretValue by
// reference); reordering map or set lists yields no op
// (docs/architecture/02-configuration-model.md "Diff semantics"). A random
// valid Bundle A and B, A with random edits (attachments, Policies added
// and removed, a Route renamed, timeouts, labels, endpoints, methods, a
// limit, a secretRef), are written to directories, and ruralz bundle diff
// --output json runs on them in process: diff(A, A) and diff(A, A') for A'
// with every map and set list and mapping of A shuffled exit 0 without a
// resource or chain change; diff(A, B) and diff(B, A) exit 0 exactly when
// their digests are equal, which are the Revisions the pipeline builds,
// and 1 otherwise; their added and removed resources are the set
// differences of the two Revisions' identities, mirrored, as are their
// modified resources and chain changes; applying diff(A, B)'s operations
// to each modified resource of A's canonical form, as the configuration
// model states, gives B's canonical bytes; and every value an operation
// shows at a secret position is a reference.
func FuzzDiff(f *testing.F) {
	for i := range uint8(12) {
		f.Add(uint64(i)*0x51ed270b+3, i)
	}
	f.Fuzz(func(t *testing.T, seed uint64, edits uint8) {
		tc := tools(t)
		r := newRand(seed)
		a := generate(r, genOptions{mode: genValid})
		b := mutate(r, a, 1+int(edits%6))
		root := t.TempDir()
		dirA, dirB, dirR := filepath.Join(root, "a"), filepath.Join(root, "b"), filepath.Join(root, "reordered")
		filesA, filesB := a.files("/etc/ruralz/oauth/client-secret"), b.files("/etc/ruralz/oauth/client-secret")
		writeTree(t, dirA, filesA)
		writeTree(t, dirB, filesB)
		var va, vb *hub.Validated
		for _, side := range []struct {
			files map[string][]byte
			v     **hub.Validated
		}{{filesA, &va}, {filesB, &vb}} {
			res := tc.run(t, memBundle{}.withFiles(side.files), noEnv, nil, false)
			if res.Validated == nil || res.HasErrors() {
				t.Fatalf("a generated Bundle does not build:\n%s\n%s", texts(res.Diagnostics), dumpFiles(side.files))
			}
			*side.v = res.Validated
		}
		src, ok := tc.parseSource(t, memBundle{}.withFiles(filesA))
		if !ok {
			t.Fatal("the generated Bundle does not parse")
		}
		writeTree(t, dirR, reshape(t, tc, r, src, false))

		for _, same := range []string{dirA, dirR} {
			d, code := runDiff(t, dirA, same)
			if code != cli.ExitOK || len(d.Resources) > 0 || len(d.Effective) > 0 || d.From.Digest != d.To.Digest {
				t.Fatalf("diff(A, %s) exits %d with %d resources and %d chain changes, %s -> %s", filepath.Base(same), code,
					len(d.Resources), len(d.Effective), d.From.Digest, d.To.Digest)
			}
		}
		ab, codeAB := runDiff(t, dirA, dirB)
		ba, codeBA := runDiff(t, dirB, dirA)
		equal := va.Revision.Digest == vb.Revision.Digest
		for _, c := range []struct {
			name string
			d    diffDoc
			code int
			from string
			to   string
		}{
			{"diff(A, B)", ab, codeAB, va.Revision.Digest.String(), vb.Revision.Digest.String()},
			{"diff(B, A)", ba, codeBA, vb.Revision.Digest.String(), va.Revision.Digest.String()},
		} {
			if c.d.From.Digest != c.from || c.d.To.Digest != c.to {
				t.Fatalf("%s compares %s -> %s, the Revisions are %s -> %s", c.name, c.d.From.Digest, c.d.To.Digest, c.from, c.to)
			}
			wantCode := cli.ExitNegative
			if equal {
				wantCode = cli.ExitOK
			}
			if c.code != wantCode || (len(c.d.Resources) == 0) != equal {
				t.Fatalf("%s exits %d with %d resources, the Revisions are equal: %v", c.name, c.code, len(c.d.Resources), equal)
			}
		}
		idsA, idsB := identities(va), identities(vb)
		checkSet(t, "diff(A, B) added", changed(ab, "added"), minus(idsB, idsA))
		checkSet(t, "diff(A, B) removed", changed(ab, "removed"), minus(idsA, idsB))
		checkSet(t, "diff(B, A) added", changed(ba, "added"), minus(idsA, idsB))
		checkSet(t, "diff(B, A) removed", changed(ba, "removed"), minus(idsB, idsA))
		checkSet(t, "diff(B, A) modified", changed(ba, "modified"), changed(ab, "modified"))
		checkMirror(t, ab, ba)
		checkOps(t, tc, ab, va, vb)
	})
}

// runDiff runs ruralz bundle diff FROM TO --output json in process and
// returns its document and exit code; any code but 0 and 1 fails t.
func runDiff(t *testing.T, from, to string) (diffDoc, int) {
	t.Helper()
	stdout, stderr, code := ruralz(t.Context(), nil, "bundle", "diff", "--output", "json", from, to)
	if code != cli.ExitOK && code != cli.ExitNegative {
		t.Fatalf("ruralz bundle diff %s %s exits %d: %s", from, to, code, stderr)
	}
	var d diffDoc
	if err := json.Unmarshal([]byte(stdout), &d); err != nil || d.Format != "ruralz.diff.v1" {
		t.Fatalf("ruralz bundle diff prints no ruralz.diff.v1 document: %v\n%s", err, stdout)
	}
	return d, code
}

// mutate returns a copy of g with n random edits, repaired to stay valid.
func mutate(r *rand.Rand, g *genBundle, n int) *genBundle {
	out := g.clone()
	for range n {
		pick(r, edits())(r, out)
	}
	out.repair()
	return out
}

// edits returns the random edits mutate draws from.
func edits() []func(r *rand.Rand, g *genBundle) {
	return []func(r *rand.Rand, g *genBundle){
		func(r *rand.Rand, g *genBundle) { // a Route timeout
			rt := pick(r, g.routes)
			if _, ok := g.timeout[rt.name]; ok && r.IntN(2) == 0 {
				delete(g.timeout, rt.name)
				return
			}
			g.timeout[rt.name] = pick(r, []string{"1s", "3s", "250ms", "90s"})
		},
		func(r *rand.Rand, g *genBundle) { // a label, metadata only
			key := pick(r, []string{"Gateway/edge", "Consumer/c0"})
			if r.IntN(2) == 0 && len(g.policies) > 0 {
				key = "Policy/" + pick(r, g.policies).name
			}
			if _, ok := g.labels[key]; ok && r.IntN(2) == 0 {
				delete(g.labels, key)
				return
			}
			g.labels[key] = pick(r, []string{"a", "platform", "x-1", "needs quotes: yes"})
		},
		func(r *rand.Rand, g *genBundle) { // attach a Policy to a Route
			rt := pick(r, g.routes)
			name := pick(r, g.policies).name
			if !slices.Contains(rt.policies, name) && g.policy(name).kind.scopes&scopesR != 0 {
				rt.policies = slices.Insert(rt.policies, r.IntN(len(rt.policies)+1), name)
			}
		},
		func(r *rand.Rand, g *genBundle) { // detach a Policy
			lists := []*[]string{&g.gateway}
			for _, rt := range g.routes {
				lists = append(lists, &rt.policies)
			}
			for _, u := range g.upstreams {
				lists = append(lists, &u.policies)
			}
			l := pick(r, lists)
			if len(*l) > 0 {
				i := r.IntN(len(*l))
				*l = slices.Delete(*l, i, i+1)
			}
		},
		func(r *rand.Rand, g *genBundle) { // reorder the Gateway's orderedMap
			r.Shuffle(len(g.gateway), func(i, j int) { g.gateway[i], g.gateway[j] = g.gateway[j], g.gateway[i] })
		},
		func(r *rand.Rand, g *genBundle) { // add a Policy, attached to the Gateway or not
			name := g.addPolicy(kindAt(r, phase.ScopeGateway), "")
			if r.IntN(2) == 0 {
				g.gateway = append(g.gateway, name)
			}
		},
		func(r *rand.Rand, g *genBundle) { // remove an unattached Policy
			for _, i := range r.Perm(len(g.policies)) {
				if p := g.policies[i]; !g.attached(p.name) {
					g.policies = slices.Delete(g.policies, i, i+1)
					return
				}
			}
		},
		func(r *rand.Rand, g *genBundle) { // a set list: methods
			rt := pick(r, g.routes)
			switch {
			case rt.methods == nil:
				rt.methods = []string{"GET"}
			case r.IntN(2) == 0:
				if !slices.Contains(rt.methods, "HEAD") {
					rt.methods = append(rt.methods, "HEAD")
				}
				slices.Reverse(rt.methods)
			default:
				rt.methods = nil
			}
		},
		func(r *rand.Rand, g *genBundle) { // a map list: endpoints
			u := pick(r, g.upstreams)
			port := 9100 + r.IntN(3)
			if i := slices.Index(u.ports, port); i >= 0 && len(u.ports) > 1 {
				u.ports = slices.Delete(u.ports, i, i+1)
			} else if i < 0 {
				u.ports = slices.Insert(u.ports, r.IntN(len(u.ports)+1), port)
			}
		},
		func(r *rand.Rand, g *genBundle) { // an atomic list: a limit
			for _, p := range g.policies {
				if p.kind.typ == v1alpha1.PolicyTypeRateLimit {
					g.requests[p.name] = 1 + r.IntN(500)
					return
				}
			}
		},
		func(r *rand.Rand, g *genBundle) { // a secretRef
			for _, p := range g.policies {
				if p.kind.typ == v1alpha1.PolicyTypeAuthUpstreamOAuth2 {
					g.secrets[p.name] = pick(r, []string{"/etc/ruralz/oauth/a", "/etc/ruralz/oauth/b"})
					return
				}
			}
		},
		func(r *rand.Rand, g *genBundle) { // rename a Route: a removal and an addition
			rt := pick(r, g.routes)
			old := rt.name
			rt.name += "x" + strconv.Itoa(r.IntN(3))
			if t, ok := g.timeout[old]; ok {
				delete(g.timeout, old)
				g.timeout[rt.name] = t
			}
		},
		func(r *rand.Rand, g *genBundle) { // exclude a Gateway Policy
			if len(g.gateway) > 0 {
				rt := pick(r, g.routes)
				rt.exclude = append(rt.exclude, pick(r, g.gateway))
				rt.exclude = slices.Compact(slices.Sorted(slices.Values(rt.exclude)))
			}
		},
	}
}

// attached reports whether some list attaches the Policy name.
func (g *genBundle) attached(name string) bool {
	if slices.Contains(g.gateway, name) {
		return true
	}
	for _, rt := range g.routes {
		if slices.Contains(rt.policies, name) || slices.Contains(rt.exclude, name) {
			return true
		}
	}
	for _, u := range g.upstreams {
		if slices.Contains(u.policies, name) {
			return true
		}
	}
	return false
}

// identities returns the resources of a Revision as "Kind/name".
func identities(v *hub.Validated) map[string]bool {
	out := map[string]bool{}
	for _, r := range v.Bundle.Resources() {
		out[r.String()] = true
	}
	return out
}

// changed returns the resources d reports with change c as "Kind/name".
func changed(d diffDoc, c string) map[string]bool {
	out := map[string]bool{}
	for _, r := range d.Resources {
		if r.Change == c {
			out[r.Kind+"/"+r.Name] = true
		}
	}
	return out
}

// minus returns the members of a not in b.
func minus(a, b map[string]bool) map[string]bool {
	out := map[string]bool{}
	for k := range a {
		if !b[k] {
			out[k] = true
		}
	}
	return out
}

// checkSet fails t unless got and want hold the same members.
func checkSet(t *testing.T, what string, got, want map[string]bool) {
	t.Helper()
	if g, w := slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(want)); !slices.Equal(g, w) {
		t.Fatalf("%s: %q, want %q", what, g, w)
	}
}

// checkMirror fails t unless the chain changes of diff(B, A) mirror those
// of diff(A, B): what one adds the other removes, and both move as many.
func checkMirror(t *testing.T, ab, ba diffDoc) {
	t.Helper()
	keys := func(d diffDoc, op string) []string {
		var out []string
		for _, c := range d.Effective {
			if c.Op == op {
				out = append(out, fmt.Sprintf("%s %s %s %s %s", c.Route, c.Phase, c.Leg, c.Policy, c.FilterClass))
			}
		}
		slices.Sort(out)
		return out
	}
	if !slices.Equal(keys(ab, "add"), keys(ba, "remove")) || !slices.Equal(keys(ab, "remove"), keys(ba, "add")) ||
		len(keys(ab, "move")) != len(keys(ba, "move")) {
		t.Fatalf("the chain changes are not mirrored:\n%+v\n%+v", ab.Effective, ba.Effective)
	}
}

// checkOps applies diff(A, B)'s operations to the canonical form of each
// modified resource of A and fails t unless that gives B's canonical
// bytes, or an operation shows a secret value that is not a reference.
func checkOps(t *testing.T, tc *toolchain, ab diffDoc, va, vb *hub.Validated) {
	t.Helper()
	idx := tc.pipe.HubIndex()
	for _, res := range ab.Resources {
		if res.Change != "modified" {
			if len(res.Ops) > 0 {
				t.Fatalf("%s/%s %s carries operations; added and removed resources carry identity only", res.Kind, res.Name, res.Change)
			}
			continue
		}
		id := hub.ID{Kind: v1alpha1.Kind(res.Kind), Name: res.Name}
		ra, okA := va.Bundle.Get(id)
		rb, okB := vb.Bundle.Get(id)
		if !okA || !okB {
			t.Fatalf("%s is modified but not in both Revisions", id)
		}
		got := applyOps(t, idx, ra, res.Ops)
		if g, w := encodeResource(t, idx, id, got), encodeResource(t, idx, id, rb.Tree); !bytes.Equal(g, w) {
			t.Fatalf("%s: the operations do not turn A's canonical form into B's\n got %s\nwant %s\nops %s", id, g, w, opsText(res.Ops))
		}
		checkSecretOps(t, idx, ra, rb, res.Ops)
	}
}

// encodeResource returns the canonical bytes of one resource.
func encodeResource(t *testing.T, idx *schemaidx.Index, id hub.ID, root *tree.Node) []byte {
	t.Helper()
	rev, err := canonical.NewEncoder(idx).EncodeResources(t.Context(), []tree.Resource{{ID: id, Root: root}}, canonical.Options{})
	if err != nil {
		t.Fatalf("encode %s: %v", id, err)
	}
	return rev.Content
}

// keyedList is a map or orderedMap list the operations touch: its
// entries' positions before them, the moves and the entries added, in
// order.
type keyedList struct {
	list    *tree.Node
	field   string
	ordered bool
	orig    map[string]int
	moved   map[string]int
	added   []*tree.Node
}

// applyOps applies ops to a copy of r's tree as the configuration model
// states: a path resolves by key; an orderedMap entry moves to its
// position, an entry neither moved nor added keeps its position and added
// entries fill the free positions in order; map entries and set elements
// take their sorted place, which normalizing the resource as stage G does
// gives.
func applyOps(t *testing.T, idx *schemaidx.Index, r *hub.Resource, ops []diffOp) *tree.Node {
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
			if key, ok := it.Get(field); ok {
				k.orig[key.Text] = i
			}
		}
		lists[list] = k
		order = append(order, k)
		return k
	}
	for _, op := range ops {
		path := opPath(t, op.Path)
		if len(path) == 0 {
			t.Fatalf("%s: an operation without a path", r.ID)
		}
		parentPath, last := path[:len(path)-1], path[len(path)-1]
		parent, ok := out.At(parentPath)
		if !ok {
			t.Fatalf("%s: no parent at %s for %s", r.ID, parentPath, op.Op)
		}
		switch last.Kind {
		case diag.ElemField:
			if op.Op == "remove" {
				parent.Delete(last.Name)
			} else {
				parent.Set(last.Name, tree.Pos{}, opValue(t, op.To))
			}
		case diag.ElemKeyed:
			k := touch(parent, last.KeyField, parentPath)
			i := slices.IndexFunc(parent.Items, func(n *tree.Node) bool { v, ok := n.Get(last.KeyField); return ok && v.Text == last.Key })
			switch op.Op {
			case "remove":
				parent.Items = slices.Delete(parent.Items, i, i+1)
			case "add":
				v := opValue(t, op.To)
				parent.Items = append(parent.Items, v)
				k.added = append(k.added, v)
			case "replace":
				parent.Items[i] = opValue(t, op.To)
			case "move":
				var to int
				if err := json.Unmarshal(op.To, &to); err != nil {
					t.Fatalf("%s: move to %s: %v", r.ID, op.To, err)
				}
				k.moved[last.Key] = to
			default:
				t.Fatalf("%s: unknown operation %q", r.ID, op.Op)
			}
		case diag.ElemItem:
			if op.Op == "add" {
				parent.Items = append(parent.Items, opValue(t, op.To))
				continue
			}
			at, ok := parent.At(diag.Path{last})
			i := slices.Index(parent.Items, at)
			if !ok || i < 0 {
				t.Fatalf("%s: no set element %v at %s", r.ID, last.Item, parentPath)
			}
			parent.Items = slices.Delete(parent.Items, i, i+1)
		default:
			t.Fatalf("%s: an operation at a list position %s", r.ID, path)
		}
	}
	for _, k := range order {
		if k.ordered {
			placeOrdered(t, r.ID, k)
		}
	}
	res := &tree.Resource{ID: r.ID, APIVersion: v1alpha1.APIVersion, Root: out}
	if ds := defaults.Normalize(idx, res, nil); ds.HasErrors() {
		t.Fatalf("%s: normalize: %s", r.ID, texts(ds))
	}
	return res.Root
}

// placeOrdered sets the order of an orderedMap list after the operations:
// moved entries at their positions, other kept entries at theirs, added
// entries in the free positions in order.
func placeOrdered(t *testing.T, id hub.ID, k *keyedList) {
	t.Helper()
	slots := make([]*tree.Node, len(k.list.Items))
	for _, it := range k.list.Items {
		if slices.Contains(k.added, it) {
			continue
		}
		key, _ := it.Get(k.field)
		pos, ok := k.moved[key.Text]
		if !ok {
			pos = k.orig[key.Text]
		}
		if pos < 0 || pos >= len(slots) || slots[pos] != nil {
			t.Fatalf("%s: entry %s cannot take position %d", id, key.Text, pos)
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

// opPath reads a ruralz.diff.v1 path: strings are members, integers list
// positions, {"item": v} set elements and any other one-member object a
// keyed entry.
func opPath(t *testing.T, raw []json.RawMessage) diag.Path {
	t.Helper()
	var out diag.Path
	for _, e := range raw {
		var s string
		if json.Unmarshal(e, &s) == nil {
			out = append(out, diag.Field(s))
			continue
		}
		var i int
		if json.Unmarshal(e, &i) == nil {
			out = append(out, diag.Index(i))
			continue
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(e, &obj); err != nil || len(obj) != 1 {
			t.Fatalf("path element %s is no string, integer or one-member object", e)
		}
		for k, v := range obj {
			if k == "item" {
				out = append(out, diag.PathElem{Kind: diag.ElemItem, Item: itemOf(v)})
				continue
			}
			var key string
			if err := json.Unmarshal(v, &key); err != nil {
				t.Fatalf("keyed path element %s: %v", e, err)
			}
			out = append(out, diag.Keyed(k, key))
		}
	}
	return out
}

// itemOf returns a set element of a path in the form tree.Node.At
// compares: a string, a json.Number, a bool, or the raw JSON of an object
// or array.
func itemOf(v json.RawMessage) any {
	d := json.NewDecoder(bytes.NewReader(v))
	d.UseNumber()
	var x any
	if d.Decode(&x) != nil {
		return v
	}
	switch x.(type) {
	case string, json.Number, bool:
		return x
	default:
		return v
	}
}

// opValue parses an operation's value as a tree, the form the profile's
// JSON front end builds.
func opValue(t *testing.T, raw json.RawMessage) *tree.Node {
	t.Helper()
	wrapped := append(append([]byte(`{"v":`), raw...), '}')
	docs, ds, err := profile.Parse(t.Context(), wrapped, 0, "op.json", profile.FormatJSON, profile.Options{})
	if err != nil || ds.HasErrors() || len(docs) != 1 {
		t.Fatalf("operation value %s does not parse: %v %s", raw, err, texts(ds))
	}
	v, _ := docs[0].Root.Get("v")
	return v
}

// opsText returns the operations as text for a failure message.
func opsText(ops []diffOp) string {
	var b strings.Builder
	for _, op := range ops {
		fmt.Fprintf(&b, "\n  %s %s from=%s to=%s", op.Op, joinRaw(op.Path), op.From, op.To)
	}
	return b.String()
}

// joinRaw joins raw JSON values with spaces.
func joinRaw(rs []json.RawMessage) string {
	parts := make([]string, len(rs))
	for i, r := range rs {
		parts[i] = string(r)
	}
	return strings.Join(parts, " ")
}

// checkSecretOps fails t when an operation shows a secret by anything but
// its reference: an operation at an x-ruralz-secret field of A or B must
// show objects holding exactly a secretRef, and one below such a field
// must change a member of that secretRef.
func checkSecretOps(t *testing.T, idx *schemaidx.Index, ra, rb *hub.Resource, ops []diffOp) {
	t.Helper()
	for _, op := range ops {
		path := opPath(t, op.Path)
		for _, r := range []*hub.Resource{ra, rb} {
			info, ok := idx.Lookup(string(r.Kind), r.Tree, path)
			if !ok || !info.InSecret {
				continue
			}
			if !info.Node.Keywords().Secret {
				if !slices.ContainsFunc(path, func(e diag.PathElem) bool { return e.Kind == diag.ElemField && e.Name == "secretRef" }) {
					t.Fatalf("%s %s: an operation inside a secret position outside its secretRef", r.ID, joinRaw(op.Path))
				}
				continue
			}
			for _, v := range []json.RawMessage{op.From, op.To} {
				if len(v) == 0 || string(v) == "null" {
					continue
				}
				var ref map[string]json.RawMessage
				if json.Unmarshal(v, &ref) != nil || len(ref) != 1 || ref["secretRef"] == nil {
					t.Fatalf("%s %s: the operation shows %s at a secret field, not a reference", r.ID, joinRaw(op.Path), v)
				}
			}
		}
	}
}
