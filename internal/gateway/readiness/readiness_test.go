// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package readiness

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/adminapi"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
)

// digest is a fixed active Revision.
func digest(t *testing.T) revision.Digest {
	t.Helper()
	d, err := revision.Parse("sha256:162af81f5de482d540e09cf25205ffe39b705fd2dcf5eca88eb42e92b098a85e")
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// probe answers one /readyz request.
func probe(t *testing.T, tr *Tracker) (status int, body string, h http.Header) {
	t.Helper()
	w := httptest.NewRecorder()
	tr.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", nil))
	return w.Code, w.Body.String(), w.Header()
}

// ready makes tr ready on d.
func ready(tr *Tracker, d revision.Digest) {
	tr.SetSecrets()
	tr.SetListeners()
	tr.SetRevision(d)
}

// TestStartsNotReady checks that a Node is not ready before its first
// activation: no Revision, listeners not yet bound.
func TestStartsNotReady(t *testing.T) {
	tr := New()
	status, body, _ := probe(t, tr)
	want := `{"status":"not_ready","reasons":[{"reason":"no_revision"},{"reason":"listeners_unbound"}]}`
	if status != http.StatusServiceUnavailable || body != want {
		t.Fatalf("readyz = %d %s, want 503 %s", status, body, want)
	}
	if tr.Ready() {
		t.Fatal("Ready before any activation")
	}
}

// TestReadyBody checks the ready answer: 200 with the status and the
// active Revision as rev-<12 hex>.
func TestReadyBody(t *testing.T) {
	tr := New()
	ready(tr, digest(t))
	status, body, _ := probe(t, tr)
	if want := `{"status":"ready","revision":"rev-162af81f5de4"}`; status != http.StatusOK || body != want {
		t.Fatalf("readyz = %d %s, want 200 %s", status, body, want)
	}
	if !tr.Ready() {
		t.Fatal("Ready() false while /readyz answers 200")
	}
}

// TestReasonsInDocumentedOrder checks the not-ready body: every failed
// condition, in the order no_revision, secrets_unresolved,
// listeners_unbound, draining, with one entry per unresolved secretRef or
// unbound listener carrying its code and non-secret detail.
func TestReasonsInDocumentedOrder(t *testing.T) {
	tr := New()
	tr.Drain()
	tr.SetListeners(Issue{Code: "RZ-CFG-039"})
	tr.SetSecrets(
		Issue{Code: "RZ-CFG-026", Detail: "file:/run/secrets/tls.pem is unreadable"},
		Issue{Code: "RZ-CFG-026", Detail: "env:DB_URL is not set"},
	)
	status, body, _ := probe(t, tr)
	want := `{"status":"not_ready","reasons":[{"reason":"no_revision"},` +
		`{"reason":"secrets_unresolved","code":"RZ-CFG-026","detail":"file:/run/secrets/tls.pem is unreadable"},` +
		`{"reason":"secrets_unresolved","code":"RZ-CFG-026","detail":"env:DB_URL is not set"},` +
		`{"reason":"listeners_unbound","code":"RZ-CFG-039"},{"reason":"draining"}]}`
	if status != http.StatusServiceUnavailable || body != want {
		t.Fatalf("readyz = %d %s\nwant 503 %s", status, body, want)
	}
}

// TestReadyOnlyWithEveryCondition checks the readiness rule over every
// combination of the four conditions: 200 exactly when there is an active
// Revision, every secretRef resolved, the listeners bound and no Drain;
// otherwise 503 naming exactly the failed conditions.
func TestReadyOnlyWithEveryCondition(t *testing.T) {
	d := digest(t)
	for mask := range 16 {
		noRevision, unresolved, unbound, draining := mask&1 != 0, mask&2 != 0, mask&4 != 0, mask&8 != 0
		t.Run(strconv.Itoa(mask), func(t *testing.T) {
			tr := New()
			var want []string
			if noRevision {
				want = append(want, adminapi.ReasonNoRevision)
			} else {
				tr.SetRevision(d)
			}
			if unresolved {
				tr.SetSecrets(Issue{Code: "RZ-CFG-026"})
				want = append(want, adminapi.ReasonSecretsUnresolved)
			}
			if unbound {
				want = append(want, adminapi.ReasonListenersUnbound)
			} else {
				tr.SetListeners()
			}
			if draining {
				tr.Drain()
				want = append(want, adminapi.ReasonDraining)
			}
			status, body, _ := probe(t, tr)
			var got adminapi.Readyz
			if err := json.Unmarshal([]byte(body), &got); err != nil {
				t.Fatal(err)
			}
			var reasons []string
			for _, r := range got.Reasons {
				reasons = append(reasons, r.Reason)
			}
			if !reflect.DeepEqual(reasons, want) {
				t.Errorf("reasons %v, want %v", reasons, want)
			}
			wantReady := len(want) == 0
			if wantStatus := map[bool]int{true: http.StatusOK, false: http.StatusServiceUnavailable}[wantReady]; status != wantStatus {
				t.Errorf("status %d, want %d", status, wantStatus)
			}
			if tr.Ready() != wantReady {
				t.Errorf("Ready() = %v, want %v", tr.Ready(), wantReady)
			}
		})
	}
}

// TestDrainIsFinal checks that a Drain fails readiness at once and that
// no later update makes the Node ready again.
func TestDrainIsFinal(t *testing.T) {
	tr := New()
	ready(tr, digest(t))
	tr.Drain()
	if tr.Ready() {
		t.Fatal("ready after Drain")
	}
	ready(tr, digest(t))
	if status, body, _ := probe(t, tr); status != http.StatusServiceUnavailable ||
		body != `{"status":"not_ready","reasons":[{"reason":"draining"}]}` {
		t.Fatalf("readyz after Drain and updates = %d %s", status, body)
	}
}

// TestRevisionCleared checks that clearing the active Revision fails
// readiness with no_revision.
func TestRevisionCleared(t *testing.T) {
	tr := New()
	ready(tr, digest(t))
	tr.SetRevision(revision.Digest{})
	if _, body, _ := probe(t, tr); body != `{"status":"not_ready","reasons":[{"reason":"no_revision"}]}` {
		t.Fatalf("readyz = %s", body)
	}
}

// TestResponseHeaders checks the /readyz headers: JSON, not cached, an
// exact length.
func TestResponseHeaders(t *testing.T) {
	tr := New()
	_, body, h := probe(t, tr)
	if got := h.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type %q", got)
	}
	if got := h.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control %q", got)
	}
	if got := h.Get("Content-Length"); got != strconv.Itoa(len(body)) {
		t.Errorf("Content-Length %q, body %d bytes", got, len(body))
	}
}

