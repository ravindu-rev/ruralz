// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package adminclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"sync"

	"github.com/ravindu-rev/ruralz/internal/adminapi"
	"github.com/ravindu-rev/ruralz/internal/cli/command"
)

// ConfigDump returns the Node's /config/dump document exactly as
// received, at most MaxConfigDumpBytes: the active Revision in
// ruralz.canonical.v1 form with its full digest and Last-Known-Good
// digest. A Node with no active Revision answers 503 RZ-RT-024, a
// ProblemError. Checking the document is the caller's.
func (c *Client) ConfigDump(ctx context.Context) ([]byte, error) {
	resp, fl, err := c.roundTrip(ctx, c.endpoint(PathConfigDump, ""), false)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	defer fl.finish()
	if err := fl.check(ctx, resp, ok200); err != nil {
		return nil, err
	}
	return fl.body(ctx, resp, PathConfigDump, c.limits.dump)
}

// Readiness is one /readyz answer: Ready for 200, else a 503 whose
// reasons say why.
type Readiness struct {
	// Ready reports a 200 answer.
	Ready bool
	// Readyz is the decoded body; unknown members are ignored.
	adminapi.Readyz
}

// Readyz returns the Node's /readyz answer, whose body is at most
// MaxReadyzBytes. A 503 with the /readyz body is an answer, not an
// error; other statuses and problem documents are errors.
func (c *Client) Readyz(ctx context.Context) (Readiness, error) {
	resp, fl, err := c.roundTrip(ctx, c.endpoint(PathReadyz, ""), false)
	if err != nil {
		return Readiness{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	defer fl.finish()
	if err := fl.check(ctx, resp, readyzBody); err != nil {
		return Readiness{}, err
	}
	b, err := fl.body(ctx, resp, PathReadyz, c.limits.readyz)
	if err != nil {
		return Readiness{}, err
	}
	var r adminapi.Readyz
	if err := json.Unmarshal(b, &r); err != nil {
		return Readiness{}, command.NoResultf("%s: the /readyz body does not decode: %w", fl.op, err)
	}
	return Readiness{Ready: resp.StatusCode == http.StatusOK, Readyz: r}, nil
}

// readyzBody accepts the /readyz answers: 200, and 503 unless it is a
// problem document.
func readyzBody(resp *http.Response) bool {
	switch resp.StatusCode {
	case http.StatusOK:
		return true
	case http.StatusServiceUnavailable:
		return !isProblem(resp.Header)
	default:
		return false
	}
}

// Tap subscribes to the Node's /tap stream with ?sample= set to sample,
// a rate in (0, 1]. The connection and header deadlines apply; the stream
// has no deadline. A refused subscription is a ProblemError: 503
// RZ-RT-019 at the subscriber limit, 401 without a valid credential.
func (c *Client) Tap(ctx context.Context, sample float64) (*TapStream, error) {
	if math.IsNaN(sample) || sample <= 0 || sample > 1 {
		return nil, command.Usagef("the /tap sample rate %v is not in (0, 1]", sample)
	}
	q := "sample=" + strconv.FormatFloat(sample, 'g', -1, 64)
	resp, fl, err := c.roundTrip(ctx, c.endpoint(PathTap, q), true)
	if err != nil {
		return nil, err
	}
	if err := fl.check(ctx, resp, ok200); err != nil {
		_ = resp.Body.Close()
		fl.finish()
		return nil, err
	}
	return newTapStream(resp, fl), nil
}

// errLineTooLarge marks a /tap line over its bound.
var errLineTooLarge = errors.New("tap line over its bound")

// tapReadBuffer is the read buffer of a /tap stream; longer lines are
// assembled from several reads up to the line bound.
const tapReadBuffer = 64 << 10

// TapStream is one /tap subscription. Next returns its lines and Close
// ends it; only Close may be called while Next waits.
type TapStream struct {
	fl        *inflight
	body      io.ReadCloser
	r         *bufio.Reader
	limit     int64
	line      []byte
	err       error // the final error of Next
	closeOnce sync.Once
}

// newTapStream hands the response body of an accepted subscription to
// its stream, which closes it.
func newTapStream(resp *http.Response, fl *inflight) *TapStream {
	return &TapStream{fl: fl, body: resp.Body, r: bufio.NewReaderSize(resp.Body, tapReadBuffer), limit: fl.c.limits.tapLine}
}

// Next returns the stream's next line without its newline: an event or a
// {"dropped":N} notice, as adminapi.TapEvent and adminapi.TapDropped
// decode them. Canceling ctx ends the wait and the stream. An error is
// final: a stream the Node ended wraps ErrStreamEnded, and a line over
// MaxTapLineBytes, its newline included, exits 2.
func (s *TapStream) Next(ctx context.Context) ([]byte, error) {
	if s.err != nil {
		return nil, s.err
	}
	stop := context.AfterFunc(ctx, func() { s.fl.cancel(context.Cause(ctx)) })
	defer stop()
	line, err := s.readLine()
	if err != nil {
		s.err = s.fl.streamFailure(ctx, err, s.limit)
		return nil, s.err
	}
	return line, nil
}

// readLine reads one line, its newline included in the bound, and
// returns a copy without the newline; io.EOF at a line boundary,
// io.ErrUnexpectedEOF inside a line.
func (s *TapStream) readLine() ([]byte, error) {
	s.line = s.line[:0]
	for {
		frag, err := s.r.ReadSlice('\n')
		if int64(len(s.line))+int64(len(frag)) > s.limit {
			return nil, errLineTooLarge
		}
		s.line = append(s.line, frag...)
		switch {
		case err == nil:
			return bytes.Clone(s.line[:len(s.line)-1]), nil
		case errors.Is(err, bufio.ErrBufferFull):
			// A line longer than the read buffer: read on.
		case errors.Is(err, io.EOF) && len(s.line) == 0:
			return nil, io.EOF
		case errors.Is(err, io.EOF):
			return nil, io.ErrUnexpectedEOF
		default:
			return nil, err
		}
	}
}

// Close ends the subscription and closes its connection; it is idempotent
// and may be called while Next waits.
func (s *TapStream) Close() error {
	s.closeOnce.Do(func() {
		s.fl.finish()
		_ = s.body.Close()
	})
	return nil
}

// streamFailure classifies a failed read of the stream: the end of the
// caller's context first, then a line over its bound, then the Node
// ending the stream.
func (fl *inflight) streamFailure(ctx context.Context, err error, limit int64) error {
	switch {
	case ctx.Err() != nil:
		return canceled(ctx, fl.op)
	case errors.Is(err, errLineTooLarge):
		return command.NoResultf("%s: a /tap line exceeds %s", fl.op, size(limit))
	case errors.Is(err, io.EOF):
		return command.NoResultf("%s: %w", fl.op, ErrStreamEnded)
	default:
		return command.NoResultf("%s: %w: %w", fl.op, ErrStreamEnded, err)
	}
}
