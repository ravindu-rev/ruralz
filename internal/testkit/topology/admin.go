// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package topology

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ravindu-rev/ruralz/internal/adminapi"
)

// Credential is what an admin request presents.
type Credential int

// Credentials.
const (
	// Operator presents the operator token, accepted on every admin path.
	Operator Credential = iota
	// Metrics presents the metrics token, accepted on /metrics only.
	Metrics
	// Anonymous presents nothing: /healthz and /readyz are open.
	Anonymous
)

// maxAdminBody bounds an admin response the harness reads.
const maxAdminBody = 256 << 20

// AdminResponse is one admin port answer.
type AdminResponse struct {
	Status int
	Header http.Header
	Body   []byte
}

// adminClient returns the client of the admin ports: it trusts the
// harness's CA and opens a connection per request, so after a handover
// each request reaches whichever process listens, as probes do.
func (t *Topology) adminClient() *http.Client {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.admin == nil {
		tr := &http.Transport{TLSClientConfig: t.adminTLS(), DisableKeepAlives: true}
		t.transports = append(t.transports, tr)
		t.admin = &http.Client{
			Transport:     tr,
			Timeout:       t.cfg.ReadyTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	return t.admin
}

// Admin GETs path on the Node's admin port with the operator token. The
// body is captured for the leak scan, except /debug/pprof/ profiles.
func (n *Node) Admin(ctx context.Context, path string) (AdminResponse, error) {
	return n.AdminAs(ctx, path, Operator)
}

// AdminAs GETs path on the Node's admin port presenting cred.
func (n *Node) AdminAs(ctx context.Context, path string, cred Credential) (AdminResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, n.AdminURL()+path, nil)
	if err != nil {
		return AdminResponse{}, fmt.Errorf("topology: %w", err)
	}
	switch cred {
	case Operator:
		req.Header.Set("Authorization", "Bearer "+n.operatorToken)
	case Metrics:
		req.Header.Set("Authorization", "Bearer "+n.metricsToken)
	default:
	}
	resp, err := n.t.adminClient().Do(req)
	if err != nil {
		return AdminResponse{}, fmt.Errorf("topology: %s admin %s: %w", n.name, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAdminBody+1))
	if err == nil && len(body) > maxAdminBody {
		err = fmt.Errorf("body above %d bytes", maxAdminBody)
	}
	if err != nil {
		return AdminResponse{}, fmt.Errorf("topology: %s admin %s: %w", n.name, path, err)
	}
	if !strings.HasPrefix(path, "/debug/pprof/") {
		if err := n.t.captures.add("admin-"+n.name, body); err != nil {
			return AdminResponse{}, err
		}
	}
	return AdminResponse{Status: resp.StatusCode, Header: resp.Header, Body: body}, nil
}

// Readyz returns the Node's /readyz document and status.
func (n *Node) Readyz(ctx context.Context) (adminapi.Readyz, int, error) {
	resp, err := n.AdminAs(ctx, "/readyz", Anonymous)
	if err != nil {
		return adminapi.Readyz{}, 0, err
	}
	var doc adminapi.Readyz
	if err := json.Unmarshal(resp.Body, &doc); err != nil {
		return adminapi.Readyz{}, resp.Status, fmt.Errorf("topology: %s /readyz body %q: %w", n.name, resp.Body, err)
	}
	return doc, resp.Status, nil
}

// WaitReady waits, within Config.ReadyTimeout of real time, until the
// Node's /readyz answers 200, as ruralz dev run --ready-timeout does; a
// Node whose process exits first fails at once.
func (n *Node) WaitReady(ctx context.Context) error {
	return n.waitReadyz(ctx, "ready", func(doc adminapi.Readyz, status int) bool {
		return status == http.StatusOK
	})
}

// WaitRevision waits, within Config.ReadyTimeout, until /readyz answers
// 200 naming the test digest testDigest, a Render's TestDigest, as ruralz
// dev run waits for a swapped render to activate.
func (n *Node) WaitRevision(ctx context.Context, testDigest string) error {
	want := DisplayRevision(testDigest)
	return n.waitReadyz(ctx, "active "+want, func(doc adminapi.Readyz, status int) bool {
		return status == http.StatusOK && doc.Revision == want
	})
}

// waitReadyz polls /readyz until done holds.
func (n *Node) waitReadyz(ctx context.Context, what string, done func(adminapi.Readyz, int) bool) error {
	p := n.Process()
	if p == nil {
		return ErrNotRunning
	}
	ctx, cancel := context.WithTimeout(ctx, n.t.cfg.ReadyTimeout)
	defer cancel()
	last := "no answer"
	for {
		doc, status, err := n.Readyz(ctx)
		switch {
		case err != nil && ctx.Err() != nil:
			// The wait's own end cut this poll off; the last answer is
			// the one to report.
		case err != nil:
			last = err.Error()
		case done(doc, status):
			return nil
		default:
			last = fmt.Sprintf("%d %+v", status, doc)
		}
		select {
		case <-p.Done():
			return fmt.Errorf("topology: %s exited (%v) before it was %s; last /readyz: %s%s", n.name, p.State(), what, last, outputTail(p))
		case <-ctx.Done():
			return fmt.Errorf("topology: %s not %s within %v; last /readyz: %s", n.name, what, n.t.cfg.ReadyTimeout, last)
		case <-time.After(PollInterval):
		}
	}
}