// TestUpdatesCopyTheirInput checks that the Tracker keeps its own copy of
// the issues it is given and that Readyz returns a copy.
func TestUpdatesCopyTheirInput(t *testing.T) {
	tr := New()
	issues := []Issue{{Code: "RZ-CFG-026", Detail: "file:/a"}}
	tr.SetSecrets(issues...)
	issues[0].Detail = "changed"
	v := tr.Readyz()
	if v.Reasons[1].Detail != "file:/a" {
		t.Fatalf("detail %q, want file:/a", v.Reasons[1].Detail)
	}
	v.Reasons[1].Detail = "changed"
	if tr.Readyz().Reasons[1].Detail != "file:/a" {
		t.Fatal("Readyz shares the Tracker's reasons")
	}
}

// TestAnswered checks Answered: false while /readyz answers only 503, and
// false while the Node is ready but no probe came; true from the first
// 200 on, even once a Drain fails readiness.
func TestAnswered(t *testing.T) {
	tr := New()
	if status, _, _ := probe(t, tr); status != http.StatusServiceUnavailable || tr.Answered() {
		t.Fatalf("not ready: probe %d, Answered %t; want 503 and false", status, tr.Answered())
	}
	ready(tr, digest(t))
	if tr.Answered() {
		t.Fatal("Answered before a probe saw the Node ready")
	}
	if status, _, _ := probe(t, tr); status != http.StatusOK || !tr.Answered() {
		t.Fatalf("ready: probe %d, Answered %t; want 200 and true", status, tr.Answered())
	}
	tr.Drain()
	if status, _, _ := probe(t, tr); status != http.StatusServiceUnavailable || !tr.Answered() {
		t.Fatalf("draining: probe %d, Answered %t; want 503 and still true", status, tr.Answered())
	}
}

// TestConcurrentUpdatesAndProbes checks, under -race, that probes read a
// whole answer while the conditions change.
func TestConcurrentUpdatesAndProbes(t *testing.T) {
	tr := New()
	d := digest(t)
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := range 500 {
			if i%2 == 0 {
				ready(tr, d)
			} else {
				tr.SetSecrets(Issue{Code: "RZ-CFG-026"})
			}
		}
	})
	for range 4 {
		wg.Go(func() {
			for range 500 {
				w := httptest.NewRecorder()
				tr.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", nil))
				var got adminapi.Readyz
				if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
					t.Error(err)
					return
				}
				if (w.Code == http.StatusOK) != (got.Status == StatusReady) {
					t.Errorf("status %d with body %s", w.Code, w.Body)
					return
				}
				_ = tr.Readyz()
			}
		})
	}
	wg.Wait()
}
