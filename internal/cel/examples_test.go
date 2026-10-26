// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cel

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/phase"
)

var update = flag.Bool("update", false, "rewrite the golden files under testdata")

// example is one expression of the documents' example Bundle in its site.
type example struct {
	site expr.Site
	src  string
}

// examples returns every CEL expression of the complete annotated example
// Bundle of docs/architecture/02-configuration-model.md, in document order,
// with the site its field and attachment give it.
func examples() []example {
	return []example{
		{at(expr.PlaceAccessLogWhen), `response.status >= 400 || duration > duration("1s")`},
		{at(expr.PlaceRetryOn), `error != null ? error.kind in ["connect", "reset"] : response.status == 503`},
		{sequential(expr.PlaceStepPathExpression), `"/orders/" + request.pathParams.orderId`},
		{sequential(expr.PlaceStepPathExpression, "order"), `"/stock?order=" + request.pathParams.orderId`},
		{at(expr.PlaceCandidateWhen), `ai.estimatedInputTokens < 100000`},
		{expr.Site{Place: expr.PlaceAuthzCELRule, Scope: phase.ScopeRoute}, `auth.claims.scope.split(" ").exists(s, s == "orders:read")`},
		{expr.Site{Place: expr.PlaceRateLimitKey, Scope: phase.ScopeGateway}, `source.ip`},
		{expr.Site{Place: expr.PlaceRateLimitKey, Scope: phase.ScopeRoute}, `consumer == null ? source.ip : consumer.name`},
		{expr.Site{Place: expr.PlaceSemanticCacheKey}, `consumer.name`},
		{expr.Site{Place: expr.PlaceHeadersRequestValue, Scope: phase.ScopeRoute}, `consumer == null ? "anonymous" : consumer.name`},
		{at(expr.PlaceCandidateWhen), `ai.estimatedInputTokens < 50000`},
		{expr.Site{Place: expr.PlaceQuotaKey, Scope: phase.ScopeRoute}, `consumer.name`},
	}
}

// TestExampleBundleEstimates checks that every expression of the example
// Bundle passes RZ-CFG-014 and RZ-CFG-015 in its site, the authz-orders rule
// included, and pins each estimate in testdata/estimates.golden, so a cel-go
// upgrade that moves an estimate shows in review. -update rewrites it.
func TestExampleBundleEstimates(t *testing.T) {
	c := newCompiler(t)
	var out bytes.Buffer
	for _, ex := range examples() {
		cc, iss := c.compile(ex.site, ex.src)
		if cc == nil {
			t.Errorf("%s %q: %+v", ex.site.Place, ex.src, iss)
			continue
		}
		fmt.Fprintf(&out, "%s\t%s\tcost %d..%d\ttracked %v\n", ex.site.Place, ex.src, cc.cost.Min, cc.cost.Max, cc.tracked)
	}
	path := filepath.Join("testdata", "estimates.golden")
	if *update {
		if err := os.WriteFile(path, out.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path) //nolint:gosec // a constant path under testdata
	if err != nil {
		t.Fatalf("read golden (run with -update): %v", err)
	}
	if !bytes.Equal(out.Bytes(), want) {
		t.Errorf("estimates differ from %s (run with -update after review):\n%s", path, out.String())
	}
}
