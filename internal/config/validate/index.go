// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package validate

import (
	"slices"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// bundleKinds are the kinds a Bundle holds, in canonical order. Environment
// and Cluster live outside it (RZ-CFG-017 at stage C), so stage H skips
// them.
func bundleKinds() []v1alpha1.Kind {
	return []v1alpha1.Kind{
		v1alpha1.KindGateway, v1alpha1.KindUpstream, v1alpha1.KindPlugin, v1alpha1.KindPolicy,
		v1alpha1.KindConsumer, v1alpha1.KindAIProvider, v1alpha1.KindAIModel, v1alpha1.KindRoute,
	}
}

// inBundle reports whether k is a Bundle kind.
func inBundle(k v1alpha1.Kind) bool { return slices.Contains(bundleKinds(), k) }

// maxHintBytes is the longest name a nearest-match hint is computed for:
// the longest metadata.name (an RFC 1123 label of 63 characters) plus
// one. A longer reference can name no resource, and
// comparing long strings costs time quadratic in their length.
const maxHintBytes = 64

// maxCandidates is how many names diag.Nearest compares at most; a hint
// gathers no more, so one hint costs the same however many names a
// Bundle holds.
const maxCandidates = 64

// nameSet is the hint candidates of one namespace (the names of a kind, a
// Gateway's listeners, the quotas of one unit): sorted and bucketed by
// byte length, and indexed by lower case for the case-insensitive match
// diag.Nearest prefers.
type nameSet struct {
	byLen map[int][]string
	lower map[string]string
}

// newNameSet returns the set of names.
func newNameSet(names []string) *nameSet {
	s := &nameSet{byLen: map[int][]string{}, lower: map[string]string{}}
	for _, n := range names {
		if len(n) <= maxHintBytes+3 {
			s.byLen[len(n)] = append(s.byLen[len(n)], n)
		}
	}
	for l := range s.byLen {
		slices.Sort(s.byLen[l])
		s.byLen[l] = slices.Compact(s.byLen[l])
		for _, n := range s.byLen[l] {
			if _, seen := s.lower[strings.ToLower(n)]; !seen {
				s.lower[strings.ToLower(n)] = n
			}
		}
	}
	return s
}

// hint returns `did you mean "<name>"?` for a name equal to input but for
// case, else for the nearest of the first maxCandidates names within 3
// bytes of its length (the same length first, then 1, 2 and 3 bytes
// shorter or longer, each in byte order), as diag.Nearest picks it; ""
// when none is near.
func (s *nameSet) hint(input string) string {
	if s == nil || len(input) > maxHintBytes {
		return ""
	}
	if n, ok := s.lower[strings.ToLower(input)]; ok {
		return "did you mean " + quote(n) + "?"
	}
	if best, ok := nearest(input, s.candidates(input)); ok {
		return "did you mean " + quote(best) + "?"
	}
	return ""
}

// candidates returns the first maxCandidates names within 3 bytes of
// input's length, in hint order.
func (s *nameSet) candidates(input string) []string {
	cands := make([]string, 0, maxCandidates)
	add := func(l int) {
		bucket := s.byLen[l]
		cands = append(cands, bucket[:min(len(bucket), maxCandidates-len(cands))]...)
	}
	add(len(input))
	for d := 1; d <= 3 && len(cands) < maxCandidates; d++ {
		add(len(input) - d)
		add(len(input) + d)
	}
	return cands
}

// quotaDecl is the first Consumer, in canonical order, that defines a
// quota name with one unit.
type quotaDecl struct {
	consumer string
	loc      diag.Location
}

// setting is a Gateway value with the location of its authored field.
type setting struct {
	value int64
	// authored reports a value written in the Bundle, not a default.
	authored bool
	// loc is the authored field's location.
	loc diag.Location
}

// related returns the "<what> <location>" pointer of an authored setting.
func (s setting) related(what string) []diag.Related {
	if !s.authored {
		return nil
	}
	return []diag.Related{{Location: s.loc, Message: what}}
}

// bundleIndex is what the checks of one run read about the whole Bundle.
// It is built before the workers start and only read afterwards.
type bundleIndex struct {
	// resources are the Bundle's resources in canonical order.
	resources []*hub.Resource
	// byKind maps each Bundle kind to its resources by name.
	byKind map[v1alpha1.Kind]map[string]*hub.Resource
	// names are the hint candidates of each kind.
	names map[v1alpha1.Kind]*nameSet
	// gateway is the Bundle's Gateway resource and gw its typed view; nil
	// without one (RZ-CFG-016 at stage C).
	gateway *hub.Resource
	gw      *v1alpha1.Gateway
	// listeners are the Gateway's listener names in list order, and
	// listenerSet their positions.
	listeners     []string
	listenerSet   map[string]int
	listenerNames *nameSet
	// adminPort, maxSteps and maxResponse are the Gateway's admin.port,
	// limits.maxCompositionSteps and limits.maxResponseBodyBytes, or
	// their schema defaults.
	adminPort   setting
	maxSteps    setting
	maxResponse setting
	// quotas maps a quota name and unit to its first declaration, and
	// quotaNames holds the names per unit for hints.
	quotas     map[string]map[v1alpha1.QuotaUnit]quotaDecl
	quotaNames map[v1alpha1.QuotaUnit]*nameSet
	// badMatch marks, by resource index, the Routes whose match criteria
	// failed an RZ-CFG-005 rule; each worker writes only its own indexes,
	// and the cross-resource passes read them after the workers end.
	badMatch []bool
}

// index builds the bundle index of b; files locates authored settings.
func (v *Validator) index(b *hub.Bundle, files *tree.FileTable, w *work) *bundleIndex {
	ix := &bundleIndex{
		resources: b.Resources(),
		byKind:    map[v1alpha1.Kind]map[string]*hub.Resource{},
		names:     map[v1alpha1.Kind]*nameSet{},
		quotas:    map[string]map[v1alpha1.QuotaUnit]quotaDecl{},
	}
	ix.badMatch = make([]bool, len(ix.resources))
	names := map[v1alpha1.Kind][]string{}
	quotaNames := map[v1alpha1.QuotaUnit][]string{}
	l := locator{files: files}
	for _, r := range ix.resources {
		w.charge(1)
		if r == nil || !inBundle(r.Kind) {
			continue
		}
		m := ix.byKind[r.Kind]
		if m == nil {
			m = map[string]*hub.Resource{}
			ix.byKind[r.Kind] = m
		}
		if _, dup := m[r.Name]; !dup {
			m[r.Name] = r
			names[r.Kind] = append(names[r.Kind], r.Name)
		}
		switch r.Kind {
		case v1alpha1.KindGateway:
			if g, ok := hub.Object[v1alpha1.Gateway](r); ok && g != nil && ix.gateway == nil {
				ix.gateway, ix.gw = r, g
			}
		case v1alpha1.KindConsumer:
			c, ok := hub.Object[v1alpha1.Consumer](r)
			if !ok || c == nil {
				continue
			}
			f := l.newFinder(r, w)
			for _, q := range c.Spec.Quotas {
				w.charge(1)
				units := ix.quotas[q.Name]
				if units == nil {
					units = map[v1alpha1.QuotaUnit]quotaDecl{}
					ix.quotas[q.Name] = units
				}
				if _, seen := units[q.Unit]; !seen {
					loc := f.path(diag.Path{diag.Field("spec"), diag.Field("quotas"), diag.Keyed("name", q.Name)})
					units[q.Unit] = quotaDecl{consumer: r.Name, loc: loc}
					quotaNames[q.Unit] = append(quotaNames[q.Unit], q.Name)
				}
			}
		default:
		}
	}
	for k, ns := range names {
		ix.names[k] = newNameSet(ns)
	}
	ix.quotaNames = map[v1alpha1.QuotaUnit]*nameSet{}
	for u, ns := range quotaNames {
		ix.quotaNames[u] = newNameSet(ns)
	}
	ix.listenerSet = map[string]int{}
	ix.adminPort = setting{value: v.defaults.adminPort}
	ix.maxSteps = setting{value: v.defaults.maxSteps}
	ix.maxResponse = setting{value: v.defaults.maxResponse}
	if ix.gw == nil {
		return ix
	}
	for i, li := range ix.gw.Spec.Listeners {
		if _, dup := ix.listenerSet[li.Name]; !dup {
			ix.listenerSet[li.Name] = i
			ix.listeners = append(ix.listeners, li.Name)
		}
	}
	ix.listenerNames = newNameSet(ix.listeners)
	f := l.newFinder(ix.gateway, w)
	spec := ix.gw.Spec
	if a := spec.Admin; a != nil && a.Port != nil {
		ix.adminPort = gatewaySetting(f, int64(*a.Port), "admin", "port")
	}
	if lim := spec.Limits; lim != nil {
		if lim.MaxCompositionSteps != nil {
			ix.maxSteps = gatewaySetting(f, int64(*lim.MaxCompositionSteps), "limits", "maxCompositionSteps")
		}
		if lim.MaxResponseBodyBytes != nil {
			ix.maxResponse = gatewaySetting(f, int64(*lim.MaxResponseBodyBytes), "limits", "maxResponseBodyBytes")
		}
	}
	return ix
}

// gatewaySetting returns the Gateway value at spec.<fields...>, authored
// when its tree node exists and no default produced it.
func gatewaySetting(f *finder, value int64, fields ...string) setting {
	p := diag.Path{diag.Field("spec")}
	for _, name := range fields {
		p = append(p, diag.Field(name))
	}
	n := f.node(p)
	return setting{value: value, authored: n != nil && n.Style != tree.StyleDefaulted, loc: f.path(p)}
}

// lookup returns the resource of kind k named name.
func (ix *bundleIndex) lookup(k v1alpha1.Kind, name string) (*hub.Resource, bool) {
	r, ok := ix.byKind[k][name]
	return r, ok
}

// otherKind returns the first resource, in canonical kind order, named
// name whose kind is not k: the target of a wrong-kind reference.
func (ix *bundleIndex) otherKind(k v1alpha1.Kind, name string) (*hub.Resource, bool) {
	for _, other := range bundleKinds() {
		if other == k {
			continue
		}
		if r, ok := ix.byKind[other][name]; ok {
			return r, true
		}
	}
	return nil, false
}
