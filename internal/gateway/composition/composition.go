// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package composition

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/gateway/admission"
	"github.com/ravindu-rev/ruralz/internal/gateway/body"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream"
	"github.com/ravindu-rev/ruralz/internal/jsonval"
	"github.com/ravindu-rev/ruralz/internal/routematch"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Leg runs one upstream leg of a step: *upstream.Upstream implements it
// (the Upstream layer's attempt loop).
type Leg interface {
	Leg(ctx context.Context, o *snapshot.Outbound, t upstream.Target) (*snapshot.UpstreamResponse, error)
}

// Decoder decodes one JSON body for CEL, the value later steps read as
// steps.<name>.body; expr.Compiler implements it. budget is the decoded
// size cap, past which it fails with expr.ErrTooLarge; built is the size
// of the values it built.
type Decoder interface {
	DecodeJSON(data []byte, budget int64) (v expr.Value, built int64, err error)
}

// Deps are the Node-wide dependencies of the engine.
type Deps struct {
	// Clock times failures (the first to fail in time decides) and the
	// steps' requests and responses for their current age; nil means
	// clock.Real().
	Clock clock.Clock
	// Budget is the Node buffer budget, limits.maxBufferedBytes. A nil
	// Budget refuses every reservation, so a gated step with a non-empty
	// body fails with RZ-RT-004.
	Budget *body.Budget
	// Units is the Node in-flight unit ceiling; an aggregate takes one
	// unit per step. nil takes none.
	Units *admission.Units
	// Decoder decodes the step bodies later steps read; nil reads every
	// such body as JSON null.
	Decoder Decoder
	// Logger records recovered step faults at error, with their stack; nil
	// discards.
	Logger *slog.Logger
}

// Engine compiles composition Routes and runs them. One Engine serves
// every snapshot of the process; it is safe for concurrent use.
type Engine struct {
	clk    clock.Clock
	budget *body.Budget
	units  *admission.Units
	dec    Decoder
	log    *slog.Logger
	// runs pools the per-request state.
	runs sync.Pool
}

// New returns an Engine over d.
func New(d Deps) *Engine {
	e := &Engine{clk: d.Clock, budget: d.Budget, units: d.Units, dec: d.Decoder, log: d.Logger}
	if e.clk == nil {
		e.clk = clock.Real()
	}
	if e.log == nil {
		e.log = slog.New(slog.DiscardHandler)
	}
	e.runs.New = func() any { return new(run) }
	return e
}

// Input is one composition Route as the snapshot compiler hands it to
// Compile.
type Input struct {
	// Route is the Route's metadata.name, for errors and logs.
	Route string
	// Composition is Route.spec.composition.
	Composition *v1alpha1.Composition
	// Methods is Route.spec.match.methods, the incoming methods that reach
	// the Route (any when empty); it tells which steps may send the client
	// body.
	Methods []string
	// Upstream returns the snapshot's leg runner of an Upstream name.
	Upstream func(name string) (Leg, bool)
	// Builder compiles the steps' pathExpression and when at their sites
	// (the composition mode and the earlier steps).
	Builder expr.Builder
	// Limits are the snapshot's effective limits.
	Limits snapshot.Limits
}

// Composition is the compiled composition of one Route in one snapshot,
// the Route's snapshot.Forwarder. It is immutable and safe for concurrent
// use.
type Composition struct {
	e     *Engine
	route string
	mode  v1alpha1.CompositionMode
	steps []step
	// maxResponse is limits.maxResponseBodyBytes, the merged body's cap.
	maxResponse int64
	reqGate     bool
}

var _ snapshot.Forwarder = (*Composition)(nil)

// step is one compiled step.
type step struct {
	name     string
	upstream string
	leg      Leg
	// method is the step's method; "" sends the incoming one.
	method string
	// path and query are a literal path's parts; hasQuery is set when it
	// held a "?", which stops the client's query being forwarded.
	path, query string
	hasQuery    bool
	pathExpr    expr.Program
	when        expr.Program
	optional    bool
	maxBody     int64
	// merges is set for an aggregate step and for a step that sets
	// target, select, rename or group.
	merges bool
	// readLater is set when a later step's expression reads this step's
	// body through steps.
	readLater bool
	// target is the parsed target; nil when absent.
	target jsonval.DotPath
	// selects keeps the named members; nil keeps every member.
	selects    map[string]struct{}
	rename     map[string]string
	collection bool
	group      string
}

// gate reports whether the step reads its response body whole.
func (s *step) gate() bool { return s.merges || s.readLater }

// Compile-time codes, as validation reports them first.
const (
	codeSchema     = "RZ-CFG-005"
	codeReference  = "RZ-CFG-009"
	codeSteps      = "RZ-CFG-031"
	codeStepBodies = "RZ-CFG-032"
	codeCELInvalid = "RZ-CFG-014"
)

