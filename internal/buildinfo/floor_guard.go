// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build go1.26 && !go1.27 && !goexperiment.jsonv2

package buildinfo

// On Go 1.26 the floor build needs GOEXPERIMENT=jsonv2, because jwx v4 uses
// encoding/json/v2 (docs/engineering/01-tech-stack-and-libraries.md, Version
// floor). The undefined identifier below names the fix; use `make floor`.
var _ = floorBuildRequires_GOEXPERIMENT_jsonv2_see_make_floor
