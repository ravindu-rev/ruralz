// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package launch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"

	"github.com/ravindu-rev/ruralz/internal/cli/command"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/loader"
	"github.com/ravindu-rev/ruralz/internal/config/pipeline"
	"github.com/ravindu-rev/ruralz/internal/config/render"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/subst"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// DefaultAdminPort is the admin.port of a Gateway that sets none.
const DefaultAdminPort = 9901

// codeSecret is RZ-CFG-026, an unresolvable secretRef: the code of a file
// reference that secret overrides leave without an entry.
const codeSecret = "RZ-CFG-026"

// Request names what a Renderer renders and how it rewrites it.
type Request struct {
	// Source is the Bundle: a directory rooted at ruralz.yaml or a
	// one-file Bundle.
	Source string
	// Environments is the --environments file; "" for none.
	Environments string
	// Environment is --env, which needs Environments; "" renders without
	// an overlay, with Variables.
	Environment string
	// Variables supplies ${VAR} values without Environment: the CLI's
	// process environment.
	Variables subst.Source
	// EphemeralPorts rewrites every listener port and admin.port to a free
	// loopback port (--ephemeral-ports; always for ruralz test run
	// --bundle).
	EphemeralPorts bool
	// Overrides is the --secret-overrides file; "" for none.
	Overrides string
	// MemoryStateStore replaces the Gateway's stateStore with driver
	// memory (--memory-state-store).
	MemoryStateStore bool
}

// Renderer renders one source into test renders in one Workspace, keeping
// each listener's and the admin port's replacement across renders.
type Renderer struct {
	p   *pipeline.Pipeline
	ws  *Workspace
	req Request
	// ports maps a listener name to its replacement port, and admin is
	// the admin port's, 0 until assigned.
	ports map[string]int
	admin int
}

// NewRenderer returns the Renderer of req in ws over p.
func NewRenderer(p *pipeline.Pipeline, ws *Workspace, req Request) *Renderer {
	return &Renderer{p: p, ws: ws, req: req, ports: map[string]int{}}
}

// Port is one port of a test render: as authored and as served.
type Port struct {
	// Name is the listener's name, or "admin".
	Name string
	// Port is the Bundle's port; TestPort is the port ruralzd binds, equal
	// to Port without a rewrite.
	Port, TestPort int
}

// Listener is one Gateway listener of a test render.
type Listener struct {
	Port
	// Protocol is http or https.
	Protocol string
}

// URL returns the loopback URL ruralzd serves the listener on.
func (l Listener) URL() string {
	scheme := "http"
	if l.Protocol == string(v1alpha1.ListenerProtocolHTTPS) {
		scheme = "https"
	}
	return scheme + "://127.0.0.1:" + strconv.Itoa(l.TestPort)
}

// Render is one test render, staged in the Workspace until Swap.
type Render struct {
	// Revision is the digest of the unmodified rendered Bundle.
	Revision revision.Digest
	// TestDigest is the digest of the test render, the Revision ruralzd
	// reports; equal to Revision when nothing was rewritten.
	TestDigest revision.Digest
	// Listeners are the Gateway's listeners in Bundle order.
	Listeners []Listener
	// Admin is the admin port.
	Admin Port
	// Rewritten reports that the ports were rewritten, so the port mapping
	// prints.
	Rewritten bool
	// Warnings are lines for standard error, such as unused secret
	// overrides.
	Warnings []string
	// Diagnostics are the warnings of the run that built the Revision.
	Diagnostics diag.List

	bytes   []byte
	secrets []secretCopy
}

// Ports returns the rewritten ports, listeners first, then the admin port;
// none without a rewrite.
func (r *Render) Ports() []Port {
	if !r.Rewritten {
		return nil
	}
	out := make([]Port, 0, len(r.Listeners)+1)
	for _, l := range r.Listeners {
		out = append(out, l.Port)
	}
	return append(out, r.Admin)
}

