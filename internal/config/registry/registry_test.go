// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Tests for WP-04 "Done when": the table equals foundation pack section 10
// row by row (01 test plan "registry table equals FP §10 row by row"; 02
// req 13), every type has a config type (config_test.go), and the served
// set is table-tested (architecture section 0 item 8, R-7).

// fpRow is one expanded row of foundation pack section 10.
type fpRow struct {
	typ     v1alpha1.PolicyType
	class   string // "auth", or "filterClass" for plugin
	phases  string
	scopes  string
	slot    string
	failure string
	planned string
}

// readFoundationPackRegistry parses the section 10 table of the binding
// foundation pack, expanding rows that list several types.
func readFoundationPackRegistry(t *testing.T) []fpRow {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "_meta", "foundation-pack.md"))
	if err != nil {
		t.Fatalf("read foundation pack: %v", err)
	}
	backticked := regexp.MustCompile("`([^`]+)`")
	var rows []fpRow
	in := false
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "## ") {
			in = strings.HasPrefix(line, "## 10. Policy type registry")
			continue
		}
		if !in || !strings.HasPrefix(line, "| `") || strings.HasPrefix(line, "| `type`") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		if len(cells) != 7 {
			t.Fatalf("foundation pack section 10 row has %d cells: %q", len(cells), line)
		}
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		for _, m := range backticked.FindAllStringSubmatch(cells[0], -1) {
			rows = append(rows, fpRow{
				typ: v1alpha1.PolicyType(m[1]), class: strings.Trim(cells[1], "`"), phases: cells[2],
				scopes: cells[3], slot: strings.Trim(cells[4], "`"), failure: cells[5], planned: cells[6],
			})
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatal("foundation pack section 10 table not found")
	}
	return rows
}

// parsePhases parses "onRequestHeaders, onResponse".
func parsePhases(t *testing.T, s string) phase.Set {
	t.Helper()
	var out phase.Set
	for _, f := range strings.Split(s, ",") {
		p, ok := phase.Parse(v1alpha1.Phase(strings.TrimSpace(f)))
		if !ok {
			t.Fatalf("unknown Phase %q in %q", f, s)
		}
		out = out.Add(p)
	}
	return out
}

// parseScopes parses "G, R, U".
func parseScopes(t *testing.T, s string) phase.ScopeSet {
	t.Helper()
	var out []phase.Scope
	for _, f := range strings.Split(s, ",") {
		switch strings.TrimSpace(f) {
		case "G":
			out = append(out, phase.ScopeGateway)
		case "R":
			out = append(out, phase.ScopeRoute)
		case "U":
			out = append(out, phase.ScopeUpstream)
		default:
			t.Fatalf("unknown scope %q in %q", f, s)
		}
	}
	return phase.Scopes(out...)
}

