// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Command ruralz runs the ruralz CLI. It only wires the process to
// package cli, which also handles SIGINT and SIGTERM; logic lives under
// internal/.
package main

import (
	"context"
	"os"

	"github.com/ravindu-rev/ruralz/internal/cli"
)

func main() {
	os.Exit(cli.Run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}
