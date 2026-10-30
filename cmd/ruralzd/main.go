// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Command ruralzd runs Ruralz Gateway (the data plane). It only wires the process to
// package gateway; logic lives under internal/. The Node handles its own signals:
// SIGTERM and SIGINT start its Drain and SIGHUP reloads its configuration.
package main

import (
	"context"
	"os"

	"github.com/ravindu-rev/ruralz/internal/gateway"
)

func main() {
	os.Exit(gateway.Run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}
