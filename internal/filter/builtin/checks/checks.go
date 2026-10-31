// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package checks is the table of offline Policy checks: the config rules
// of each Policy type that the schema cannot state, which stage H of the
// configuration pipeline runs in ruralz, ruralz-control and ruralzd
// (docs/architecture/02-configuration-model.md "Semantic validation").
// Each binary passes Table() to the pipeline (pipeline.Options.Checks), so
// internal/config never imports internal/filter and the three binaries
// give identical offline diagnostics for the same Bundle.
//
// Table() links into all three binaries, so it imports no package that
// links a module cmd/ruralz must not link
// (docs/engineering/01-tech-stack-and-libraries.md "Dependency graph"). A
// type's check lives in its Filter package unless that package links such
// a module: the auth.jwt Filter links jwx, so its check is
// internal/filter/auth/jwt/check, which the Filter's Build imports for the
// same rules. A type the schema admits whose Filter is not served yet
// keeps its check at its own path, as authz.geoip's does in
// internal/filter/authz/geoip until its Filter ships, Planned (M2)
// (docs/engineering/02-repository-layout-and-conventions.md "Package
// layers").
package checks

import (
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/filter/auth/apikey"
	jwtcheck "github.com/ravindu-rev/ruralz/internal/filter/auth/jwt/check"
	"github.com/ravindu-rev/ruralz/internal/filter/auth/upstreamoauth2"
	authzcel "github.com/ravindu-rev/ruralz/internal/filter/authz/cel"
	"github.com/ravindu-rev/ruralz/internal/filter/authz/geoip"
	"github.com/ravindu-rev/ruralz/internal/filter/authz/ip"
	"github.com/ravindu-rev/ruralz/internal/filter/cors"
	"github.com/ravindu-rev/ruralz/internal/filter/header"
	"github.com/ravindu-rev/ruralz/internal/filter/ratelimit"
	"github.com/ravindu-rev/ruralz/internal/filter/transform/request"
	"github.com/ravindu-rev/ruralz/internal/filter/transform/response"
	"github.com/ravindu-rev/ruralz/internal/filter/validation/jsonschema"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Table returns a new map holding the offline check of every Policy type
// whose config rules the configuration model registers beyond the schema,
// served or not, in foundation pack section 10 row order:
//
//   - auth.jwt: issuers and their audiences (RZ-CFG-005), an https jwksUrl
//     (RZ-CFG-037);
//   - auth.api-key: a config.header that can carry keys (RZ-CFG-005);
//   - authz.cel: a non-empty rule (RZ-CFG-005);
//   - authz.ip: CIDR or address entries, not both lists empty (RZ-CFG-005);
//   - authz.geoip: not both lists empty (RZ-CFG-005), from M1 although the
//     Filter is Planned (M2);
//   - ratelimit: at most 8 limits[] entries with distinct requests and
//     window, whole-millisecond windows, burst and perNodeCeiling ranges
//     (RZ-CFG-005);
//   - validation.json-schema: a schema the Node compiles (RZ-CFG-005);
//   - cors: origins, tokens and the credentials wildcards (RZ-CFG-005);
//   - headers, transform.request and transform.response: names, values,
//     paths, patterns and the entry limit (RZ-CFG-005);
//   - auth.upstream-oauth2: an https tokenUrl (RZ-CFG-037), a positive
//     timeout (RZ-CFG-005).
//
// The other types have no check: auth.basic and auth.upstream-sigv4 are
// validated by the schema alone, auth.mtls's subjects rules are the
// schema's (stage F), a quota config is the schema's and its consumerQuota
// a cross-resource rule of stage H (RZ-CFG-009), a cache config holds only
// CEL, which stage J compiles, the authz.opa, authz.cedar and ai.* configs
// register no rule yet, and the plugin configSchema rule ships with the
// plugin Policy, Planned (M2). Each call returns a fresh map, so a caller
// may keep or change its copy.
func Table() hub.Checks {
	return hub.Checks{
		v1alpha1.PolicyTypeAuthJWT:              jwtcheck.Check,
		v1alpha1.PolicyTypeAuthAPIKey:           apikey.Check,
		v1alpha1.PolicyTypeAuthzCEL:             authzcel.Check,
		v1alpha1.PolicyTypeAuthzIP:              ip.Check,
		v1alpha1.PolicyTypeAuthzGeoIP:           geoip.Check,
		v1alpha1.PolicyTypeRateLimit:            ratelimit.Check,
		v1alpha1.PolicyTypeValidationJSONSchema: jsonschema.Check,
		v1alpha1.PolicyTypeCORS:                 cors.Check,
		v1alpha1.PolicyTypeHeaders:              header.Check,
		v1alpha1.PolicyTypeTransformRequest:     request.Check,
		v1alpha1.PolicyTypeTransformResponse:    response.Check,
		v1alpha1.PolicyTypeAuthUpstreamOAuth2:   upstreamoauth2.Check,
	}
}
