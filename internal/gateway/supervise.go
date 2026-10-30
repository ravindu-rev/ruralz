// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"

	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
)

// errReturned is the cause of a background part whose Run returned nil
// before the Node stopped it.
var errReturned = errors.New("returned before the Node stopped it")

// supervisor runs background parts on goroutines it owns, all ended by its
// context (docs/architecture/03-data-plane.md "Process and concurrency
// model"): a part whose Run returns before the context ends stopped after
// boot, and a panic in any goroutine is caught; both are logged at error
// and fail the Node, a Drain on the stop-signal timeline that ends with
// exit 1. A part ended by the context is no failure.
type supervisor struct {
	ctx  context.Context
	log  *slog.Logger
	fail func(error)
	wg   sync.WaitGroup
}

// newSupervisor returns a supervisor of goroutines ended by ctx, failing
// the Node through fail.
func newSupervisor(ctx context.Context, log *slog.Logger, fail func(error)) *supervisor {
	return &supervisor{ctx: ctx, log: log, fail: fail}
}

// goRun runs the background part name until the supervisor's context
// ends: it must not return before.
func (s *supervisor) goRun(name string, run func(context.Context) error) {
	s.wg.Go(func() {
		panicked, err := s.call(name, run)
		if s.ctx.Err() != nil && !panicked {
			return
		}
		if err == nil {
			err = errReturned
		}
		if !panicked {
			s.log.ErrorContext(s.ctx, "a part of the Node stopped", "part", name, catalog.KeyError, err.Error())
		}
		s.fail(fmt.Errorf("%s: %w", name, err))
	})
}

// goFunc runs f, which may return at any time; only a panic fails the
// Node.
func (s *supervisor) goFunc(name string, f func(context.Context)) {
	s.wg.Go(func() {
		if panicked, err := s.call(name, func(ctx context.Context) error { f(ctx); return nil }); panicked {
			s.fail(fmt.Errorf("%s: %w", name, err))
		}
	})
}

// call runs run, recovering a panic: it is logged with its stack and
// returned as an error.
func (s *supervisor) call(name string, run func(context.Context) error) (panicked bool, err error) {
	defer func() {
		if v := recover(); v != nil {
			s.log.ErrorContext(s.ctx, "a part of the Node panicked", "part", name,
				catalog.KeyError, fmt.Sprint(v), catalog.KeyPanicType, fmt.Sprintf("%T", v), catalog.KeyStack, string(debug.Stack()))
			panicked, err = true, fmt.Errorf("panic: %v", v)
		}
	}()
	return false, run(s.ctx)
}

// wait waits until every goroutine of the supervisor has ended, or ctx
// ends first.
func (s *supervisor) wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("supervisor: %w", ctx.Err())
	}
}