// AdminURL returns the loopback URL of ruralzd's admin port.
func (r *Render) AdminURL() string { return "http://127.0.0.1:" + strconv.Itoa(r.Admin.TestPort) }

// Mapping returns the port mapping lines: "<listener>: <port> -> <port>"
// per listener, then "admin: <port> -> <port>"; none without a rewrite.
func (r *Render) Mapping() []string {
	ports := r.Ports()
	out := make([]string, 0, len(ports))
	for _, p := range ports {
		out = append(out, fmt.Sprintf("%s: %d -> %d", p.Name, p.Port, p.TestPort))
	}
	return out
}

// SameMapping reports whether r and o rewrite the same ports the same way.
func (r *Render) SameMapping(o *Render) bool {
	return o != nil && slices.Equal(r.Ports(), o.Ports())
}

// Failure is a render the Bundle failed: the run's diagnostics, or the
// findings of the rewrite, such as a file reference secret overrides leave
// without an entry. ruralz dev run prints them and keeps watching.
type Failure struct {
	Diagnostics diag.List
}

// Error summarizes the failure.
func (f *Failure) Error() string {
	n := 0
	for _, d := range f.Diagnostics {
		if d.Severity != diag.SeverityWarning {
			n++
		}
	}
	if n == 1 {
		return "the render failed with 1 error"
	}
	return "the render failed with " + strconv.Itoa(n) + " errors"
}

// Render renders the source and stages its test render. A render the
// Bundle fails is a *Failure; an input that cannot be read, a source, the
// environments file or the secret overrides, is an error exiting 2, as are
// ctx's errors and faults of the release.
func (r *Renderer) Render(ctx context.Context) (*Render, error) {
	req := pipeline.Request{Source: loader.Source{Path: r.req.Source}, Render: true}
	if r.req.Environment != "" {
		req.Environments, req.Environment = loader.Source{Path: r.req.Environments}, r.req.Environment
	} else {
		req.Variables = r.req.Variables
	}
	res, err := r.p.Run(ctx, req)
	if err != nil {
		return nil, sourceError(r.req.Source, err)
	}
	if res.Validated == nil {
		return nil, &Failure{Diagnostics: res.Diagnostics}
	}
	var ov *overrides
	if r.req.Overrides != "" {
		if ov, err = readOverrides(ctx, r.req.Overrides, r.ws.SecretRoot()); err != nil {
			return nil, err
		}
	}
	out := &Render{Revision: res.Validated.Revision.Digest, Diagnostics: res.Diagnostics}
	rewrite, failed, err := r.rewrite(ctx, res, out, ov)
	if err != nil {
		return nil, err
	}
	if len(failed) > 0 {
		return nil, &Failure{Diagnostics: failed}
	}
	var buf bytes.Buffer
	ds, err := r.p.WriteRendered(ctx, &buf, res, render.FormatYAML)
	if err != nil {
		return nil, command.NoResultf("write the test render: %w", err)
	}
	if len(ds) > 0 {
		return nil, &Failure{Diagnostics: ds}
	}
	out.bytes = buf.Bytes()
	if err := writeAtomic(r.ws.staged(), out.bytes); err != nil {
		return nil, noResult("stage the test render", err)
	}
	out.TestDigest = out.Revision
	if rewrite {
		if out.TestDigest, err = r.testDigest(ctx); err != nil {
			return nil, err
		}
	}
	if ov != nil {
		out.secrets = ov.copies()
		out.Warnings = ov.unused()
	}
	return out, nil
}

// testDigest runs the staged test render through the pipeline, as ruralzd
// loads it: one file, no Environment and no variable, since a rendered
// Bundle holds no ${VAR} left to substitute.
func (r *Renderer) testDigest(ctx context.Context) (revision.Digest, error) {
	res, err := r.p.Run(ctx, pipeline.Request{Source: loader.Source{Path: r.ws.staged()}})
	if err != nil {
		return revision.Digest{}, command.NoResultf("check the test render: %w", err)
	}
	if res.Validated == nil {
		return revision.Digest{}, &Failure{Diagnostics: res.Diagnostics}
	}
	return res.Validated.Revision.Digest, nil
}