// TestTableMatchesFoundationPack is the WP-04 "Done when" table test: the
// registry equals foundation pack section 10 row by row, in row order (02
// req 13; 01 req 40 slots; OQ-configuration-model-8 (a) defaults).
func TestTableMatchesFoundationPack(t *testing.T) {
	reg := New()
	rows := readFoundationPackRegistry(t)
	var fpTypes []v1alpha1.PolicyType
	for _, row := range rows {
		fpTypes = append(fpTypes, row.typ)
	}
	if got := reg.Types(); !slices.Equal(got, fpTypes) {
		t.Fatalf("registry types = %v\nfoundation pack types = %v", got, fpTypes)
	}
	planned := regexp.MustCompile(`^Planned \((M[0-9])\)$`)
	for _, row := range rows {
		t.Run(string(row.typ), func(t *testing.T) {
			e, ok := reg.Lookup(row.typ)
			if !ok {
				t.Fatal("not registered")
			}
			if row.class == "filterClass" {
				if !e.ClassFromSpec || e.Class != phase.ClassCustom {
					t.Errorf("class: ClassFromSpec=%v Class=%v, want spec.filterClass with default custom", e.ClassFromSpec, e.Class)
				}
			} else if c, ok := phase.ParseClass(v1alpha1.FilterClass(row.class)); !ok || e.Class != c || e.ClassFromSpec {
				t.Errorf("class = %v (from spec %v), want %q", e.Class, e.ClassFromSpec, row.class)
			}

			var wantScopes phase.ScopeSet
			if row.scopes == "Scopes matching those Phases" {
				wantScopes = phase.Scopes(phase.ScopeGateway, phase.ScopeRoute, phase.ScopeUpstream)
			} else {
				wantScopes = parseScopes(t, row.scopes)
			}
			if e.Scopes != wantScopes {
				t.Errorf("scopes = %s, want %s (%q)", scopeList(e.Scopes), scopeList(wantScopes), row.scopes)
			}

			var client, upstream phase.Set
			fromPlugin := strings.HasPrefix(row.phases, "The Plugin")
			if !fromPlugin {
				clientText, upText, hasU := strings.Cut(row.phases, "; at U: ")
				switch {
				case wantScopes == phase.Scopes(phase.ScopeUpstream):
					upstream = parsePhases(t, clientText)
				case hasU:
					client, upstream = parsePhases(t, clientText), parsePhases(t, upText)
				default:
					client = parsePhases(t, clientText)
				}
			}
			if e.PhasesFromPlugin != fromPlugin || e.Phases != client || e.UpstreamPhases != upstream {
				t.Errorf("phases = %v, at U %v, from Plugin %v; want %v, at U %v, from Plugin %v (%q)",
					e.Phases.Phases(), e.UpstreamPhases.Phases(), e.PhasesFromPlugin,
					client.Phases(), upstream.Phases(), fromPlugin, row.phases)
			}

			wantSlot := row.slot
			if wantSlot == "name" {
				wantSlot = SlotName
			}
			if e.Slot != wantSlot {
				t.Errorf("slot = %q, want %q", e.Slot, wantSlot)
			}

			def, allowed, ok := strings.Cut(row.failure, "; ")
			if !ok {
				t.Fatalf("failureMode cell %q", row.failure)
			}
			if e.DefaultFailureMode != v1alpha1.FailureMode(def) {
				t.Errorf("default failureMode = %q, want %q", e.DefaultFailureMode, def)
			}
			switch allowed {
			case "closed only":
				if !e.ClosedOnly {
					t.Error("open allowed, want closed only")
				}
			case "either":
				if e.ClosedOnly {
					t.Error("closed only, want either")
				}
			case "closed only for auth and authz classes":
				if e.ClosedOnly || !e.ClassFromSpec {
					t.Error("want closed only decided by filterClass")
				}
			default:
				t.Fatalf("allowed failureMode cell %q", allowed)
			}

			m := planned.FindStringSubmatch(row.planned)
			if m == nil || e.Planned != m[1] {
				t.Errorf("planned = %q, want %q", e.Planned, row.planned)
			}
		})
	}
}

// want is the expected registry row of one type, from the area specs.
type want struct {
	class       phase.Class
	phases      phase.Set
	upPhases    phase.Set
	scopes      phase.ScopeSet
	slot        string
	failureMode v1alpha1.FailureMode
	closedOnly  bool
	served      bool
}

var (
	scG   = phase.Scopes(phase.ScopeGateway, phase.ScopeRoute)
	scR   = phase.Scopes(phase.ScopeRoute)
	scU   = phase.Scopes(phase.ScopeUpstream)
	scGRU = phase.Scopes(phase.ScopeGateway, phase.ScopeRoute, phase.ScopeUpstream)
	rqH   = phase.Of(phase.OnRequestHeaders)
	rqB   = phase.Of(phase.OnRequestBody)
)

