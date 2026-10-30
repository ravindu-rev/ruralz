// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package compose

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/profile"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// parseYAML parses the one-document YAML file at path with the restricted
// profile, the repository's only YAML parser: the suite's files use no
// anchor, alias or tag, so the profile reads them as docker compose does.
func parseYAML(t testing.TB, path string) *tree.Node {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // G304: a file of the repository
	if err != nil {
		t.Fatal(err)
	}
	docs, diags, err := profile.Parse(context.Background(), data, 0, path, profile.FormatYAML, profile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(diags) > 0 {
		t.Fatalf("%s: %v", path, diags)
	}
	if len(docs) != 1 {
		t.Fatalf("%s holds %d documents, want 1", path, len(docs))
	}
	return docs[0].Root
}

// parseYAMLDocs parses every document of the YAML file at path.
func parseYAMLDocs(t testing.TB, path string) []*tree.Node {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // G304: a file of the repository
	if err != nil {
		t.Fatal(err)
	}
	docs, diags, err := profile.Parse(context.Background(), data, 0, path, profile.FormatYAML, profile.Options{})
	if err != nil || len(diags) > 0 {
		t.Fatalf("%s: %v %v", path, err, diags)
	}
	out := make([]*tree.Node, 0, len(docs))
	for _, d := range docs {
		out = append(out, d.Root)
	}
	return out
}

// node walks keys down maps; nil when a key is missing.
func node(n *tree.Node, keys ...string) *tree.Node {
	for _, k := range keys {
		v, ok := n.Get(k)
		if !ok {
			return nil
		}
		n = v
	}
	return n
}

// scalar returns the text of a scalar node: a string, an integer's
// decimal, a float's text, "true" or "false"; "" for anything else.
func scalar(n *tree.Node) string {
	if n == nil {
		return ""
	}
	switch n.Kind {
	case tree.KindString, tree.KindInt, tree.KindFloat:
		return n.Text
	case tree.KindBool:
		if n.Bool {
			return "true"
		}
		return "false"
	case tree.KindNull, tree.KindMap, tree.KindList:
		return ""
	}
	return ""
}

// scalars returns the scalars of a list node.
func scalars(n *tree.Node) []string {
	if n == nil || n.Kind != tree.KindList {
		return nil
	}
	out := make([]string, 0, len(n.Items))
	for _, it := range n.Items {
		out = append(out, scalar(it))
	}
	return out
}

// keys returns the member keys of a map node in authored order.
func keys(n *tree.Node) []string {
	if n == nil || n.Kind != tree.KindMap {
		return nil
	}
	out := make([]string, 0, len(n.Members))
	for _, m := range n.Members {
		out = append(out, m.Key)
	}
	return out
}

// service is one service of a Compose file.
type service struct {
	file, name string
	n          *tree.Node
}

// services returns the services of a parsed Compose file in order.
func services(file string, root *tree.Node) []service {
	svcs := node(root, "services")
	out := make([]service, 0, len(keys(svcs)))
	for _, name := range keys(svcs) {
		out = append(out, service{file: file, name: name, n: node(svcs, name)})
	}
	return out
}

// get returns the scalar of a service field.
func (s service) get(keys ...string) string { return scalar(node(s.n, keys...)) }

// list returns the scalars of a service list field.
func (s service) list(keys ...string) []string { return scalars(node(s.n, keys...)) }

// has reports whether the service sets a field.
func (s service) has(key string) bool { return node(s.n, key) != nil }

// imageVar returns the variable the service's image is, such as
// RURALZ_COMPOSE_IMAGE for "${RURALZ_COMPOSE_IMAGE:?...}", or "".
func (s service) imageVar() string {
	img := s.get("image")
	rest, ok := strings.CutPrefix(img, "${")
	if !ok {
		return ""
	}
	end := strings.IndexAny(rest, ":}")
	if end < 0 {
		return ""
	}
	return rest[:end]
}

// isRuralzd reports whether the service runs a ruralzd image.
func (s service) isRuralzd() bool { return slices.Contains(ruralzdImageVars(), s.imageVar()) }

// mount is one volume of a service, short or long form.
type mount struct {
	typ, source, target string
	readOnly            bool
}

// mounts returns the service's volumes.
func (s service) mounts() []mount {
	v := node(s.n, "volumes")
	if v == nil {
		return nil
	}
	out := make([]mount, 0, len(v.Items))
	for _, it := range v.Items {
		if it.Kind == tree.KindString {
			// source:target[:mode]; a source without a slash is a named
			// volume.
			parts := strings.Split(it.Text, ":")
			m := mount{typ: "volume", source: parts[0]}
			if len(parts) > 1 {
				m.target = parts[1]
			}
			if strings.ContainsAny(m.source, "/.$") {
				m.typ = "bind"
			}
			m.readOnly = len(parts) > 2 && slices.Contains(strings.Split(parts[2], ","), "ro")
			out = append(out, m)
			continue
		}
		out = append(out, mount{
			typ:      scalar(node(it, "type")),
			source:   scalar(node(it, "source")),
			target:   scalar(node(it, "target")),
			readOnly: scalar(node(it, "read_only")) == "true",
		})
	}
	return out
}

// mountAt returns the service's mount at target.
func (s service) mountAt(target string) (mount, bool) {
	for _, m := range s.mounts() {
		if m.target == target {
			return m, true
		}
	}
	return mount{}, false
}

// argAfter returns the item that follows flag in the service's command,
// or "" when the command lacks the flag.
func (s service) argAfter(flag string) string {
	cmd := s.list("command")
	for i, a := range cmd {
		if a == flag && i+1 < len(cmd) {
			return cmd[i+1]
		}
	}
	return ""
}