// rewrite turns the rendered resources of res into the test render in
// place: the ports of the Gateway, every secretRef an override names, the
// State Store; it fills out's ports and reports whether anything changed.
// Findings, such as a file reference without an override, come back as
// diagnostics.
func (r *Renderer) rewrite(ctx context.Context, res *pipeline.Result, out *Render, ov *overrides) (bool, diag.List, error) {
	schemas := r.p.Schemas()
	changed := false
	var failed diag.List
	gateways := 0
	for _, rs := range res.Rendered {
		if rs == nil || rs.Root == nil {
			continue
		}
		if rs.ID.Kind == v1alpha1.KindGateway {
			gateways++
			c, err := r.rewriteGateway(ctx, rs.Root, out)
			if err != nil {
				return false, nil, err
			}
			changed = changed || c
		}
		if ov == nil {
			continue
		}
		idx, ok := schemas.Index(rs.APIVersion)
		if !ok {
			return false, nil, command.NoResultf("the test render: %s/%s has the unserved apiVersion %q", rs.ID.Kind, rs.ID.Name, rs.APIVersion)
		}
		resource := &diag.ResourceID{Kind: string(rs.ID.Kind), Name: rs.ID.Name}
		walk := func(c *schemaidx.Cursor) error {
			if !c.Info.InSecret || c.Node.Kind != tree.KindMap {
				return nil
			}
			ref, ok := c.Node.Get("secretRef")
			if !ok || ref.Kind != tree.KindMap {
				return schemaidx.ErrSkipChildren
			}
			provider, name := text(ref, "provider"), text(ref, "name")
			switch dst, ok := ov.use(provider, name); {
			case ok:
				ref.Set("provider", tree.Pos{}, stringNode(string(v1alpha1.SecretProviderFile)))
				ref.Set("name", tree.Pos{}, stringNode(dst))
				changed = true
			case provider == string(v1alpha1.SecretProviderFile):
				failed = append(failed, diag.Diagnostic{
					Code: codeSecret, Severity: diag.SeverityError,
					Location: res.Files.Location(ref.Pos), Resource: resource, Path: c.Path(),
					Message: fmt.Sprintf("secretRef %s:%s has no entry in --secret-overrides %s; with overrides every file reference needs one",
						provider, name, ov.file),
				})
			}
			return schemaidx.ErrSkipChildren
		}
		if err := idx.Walk(string(rs.ID.Kind), rs.Root, walk); err != nil {
			return false, nil, command.NoResultf("the test render: %s/%s: %w", rs.ID.Kind, rs.ID.Name, err)
		}
	}
	if gateways != 1 {
		return false, nil, command.NoResultf("the test render: the rendered Bundle holds %d Gateways, want one", gateways)
	}
	failed.Sort()
	return changed, failed, nil
}