// TestTableSpecRows restates each row from the requirement that fixes it,
// independently of the foundation pack parser.
func TestTableSpecRows(t *testing.T) {
	const (
		closed = v1alpha1.FailureModeClosed
		open   = v1alpha1.FailureModeOpen
	)
	authRow := want{phase.ClassAuth, rqH, 0, scG, "auth", closed, true, true} // 06 req 1
	tests := map[v1alpha1.PolicyType]want{
		v1alpha1.PolicyTypeAuthJWT:    authRow,
		v1alpha1.PolicyTypeAuthAPIKey: authRow,
		v1alpha1.PolicyTypeAuthBasic:  authRow,
		v1alpha1.PolicyTypeAuthMTLS:   authRow,
		// 06 req 53 and 56: authz stacks (slot = name), closed only.
		v1alpha1.PolicyTypeAuthzCEL:   {phase.ClassAuthz, phase.Of(phase.OnRequestHeaders, phase.OnRequestBody), 0, scG, SlotName, closed, true, true},
		v1alpha1.PolicyTypeAuthzOPA:   {phase.ClassAuthz, phase.Of(phase.OnRequestHeaders, phase.OnRequestBody), 0, scG, SlotName, closed, true, false},
		v1alpha1.PolicyTypeAuthzCedar: {phase.ClassAuthz, phase.Of(phase.OnRequestHeaders, phase.OnRequestBody), 0, scG, SlotName, closed, true, false},
		v1alpha1.PolicyTypeAuthzIP:    {phase.ClassAuthz, rqH, 0, scG, SlotName, closed, true, true},
		v1alpha1.PolicyTypeAuthzGeoIP: {phase.ClassAuthz, rqH, 0, scG, SlotName, closed, true, false},
		// 05 req 56.
		v1alpha1.PolicyTypeRateLimit: {phase.ClassAdmission, rqH, 0, scG, SlotName, open, false, true},
		// 05 req 69; OQ-configuration-model-8 (a): quota open.
		v1alpha1.PolicyTypeQuota: {phase.ClassAdmission, phase.Of(phase.OnRequestHeaders, phase.OnLog), 0, scG, SlotName, open, false, true},
		// 07 req 1 and 3.
		v1alpha1.PolicyTypeValidationJSONSchema: {phase.ClassValidation, rqB, 0, scR, "validation", closed, false, true},
		// 06 req 64.
		v1alpha1.PolicyTypeCORS: {phase.ClassCORS, phase.Of(phase.OnRequestHeaders, phase.OnResponse), 0, scG, "cors", closed, false, true},
		// 05 req 75 and T 97 (R only); R-40 keeps the Phases.
		v1alpha1.PolicyTypeCache: {phase.ClassCache, phase.Of(phase.OnRequestHeaders, phase.OnResponse), 0, scR, "cache", open, false, true},
		// 07 req 1.
		v1alpha1.PolicyTypeHeaders: {
			phase.ClassTransform, phase.Of(phase.OnRequestHeaders, phase.OnResponse),
			phase.Of(phase.OnUpstreamRequest, phase.OnUpstreamResponseHeaders), scGRU, SlotName, closed, false, true,
		},
		v1alpha1.PolicyTypeTransformRequest:  {phase.ClassTransform, rqB, phase.Of(phase.OnUpstreamRequest), scGRU, SlotName, closed, false, true},
		v1alpha1.PolicyTypeTransformResponse: {phase.ClassTransform, phase.Of(phase.OnResponse), phase.Of(phase.OnUpstreamResponseBody), scGRU, SlotName, closed, false, true},
		// 06 req 68.
		v1alpha1.PolicyTypeAuthUpstreamOAuth2: {phase.ClassUpstreamAuth, 0, phase.Of(phase.OnUpstreamRequest), scU, "upstream-auth", closed, true, true},
		v1alpha1.PolicyTypeAuthUpstreamSigV4:  {phase.ClassUpstreamAuth, 0, phase.Of(phase.OnUpstreamRequest), scU, "upstream-auth", closed, true, false},
		// OQ-configuration-model-8 (a): ai.token-budget closed.
		v1alpha1.PolicyTypeAITokenBudget:   {phase.ClassAdmission, phase.Of(phase.OnRequestBody, phase.OnChunk, phase.OnLog), 0, scG, SlotName, closed, false, false},
		v1alpha1.PolicyTypeAISemanticCache: {phase.ClassCache, phase.Of(phase.OnRequestBody, phase.OnResponse), 0, scR, "semantic-cache", open, false, false},
		v1alpha1.PolicyTypeAIGuardrail:     {phase.ClassValidation, phase.Of(phase.OnRequestBody, phase.OnChunk, phase.OnResponse), 0, scG, SlotName, closed, false, false},
		v1alpha1.PolicyTypePlugin:          {phase.ClassCustom, 0, 0, scGRU, SlotName, closed, false, false},
	}
	reg := New()
	if len(tests) != len(reg.Entries()) {
		t.Fatalf("spec rows cover %d types, registry has %d", len(tests), len(reg.Entries()))
	}
	for typ, w := range tests {
		t.Run(string(typ), func(t *testing.T) {
			e, ok := reg.Lookup(typ)
			if !ok {
				t.Fatal("not registered")
			}
			got := want{e.Class, e.Phases, e.UpstreamPhases, e.Scopes, e.Slot, e.DefaultFailureMode, e.ClosedOnly, e.Served}
			if got != w {
				t.Errorf("row = %+v\nwant  %+v", got, w)
			}
			if (typ == v1alpha1.PolicyTypePlugin) != (e.ClassFromSpec || e.PhasesFromPlugin) {
				t.Error("only plugin takes its class and Phases from spec and Plugin")
			}
		})
	}
}

