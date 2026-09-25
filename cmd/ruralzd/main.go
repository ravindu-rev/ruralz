// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Command ruralzd runs Ruralz Gateway (the data plane). It only wires the process to
// package gateway; logic lives under internal/.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/ravindu-rev/ruralz/internal/gateway"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := gateway.Run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
