// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package raft

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"

	"github.com/ravindu-rev/ruralz/internal/controlstore"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
)

// Content-class requests and statuses.
const (
	contentPut = "put"
	contentGet = "get"

	statusOK      = "ok"
	statusMissing = "missing"
	statusError   = "error"
)

// contentRequest is one content-class request; a put's object bytes
// follow it.
type contentRequest struct {
	Op     string              `json:"op"`
	Digest controlstore.Digest `json:"digest"`
	Kind   controlstore.Kind   `json:"kind,omitempty"`
	Format string              `json:"format,omitempty"`
	Size   int64               `json:"size,omitempty"`
}

// contentResponse answers one request; a found object's bytes follow a
// get's.
type contentResponse struct {
	Status string            `json:"status"`
	Detail string            `json:"detail,omitempty"`
	Kind   controlstore.Kind `json:"kind,omitempty"`
	Format string            `json:"format,omitempty"`
	Size   int64             `json:"size,omitempty"`
}

// chunk is the size of one limited content write.
const chunk = 64 << 10

// limitedWriter sends through a peer's budget at content priority, so
// content yields to Raft.
type limitedWriter struct {
	ctx context.Context
	w   io.Writer
	lim *limiter
}

// Write sends p in chunks.
func (l *limitedWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		n := min(len(p), chunk)
		if err := l.lim.wait(l.ctx, n, false); err != nil {
			return written, err
		}
		m, err := l.w.Write(p[:n])
		written += m
		if err != nil {
			return written, err
		}
		p = p[n:]
	}
	return written, nil
}

// serveContent serves one content-class connection: puts from the
// replica storing an object on a quorum, gets from one fetching a missing
// object.
func (s *Store) serveContent(ctx context.Context, pc *PeerConn) {
	br := bufio.NewReaderSize(pc, chunk)
	bw := bufio.NewWriterSize(&limitedWriter{ctx: ctx, w: pc, lim: s.trans.lim.of(pc.Peer.ServerID)}, chunk)
	for {
		var req contentRequest
		if err := readHeader(br, &req); err != nil {
			return
		}
		var resp contentResponse
		var body *controlstore.Object
		switch req.Op {
		case contentPut:
			resp = s.receive(req, br)
			if resp.Status == statusError {
				_ = writeHeader(bw, resp)
				_ = bw.Flush()
				return
			}
		case contentGet:
			o, err := s.content.get(req.Digest)
			switch {
			case errors.Is(err, controlstore.ErrNotFound):
				resp = contentResponse{Status: statusMissing}
			case err != nil:
				resp = contentResponse{Status: statusError, Detail: err.Error()}
			default:
				resp = contentResponse{Status: statusOK, Kind: o.Kind, Format: o.Format, Size: int64(len(o.Data))}
				body = &o
			}
		default:
			_ = writeHeader(bw, contentResponse{Status: statusError, Detail: "unknown op"})
			_ = bw.Flush()
			return
		}
		if err := writeHeader(bw, resp); err != nil {
			return
		}
		if body != nil {
			if _, err := bw.Write(body.Data); err != nil {
				return
			}
		}
		if err := bw.Flush(); err != nil {
			return
		}
	}
}

// receive stores one pushed object, verifying it as it streams.
func (s *Store) receive(req contentRequest, r io.Reader) contentResponse {
	probe := controlstore.Object{Kind: req.Kind, Format: req.Format}
	if err := probe.Validate(); err != nil || req.Size < 0 ||
		(req.Kind == controlstore.KindAuditSegment && req.Size > controlstore.MaxAuditSegmentBytes) {
		return contentResponse{Status: statusError, Detail: "malformed object header"}
	}
	if s.content.has(req.Digest) {
		if _, err := io.CopyN(io.Discard, r, req.Size); err != nil {
			return contentResponse{Status: statusError, Detail: err.Error()}
		}
		return contentResponse{Status: statusOK}
	}
	if err := s.content.write(req.Digest, objectHeader{Kind: req.Kind, Format: req.Format, Size: req.Size}, io.LimitReader(r, req.Size)); err != nil {
		return contentResponse{Status: statusError, Detail: err.Error()}
	}
	return contentResponse{Status: statusOK}
}

// push stores objects on one peer.
func (s *Store) push(ctx context.Context, address string, objs []controlstore.Object) error {
	tc, id, err := s.trans.dial(ctx, address, controlstore.ClassContent)
	if err != nil {
		return err
	}
	defer s.trans.release(tc)
	stop := context.AfterFunc(ctx, func() { _ = tc.Close() })
	defer stop()
	br := bufio.NewReaderSize(tc, chunk)
	bw := bufio.NewWriterSize(&limitedWriter{ctx: ctx, w: tc, lim: s.trans.lim.of(id.ServerID)}, chunk)
	for _, o := range objs {
		req := contentRequest{Op: contentPut, Digest: o.Digest(), Kind: o.Kind, Format: o.Format, Size: int64(len(o.Data))}
		if err := writeHeader(bw, req); err != nil {
			return err
		}
		if _, err := bw.Write(o.Data); err != nil {
			return err
		}
		if err := bw.Flush(); err != nil {
			return err
		}
		var resp contentResponse
		if err := readHeader(br, &resp); err != nil {
			return err
		}
		if resp.Status != statusOK {
			return fmt.Errorf("controlstore: %s refused %s: %s", address, req.Digest, resp.Detail)
		}
	}
	return nil
}

