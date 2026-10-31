// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package reload

import (
	"context"
	"fmt"

	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/telemetry"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// defaultAPIKeyHeader is auth.api-key's config.header default.
const defaultAPIKeyHeader = "x-api-key"

// Telemetry is the Node's telemetry runtime as an activation switches it
// (*telemetry.Runtime).
type Telemetry interface {
	// Apply installs the Revision's telemetry settings.
	Apply(ctx context.Context, s telemetry.Settings) error
	// SetActiveRevision sets the Revision later process records carry.
	SetActiveRevision(display string)
	// SetCredentialHeaders sets the header names process logs redact
	// beside the built-in credential headers.
	SetCredentialHeaders(names []string)
}

// TelemetryPart returns the telemetry runtime as a Part: Prepare compiles
// Gateway.spec.telemetry with the Revision's resolved secrets (an OTLP
// TLS reference that does not resolve is RZ-CFG-026); Commit, right after
// the swap, installs it, so counters carry over, the OTLP connection is
// kept, and a changed endpoint or TLS setting swaps the exporter, then
// makes process records carry the new Revision and redact its auth.api-key
// header names.
func TelemetryPart(t Telemetry) Part { return telemetryPart{t: t} }

type telemetryPart struct{ t Telemetry }

func (p telemetryPart) Prepare(_ context.Context, next Compiled) (Switch, error) {
	snap := next.Snapshot()
	var spec *v1alpha1.Telemetry
	var headers []string
	if v := snap.Validated; v != nil && v.Bundle != nil {
		if g := v.Bundle.Gateway(); g != nil {
			spec = g.Spec.Telemetry
		}
		headers = apiKeyHeaders(v)
	}
	s, err := telemetry.CompileSettings(spec, next.Secrets())
	if err != nil {
		return nil, fmt.Errorf("telemetry: %w", err)
	}
	return &telemetrySwitch{t: p.t, s: s, revision: snap.Revision.Digest.Short(), headers: headers}, nil
}

// telemetrySwitch installs one Revision's telemetry settings.
type telemetrySwitch struct {
	t        Telemetry
	s        telemetry.Settings
	revision string
	headers  []string
	done     bool
}

func (s *telemetrySwitch) Commit(ctx context.Context) error {
	s.done = true
	s.t.SetActiveRevision(s.revision)
	s.t.SetCredentialHeaders(s.headers)
	if err := s.t.Apply(ctx, s.s); err != nil {
		return fmt.Errorf("telemetry: %w", err)
	}
	return nil
}

func (s *telemetrySwitch) Abort() {
	if !s.done {
		s.s.Close()
	}
}

// apiKeyHeaders returns the config.header of every auth.api-key Policy of
// the Revision, the credential headers it adds to process-log redaction.
func apiKeyHeaders(v *hub.Validated) []string {
	var names []string
	for _, p := range v.Bundle.Policies() {
		if p.Spec.Type != v1alpha1.PolicyTypeAuthAPIKey {
			continue
		}
		name := defaultAPIKeyHeader
		if r, ok := v.Bundle.Get(hub.ID{Kind: v1alpha1.KindPolicy, Name: p.Metadata.Name}); ok {
			if c, ok := r.Config.(*v1alpha1.AuthAPIKeyConfig); ok && c != nil && c.Header != nil && *c.Header != "" {
				name = *c.Header
			}
		}
		names = append(names, name)
	}
	return names
}
