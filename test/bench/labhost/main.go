// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Command labhost is the benchmark lab's helper binary, which the harness
// builds for each run and copies to the lab hosts; lab holds its commands.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/ravindu-rev/ruralz/test/bench/labhost/lab"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := lab.Main(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
