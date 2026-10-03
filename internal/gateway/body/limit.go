// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package body

import (
	"errors"
	"io"
)

// Limited reads a streamed body and cuts it at its limit (spec 04 req 37):
// up to limit bytes pass, and a body passing the limit ends with
// ErrTooLarge (before commit the client gets 413 RZ-RT-003; after commit
// the leg is aborted and the stream reset). It counts the bytes it
// returned. The zero value reads nothing; Reset binds a reader. A Limited
// is read by one goroutine at a time.
type Limited struct {
	r     io.Reader
	left  int64
	count int64
	err   error
}

// LimitReader returns r cut at limit bytes.
func LimitReader(r io.Reader, limit int64) *Limited {
	l := &Limited{}
	l.Reset(r, limit)
	return l
}

// Reset binds r with limit, clearing the count and any sticky error.
func (l *Limited) Reset(r io.Reader, limit int64) {
	l.r, l.left, l.count, l.err = r, max(0, limit), 0, nil
}

// Read reads from the bound reader. It asks for at most one byte past the
// remaining limit, so a body that ends exactly at the limit passes and one
// byte more is ErrTooLarge. The first error, io.EOF included, is sticky.
func (l *Limited) Read(p []byte) (int, error) {
	if l.err != nil {
		return 0, l.err
	}
	if l.r == nil {
		l.err = io.EOF
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	if int64(len(p))-1 > l.left {
		p = p[:l.left+1]
	}
	n, err := l.r.Read(p)
	if int64(n) <= l.left {
		l.left -= int64(n)
		l.count += int64(n)
		l.err = err
		return n, err
	}
	n = int(l.left)
	l.count += int64(n)
	l.left = 0
	l.err = ErrTooLarge
	return n, ErrTooLarge
}

// Count returns the bytes returned so far (ruralz_http_request_body_bytes).
func (l *Limited) Count() int64 { return l.count }

// Exceeded reports whether the body passed the limit.
func (l *Limited) Exceeded() bool { return errors.Is(l.err, ErrTooLarge) }
