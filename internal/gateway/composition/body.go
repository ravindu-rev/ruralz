// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package composition

import (
	"errors"
	"io"
	"sync"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/gateway/body"
)

// errBodyClosed is returned by a read of a held body after Close.
var errBodyClosed = errors.New("composition: response body closed")

// heldBody is a response body the engine holds whole: the merged bytes,
// reserved through acct, or a final step's gated body, whose buffer holds
// its own reservation. Close returns the reservation once; a read after it
// fails. It is safe for concurrent use.
type heldBody struct {
	mu     sync.Mutex
	data   []byte
	off    int
	acct   body.Account
	buf    *body.Buffer
	closed bool
}

// Read reads the held bytes.
func (b *heldBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return 0, errBodyClosed
	}
	if b.off >= len(b.data) {
		return 0, io.EOF
	}
	n := copy(p, b.data[b.off:])
	b.off += n
	return n, nil
}

// Close returns the reservation; a second Close does nothing.
func (b *heldBody) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
	b.data = nil
	b.acct.ReleaseAll()
	if b.buf != nil {
		b.buf.Release()
		b.buf = nil
	}
	return nil
}

// limitedBody is the streamed body of a final step, cut where it passes
// the step's maxBodyBytes: the read that passes it fails with an error
// carrying RZ-UP-009, which ends the stream after commit
// (docs/architecture/03-data-plane.md "Failures and partial responses").
// Close closes the leg's body, which ends its attempt.
type limitedBody struct {
	lim body.Limited
	rc  io.ReadCloser
}

// newLimitedBody returns rc cut at limit bytes.
func newLimitedBody(rc io.ReadCloser, limit int64) *limitedBody {
	b := &limitedBody{rc: rc}
	b.lim.Reset(rc, limit)
	return b
}

// Read reads the leg's body within the limit.
func (b *limitedBody) Read(p []byte) (int, error) {
	n, err := b.lim.Read(p)
	if errors.Is(err, body.ErrTooLarge) {
		err = errcode.Wrap(CodeAfterCommit, err)
	}
	return n, err
}

// Close closes the leg's body.
func (b *limitedBody) Close() error { return b.rc.Close() }