// Compile builds the Composition of one Route for one snapshot, off the
// request path. Its errors carry the RZ-CFG code validation gives the same
// composition.
func (e *Engine) Compile(in Input) (*Composition, error) {
	c := in.Composition
	if c == nil || len(c.Steps) == 0 {
		return nil, compileErr(codeSchema, in.Route, "composition has no steps")
	}
	switch c.Mode {
	case v1alpha1.CompositionModeAggregate, v1alpha1.CompositionModeSequential, v1alpha1.CompositionModeConditional:
	default:
		return nil, compileErr(codeSchema, in.Route, "composition mode "+strconv.Quote(string(c.Mode))+" is unknown")
	}
	if limit := in.Limits.MaxCompositionSteps; limit > 0 && len(c.Steps) > limit {
		return nil, compileErr(codeSteps, in.Route, fmt.Sprintf("composition has %d steps, more than limits.maxCompositionSteps (%d)", len(c.Steps), limit))
	}
	bodies, err := stepBodies(c.Steps, in.Limits.MaxResponseBodyBytes)
	if err != nil {
		return nil, fmt.Errorf("composition: route %q: %w", in.Route, err)
	}
	names := make([]string, len(c.Steps))
	for i := range c.Steps {
		names[i] = c.Steps[i].Name
	}
	comp := &Composition{e: e, route: in.Route, mode: c.Mode, steps: make([]step, len(c.Steps)), maxResponse: in.Limits.MaxResponseBodyBytes}
	for i := range c.Steps {
		st, err := compileStep(&in, c.Mode, &c.Steps[i], names[:i:i])
		if err != nil {
			return nil, fmt.Errorf("composition: route %q: step %q: %w", in.Route, c.Steps[i].Name, err)
		}
		st.maxBody = bodies[i]
		comp.steps[i] = st
	}
	if c.Mode == v1alpha1.CompositionModeSequential {
		markReadLater(comp.steps)
	}
	comp.reqGate = requestGate(c.Mode, comp.steps, in.Methods)
	return comp, nil
}

// compileStep compiles one step; earlier are the names of the steps before
// it in list order.
func compileStep(in *Input, mode v1alpha1.CompositionMode, s *v1alpha1.CompositionStep, earlier []string) (step, error) {
	st := step{
		name: s.Name, upstream: s.Upstream, method: s.Method,
		optional:   s.Optional != nil && *s.Optional,
		collection: s.Collection != nil && *s.Collection,
		group:      s.Group,
	}
	found := false
	if in.Upstream != nil {
		st.leg, found = in.Upstream(s.Upstream)
	}
	if !found || st.leg == nil {
		return step{}, errcode.Errorf(codeReference, "Upstream %q not found", s.Upstream)
	}
	switch {
	case s.Path != "" && s.PathExpression != "":
		return step{}, errcode.Errorf(codeSchema, "path and pathExpression are both set")
	case s.PathExpression != "":
		prg, err := compileCEL(in.Builder, expr.Site{Place: expr.PlaceStepPathExpression, Mode: mode, EarlierSteps: earlier}, s.PathExpression)
		if err != nil {
			return step{}, err
		}
		st.pathExpr = prg
	default:
		p, q, hasQuery := strings.Cut(s.Path, "?")
		if err := routematch.CheckForwardPath(p); err != nil {
			return step{}, errcode.Errorf(codeSchema, "path is not a valid step path: %w", err)
		}
		if hasQuery {
			if err := routematch.CheckForwardQuery(q); err != nil {
				return step{}, errcode.Errorf(codeSchema, "path query holds a control byte: %w", err)
			}
		}
		st.path, st.query, st.hasQuery = p, q, hasQuery
	}
	if s.When != "" {
		if mode == v1alpha1.CompositionModeAggregate {
			return step{}, errcode.Errorf(codeSchema, "an aggregate step never evaluates when")
		}
		prg, err := compileCEL(in.Builder, expr.Site{Place: expr.PlaceStepWhen, Mode: mode, EarlierSteps: earlier}, s.When)
		if err != nil {
			return step{}, err
		}
		st.when = prg
	}
	if s.Target != "" {
		p, err := jsonval.ParseDotPath(s.Target)
		if err != nil {
			return step{}, errcode.Errorf(codeSchema, "target is not a valid dot path: %w", err)
		}
		st.target = p
	}
	if st.collection && st.group == "" {
		return step{}, errcode.Errorf(codeSchema, "collection needs group")
	}
	if len(s.Select) > 0 {
		st.selects = make(map[string]struct{}, len(s.Select))
		for _, f := range s.Select {
			st.selects[f] = struct{}{}
		}
	}
	if len(s.Rename) > 0 {
		st.rename = s.Rename
	}
	st.merges = mode == v1alpha1.CompositionModeAggregate || st.target != nil || st.selects != nil || st.rename != nil || st.group != ""
	return st, nil
}

