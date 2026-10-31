// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package handover

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// line is one message line read from a peer, or the error that ended the
// reading.
type line struct {
	b   []byte
	err error
}

// peer is one connection of the handover socket. Its reader goroutine
// reads one line at a time and hands it over on in, so a session waits on
// a message, its timers on the injected clock and the Drain in one
// select; the error that ends the reading, io.EOF when the other process
// closed, comes last. Writes are synchronous: no phase of the protocol
// sends enough to fill the socket's buffer, and each write ends when its
// context does.
type peer struct {
	c    net.Conn
	in   chan line
	quit chan struct{}
	done chan struct{}
	once sync.Once
	wmu  sync.Mutex
}

// newPeer starts reading c; close stops it.
func newPeer(c net.Conn) *peer {
	p := &peer{c: c, in: make(chan line), quit: make(chan struct{}), done: make(chan struct{})}
	go p.read()
	return p
}

// read reads lines of at most MaxMessage bytes until an error, handing
// each over on in.
func (p *peer) read() {
	defer close(p.done)
	br := bufio.NewReaderSize(p.c, MaxMessage+1)
	for {
		b, err := br.ReadSlice('\n')
		if err != nil {
			if errors.Is(err, bufio.ErrBufferFull) {
				err = fmt.Errorf("%w: a message is longer than %d bytes", ErrProtocol, MaxMessage)
			}
			p.deliver(line{err: err})
			return
		}
		if !p.deliver(line{b: bytes.Clone(b[:len(b)-1])}) {
			return
		}
	}
}

// deliver hands l over; false once the peer is closed.
func (p *peer) deliver(l line) bool {
	select {
	case p.in <- l:
		return true
	case <-p.quit:
		return false
	}
}

// send writes v as one message line; the write ends when ctx does.
func (p *peer) send(ctx context.Context, v any) error {
	b, err := encode(v)
	if err != nil {
		return err
	}
	p.wmu.Lock()
	defer p.wmu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = p.c.SetWriteDeadline(time.Unix(1, 0)) })
	defer stop()
	if _, err := p.c.Write(b); err != nil {
		return fmt.Errorf("handover: write: %w", err)
	}
	return nil
}

// close closes the connection and waits for the reader goroutine; it is
// idempotent.
func (p *peer) close() {
	p.once.Do(func() {
		close(p.quit)
		_ = p.c.Close()
	})
	<-p.done
}
