// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package topology

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/profile"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// DefaultAdminPort is the admin.port of a Gateway that sets none.
const DefaultAdminPort = 9901

// Render is one test render of a Node: the rendered Bundle with every
// listener port, the admin port and every secretRef rewritten
// (docs/reference/01-cli-and-api-surface.md "Local ruralzd launched by the
// CLI").
type Render struct {
	// Bytes is the test render the Node loads from RURALZ_CONFIG.
	Bytes []byte
	// Revision is the Revision digest of the unmodified rendered Bundle,
	// and TestDigest that of Bytes, the digest the Node reports; they are
	// equal only when nothing was rewritten.
	Revision, TestDigest string
	// Listeners are the Gateway's listeners in Bundle order.
	Listeners []Listener
	// AuthoredAdminPort is the Bundle's admin.port (DefaultAdminPort when
	// unset) and AdminPort the free port it became.
	AuthoredAdminPort, AdminPort int
	// Secrets are the Bundle's secret references, each once, in order.
	Secrets []SecretRef
}

// Listener is one Gateway listener of a test render.
type Listener struct {
	// Name and Protocol are as authored.
	Name, Protocol string
	// AuthoredPort is the Bundle's port and Port the free port it became.
	AuthoredPort, Port int
}

// Mapping returns the port mapping lines ruralz test run prints, one per
// rewritten port: "<listener>: <port> -> <port>", then "admin: <port> ->
// <port>".
func (r *Render) Mapping() string {
	var b strings.Builder
	for _, l := range r.Listeners {
		fmt.Fprintf(&b, "%s: %d -> %d\n", l.Name, l.AuthoredPort, l.Port)
	}
	fmt.Fprintf(&b, "admin: %d -> %d\n", r.AuthoredAdminPort, r.AdminPort)
	return b.String()
}

// DisplayRevision returns the "rev-<12 hex>" form of a "sha256:<64 hex>"
// digest, as /readyz reports it, or "" for another string.
func DisplayRevision(digest string) string {
	hex, ok := strings.CutPrefix(digest, "sha256:")
	if !ok || len(hex) < 12 {
		return ""
	}
	return "rev-" + hex[:12]
}

// renderBundle builds the Node's test render of src: listener ports and
// admin.port become free loopback ports, the same for a listener name
// across renders, and every secretRef a file reference under the Node's
// secret root that keeps its key. src is a rendered Bundle in the
// restricted YAML profile holding exactly one Gateway.
func (n *Node) renderBundle(ctx context.Context, src []byte) (*Render, error) {
	docs, diags, err := profile.Parse(ctx, src, 1, "bundle.yaml", profile.FormatYAML, profile.Options{})
	if err != nil {
		return nil, fmt.Errorf("topology: %s: %w", n.name, err)
	}
	if len(diags) > 0 {
		return nil, fmt.Errorf("topology: %s: the rendered Bundle does not parse: %s", n.name, diagText(diags))
	}
	schemas, err := n.t.schemaSet()
	if err != nil {
		return nil, err
	}
	r := &Render{}
	seen := map[string]bool{}
	roots := make([]*tree.Node, 0, len(docs))
	gateways := 0
	for _, d := range docs {
		root := d.Root
		roots = append(roots, root)
		apiVersion, kind := text(root, "apiVersion"), text(root, "kind")
		idx, ok := schemas.Index(apiVersion)
		if !ok {
			return nil, fmt.Errorf("topology: %s: a %s resource has the unserved apiVersion %q", n.name, kind, apiVersion)
		}
		if kind == "Gateway" {
			gateways++
			if err := n.rewritePorts(ctx, root, r); err != nil {
				return nil, err
			}
		}
		walk := func(c *schemaidx.Cursor) error {
			if !c.Info.InSecret || c.Node.Kind != tree.KindMap {
				return nil
			}
			// A SecretValue field holds {secretRef: {...}}; a SecretRef
			// field, such as a Consumer API key's secretRef, holds the
			// reference itself.
			ref, ok := c.Node.Get("secretRef")
			if !ok {
				if _, isRef := c.Node.Get("provider"); isRef {
					ref, ok = c.Node, true
				}
			}
			if !ok || ref.Kind != tree.KindMap {
				return schemaidx.ErrSkipChildren
			}
			if n.authored(text(ref, "provider"), text(ref, "name"), text(ref, "key")) {
				return schemaidx.ErrSkipChildren
			}
			sr, err := n.rewriteRef(ref)
			if err != nil {
				return err
			}
			if !seen[sr.Ref] {
				seen[sr.Ref] = true
				r.Secrets = append(r.Secrets, sr)
			}
			return schemaidx.ErrSkipChildren
		}
		if err := idx.Walk(kind, root, walk); err != nil {
			return nil, fmt.Errorf("topology: %s: %s: %w", n.name, kind, err)
		}
	}
	if gateways != 1 {
		return nil, fmt.Errorf("topology: %s: the rendered Bundle holds %d Gateways, want exactly one to rewrite", n.name, gateways)
	}
	// The parsed tree still holds the rendered Bundle's "$${" escapes, since
	// substitution never runs on it, so it is written verbatim: the default
	// encoder would escape them again and change the values served.
	var out bytes.Buffer
	if err := profile.EncodeWith(&out, roots, profile.EncodeOptions{Verbatim: true}); err != nil {
		return nil, fmt.Errorf("topology: %s: %w", n.name, err)
	}
	r.Bytes = out.Bytes()
	if r.Revision, err = n.t.cfg.Digest(src); err != nil {
		return nil, fmt.Errorf("topology: %s: Revision digest: %w", n.name, err)
	}
	if r.TestDigest, err = n.t.cfg.Digest(r.Bytes); err != nil {
		return nil, fmt.Errorf("topology: %s: test digest: %w", n.name, err)
	}
	return r, nil
}

