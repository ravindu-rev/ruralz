// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package apikey

import (
	"errors"
	"fmt"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/filter/auth"
	"github.com/ravindu-rev/ruralz/internal/httpfield"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

var _ hub.PolicyCheck = Check

// Check is the offline check of an auth.api-key Policy (a hub.PolicyCheck
// run at stage H by every binary): a config.header that cannot carry keys
// (checkHeader) is RZ-CFG-005 at spec.config.header. It reads no file or
// network. A resource whose Config is not a decoded AuthAPIKeyConfig
// yields nothing; the schema reports malformed members.
func Check(r *hub.Resource, loc func(diag.Path) diag.Location, add func(diag.Diagnostic)) {
	if r == nil || add == nil {
		return
	}
	cfg, ok := r.Config.(*v1alpha1.AuthAPIKeyConfig)
	if !ok || cfg == nil {
		return
	}
	err := checkHeader(cfg.Header)
	if err == nil {
		return
	}
	p := diag.Path{diag.Field("spec"), diag.Field("config"), diag.Field("header")}
	var l diag.Location
	if loc != nil {
		l = loc(p)
	}
	add(diag.Diagnostic{
		Code:     codeSchema,
		Severity: diag.SeverityError,
		Location: l,
		Resource: r.ResourceID(),
		Path:     p,
		Message:  err.Error(),
	})
}

// checkHeader applies the field-name rule of the headers Policy to the
// header an auth.api-key Policy reads (config.header, absent or empty
// meaning x-api-key): the name must be an RFC 9110 token of at most
// httpfield.MaxNameBytes bytes and no field the Node manages
// (httpfield.Protect: pseudo-headers, host, the hop-by-hop fields,
// content-length, expect, traceparent and tracestate). It returns why the
// name fails, or nil.
func checkHeader(configured *string) error {
	name := auth.APIKeyHeader(configured)
	if err := httpfield.CheckName(name); err != nil {
		return fmt.Errorf("header %q is not a field name: %w", name, err)
	}
	if p := httpfield.Protect(name); p != httpfield.Unprotected {
		return errors.New(p.Message(name))
	}
	return nil
}
