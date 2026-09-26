// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build tools

// Package modpin keeps the M1 third-party modules in go.mod while their first
// importer is still a later work package: go mod tidy keeps requirements
// imported only from this tools-tagged file, and no binary links it. The
// file is deleted once every module has a real importer.
package modpin

import (
	_ "cel.dev/cel-go/cel"
	_ "github.com/goccy/go-yaml"
	_ "github.com/lestrrat-go/jwx/v4/jwk"
	_ "github.com/prometheus/client_golang/prometheus"
	_ "github.com/prometheus/common/expfmt"
	_ "github.com/prometheus/otlptranslator"
	_ "github.com/redis/rueidis"
	_ "github.com/santhosh-tekuri/jsonschema/v6"
	_ "github.com/testcontainers/testcontainers-go"
	_ "github.com/testcontainers/testcontainers-go/modules/redis"
	_ "github.com/testcontainers/testcontainers-go/modules/valkey"
	_ "go.opentelemetry.io/contrib/bridges/otelslog"
	_ "go.opentelemetry.io/otel"
	_ "go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	_ "go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	_ "go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	_ "go.opentelemetry.io/otel/exporters/prometheus"
	_ "go.opentelemetry.io/otel/log"
	_ "go.opentelemetry.io/otel/metric"
	_ "go.opentelemetry.io/otel/sdk/log"
	_ "go.opentelemetry.io/otel/sdk/metric"
	_ "go.opentelemetry.io/otel/sdk/trace"
	_ "go.opentelemetry.io/otel/trace"
	_ "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	_ "golang.org/x/net/http2"
	_ "golang.org/x/sys/cpu"
	_ "google.golang.org/grpc"
)
