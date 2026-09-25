// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Command ruralz runs the ruralz CLI. It only wires the process to
// package cli; logic lives under internal/.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/ravindu-rev/ruralz/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
