// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package lookups

import "github.com/ravindu-rev/ruralz/internal/config/tree"

// counted stands for the counted lookups of the package: the detector
// skips lookup.go.
func counted(n *tree.Node) { n.Get("a") }