// TestServedSet: RZ-CFG-040 applies to exactly plugin, authz.opa,
// authz.cedar, authz.geoip, auth.upstream-sigv4 and ai.* in M1
// (architecture WP-04 scope, section 0 item 8, OQ-vision-and-positioning-10).
func TestServedSet(t *testing.T) {
	unserved := []v1alpha1.PolicyType{
		v1alpha1.PolicyTypeAuthzOPA, v1alpha1.PolicyTypeAuthzCedar, v1alpha1.PolicyTypeAuthzGeoIP,
		v1alpha1.PolicyTypeAuthUpstreamSigV4, v1alpha1.PolicyTypeAITokenBudget,
		v1alpha1.PolicyTypeAISemanticCache, v1alpha1.PolicyTypeAIGuardrail, v1alpha1.PolicyTypePlugin,
	}
	served := []v1alpha1.PolicyType{
		v1alpha1.PolicyTypeAuthJWT, v1alpha1.PolicyTypeAuthAPIKey, v1alpha1.PolicyTypeAuthBasic,
		v1alpha1.PolicyTypeAuthMTLS, v1alpha1.PolicyTypeAuthzCEL, v1alpha1.PolicyTypeAuthzIP,
		v1alpha1.PolicyTypeRateLimit, v1alpha1.PolicyTypeQuota, v1alpha1.PolicyTypeValidationJSONSchema,
		v1alpha1.PolicyTypeCORS, v1alpha1.PolicyTypeCache, v1alpha1.PolicyTypeHeaders,
		v1alpha1.PolicyTypeTransformRequest, v1alpha1.PolicyTypeTransformResponse,
		v1alpha1.PolicyTypeAuthUpstreamOAuth2,
	}
	reg := New()
	if len(served)+len(unserved) != len(reg.Types()) {
		t.Fatalf("served table covers %d types, registry has %d", len(served)+len(unserved), len(reg.Types()))
	}
	for _, typ := range served {
		if !reg.Served(typ) {
			t.Errorf("%s not served", typ)
		}
	}
	for _, typ := range unserved {
		if reg.Served(typ) {
			t.Errorf("%s served, want RZ-CFG-040", typ)
		}
		if strings.HasPrefix(string(typ), "ai.") != (typ == v1alpha1.PolicyTypeAITokenBudget || typ == v1alpha1.PolicyTypeAISemanticCache || typ == v1alpha1.PolicyTypeAIGuardrail) {
			t.Errorf("%s: ai.* bookkeeping", typ)
		}
	}
	if reg.Served("rateLimit") {
		t.Error("unknown type served")
	}
	// In M1 the served set is exactly the Planned (M1) types.
	for _, e := range reg.Entries() {
		if e.Served != (e.Planned == "M1") {
			t.Errorf("%s: served %v, planned %s", e.Type, e.Served, e.Planned)
		}
	}
}

// TestRegistrySlots: the fixed registry slots are exactly those of 01 req
// 40 (auth for the four client auth types, validation, cors, cache,
// upstream-auth, semantic-cache); every other type stacks by name.
func TestRegistrySlots(t *testing.T) {
	got := map[string][]v1alpha1.PolicyType{}
	for _, e := range New().Entries() {
		if e.Slot != SlotName {
			got[e.Slot] = append(got[e.Slot], e.Type)
		}
	}
	w := map[string][]v1alpha1.PolicyType{
		"auth": {
			v1alpha1.PolicyTypeAuthJWT, v1alpha1.PolicyTypeAuthAPIKey,
			v1alpha1.PolicyTypeAuthBasic, v1alpha1.PolicyTypeAuthMTLS,
		},
		"validation":     {v1alpha1.PolicyTypeValidationJSONSchema},
		"cors":           {v1alpha1.PolicyTypeCORS},
		"cache":          {v1alpha1.PolicyTypeCache},
		"upstream-auth":  {v1alpha1.PolicyTypeAuthUpstreamOAuth2, v1alpha1.PolicyTypeAuthUpstreamSigV4},
		"semantic-cache": {v1alpha1.PolicyTypeAISemanticCache},
	}
	if len(got) != len(w) {
		t.Fatalf("fixed slots = %v, want %v", got, w)
	}
	for slot, types := range w {
		if !slices.Equal(got[slot], types) {
			t.Errorf("slot %q = %v, want %v", slot, got[slot], types)
		}
	}
}