// rewriteGateway rewrites the Gateway envelope gw: listener ports and
// admin.port with ephemeral ports, the State Store with memory. It records
// the ports in out and reports whether it changed gw.
func (r *Renderer) rewriteGateway(ctx context.Context, gw *tree.Node, out *Render) (bool, error) {
	spec, ok := gw.Get("spec")
	if !ok || spec.Kind != tree.KindMap {
		return false, command.NoResultf("the test render: the Gateway has no spec")
	}
	changed := false
	if ls, ok := spec.Get("listeners"); ok && ls.Kind == tree.KindList {
		for _, l := range ls.Items {
			name, protocol := text(l, "name"), text(l, "protocol")
			port, err := intMember(l, "port", -1)
			if err != nil || name == "" || port < 0 {
				return false, command.NoResultf("the test render: Gateway listener %q has no name or integer port", name)
			}
			test := port
			if r.req.EphemeralPorts {
				if test, err = r.portFor(ctx, name); err != nil {
					return false, err
				}
				l.Set("port", tree.Pos{}, intNode(test))
				changed = true
			}
			out.Listeners = append(out.Listeners, Listener{Port: Port{Name: name, Port: port, TestPort: test}, Protocol: protocol})
		}
	}
	admin, ok := spec.Get("admin")
	adminPort, err := intMember(admin, "port", DefaultAdminPort)
	if err != nil {
		return false, command.NoResultf("the test render: admin.port: %w", err)
	}
	out.Admin = Port{Name: "admin", Port: adminPort, TestPort: adminPort}
	if r.req.EphemeralPorts {
		if r.admin == 0 {
			if r.admin, err = r.freePort(ctx); err != nil {
				return false, err
			}
		}
		if !ok || admin.Kind != tree.KindMap {
			admin = &tree.Node{Kind: tree.KindMap}
			spec.Set("admin", tree.Pos{}, admin)
		}
		admin.Set("port", tree.Pos{}, intNode(r.admin))
		out.Admin.TestPort = r.admin
		out.Rewritten, changed = true, true
	}
	if r.req.MemoryStateStore {
		spec.Set("stateStore", tree.Pos{}, &tree.Node{Kind: tree.KindMap, Members: []tree.Member{
			{Key: "driver", Value: stringNode(string(v1alpha1.StateStoreDriverMemory))},
		}})
		changed = true
	}
	return changed, nil
}

// portFor returns the replacement port of listener name, assigning a free
// one the first time the name appears.
func (r *Renderer) portFor(ctx context.Context, name string) (int, error) {
	if p, ok := r.ports[name]; ok {
		return p, nil
	}
	p, err := r.freePort(ctx)
	if err != nil {
		return 0, err
	}
	r.ports[name] = p
	return p, nil
}

// freePort returns a free loopback port no port of this Renderer holds.
func (r *Renderer) freePort(ctx context.Context) (int, error) {
	taken := func(p int) bool {
		if p == r.admin {
			return true
		}
		for _, q := range r.ports {
			if q == p {
				return true
			}
		}
		return false
	}
	return freeLoopbackPort(ctx, taken)
}

// Swap renames the staged test render of rend over the file ruralzd
// watches, after copying its secret files into the private secret root,
// so ruralzd finds them when it loads the render.
func (r *Renderer) Swap(rend *Render) error {
	if err := copySecrets(rend.secrets); err != nil {
		return err
	}
	if err := writeAtomic(r.ws.Config(), rend.bytes); err != nil {
		return noResult("swap the test render", err)
	}
	_ = os.Remove(r.ws.staged())
	return nil
}

// RefreshSecrets copies the secret files of rend again, for a render that
// changed no byte of the test render but whose secret files changed:
// ruralzd's file watch then rotates the secrets without a new Revision.
func (r *Renderer) RefreshSecrets(rend *Render) error { return copySecrets(rend.secrets) }

// sourceError maps an error of a pipeline run over the source: a source
// the loader cannot read exits 2 naming it; ctx's errors and faults of
// the release pass, exiting by command.Code.
func sourceError(source string, err error) error {
	if errors.Is(err, loader.ErrSource) {
		return command.NoResultf("cannot read %s: %w", source, err)
	}
	return err
}

// noResult is the exit 2 error of a step that failed.
func noResult(what string, err error) error { return command.NoResultf("%s: %w", what, err) }

// text returns the string member key of a map node, or "".
func text(n *tree.Node, key string) string {
	v, ok := n.Get(key)
	if !ok || v.Kind != tree.KindString {
		return ""
	}
	return v.Text
}

// intMember returns the integer member key of a map node, def when n or
// the member is absent.
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

func stringNode(s string) *tree.Node { return &tree.Node{Kind: tree.KindString, Text: s} }