// compileCEL compiles src at site; a compile failure carries the first
// Issue's code (RZ-CFG-014 or RZ-CFG-015).
func compileCEL(b expr.Builder, site expr.Site, src string) (expr.Program, error) {
	if b == nil {
		return nil, errcode.Errorf(codeCELInvalid, "CEL %s: no program builder", site.Place)
	}
	prg, iss := b.Compile(site, src)
	if prg != nil {
		return prg, nil
	}
	if len(iss) == 0 {
		return nil, errcode.Errorf(codeCELInvalid, "CEL %s: compile failed", site.Place)
	}
	code := iss[0].Code
	if !errcode.Valid(code) {
		code = codeCELInvalid
	}
	return nil, errcode.Errorf(code, "%s", iss[0].Message)
}

// stepBodies returns each step's maxBodyBytes: its own value, else an
// equal share of what limits.maxResponseBodyBytes leaves after the
// explicit values (docs/architecture/02-configuration-model.md "Body
// buffering and limits"). Explicit values that pass the limit, or leave a
// step without one no byte, are RZ-CFG-032; a value below 1 byte is
// RZ-CFG-005.
func stepBodies(steps []v1alpha1.CompositionStep, maxResponse int64) ([]int64, error) {
	out := make([]int64, len(steps))
	var total int64
	free := 0
	for i := range steps {
		s := &steps[i]
		if s.MaxBodyBytes == nil {
			free++
			continue
		}
		v := int64(*s.MaxBodyBytes)
		if v < 1 {
			return nil, errcode.Errorf(codeSchema, "step %q: maxBodyBytes is below 1 byte", s.Name)
		}
		out[i] = v
		if total += v; maxResponse > 0 && (total > maxResponse || total < 0) {
			return nil, errcode.Errorf(codeStepBodies, "step maxBodyBytes values up to step %q total more than limits.maxResponseBodyBytes (%d)", s.Name, maxResponse)
		}
	}
	if free == 0 {
		return out, nil
	}
	share := int64(0)
	if maxResponse > total {
		share = (maxResponse - total) / int64(free)
	}
	if share < 1 {
		return nil, errcode.Errorf(codeStepBodies, "steps without maxBodyBytes get no share of limits.maxResponseBodyBytes (%d)", maxResponse)
	}
	for i := range out {
		if out[i] == 0 {
			out[i] = share
		}
	}
	return out, nil
}

// markReadLater marks the steps of a sequential composition whose body a
// later step's expression reads through steps: the constant keys whose
// body it selects, or, for a computed key or the whole map, every earlier
// step.
func markReadLater(steps []step) {
	for j := range steps {
		for _, prg := range [...]expr.Program{steps[j].pathExpr, steps[j].when} {
			if prg == nil {
				continue
			}
			refs := prg.Refs()
			if refs.AnyStep {
				for i := range j {
					steps[i].readLater = true
				}
				continue
			}
			for _, name := range refs.StepBodies {
				for i := range j {
					if steps[i].name == name {
						steps[i].readLater = true
					}
				}
			}
		}
	}
}

// requestGate reports whether the Route gates the client request body:
// two or more steps may send it in one request (a step without a method
// always does, one with a method when that method can reach the Route; a
// conditional composition runs one step, so it sends the body once at
// most), or an expression reads request.body.
func requestGate(mode v1alpha1.CompositionMode, steps []step, methods []string) bool {
	senders := 0
	for i := range steps {
		if m := steps[i].method; m == "" || mayReceive(methods, m) {
			senders++
		}
	}
	if senders >= 2 && mode != v1alpha1.CompositionModeConditional {
		return true
	}
	for i := range steps {
		for _, prg := range [...]expr.Program{steps[i].pathExpr, steps[i].when} {
			if prg != nil && prg.Refs().RequestBody {
				return true
			}
		}
	}
	return false
}

// mayReceive reports whether a request with method m can reach a Route
// whose match.methods is methods: any method when it is empty, else one it
// lists; OPTIONS always can, as a CORS preflight meets methods that list
// the method it asks for (docs/architecture/03-data-plane.md "Compiled
// structure").
func mayReceive(methods []string, m string) bool {
	return len(methods) == 0 || slices.Contains(methods, m) || m == http.MethodOptions
}

// RequestGate reports whether the Route must gate the client request body
// (snapshot.BodyNeeds.RequestGate): two or more steps may send it, since a
// streamed body is sent once, or an expression reads request.body.
func (c *Composition) RequestGate() bool { return c.reqGate }

// compileErr returns a compile error of route with code.
func compileErr(code, route, msg string) error {
	return errcode.Errorf(code, "composition: route %q: %s", route, msg)
}
