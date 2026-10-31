// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package validate

import (
	"strings"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// plugin checks a Plugin: spec.image ends in "@sha256:" and 64 lower-case
// hexadecimal characters (revision.Parse), so every Node fetches the
// bytes the Revision names (RZ-CFG-021); the tag is informational. The
// config of a plugin Policy is checked against the Plugin's configSchema
// with the plugin Policy, Planned (M2).
func (u *unit) plugin() {
	p, ok := hub.Object[v1alpha1.Plugin](u.r)
	if !ok || p == nil {
		return
	}
	if why := digestProblem(p.Spec.Image); why != "" {
		u.report(CodePlugin, specPath(diag.Field("image")), "Plugin image "+quote(p.Spec.Image)+" "+why,
			"pin the artifact by digest, as in <repository>:<tag>@sha256:<64 hex>", nil)
	}
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
