// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package validate

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/config/canonical"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/pluginhost/configschema"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// plugin checks a Plugin: spec.image ends in "@sha256:" and 64 lower-case
// hexadecimal characters (revision.Parse), so every Node fetches the
// bytes the Revision names (RZ-CFG-021); the tag is informational; and its
// configSchema's key declarations (pluginMarkers). The config of a
// plugin Policy is checked against the Plugin's configSchema with the
// plugin Policy, Planned (M2).
func (u *unit) plugin() {
	p, ok := hub.Object[v1alpha1.Plugin](u.r)
	if !ok || p == nil {
		return
	}
	if why := digestProblem(p.Spec.Image); why != "" {
		u.report(CodePlugin, specPath(diag.Field("image")), "Plugin image "+quote(p.Spec.Image)+" "+why,
			"pin the artifact by digest, as in <repository>:<tag>@sha256:<64 hex>", nil)
	}
	u.pluginMarkers(p)
}

// digestProblem returns why image names no artifact by digest, or "".
func digestProblem(image string) string {
	i := strings.LastIndexByte(image, '@')
	if i < 0 {
		return "has no @sha256: digest"
	}
	d := image[i+1:]
	if !strings.HasPrefix(d, "sha256:") {
		return "has a digest that is not sha256"
	}
	if _, err := revision.Parse(d); err != nil {
		return "has a sha256 digest that is not 64 lower-case hexadecimal characters"
	}
	return ""
}

// Codes of the plugin Policy rules of stage H.
const (
	// CodeLiteral is RZ-CFG-012: a literal in a field a Plugin's
	// configSchema marks secret or key.
	CodeLiteral = "RZ-CFG-012"
	// CodeReservation is RZ-CFG-052: Plugin pools that can never fit the
	// Node Plugin memory cap.
	CodeReservation = "RZ-CFG-052"
)

// pluginSecrets lists the secret uses of a plugin Policy's config: each
// field its Plugin's configSchema marks x-ruralz-secret or x-ruralz-key
// holds a SecretValue, which the Node resolves and passes only to
// rz_configure, a key staying in the host (docs/architecture/05-wasm-
// plugin-system.md "Host Function rules"); a literal there is RZ-CFG-012.
// Such a secret never leaves the Node (destination local). A marker the
// configSchema states wrongly is the Plugin's finding.
func (u *unit) pluginSecrets(p *v1alpha1.Policy) {
	plugin, ok := u.ix.byKind[v1alpha1.KindPlugin][p.Spec.Plugin]
	if !ok {
		return
	}
	pl, ok := hub.Object[v1alpha1.Plugin](plugin)
	if !ok || pl == nil {
		return
	}
	fields, err := configschema.Fields(pl.Spec.ConfigSchema)
	if err != nil {
		return
	}
	cfg := map[string]any{}
	if c, ok := u.r.Config.(*v1alpha1.PluginConfig); ok && c != nil {
		cfg = map[string]any(*c)
	}
	for _, r := range configschema.Refs(fields, cfg) {
		path := specPath(diag.Field("config"))
		for _, e := range r.At {
			if i, err := strconv.Atoi(e); err == nil {
				path = append(path, diag.Index(i))
			} else {
				path = append(path, diag.Field(e))
			}
		}
		if r.Literal {
			u.report(CodeLiteral, path, "Plugin "+quote(pl.Metadata.Name)+" configSchema marks "+configschema.PathText(r.At)+
				" secret, and it holds a literal", "write {secretRef: {provider, name}}; keys never appear in plaintext config", nil)
			continue
		}
		u.secrets = append(u.secrets, hub.SecretUse{
			Ref: r.Ref, Kind: secret.KindOpaque, Resource: *u.r.ResourceID(), Path: path,
			Loc: u.f.path(path), Destination: hub.DestinationLocal,
		})
	}
}

// pluginMarkers reports a configSchema whose markers the host cannot
// honor (RZ-CFG-021, the configSchema rule): an x-ruralz-secret other than
// true, or an x-ruralz-key with an unknown JWA algorithm or operations its
// keys do not support.
func (u *unit) pluginMarkers(p *v1alpha1.Plugin) {
	if _, err := configschema.Fields(p.Spec.ConfigSchema); err != nil {
		u.report(CodePlugin, specPath(diag.Field("configSchema")), "Plugin "+quote(p.Metadata.Name)+" configSchema: "+err.Error(),
			"mark a secret field x-ruralz-secret: true and a key field x-ruralz-key: {algorithm: <JWA>, operations: [sign, verify] or [hmac]}", nil)
	}
}

// Plugin pool sizing that stage H checks offline, the targets of
// docs/architecture/05-wasm-plugin-system.md "Default limits": a pool's
// share of the Node Plugin memory cap (25%, 50% for a state.* Plugin),
// the warm minimum of one instance per pool, and the defaults of
// limits.memoryBytes and of the cap.
const (
	poolShare          = 25
	statePoolShare     = 50
	defaultMemoryBytes = 16 << 20
	defaultNodeCap     = 2 << 30
)

