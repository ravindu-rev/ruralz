// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package celtypes

import (
	"cel.dev/cel-go/common/types/ref"

	"github.com/ravindu-rev/ruralz/internal/expr"
)

// prepared is the CEL implementation's cache in expr.Route.Prepared and
// expr.Consumer.Prepared: values converted once per snapshot and shared
// read-only (03 req 23).
type prepared struct {
	// labels is metadata.labels with its keys sorted once.
	labels ref.Val
}

// PrepareRoute caches the CEL values of r in r.Prepared once per snapshot,
// so route.labels iterates without sorting per request. Call it while
// building the snapshot, before any evaluation reads r; r.Labels must not
// change afterwards. A Route without it reads the same values, converting
// on selection.
func PrepareRoute(r *expr.Route) {
	if r != nil {
		r.Prepared = &prepared{labels: sortedLabels(r.Labels)}
	}
}

// PrepareConsumer caches the CEL values of c in c.Prepared, as PrepareRoute
// does for a Route (consumer.labels; tags and quotas are sorted slices the
// views read in place).
func PrepareConsumer(c *expr.Consumer) {
	if c != nil {
		c.Prepared = &prepared{labels: sortedLabels(c.Labels)}
	}
}

// sortedLabels returns the labels view with its keys sorted once.
func sortedLabels(m map[string]string) ref.Val {
	return mapView[*sortedStrings]{&sortedStrings{m: m, keys: sortedMapKeys(m)}}
}
