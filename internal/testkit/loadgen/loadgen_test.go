// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package loadgen

import (
	"context"
	"errors"
	"io"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
)

// The schedule offers exactly Rate × Duration requests, request k at
// start + k/Rate.
func TestOpenLoopScheduleIsExact(t *testing.T) {
	fk := clocktest.New(epoch())
	s := seen{clk: fk}
	tgt := targetFunc(func(context.Context) Response {
		s.add()
		return Response{Status: 200}
	})
	g := start(t, tgt, Config{Rate: 10, Duration: time.Second, Clock: fk})
	res := drive(t, g, fk, ms(100))
	times := s.list()
	if len(times) != 10 {
		t.Fatalf("%d requests, want 10", len(times))
	}
	for k, at := range times {
		if !at.Equal(epoch().Add(ms(100 * k))) {
			t.Errorf("request %d sent at %v, want %v", k, at, epoch().Add(ms(100*k)))
		}
	}
	if res.Offered != 10 || res.Outcomes[Success] != 10 || res.Statuses[200] != 10 {
		t.Errorf("offered %d success %d statuses %v; want 10, 10, 10 of 200", res.Offered, res.Outcomes[Success], res.Statuses)
	}
}

// stallFirst returns a target that holds its first request until release
// is closed or the request's context ends, answering status at once to
// the others.
func stallFirst(s *seen, release <-chan struct{}, status int) Target {
	return targetFunc(func(ctx context.Context) Response {
		if s.add() == 0 {
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
		return Response{Status: status}
	})
}

// A stalled request never delays the schedule: the requests due behind it
// are dispatched at their intended times into the backlog, open loop, and
// go out once a worker is free.
func TestStallDoesNotDelayTheSchedule(t *testing.T) {
	fk := clocktest.New(epoch())
	release := make(chan struct{})
	s := seen{clk: fk}
	g := start(t, stallFirst(&s, release, 200), Config{Rate: 10, Duration: time.Second, MaxInFlight: 1, Clock: fk})
	waitFor(t, "request 0 in flight", func() bool { return progressOf(g).inFlight == 1 })
	fk.Advance(time.Second)
	waitFor(t, "requests 1 to 9 backlogged", func() bool { return progressOf(g).backlog == 9 })
	if p := progressOf(g); p.scheduled != 10 || p.done != 0 {
		t.Fatalf("while request 0 stalls: %d scheduled, %d done; want 10 and 0", p.scheduled, p.done)
	}
	close(release)
	res, err := g.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcomes[Success] != 10 || res.Outcomes[Missed] != 0 {
		t.Errorf("success %d missed %d, want 10 and 0", res.Outcomes[Success], res.Outcomes[Missed])
	}
	if n := len(s.list()); n != 10 {
		t.Errorf("the target saw %d requests, want 10", n)
	}
}

// A request finding every worker busy and the backlog full is Missed:
// counted, never sent.
func TestMissedWhenNoWorkerOrBacklogRoom(t *testing.T) {
	for _, c := range []struct {
		name               string
		backlog            int
		wantMissed, wantOK int64
	}{
		{"no backlog", -1, 9, 1},
		{"backlog of three", 3, 6, 4},
	} {
		t.Run(c.name, func(t *testing.T) {
			fk := clocktest.New(epoch())
			release := make(chan struct{})
			s := seen{clk: fk}
			g := start(t, stallFirst(&s, release, 204), Config{Rate: 10, Duration: time.Second, MaxInFlight: 1, MaxBacklog: c.backlog, Clock: fk})
			waitFor(t, "request 0 in flight", func() bool { return progressOf(g).inFlight == 1 })
			fk.Advance(time.Second)
			waitFor(t, "the misses", func() bool { return progressOf(g).missed == c.wantMissed })
			close(release)
			res, err := g.Wait()
			if err != nil {
				t.Fatal(err)
			}
			if res.Offered != 10 || res.Outcomes[Missed] != c.wantMissed || res.Outcomes[Success] != c.wantOK {
				t.Errorf("offered %d missed %d success %d, want 10, %d, %d", res.Offered, res.Outcomes[Missed], res.Outcomes[Success], c.wantMissed, c.wantOK)
			}
			if sent := int64(len(s.list())); sent != c.wantOK {
				t.Errorf("the target saw %d requests, want %d: a Missed request is never sent", sent, c.wantOK)
			}
		})
	}
}

// Without Duration the run lasts until its context ends; the requests
// already due went out, and Wait returns the context's error.
func TestContextEndsAnOpenEndedRun(t *testing.T) {
	fk := clocktest.New(epoch())
	ctx, cancel := context.WithCancel(t.Context())
	g, err := Start(ctx, ok(), Config{Rate: 10, Clock: fk})
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		waitFor(t, "the generator to go idle", func() bool { return idle(g, fk) })
		fk.Advance(ms(100))
	}
	waitFor(t, "the generator to go idle", func() bool { return idle(g, fk) })
	cancel()
	res, err := g.Wait()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait error = %v, want context.Canceled", err)
	}
	if res.Offered != 4 || res.Outcomes[Success] != 4 {
		t.Errorf("offered %d success %d, want 4 and 4", res.Offered, res.Outcomes[Success])
	}
}