// rewritePorts gives every listener of the Gateway gw its Node port and
// sets admin.port, adding the admin block when the Bundle has none.
func (n *Node) rewritePorts(ctx context.Context, gw *tree.Node, r *Render) error {
	spec, ok := gw.Get("spec")
	if !ok || spec.Kind != tree.KindMap {
		return fmt.Errorf("topology: %s: the Gateway has no spec", n.name)
	}
	if ls, ok := spec.Get("listeners"); ok && ls.Kind == tree.KindList {
		for _, l := range ls.Items {
			name, protocol := text(l, "name"), text(l, "protocol")
			authored, err := intMember(l, "port", -1)
			if err != nil || name == "" || authored < 0 {
				return fmt.Errorf("topology: %s: a Gateway listener without a name or an integer port", n.name)
			}
			port, ok := n.ports[name]
			if !ok {
				if port, err = n.t.freePort(ctx); err != nil {
					return err
				}
				n.ports[name] = port
			}
			l.Set("port", tree.Pos{}, intNode(port))
			r.Listeners = append(r.Listeners, Listener{Name: name, Protocol: protocol, AuthoredPort: authored, Port: port})
		}
	}
	admin, ok := spec.Get("admin")
	if !ok || admin.Kind != tree.KindMap {
		admin = &tree.Node{Kind: tree.KindMap}
		spec.Set("admin", tree.Pos{}, admin)
	}
	authored, err := intMember(admin, "port", DefaultAdminPort)
	if err != nil {
		return fmt.Errorf("topology: %s: admin.port: %w", n.name, err)
	}
	if n.adminPort == 0 {
		if n.adminPort, err = n.t.freePort(ctx); err != nil {
			return err
		}
	}
	admin.Set("port", tree.Pos{}, intNode(n.adminPort))
	r.AuthoredAdminPort, r.AdminPort = authored, n.adminPort
	return nil
}

// authored reports whether NodeOptions.AuthoredSecrets keeps the reference
// as authored: the list names it, or names its provider and name without
// a key.
func (n *Node) authored(provider, name, key string) bool {
	return slices.Contains(n.opts.AuthoredSecrets, refString(provider, name, key)) ||
		(key != "" && slices.Contains(n.opts.AuthoredSecrets, refString(provider, name, "")))
}

// rewriteRef turns the secretRef ref into a file reference under the
// Node's secret root, keeping its key.
func (n *Node) rewriteRef(ref *tree.Node) (SecretRef, error) {
	sr := SecretRef{Provider: text(ref, "provider"), Name: text(ref, "name"), Key: text(ref, "key")}
	sr.Ref = refString(sr.Provider, sr.Name, sr.Key)
	path, err := secretPath(n.secretRoot, sr.Provider, sr.Name)
	if err != nil {
		return SecretRef{}, fmt.Errorf("%s: %w", n.name, err)
	}
	sr.Path = path
	ref.Set("provider", tree.Pos{}, stringNode("file"))
	ref.Set("name", tree.Pos{}, stringNode(path))
	return sr, nil
}

// schemaSet loads the embedded schema indexes once.
func (t *Topology) schemaSet() (*schemaidx.Set, error) {
	t.schemasOnce.Do(func() {
		t.schemas, t.schemasErr = schemaidx.Embedded()
		if t.schemasErr != nil {
			t.schemasErr = fmt.Errorf("topology: %w", t.schemasErr)
		}
	})
	return t.schemas, t.schemasErr
}

// text returns the string member key of a map node, or "".
func text(n *tree.Node, key string) string {
	v, ok := n.Get(key)
	if !ok || v.Kind != tree.KindString {
		return ""
	}
	return v.Text
}

// intMember returns the integer member key of a map node, def when absent.
func intMember(n *tree.Node, key string, def int) (int, error) {
	v, ok := n.Get(key)
	if !ok {
		return def, nil
	}
	if v.Kind != tree.KindInt {
		return 0, errors.New("not an integer")
	}
	return strconv.Atoi(v.Text)
}

func intNode(i int) *tree.Node { return &tree.Node{Kind: tree.KindInt, Text: strconv.Itoa(i)} }

func stringNode(s string) *tree.Node {
	return &tree.Node{Kind: tree.KindString, Style: tree.StyleDoubleQuoted, Text: s}
}

// diagText formats the first diagnostics of a parse.
func diagText(l diag.List) string {
	var parts []string
	for i, d := range l {
		if i == 3 {
			parts = append(parts, fmt.Sprintf("and %d more", len(l)-3))
			break
		}
		parts = append(parts, fmt.Sprintf("%s %d:%d %s", d.Code, d.Location.Line, d.Location.Column, d.Message))
	}
	return strings.Join(parts, "; ")
}
