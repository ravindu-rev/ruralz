// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package celtypes

import (
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"

	"github.com/ravindu-rev/ruralz/internal/expr"
)

// prepared is the CEL implementation's cache in expr.Route.Prepared and
// expr.Consumer.Prepared: the CEL values of the view's fields, converted
// once per snapshot and shared read-only, so selecting a field, reading a
// label or testing a tag allocates nothing. A string boxed into a ref.Val
// costs an allocation, which a lazy view pays on every selection.
type prepared struct {
	// name is metadata.name; tier is the Consumer's spec.tier.
	name, tier ref.Val
	// labels is metadata.labels: keys sorted once, values converted.
	labels ref.Val
	// tags and quotas are the Consumer's lists, elements converted.
	tags, quotas ref.Val
}

// PrepareRoute caches the CEL values of r in r.Prepared once per snapshot,
// so route.name and route.labels read without converting or sorting per
// request. Call it while building the snapshot, before any evaluation
// reads r; r must not change afterwards. A Route without it reads the same
// values, converting on selection.
func PrepareRoute(r *expr.Route) {
	if r != nil {
		r.Prepared = &prepared{name: types.String(r.Name), labels: preparedLabels(r.Labels)}
	}
}

// PrepareConsumer caches the CEL values of c in c.Prepared, as PrepareRoute
// does for a Route: consumer.name, tier, tags, labels and quotas.
func PrepareConsumer(c *expr.Consumer) {
	if c != nil {
		c.Prepared = &prepared{
			name:   types.String(c.Name),
			tier:   types.String(c.Tier),
			labels: preparedLabels(c.Labels),
			tags:   preparedList(c.Tags),
			quotas: preparedList(c.Quotas),
		}
	}
}

// preparedLabels returns the labels view with its keys sorted and its
// values converted once.
func preparedLabels(m map[string]string) ref.Val {
	s := &sortedStrings{m: m, keys: sortedMapKeys(m), vals: make(map[string]ref.Val, len(m))}
	for k, v := range m {
		s.vals[k] = types.String(v)
	}
	return mapView[*sortedStrings]{s}
}

// preparedList returns the list view of s with its elements converted
// once.
func preparedList(s []string) ref.Val {
	l := &valueList{strs: s, vals: make([]ref.Val, len(s))}
	for i, v := range s {
		l.vals[i] = types.String(v)
	}
	return listView[*valueList]{l}
}
