// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build tools

// Package modpin keeps the M1 third-party modules in go.mod while their first
// importer is not written yet: go mod tidy keeps requirements imported only
// from this tools-tagged file, and no binary links it. The file is deleted
// once every module has a real importer.
package modpin

import (
	_ "github.com/redis/rueidis"
	_ "go.opentelemetry.io/otel/metric"
	_ "golang.org/x/net/http2"
	_ "golang.org/x/sys/cpu"
)