// checkPluginPools reports Plugin pools that can never fit the Node
// Plugin memory cap of the Revision (RZ-CFG-052), computed alike in every
// binary: a Plugin whose limits.memoryBytes passes its pool's share, at
// its limits, and pools whose warm minimums, one instance each, pass the
// cap together, at the Gateway's limits.maxPluginMemoryBytes. Plugin
// Policies share a pool, as on a Node, when their Plugins' image digests
// and limits and their canonical configs are equal.
func checkPluginPools(ix *bundleIndex, l locator, out *sink) {
	nodeCap := int64(defaultNodeCap)
	if ix.gw != nil && ix.gw.Spec.Limits != nil && ix.gw.Spec.Limits.MaxPluginMemoryBytes != nil {
		nodeCap = int64(*ix.gw.Spec.Limits.MaxPluginMemoryBytes)
	}
	pools := map[string]bool{}
	var warm int64
	oversized := map[string]bool{}
	for _, r := range ix.resources {
		p, ok := hub.Object[v1alpha1.Policy](r)
		if !ok || p == nil || p.Spec.Type != v1alpha1.PolicyTypePlugin {
			continue
		}
		pr, ok := ix.byKind[v1alpha1.KindPlugin][p.Spec.Plugin]
		if !ok {
			continue
		}
		pl, ok := hub.Object[v1alpha1.Plugin](pr)
		if !ok || pl == nil {
			continue
		}
		mem := int64(defaultMemoryBytes)
		if lim := pl.Spec.Limits; lim != nil && lim.MemoryBytes != nil {
			mem = int64(*lim.MemoryBytes)
		}
		share := int64(poolShare)
		if slices.Contains(pl.Spec.Capabilities, v1alpha1.CapabilityStateRead) || slices.Contains(pl.Spec.Capabilities, v1alpha1.CapabilityStateWrite) {
			share = statePoolShare
		}
		if mem > nodeCap*share/100 {
			if !oversized[pl.Metadata.Name] {
				oversized[pl.Metadata.Name] = true
				path := specPath(diag.Field("limits"), diag.Field("memoryBytes"))
				out.add(diag.Diagnostic{
					Code: CodeReservation, Severity: diag.SeverityError, Resource: pr.ResourceID(), Path: path,
					Location: l.position(pr, nodeOr(pr, path)),
					Message: fmt.Sprintf("Plugin %s limits.memoryBytes %d is above its pool's share of the Node Plugin memory cap (%d%% of %d bytes)",
						quote(pl.Metadata.Name), mem, share, nodeCap),
					Hint: fmt.Sprintf("lower limits.memoryBytes to at most %d bytes, or raise the Gateway's spec.limits.maxPluginMemoryBytes", nodeCap*share/100),
				})
			}
			continue
		}
		timeout := ""
		if lim := pl.Spec.Limits; lim != nil && lim.Timeout != nil {
			timeout = strconv.FormatInt(int64(*lim.Timeout), 10)
		}
		// The image's digest, not its reference: a tag is informational.
		digest := pl.Spec.Image
		if i := strings.LastIndexByte(digest, '@'); i >= 0 {
			digest = digest[i+1:]
		}
		key := digest + "\x00" + strconv.FormatInt(mem, 10) + "\x00" + timeout + "\x00" + configOf(r)
		if !pools[key] {
			pools[key] = true
			warm += mem
		}
	}
	if warm > nodeCap && ix.gateway != nil {
		path := specPath(diag.Field("limits"), diag.Field("maxPluginMemoryBytes"))
		out.add(diag.Diagnostic{
			Code: CodeReservation, Severity: diag.SeverityError, Resource: ix.gateway.ResourceID(), Path: path,
			Location: l.position(ix.gateway, nodeOr(ix.gateway, path)),
			Message:  fmt.Sprintf("the Plugin pools' warm minimums reserve %d bytes, above the Node Plugin memory cap of %d bytes", warm, nodeCap),
			Hint:     fmt.Sprintf("raise spec.limits.maxPluginMemoryBytes to at least %d bytes, or lower the Plugins' limits.memoryBytes", warm),
		})
	}
}

// nodeOr returns the tree position of path in r, or of r itself.
func nodeOr(r *hub.Resource, path diag.Path) tree.Pos {
	if n, ok := r.Tree.At(path); ok && n != nil {
		return n.Pos
	}
	return r.Tree.Pos
}

// configOf returns the canonical bytes of a Policy's spec.config.
func configOf(r *hub.Resource) string {
	n, ok := r.Tree.At(diag.Path{diag.Field("spec"), diag.Field("config")})
	if !ok {
		return ""
	}
	b, err := canonical.AppendNode(nil, n)
	if err != nil {
		return ""
	}
	return string(b)
}
