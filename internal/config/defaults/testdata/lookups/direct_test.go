// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package lookups

import "github.com/ravindu-rev/ruralz/internal/config/tree"

// inTest stands for a test helper: the detector skips test files.
func inTest(n *tree.Node) { n.Get("a") }