// The run's context ends the schedule at once and cancels requests in
// flight; Wait returns the context's error with the partial result.
func TestContextEndCancelsInFlight(t *testing.T) {
	fk := clocktest.New(epoch())
	ctx, cancel := context.WithCancel(t.Context())
	tgt := targetFunc(func(ctx context.Context) Response {
		<-ctx.Done()
		return Response{Err: ctx.Err()}
	})
	g, err := Start(ctx, tgt, Config{Rate: 10, Duration: time.Second, Clock: fk})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "request 0 in flight", func() bool { return progressOf(g).inFlight == 1 })
	cancel()
	res, err := g.Wait()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait error = %v, want context.Canceled", err)
	}
	if res.Offered != 1 || res.Outcomes[Canceled] != 1 {
		t.Errorf("offered %d canceled %d, want 1 and 1", res.Offered, res.Outcomes[Canceled])
	}
}

// The request timeout runs on the injected clock; a target that only
// sees its context canceled still reports Timeout.
func TestTimeoutRunsOnTheInjectedClock(t *testing.T) {
	fk := clocktest.New(epoch())
	tgt := targetFunc(func(ctx context.Context) Response {
		<-ctx.Done()
		return Response{Err: ctx.Err()} // context.Canceled, as net/http reports
	})
	g := start(t, tgt, Config{Rate: 1, Duration: time.Second, Timeout: 2 * time.Second, Clock: fk})
	waitFor(t, "the request's timeout to be armed", func() bool { return fk.Pending() == 1 })
	fk.Advance(2*time.Second - 1)
	if progressOf(g).done != 0 {
		t.Fatal("the request ended before its timeout")
	}
	fk.Advance(1)
	res, err := g.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcomes[Timeout] != 1 {
		t.Fatalf("outcomes %v, want one Timeout", res.Outcomes)
	}
}

// Responses are counted by status, and error responses by the RZ code of
// their problem document; a success carries no counted code.
func TestStatusesAndCodes(t *testing.T) {
	fk := clocktest.New(epoch())
	s := seen{clk: fk}
	tgt := targetFunc(func(context.Context) Response {
		switch s.add() % 10 {
		case 0:
			return Response{Status: 503, Code: "RZ-RT-005"}
		case 1:
			return Response{Status: 429, Code: "RZ-RL-001"}
		case 2:
			return Response{Status: 200, Code: "RZ-RT-005"}
		}
		return Response{Status: 200}
	})
	g := start(t, tgt, Config{Rate: 10, Duration: 3 * time.Second, Clock: fk})
	res := drive(t, g, fk, ms(100))
	if res.Offered != 30 || res.Outcomes[ServerError] != 3 || res.Outcomes[ClientError] != 3 || res.Outcomes[Success] != 24 {
		t.Errorf("offered %d outcomes %v, want 30 with 3 server errors, 3 client errors and 24 successes", res.Offered, res.Outcomes)
	}
	if res.Statuses[503] != 3 || res.Statuses[429] != 3 || res.Statuses[200] != 24 {
		t.Errorf("statuses %v, want 3 of 503, 3 of 429, 24 of 200", res.Statuses)
	}
	if len(res.Codes) != 2 || res.Codes["RZ-RT-005"] != 3 || res.Codes["RZ-RL-001"] != 3 {
		t.Errorf("codes %v, want RZ-RT-005 and RZ-RL-001 three times each", res.Codes)
	}
}

