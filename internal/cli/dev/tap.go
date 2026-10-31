// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package dev

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"strconv"
	"syscall"
	"time"

	"github.com/ravindu-rev/ruralz/internal/cli/adminclient"
	"github.com/ravindu-rev/ruralz/internal/cli/command"
	"github.com/ravindu-rev/ruralz/internal/clock"
)

// Backoff of a new /tap subscription after the Node ended the stream
// (targets): from tapBackoffMin, doubling up to tapBackoffMax.
const (
	tapBackoffMin = time.Second
	tapBackoffMax = 30 * time.Second
)

// tapHelp is the paragraph of ruralz dev tap --help.
const tapHelp = `Subscribes to the Node's /tap stream and prints one line per sampled
exchange, its header values on indented lines (> request, < response),
credentials redacted by the Node. --output json re-emits every line as
received. When the Node ends the stream, the command subscribes again with
backoff from 1s to 30s until Ctrl-C; a refused subscription exits 2.`

// TapSpec returns the ruralz dev tap command.
func TapSpec(o Options) command.Spec {
	return command.Spec{
		Path:    []string{"dev", "tap"},
		Summary: "Stream redacted /tap metadata",
		Help:    tapHelp,
		New:     func() command.Command { return &tapCommand{opts: o} },
	}
}

// tapCommand is one run of ruralz dev tap.
type tapCommand struct {
	opts   Options
	flags  *command.Flags
	admin  adminclient.Flags
	route  string
	sample string
	output string
}

// Flags binds the admin flags with --admin and without --timeout, since a
// stream has no body deadline, then --route, --sample and --output.
func (c *tapCommand) Flags(f *command.Flags) {
	c.flags = f
	c.admin.Bind(f, adminclient.FlagOptions{URL: true})
	f.String(&c.route, "route", "NAME", "", "Only print the events of this Route (a client-side filter)", command.CompleteNone)
	f.String(&c.sample, "sample", "RATE", "1", "Share of exchanges the Node samples, in (0, 1]", command.CompleteNone)
	f.Output(&c.output, "text", "text", "json")
}

// Run subscribes and prints the stream until Ctrl-C or a refusal.
func (c *tapCommand) Run(ctx context.Context, sio command.IO, _ []string) error {
	rate, err := parseSample(c.sample)
	if err != nil {
		return err
	}
	if c.flags.IsSet("route") && c.route == "" {
		return command.Usagef("--route needs a Route name")
	}
	client, err := adminclient.New(c.admin.Config(sio), adminclient.Options{
		Stderr: sio.Stderr, Clock: c.opts.timer(), Rand: c.opts.Rand, Version: c.opts.version(),
	})
	if err != nil {
		return err
	}
	defer client.Close()
	t := &tapper{
		client: client, clock: c.opts.timer(), rate: rate, route: c.route,
		json:  c.output == "json",
		out:   command.NewLineWriter(sio.Stdout),
		errw:  command.NewLineWriter(sio.Stderr),
		color: c.output != "json" && command.Colors(sio, sio.Stdout, c.opts.isTerminalFunc()),
	}
	return t.run(ctx)
}

// parseSample reads --sample RATE: a number in (0, 1].
func parseSample(s string) (float64, error) {
	rate, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(rate) || rate <= 0 || rate > 1 {
		return 0, command.Usagef("invalid value %q for --sample: want a number in (0, 1]", s)
	}
	return rate, nil
}

// tapper runs the subscriptions of one ruralz dev tap.
type tapper struct {
	client *adminclient.Client
	clock  clock.Clock
	rate   float64
	route  string
	json   bool
	color  bool
	out    *command.LineWriter
	errw   *command.LineWriter
}

// run subscribes, prints each stream and subscribes again when the Node
// ends it. The first subscription must succeed; a later one the Node
// refuses ends the command, while an unreachable Node is waited for.
func (t *tapper) run(ctx context.Context) error {
	backoff := tapBackoffMin
	first := true
	for {
		stream, err := t.client.Tap(ctx, t.rate)
		switch {
		case err == nil:
		case first || ctx.Err() != nil || !unreachable(err):
			return err
		default:
			if err := t.wait(ctx, err, &backoff); err != nil {
				return err
			}
			continue
		}
		if !first {
			t.notice("subscribed again to %s", t.client.URL())
		}
		first, backoff = false, tapBackoffMin
		err = t.print(ctx, stream)
		_ = stream.Close()
		if ctx.Err() != nil || !errors.Is(err, adminclient.ErrStreamEnded) {
			return err
		}
		if err := t.wait(ctx, err, &backoff); err != nil {
			return err
		}
	}
}

// wait says why the command subscribes again and waits backoff, which it
// then doubles up to tapBackoffMax; Ctrl-C ends the wait.
func (t *tapper) wait(ctx context.Context, cause error, backoff *time.Duration) error {
	t.notice("%v; subscribing again in %s", cause, command.FormatDuration(*backoff))
	if err := t.clock.Sleep(ctx, *backoff); err != nil {
		return context.Cause(ctx)
	}
	*backoff = min(2**backoff, tapBackoffMax)
	return nil
}

// notice writes one progress line on standard error.
func (t *tapper) notice(format string, args ...any) {
	_ = t.errw.Line("ruralz dev tap: " + fmt.Sprintf(format, args...))
}

// print writes every line of stream until it ends: an event as its text
// form or as received, a drop notice as a warning and, with --output json,
// as received.
func (t *tapper) print(ctx context.Context, stream *adminclient.TapStream) error {
	for {
		line, err := stream.Next(ctx)
		if err != nil {
			return err
		}
		if err := t.line(line); err != nil {
			return command.NoResult(fmt.Errorf("write standard output: %w", err))
		}
	}
}

// line handles one /tap line.
func (t *tapper) line(line []byte) error {
	ev, dropped, ok := decodeTapLine(line)
	switch {
	case ok && dropped >= 0:
		_ = t.errw.Line(droppedWarning(dropped))
		if t.json {
			return t.out.Raw(line)
		}
		return nil
	case ok && t.route != "" && ev.Route != t.route:
		return nil
	case t.json:
		return t.out.Raw(line)
	case !ok:
		_ = t.errw.Line("warning: skipping a /tap line that is not an event")
		return nil
	default:
		return t.out.Raw(formatEvent(ev, t.color))
	}
}

// droppedWarning is the warning of a {"dropped":N} line.
func droppedWarning(n int64) string {
	if n == 1 {
		return "warning: the Node dropped 1 /tap event"
	}
	return "warning: the Node dropped " + strconv.FormatInt(n, 10) + " /tap events"
}

// unreachable reports whether a failed subscription only says that the
// Node could not be reached or answered nothing: a connection that failed,
// was reset or closed, or missed its connection or header deadline. Such a
// Node may be restarting, so ruralz dev tap waits for it. A Node that
// answered, with a problem document, a redirect or another status, refused
// the subscription, and a usage, credential or cleartext problem is the
// operator's to fix.
func unreachable(err error) bool {
	if _, ok := errors.AsType[*adminclient.DeadlineError](err); ok {
		return true
	}
	if _, ok := errors.AsType[*net.OpError](err); ok {
		return true
	}
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EPIPE)
}
