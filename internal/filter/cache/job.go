// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cache

import (
	"context"
	"errors"
	"net/http"

	"github.com/ravindu-rev/ruralz/internal/filter/cache/revalidate"
	"github.com/ravindu-rev/ruralz/internal/statestore"
)

// errTooLarge reports a refreshed entry past MaxEntry.
var errTooLarge = errors.New("cache: refreshed entry exceeds the size cap")

// jobEntry is a stale entry as a background revalidation sees it
// (revalidate.Entry), decoded over bytes no request reuses.
type jobEntry struct{ e *entry }

var _ revalidate.Entry = (*jobEntry)(nil)

// Request implements revalidate.Entry: the saved request, with its own
// header map and no body.
func (j *jobEntry) Request(ctx context.Context) (*http.Request, error) {
	r, err := http.NewRequestWithContext(ctx, j.e.method, j.e.target, nil)
	if err != nil {
		return nil, err
	}
	r.Header = j.e.reqHeader.Clone()
	if r.Header == nil {
		r.Header = http.Header{}
	}
	return r, nil
}

// Header implements revalidate.Entry.
func (j *jobEntry) Header() http.Header { return j.e.header }

// Authorized implements revalidate.Entry.
func (j *jobEntry) Authorized() bool { return j.e.authorized() }

// Token implements revalidate.Entry.
func (j *jobEntry) Token() statestore.Token { return j.e.token }

// Refresh implements revalidate.Entry: the stored entry with the
// freshened header and the restarted initial age, under the same token;
// past MaxEntry it is errTooLarge, which ends the stale window.
func (j *jobEntry) Refresh(r *revalidate.Refreshed) ([]byte, error) {
	e := *j.e
	e.header, e.initialAge = r.Header, r.InitialAge
	b := appendEntry(nil, &e)
	if len(b) > MaxEntry {
		return nil, errTooLarge
	}
	return b, nil
}

// Replace implements revalidate.Entry: the full response as the leg
// delivered it, its header kept as a store keeps it, over the stored
// entry's saved request, Route, Upstream, flags and token. The worker
// checks the size cap.
func (j *jobEntry) Replace(r *revalidate.Replacement) ([]byte, error) {
	e := *j.e
	e.status, e.header, e.initialAge, e.body = r.Status, storedHeader(r.Header, r.Received), r.InitialAge, r.Body
	return appendEntry(nil, &e), nil
}