// TestEntryInvariants checks every row against the Phase and scope model
// of 02 req 14 (client-leg Phases at G and R, upstream-leg Phases at U)
// and foundation pack section 8.10 (security types closed by default).
func TestEntryInvariants(t *testing.T) {
	for _, e := range New().Entries() {
		t.Run(string(e.Type), func(t *testing.T) {
			hasClient := e.Scopes.Has(phase.ScopeGateway) || e.Scopes.Has(phase.ScopeRoute)
			hasUp := e.Scopes.Has(phase.ScopeUpstream)
			if e.Scopes.Has(phase.ScopeNone) {
				t.Error("ScopeNone allowed")
			}
			if e.Scopes.Has(phase.ScopeGateway) && !e.Scopes.Has(phase.ScopeRoute) {
				t.Error("Gateway scope without Route scope")
			}
			for _, p := range e.Phases.Phases() {
				if !p.ClientLeg() {
					t.Errorf("client Phase %s is not on the client leg", p)
				}
			}
			for _, p := range e.UpstreamPhases.Phases() {
				if !p.UpstreamLeg() {
					t.Errorf("Upstream Phase %s is not on the upstream leg", p)
				}
			}
			if !e.PhasesFromPlugin {
				if hasClient == e.Phases.Empty() {
					t.Errorf("client scope %v but client Phases %v", hasClient, e.Phases.Phases())
				}
				if hasUp == e.UpstreamPhases.Empty() {
					t.Errorf("Upstream scope %v but Upstream Phases %v", hasUp, e.UpstreamPhases.Phases())
				}
			}
			if e.ClosedOnly && e.DefaultFailureMode != v1alpha1.FailureModeClosed {
				t.Error("closed-only type defaults to open")
			}
			security := e.Class == phase.ClassAuth || e.Class == phase.ClassAuthz || e.Class == phase.ClassUpstreamAuth
			if security != e.ClosedOnly {
				t.Errorf("class %s: closed only %v (foundation pack section 8.10)", e.Class, e.ClosedOnly)
			}
			if e.NewConfig == nil {
				t.Error("no config type")
			}
			if e.Planned != "M1" && e.Planned != "M2" && e.Planned != "M3" {
				t.Errorf("planned %q", e.Planned)
			}
		})
	}
}

// TestLookup covers Lookup, Types and the copy semantics of Entries.
func TestLookup(t *testing.T) {
	reg := New()
	if _, ok := reg.Lookup("rateLimit"); ok {
		t.Error(`Lookup("rateLimit") succeeded; foundation pack section 10 forbids the spelling`)
	}
	for _, bad := range []v1alpha1.PolicyType{"", "jwt", "apiKey", "rate-limit", "tokenBudget", "ipfilter", "geoip", "AUTH.JWT"} {
		if _, ok := reg.Lookup(bad); ok {
			t.Errorf("Lookup(%q) succeeded", bad)
		}
	}
	es := reg.Entries()
	es[0].Slot = "changed"
	es[0].Served = false
	if e, _ := reg.Lookup(es[0].Type); e.Slot != "auth" || !e.Served {
		t.Error("Entries returned the registry's own table")
	}
	ts := reg.Types()
	ts[0] = "changed"
	if reg.Types()[0] != v1alpha1.PolicyTypeAuthJWT {
		t.Error("Types returned the registry's own slice")
	}
	var zero Registry
	if _, ok := zero.Lookup(v1alpha1.PolicyTypeAuthJWT); ok || len(zero.Types()) != 0 {
		t.Error("zero Registry is not empty")
	}
}

// TestConcurrentUse: a Registry is immutable and safe for concurrent use
// (01 section 3 concurrency model); run under -race.
func TestConcurrentUse(t *testing.T) {
	reg := New()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for _, typ := range reg.Types() {
				e, ok := reg.Lookup(typ)
				if !ok {
					t.Errorf("Lookup(%s) failed", typ)
					return
				}
				p := &v1alpha1.Policy{Metadata: v1alpha1.ObjectMeta{Name: "p"}, Spec: v1alpha1.PolicySpec{Type: typ}}
				_ = e.EffectiveSlot(p)
				if _, err := reg.NewConfig(typ); err != nil {
					t.Error(err)
				}
				for _, s := range []phase.Scope{phase.ScopeGateway, phase.ScopeRoute, phase.ScopeUpstream} {
					_, _ = reg.Phases(Attachment{Policy: p, Scope: s}, nil)
				}
			}
		})
	}
	wg.Wait()
}
