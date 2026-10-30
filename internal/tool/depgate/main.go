// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Command depgate runs the dependency gates of CI stage 6 over the packages
// each binary links, per shipped platform (`go list -deps -test=false` with
// CGO_ENABLED=0), as docs/engineering/01-tech-stack-and-libraries.md fixes:
//
//		depgate [-C DIR] [-notices DIR]
//
//	  - G2 license gate: Apache-2.0, MIT, BSD-2-Clause, BSD-3-Clause or ISC;
//	    MPL-2.0 only for the named modules; EDL-1.0 where elected. A linked
//	    package is governed by the license texts of the nearest directory,
//	    from its own up to its module root, that holds one;
//	  - G3 crypto denylist: golang.org/x/crypto only on the delegation
//	    allowlist, and no other non-standard package with "crypto" in its
//	    path unless it has an exception row;
//	  - the ruralzd Raft denylist (P2: the gateway never links Raft);
//	  - the ruralz jwx denylist: the offline Policy checks every binary
//	    links import no module the CLI does not link, so the CLI links no
//	    JOSE;
//	  - advisory version floors;
//	  - no test support in any binary: this module's internal/testkit,
//	    internal/tool and test/ trees and its "<name>test" packages, the
//	    standard library's testing packages and the test-only modules.
//
// The targets are ruralzd and ruralz-control on linux/amd64, linux/arm64,
// darwin/arm64 and darwin/amd64, and ruralz on those and windows/amd64.
// With -notices, once every gate passes, depgate writes each artifact's
// THIRD_PARTY_LICENSES and NOTICE (ADR-0002 release audit) into
// DIR/<binary>_<os>_<arch>/; DIR must be absent or empty. THIRD_PARTY_LICENSES
// reproduces the license texts of the Go standard library and of every
// linked module with the address of its source, stating where MPL-2.0
// source is available; NOTICE is the repository NOTICE followed by the
// linked modules' NOTICE texts and the license elections.
//
// Exit status: 0 pass, 1 findings, 2 usage or I/O error.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := runMain(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// runMain parses the flags, runs the gates and returns the exit status.
func runMain(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("depgate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var o options
	fs.StringVar(&o.dir, "C", ".", "module root")
	fs.StringVar(&o.notices, "notices", "", "write each artifact's THIRD_PARTY_LICENSES and NOTICE under this directory once the gates pass")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "depgate: unexpected arguments %q\n", fs.Args())
		return 2
	}
	findings, err := run(ctx, o)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "depgate:", err)
		return 2
	}
	for _, f := range findings {
		_, _ = fmt.Fprintln(stderr, f)
	}
	if len(findings) > 0 {
		_, _ = fmt.Fprintf(stderr, "depgate: %d finding(s)\n", len(findings))
		return 1
	}
	_, _ = fmt.Fprintln(stdout, "depgate: G2, G3, the Raft and jwx denylists, advisory floors and test support denial pass")
	if o.notices != "" {
		_, _ = fmt.Fprintf(stdout, "depgate: wrote THIRD_PARTY_LICENSES and NOTICE of %d artifacts under %s\n", len(targets()), o.notices)
	}
	return 0
}
