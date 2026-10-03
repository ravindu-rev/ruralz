// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"maps"
	"slices"

	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// CompileConsumer returns the read-only view of c exposed to CEL and other
// Filters (spec 06 requirement 21; docs/architecture/02-configuration-model.md
// "Variables", consumer): name, tier, tags (sorted, duplicates removed),
// labels (metadata.labels), quotas (the quota names, sorted) and, for the
// quota and ai.token-budget Policies, the quotas by name (the first entry
// wins on a duplicate name). Slices and maps are copies, so the view does
// not alias the Bundle; they are never nil, except QuotaByName, which is
// nil when there are no quotas.
func CompileConsumer(c *v1alpha1.Consumer) *expr.Consumer {
	tags := slices.Clone(c.Spec.Tags)
	if tags == nil {
		tags = []string{}
	}
	slices.Sort(tags)
	tags = slices.Compact(tags)

	quotas := make([]string, 0, len(c.Spec.Quotas))
	var byName map[string]v1alpha1.Quota
	if len(c.Spec.Quotas) > 0 {
		byName = make(map[string]v1alpha1.Quota, len(c.Spec.Quotas))
	}
	for _, q := range c.Spec.Quotas {
		quotas = append(quotas, q.Name)
		if _, dup := byName[q.Name]; !dup {
			byName[q.Name] = q
		}
	}
	slices.Sort(quotas)
	quotas = slices.Compact(quotas)

	labels := maps.Clone(c.Metadata.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	return &expr.Consumer{
		Name:        c.Metadata.Name,
		Tier:        c.Spec.Tier,
		Tags:        tags,
		Labels:      labels,
		Quotas:      quotas,
		QuotaByName: byName,
	}
}

// Consumers is the compiled Consumer set of one snapshot: the expr.Consumer
// views by name, each carrying its quotas by name for the quota and
// ai.token-budget Policies (spec 06 requirement 21). It is immutable and
// safe for concurrent reads.
type Consumers struct {
	byName map[string]*expr.Consumer
}

// CompileConsumers compiles cs; a duplicate name keeps the first (the
// Bundle rejects duplicates with RZ-CFG-008 before this runs).
func CompileConsumers(cs []*v1alpha1.Consumer) *Consumers {
	s := &Consumers{byName: make(map[string]*expr.Consumer, len(cs))}
	for _, c := range cs {
		if c == nil {
			continue
		}
		if _, dup := s.byName[c.Metadata.Name]; !dup {
			s.byName[c.Metadata.Name] = CompileConsumer(c)
		}
	}
	return s
}

// CompileBundle compiles the Consumers of b; a nil b gives an empty set.
func CompileBundle(b *hub.Bundle) *Consumers {
	if b == nil {
		return CompileConsumers(nil)
	}
	return CompileConsumers(b.Consumers())
}

// Map returns the views by name, as snapshot.Snapshot.Consumers and
// filter.BuildEnv.Consumers hold them. The map is shared: read it only.
func (s *Consumers) Map() map[string]*expr.Consumer { return s.byName }

// Get returns the named Consumer's view.
func (s *Consumers) Get(name string) (*expr.Consumer, bool) {
	c, ok := s.byName[name]
	return c, ok
}

// Len returns the number of Consumers.
func (s *Consumers) Len() int { return len(s.byName) }

// Quota returns the quota named quota of the Consumer named consumer, read
// from the view's QuotaByName; false when either is unknown (RZ-RL-004 or
// RZ-AI-007 for the calling Policy).
func (s *Consumers) Quota(consumer, quota string) (v1alpha1.Quota, bool) {
	c, ok := s.byName[consumer]
	if !ok {
		return v1alpha1.Quota{}, false
	}
	q, ok := c.QuotaByName[quota]
	return q, ok
}