// fetch fetches one object from a peer into the local store.
func (s *Store) fetch(ctx context.Context, address string, d controlstore.Digest) error {
	tc, _, err := s.trans.dial(ctx, address, controlstore.ClassContent)
	if err != nil {
		return err
	}
	defer s.trans.release(tc)
	stop := context.AfterFunc(ctx, func() { _ = tc.Close() })
	defer stop()
	br := bufio.NewReaderSize(tc, chunk)
	if err := writeHeader(tc, contentRequest{Op: contentGet, Digest: d}); err != nil {
		return err
	}
	var resp contentResponse
	if err := readHeader(br, &resp); err != nil {
		return err
	}
	switch resp.Status {
	case statusOK:
	case statusMissing:
		return fmt.Errorf("%w: %s does not hold %s", controlstore.ErrNotFound, address, d)
	default:
		return fmt.Errorf("controlstore: %s: %s", address, resp.Detail)
	}
	probe := controlstore.Object{Kind: resp.Kind, Format: resp.Format}
	if err := probe.Validate(); err != nil || resp.Size < 0 {
		return fmt.Errorf("%w: %s sent a malformed object header", controlstore.ErrInvalid, address)
	}
	return s.content.write(d, objectHeader{Kind: resp.Kind, Format: resp.Format, Size: resp.Size}, io.LimitReader(br, resp.Size))
}

// putQuorum stores objects here and on the other voters until a majority
// of the voter set holds them, else RZ-CP-013. Pushes past the quorum
// stop; replicas without an object fetch it when they need it.
func (s *Store) putQuorum(ctx context.Context, objs []controlstore.Object) error {
	for _, o := range objs {
		if err := s.content.put(o); err != nil {
			return controlstore.Errorf(controlstore.ErrContentQuorum, "store %s locally: %w", o.Digest(), err)
		}
	}
	var voters []controlstore.Member
	s.machine.View(func(v controlstore.View) {
		for _, m := range v.Members() {
			if m.Voter {
				voters = append(voters, m)
			}
		}
	})
	need := len(voters)/2 + 1
	have := 0
	var others []controlstore.Member
	for _, m := range voters {
		if m.ServerID == s.id {
			have++
		} else {
			others = append(others, m)
		}
	}
	if have >= need {
		return nil
	}
	pctx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan error, len(others))
	var wg sync.WaitGroup
	for _, m := range others {
		wg.Go(func() { results <- s.push(pctx, m.Address, objs) })
	}
	var errs []error
	for range others {
		err := <-results
		if err == nil {
			have++
			if have >= need {
				break
			}
			continue
		}
		errs = append(errs, err)
	}
	cancel()
	wg.Wait()
	if have >= need {
		return nil
	}
	return controlstore.Errorf(controlstore.ErrContentQuorum, "content reached %d of %d voters, a quorum is %d: %w",
		have, len(voters), need, errors.Join(errs...))
}

// PutContent stores objects here and on a replica quorum.
func (s *Store) PutContent(ctx context.Context, objs ...controlstore.Object) error {
	if err := s.check(); err != nil {
		return err
	}
	for _, o := range objs {
		if err := o.Validate(); err != nil {
			return err
		}
	}
	return s.putQuorum(ctx, objs)
}

// HasContent reports whether an object is stored here.
func (s *Store) HasContent(d controlstore.Digest) bool { return s.content.has(d) }

// Content returns an object, fetching it from a peer when it is missing.
func (s *Store) Content(ctx context.Context, d controlstore.Digest) (controlstore.Object, error) {
	if err := s.check(); err != nil {
		return controlstore.Object{}, err
	}
	o, err := s.content.get(d)
	if err == nil || !errors.Is(err, controlstore.ErrNotFound) {
		return o, err
	}
	if err := s.fetchFromPeers(ctx, d); err != nil {
		return controlstore.Object{}, err
	}
	return s.content.get(d)
}

// fetchFromPeers fetches one object from the leader, then the other
// members, until one holds it.
func (s *Store) fetchFromPeers(ctx context.Context, d controlstore.Digest) error {
	var errs []error
	for _, addr := range s.peerAddresses() {
		err := s.fetch(ctx, addr, d)
		if err == nil {
			return nil
		}
		errs = append(errs, err)
		if ctx.Err() != nil {
			break
		}
	}
	return fmt.Errorf("%w: no peer holds %s: %w", controlstore.ErrNotFound, d, errors.Join(errs...))
}

// peerAddresses returns the leader's address, then the other members',
// excluding this replica.
func (s *Store) peerAddresses() []string {
	_, leader := s.Leader()
	var out []string
	if leader != "" && leader != s.cfg.Address {
		out = append(out, leader)
	}
	s.machine.View(func(v controlstore.View) {
		for _, m := range v.Members() {
			if m.ServerID != s.id && m.Address != leader {
				out = append(out, m.Address)
			}
		}
	})
	return out
}

// fetchLoop fetches the referenced content this replica lacks: after
// entries, an InstallSnapshot or a join, and again at every status report
// until it holds everything, so an assignment is served once its content
// is local.
func (s *Store) fetchLoop() {
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.fetchCh:
		}
		var refs []controlstore.Digest
		s.machine.View(func(v controlstore.View) { refs = v.References() })
		for _, d := range s.content.missing(refs) {
			if err := s.fetchFromPeers(s.ctx, d); err != nil && s.ctx.Err() == nil {
				s.log.Debug("content fetch failed", slog.String(catalog.KeyError, err.Error()))
			}
		}
	}
}
