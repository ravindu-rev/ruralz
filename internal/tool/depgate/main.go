// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Command depgate runs the dependency gates of CI stage 6 over the packages
// each binary links, per shipped platform (`go list -deps -test=false` with
// CGO_ENABLED=0), as docs/engineering/01-tech-stack-and-libraries.md fixes:
//
//   - G2 license gate: Apache-2.0, MIT, BSD-2-Clause, BSD-3-Clause or ISC;
//     MPL-2.0 only for the named modules; EDL-1.0 where elected;
//   - G3 crypto denylist: golang.org/x/crypto only on the delegation
//     allowlist, and no other non-standard package with "crypto" in its
//     path unless it has an exception row;
//   - the ruralzd Raft denylist (P2: the gateway never links Raft);
//   - advisory version floors.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	findings, err := run(ctx, ".")
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "depgate:", err)
		os.Exit(2)
	}
	for _, f := range findings {
		fmt.Fprintln(os.Stderr, f)
	}
	if len(findings) > 0 {
		fmt.Fprintf(os.Stderr, "depgate: %d finding(s)\n", len(findings))
		os.Exit(1)
	}
	_, _ = fmt.Fprintln(os.Stdout, "depgate: G2, G3, Raft denylist and advisory floors pass")
}
