// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"context"
	"strconv"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// CodeArtifact is RZ-CFG-028: a Plugin artifact unavailable, of another
// digest, or disagreeing with its Plugin.spec; every failure of the online
// stage.
const CodeArtifact = "RZ-CFG-028"

// Online is the online stage, which follows the digest and never changes
// it (docs/architecture/02-configuration-model.md "Validation and diff
// semantics"): it fetches each Plugin artifact by digest and checks its ABI,
// exported Phases and requested Capabilities against Plugin.spec. Fetching
// Plugin artifacts is Planned (M2), so in M1 it fails each Plugin of v with
// RZ-CFG-028 at the Plugin resource, naming the Plugin, in canonical order,
// and passes a Bundle without Plugins. ruralz bundle build runs it unless
// --offline is set and ruralz bundle validate only with --online
// (Request.Online); a Node runs it at activation after its serve check.
// The error is ctx's.
func (p *Pipeline) Online(ctx context.Context, v *hub.Validated) (diag.List, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if v == nil || v.Bundle == nil {
		return nil, nil
	}
	var ds diag.List
	for _, r := range v.Bundle.Resources() {
		if r.Kind != v1alpha1.KindPlugin {
			continue
		}
		ds = append(ds, diag.Diagnostic{
			Code: CodeArtifact, Severity: diag.SeverityError, Location: r.Source.Start, Resource: r.ResourceID(),
			Message: "Plugin " + strconv.Quote(r.Name) + " cannot be checked online: fetching Plugin artifacts by digest is Planned (M2)",
		})
	}
	return ds, nil
}