func TestStartRejects(t *testing.T) {
	good := func() Config { return Config{Rate: 10, Duration: time.Second} }
	for _, c := range []struct {
		name string
		edit func(*Config)
	}{
		{"zero rate", func(c *Config) { c.Rate = 0 }},
		{"negative rate", func(c *Config) { c.Rate = -1 }},
		{"NaN rate", func(c *Config) { c.Rate = math.NaN() }},
		{"infinite rate", func(c *Config) { c.Rate = math.Inf(1) }},
		{"negative duration", func(c *Config) { c.Duration = -1 }},
		{"negative timeout", func(c *Config) { c.Timeout = -1 }},
		{"negative in-flight bound", func(c *Config) { c.MaxInFlight = -1 }},
	} {
		cfg := good()
		c.edit(&cfg)
		if _, err := Start(t.Context(), ok(), cfg); err == nil {
			t.Errorf("%s: Start succeeded, want an error", c.name)
		}
	}
	if _, err := Start(t.Context(), nil, good()); err == nil {
		t.Error("nil target: Start succeeded, want an error")
	}
	if _, err := Run(t.Context(), ok(), Config{}); err == nil {
		t.Error("Run with a zero Config succeeded, want an error")
	}
}

func TestNormalizeOutcome(t *testing.T) {
	for _, c := range []struct {
		name               string
		r                  Response
		timedOut, runEnded bool
		want               Outcome
	}{
		{"error classified", Response{Err: io.EOF}, false, false, Closed},
		{"status classified", Response{Status: 503}, false, false, ServerError},
		{"client status", Response{Status: 429}, false, false, ClientError},
		{"plain success", Response{}, false, false, Success},
		{"timeout overrides a failure", Response{Err: context.Canceled}, true, false, Timeout},
		{"run end overrides a failure", Response{Err: context.Canceled}, false, true, Canceled},
		{"a response stands after a timeout", Response{Outcome: ServerError, Status: 500}, true, false, ServerError},
		{"explicit outcome kept", Response{Outcome: Reset, Err: io.EOF}, false, false, Reset},
		{"missed stays missed", Response{Outcome: Missed}, true, true, Missed},
	} {
		if got := normalize(c.r, c.timedOut, c.runEnded); got != c.want {
			t.Errorf("%s: normalize = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestOutcomeNames(t *testing.T) {
	names := map[string]bool{}
	for o := Outcome(0); o < NumOutcomes; o++ {
		n := o.String()
		if names[n] || strings.HasPrefix(n, "Outcome(") {
			t.Errorf("outcome %d name %q is duplicate or unnamed", o, n)
		}
		names[n] = true
	}
	if got := NumOutcomes.String(); got != "Outcome(11)" {
		t.Errorf("NumOutcomes.String() = %q", got)
	}
	for _, c := range []struct {
		status int
		want   Outcome
	}{{200, Success}, {302, Success}, {404, ClientError}, {599, ServerError}} {
		if got := OutcomeForStatus(c.status); got != c.want {
			t.Errorf("OutcomeForStatus(%d) = %v, want %v", c.status, got, c.want)
		}
	}
	if !ServerError.Response() || Reset.Response() {
		t.Error("Response() misclassifies")
	}
}
