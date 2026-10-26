// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"os"
	"strings"
)

// otelPrefix starts every variable the OpenTelemetry SDK reads.
const otelPrefix = "OTEL_"

// Environ is the process environment as it was before ScrubOTELEnvironment
// removed the OTEL_* variables: the lookup of ${VAR} substitution, so
// ${OTEL_EXPORTER_OTLP_ENDPOINT:-...} in a source Bundle still resolves
// (docs/architecture/02-configuration-model.md "Environment substitution").
type Environ struct {
	vars map[string]string
}

// ScrubOTELEnvironment snapshots the environment, then removes every
// variable whose name starts with OTEL_ (byte for byte), so no
// OpenTelemetry constructor takes an option from the environment (ADR-0018
// row Export). A binary calls it first, before any OpenTelemetry code runs.
func ScrubOTELEnvironment() Environ {
	env := os.Environ()
	e := Environ{vars: make(map[string]string, len(env))}
	for _, kv := range env {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || name == "" {
			continue
		}
		e.vars[name] = value
		if strings.HasPrefix(name, otelPrefix) {
			_ = os.Unsetenv(name)
		}
	}
	return e
}

// Lookup returns the value name had before the scrub.
func (e Environ) Lookup(name string) (string, bool) {
	v, ok := e.vars[name]
	return v, ok
}
