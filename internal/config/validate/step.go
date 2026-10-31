// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package validate

import (
	"errors"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/jsonval"
	"github.com/ravindu-rev/ruralz/internal/routematch"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// step checks one composition step at at, each failure RZ-CFG-005 at its
// field, so the composition engine never refuses what validation passed
// (docs/architecture/02-configuration-model.md "Route" and "Semantic
// validation"; docs/architecture/03-data-plane.md "Composition engine"):
//
//   - a literal path starts with "/" and passes the hardening of the
//     client path with no "." or ".." segment in any spelling
//     (routematch.CheckForwardPath), the rule the engine applies to a
//     pathExpression result at runtime; the query after its first "?"
//     holds no raw control byte, which no request line can carry
//     (routematch.CheckForwardQuery);
//   - target is a dot path (jsonval.ParseDotPath): no empty segment, no
//     escape other than "\.", "\*" and "\\", no "*" inside a longer
//     segment;
//   - collection: true has a non-empty group: the schema states the rule
//     for an absent group (stage F), and an empty one is no group either.
func (u *unit) step(at diag.Path, s *v1alpha1.CompositionStep) {
	u.w.charge(1)
	if p := at.Append(diag.Field("path")); s.Path != "" || u.f.node(p) != nil {
		if msg := stepPathText(s.Path); msg != "" {
			u.report(CodeSchema, p, msg, "", nil)
		}
	}
	if s.Target != "" {
		if _, err := jsonval.ParseDotPath(s.Target); err != nil {
			u.report(CodeSchema, at.Append(diag.Field("target")),
				"target "+quote(s.Target)+" is not a valid dot path ("+dotPathReason(err)+")",
				`"." separates segments; "\." writes a literal dot, "\*" a literal star and "\\" a backslash`, nil)
		}
	}
	if s.Collection != nil && *s.Collection && s.Group == "" {
		if g := at.Append(diag.Field("group")); u.f.node(g) != nil {
			u.report(CodeSchema, g, "step "+quote(s.Name)+" sets collection: true with an empty group",
				"name the key the array merges under", nil)
		}
	}
}

// stepPathText returns the finding for a literal step path, "" when it
// passes.
func stepPathText(path string) string {
	p, q, hasQuery := strings.Cut(path, "?")
	if err := routematch.CheckForwardPath(p); err != nil {
		if pe := (*routematch.PathError)(nil); errors.As(err, &pe) && pe.Reason == routematch.RejectNotOriginForm {
			return "step path " + quote(path) + " does not begin with /"
		}
		return "step path " + quote(path) + " is not a valid path (" + pathReason(err) + ")"
	}
	if hasQuery {
		if err := routematch.CheckForwardQuery(q); err != nil {
			return "step path " + quote(path) + " holds a control byte in its query"
		}
	}
	return ""
}

// pathReason names the hardening rule err broke.
func pathReason(err error) string {
	if pe := (*routematch.PathError)(nil); errors.As(err, &pe) {
		return pe.Reason.String()
	}
	return err.Error()
}

// dotPathReason names the dot path rule err broke.
func dotPathReason(err error) string {
	switch {
	case errors.Is(err, jsonval.ErrEmptySegment):
		return "empty segment"
	case errors.Is(err, jsonval.ErrBadEscape):
		return "invalid escape"
	case errors.Is(err, jsonval.ErrBareStar):
		return "unescaped * inside a segment"
	default:
		return err.Error()
	}
}
