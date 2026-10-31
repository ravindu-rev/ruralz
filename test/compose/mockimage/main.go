// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Command mockimage is the entry point of the Compose suite's test-only mock
// image, which the suite builds from it on a scratch base and never
// publishes: it only wires the process to package
// github.com/ravindu-rev/ruralz/test/compose/mock.
package main

import (
	"context"
	"os"

	"github.com/ravindu-rev/ruralz/test/compose/mock"
)

func main() {
	os.Exit(mock.Main(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}
