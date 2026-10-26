// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"fmt"

	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// Service is the binary a Runtime serves; it is the resource's
// service.name.
type Service uint8

// Services.
const (
	// ServiceGateway is ruralzd, Ruralz Gateway.
	ServiceGateway Service = iota + 1
	// ServiceControl is ruralz-control, Ruralz Control; Planned (M2).
	ServiceControl
)

// Name returns the service.name: the binary name.
func (s Service) Name() string {
	switch s {
	case ServiceGateway:
		return "ruralzd"
	case ServiceControl:
		return "ruralz-control"
	default:
		return ""
	}
}

// newResource returns the resource every provider shares: service.name,
// service.version and service.instance.id, under the schema URL of the
// semantic conventions the SDK uses, and nothing else (no detectors, never
// the Revision; docs/architecture/10-observability.md "Telemetry
// pipeline").
func newResource(s Service, version, instance string) (*resource.Resource, error) {
	if s.Name() == "" {
		return nil, fmt.Errorf("telemetry: unknown service %d", s)
	}
	if instance == "" {
		return nil, fmt.Errorf("telemetry: %s has no instance ID", s.Name())
	}
	return resource.NewWithAttributes(semconv.SchemaURL,
		semconv.ServiceName(s.Name()),
		semconv.ServiceVersion(version),
		semconv.ServiceInstanceID(instance),
	), nil
}
