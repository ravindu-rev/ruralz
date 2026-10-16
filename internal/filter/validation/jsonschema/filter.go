// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonschema

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/jsonval"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Type is the Policy type this package implements.
const Type = v1alpha1.PolicyTypeValidationJSONSchema

// Codes, each registered in internal/errcode.
const (
	// CodeConfig is an invalid config.schema (07 req 75).
	CodeConfig = "RZ-CFG-005"
	// CodeRejected is a request body the Policy refuses (07 reqs 78 to 81).
	CodeRejected = "RZ-RT-009"
)

// Problem details of RZ-RT-009 (07 reqs 18, 78 to 81). They are fixed
// operator-facing text; a mismatch names a keyword location of the schema
// and never request content.
const (
	// DetailContentType: the media type is not JSON (07 req 78).
	DetailContentType = "Content-Type is not JSON"
	// DetailEmpty: the body is empty (07 req 79).
	DetailEmpty = "Request body is empty"
	// DetailInvalid: the body is not one strict JSON value (07 req 80).
	DetailInvalid = "Request body is not valid JSON"
	// DetailNumber: a body number is outside the number bounds while the
	// schema compares numbers (architecture R-77, package documentation).
	DetailNumber = "Request body has a number out of range"
	// DetailMismatch prefixes the keyword location of a schema mismatch
	// (07 req 81); a mismatch at the schema root is DetailMismatch alone.
	DetailMismatch = "Request body does not match the schema"
)

// Errors handed to the executor as the cause of cannot decide.
var (
	// errDecodedTooLarge: the decoded body costs more than
	// filter.DecodedLimitFactor times the raw limit (413 RZ-RT-003).
	errDecodedTooLarge = fmt.Errorf("validation.json-schema: decoded body over its limit: %w", filter.ErrTooLarge)
	// errWorkingSetTooLarge: the decision pass may hold more than
	// WorkingSetFactor times the raw limit besides the decoded values (413
	// RZ-RT-003, architecture R-77).
	errWorkingSetTooLarge = fmt.Errorf("validation.json-schema: validating the body may hold more than its working-set limit: %w", filter.ErrTooLarge)
	// errNestingTooDeep: a body value is nested as deep as the schema's
	// nesting limit, where one value may take more than MaxEvaluations
	// evaluations, on a path whose state does not bound it (413
	// RZ-RT-003, architecture R-77).
	errNestingTooDeep = fmt.Errorf("validation.json-schema: the body nests deeper than validating it under the schema is bounded: %w", filter.ErrTooLarge)
	// errWorkTooLarge: validating the body may take more than WorkPerByte
	// work units per byte of the raw limit (413 RZ-RT-003, architecture
	// R-77).
	errWorkTooLarge = fmt.Errorf("validation.json-schema: validating the body may take more than its work limit: %w", filter.ErrTooLarge)
	// errReservationTooLarge: the decoded values, what their maps, arrays
	// and strings hold beyond the decoder's charge, and the decision pass's
	// working set exceed ReservationFactor times the raw limit together (413
	// RZ-RT-003, architecture R-77).
	errReservationTooLarge = fmt.Errorf("validation.json-schema: the decoded body and the working set of validating it exceed the reservation limit: %w", filter.ErrTooLarge)
	// errNoMessage: the Exchange has no message in onRequestBody.
	errNoMessage = errors.New("validation.json-schema: no request message")
)

// maxDetailLocation bounds the keyword location a detail quotes
// (proposed): a longer one is cut back to its longest ancestor that fits,
// which is still a location in the schema.
const maxDetailLocation = 256

// mismatchDetail returns the RZ-RT-009 detail for a keyword location.
func mismatchDetail(keywordLocation string) string {
	if len(keywordLocation) > maxDetailLocation {
		keywordLocation = keywordLocation[:strings.LastIndexByte(keywordLocation[:maxDetailLocation+1], '/')]
	}
	if keywordLocation == "" {
		return DetailMismatch
	}
	return DetailMismatch + " at " + keywordLocation
}

// Factory builds validation.json-schema Filters (filter.Factory).
type Factory struct{}

var _ filter.Factory = Factory{}

// sharedKey keys a compiled Schema in a snapshot's filter.Shared.
type sharedKey struct{ sum [32]byte }

// Build compiles env.Config's schema (07 req 75), reusing the Schema of
// env.Previous when the normalized document is unchanged and sharing one
// Schema per document within a snapshot (07 req 77). A schema that does
// not compile is an error carrying RZ-CFG-005, as are a config of the
// wrong type and an empty schema. A ctx that has ended is its error,
// unchanged and without a code, at the start and between the stages of a
// compile (the library's own compile cannot be interrupted), so a
// canceled or superseded snapshot compile stops early (minor review
// finding); so is a failure of env.Shared that is no finding about the
// schema. Such an error without an RZ-CFG code is an operational failure
// of the snapshot compile, not a rejection of the Revision: the
// filter.Factory contract says every error carries one, and a contract
// request to its owner (WP-01) and to the snapshot compiler (WP-70) asks
// to record these exceptions (minor review finding).
func (Factory) Build(ctx context.Context, env filter.BuildEnv) (filter.Filter, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cfg, ok := env.Config.(*v1alpha1.ValidationJSONSchemaConfig)
	if !ok || cfg == nil {
		return nil, errcode.Errorf(CodeConfig, "validation.json-schema: config is %T, want *v1alpha1.ValidationJSONSchemaConfig", env.Config)
	}
	if len(cfg.Schema) == 0 {
		return nil, errcode.Errorf(CodeConfig, "validation.json-schema: config.schema is required")
	}
	status := errcode.Status(CodeRejected)
	if status == 0 {
		return nil, errcode.Errorf(CodeConfig, "validation.json-schema: %s has no registered status", CodeRejected)
	}
	p, issues := prepare(cfg.Schema)
	if len(issues) > 0 {
		return nil, errcode.Wrap(CodeConfig, &CompileError{Issues: issues})
	}
	limits := limitsOf(env.Limits.MaxRequestBodyBytes)
	prev, _ := env.Previous.(*Filter)
	if prev != nil && prev.schema.sum == p.sum && prev.status == status && prev.limits == limits {
		// Carried over unchanged: its Schema also serves the other
		// Policies of this snapshot with the same document that build
		// after it. When one of them built first (a new Policy has no
		// Previous, so it compiled the document again), this Policy takes
		// that Schema too, so the snapshot holds one per document (minor
		// review finding).
		if env.Shared == nil {
			return prev, nil
		}
		v, err := env.Shared.Get(sharedKey{p.sum}, func() (any, error) { return prev.schema, nil })
		if err != nil {
			return nil, err
		}
		if s, _ := v.(*Schema); s != nil && s != prev.schema && s.sum == p.sum {
			return newFilter(s, status, limits), nil
		}
		return prev, nil
	}
	build := func() (any, error) { return compileShared(ctx, p) }
	if prev != nil && prev.schema.sum == p.sum {
		// Same document under new limits: the previous Schema, also for
		// the other Policies of this snapshot with the same document.
		build = func() (any, error) { return prev.schema, nil }
	}
	var v any
	var err error
	if env.Shared != nil {
		v, err = env.Shared.Get(sharedKey{p.sum}, build)
	} else {
		v, err = build()
	}
	if err != nil {
		// compileShared's own errors carry RZ-CFG-005; any other, such as a
		// canceled snapshot compile, is not a finding about the schema.
		return nil, err
	}
	schema, _ := v.(*Schema)
	if schema == nil || schema.sum != p.sum {
		return nil, fmt.Errorf("validation.json-schema: shared schema has type %T", v)
	}
	return newFilter(schema, status, limits), nil
}

// compileShared compiles a prepared document, wrapping failures with
// RZ-CFG-005; an ended ctx stops it between stages with its error.
func compileShared(ctx context.Context, p *prepared) (*Schema, error) {
	s, issues, err := compilePrepared(p, refusingLoader{}, ctx.Err)
	if err != nil {
		return nil, err
	}
	if len(issues) > 0 {
		return nil, errcode.Wrap(CodeConfig, &CompileError{Issues: issues})
	}
	return s, nil
}

// defaultMaxRequestBodyBytes is the limits.maxRequestBodyBytes a Gateway
// gets when it sets none (10 MiB, the configuration default; 07 section 9
// item 24), used when the snapshot leaves BuildEnv.Limits unset.
const defaultMaxRequestBodyBytes = 10 << 20

// WorkingSetFactor bounds the memory validation may hold (architecture
// R-77, package documentation "Working set"): besides the decoded values,
// the decision pass may hold at most WorkingSetFactor times
// limits.maxRequestBodyBytes, or MinScaledLimit when that is larger, or
// the body is oversized, and the location pass runs only when it fits the
// same bound.
const WorkingSetFactor = 2 * filter.DecodedLimitFactor

// ReservationFactor bounds what Handle reserves for one body (architecture
// R-77): the decoded values at the decoder's charge, what their maps,
// arrays and strings hold beyond it (internal/jsonval charges neither an
// object's hash table nor an array's spare capacity), and the working set
// of the pass that runs, together at most ReservationFactor times
// limits.maxRequestBodyBytes (or MinScaledLimit, the larger), so that with
// the raw body the request side holds at most ReservationFactor+1 times
// the raw limit (130 MiB at the 10 MiB default), and 1.5 MiB more below
// MinScaledLimit. A body whose reservation would exceed it is oversized
// before anything is reserved; a failing body whose location pass would
// exceed it is refused with DetailMismatch alone.
const ReservationFactor = 12

// bodyLimits are the limits a Filter applies to one request body, all
// derived from limits.maxRequestBodyBytes.
type bodyLimits struct {
	// decoded bounds the decoded values at the decoder's charge (bytes),
	// workingSet what either pass may hold besides them (bytes), reserve
	// what Handle reserves for the body (bytes), work the work of
	// validation (work units). A Filter built outside Factory.Build leaves
	// them zero: no bound.
	decoded, workingSet, reserve, work int64
}

// MinScaledLimit is the smallest limits.maxRequestBodyBytes the
// working-set, reservation and work limits scale from (proposed,
// architecture R-77): below it they are those of MinScaledLimit, 1 MiB,
// 1.5 MiB and 2^25 work units. What validating any body holds and takes
// has parts that do not grow with the body: the decision pass's wrapper,
// a call-stack chain per value along the path, a regexp matcher of the
// largest program that may match a string, the evaluations at the root;
// scaled from a limit of a few KiB they exceeded it, so a valid body of a
// few bytes was refused with 413 (review finding: {} under true at 1 and
// 2 KiB, a 616-byte order composed four levels deep at 16 KiB). The
// decoded limit still scales from the raw limit (architecture R-63).
const MinScaledLimit = 128 << 10

// limitsOf returns the limits for a raw body limit: filter.DecodedLimitFactor
// times it for the decoded values (architecture R-63), and from it or
// MinScaledLimit, the larger, WorkingSetFactor times for the working set,
// ReservationFactor times for the reservation and WorkPerByte times for
// the work. An unset limit is the configuration default, so a snapshot
// that forgets Limits never decodes without a budget; a limit too large to
// multiply gets the largest bounds.
func limitsOf(rawLimit int64) bodyLimits {
	scaled := rawLimit
	if scaled > 0 {
		scaled = max(scaled, MinScaledLimit)
	}
	return bodyLimits{
		decoded: decodedLimit(rawLimit), workingSet: scaledLimit(scaled, WorkingSetFactor),
		reserve: scaledLimit(scaled, ReservationFactor), work: workLimit(scaled),
	}
}

// refusal returns the error that refuses a scanned body under the limits
// before the library runs, or nil: a value as deep as the schema's nesting
// limit where the states do not bound it, a decision pass whose working
// set is over its limit, or work over the work limit (each
// filter.ErrTooLarge).
func (l bodyLimits) refusal(scan *bodyScan) error {
	switch {
	case scan.deep:
		return errNestingTooDeep
	case l.workingSet > 0 && scan.decideMemory() > l.workingSet:
		return errWorkingSetTooLarge
	case scan.workOver():
		return errWorkTooLarge
	}
	return nil
}

// decodedLimit returns the decode cost budget for a raw body limit:
// filter.DecodedLimitFactor times the limit (architecture R-63).
func decodedLimit(rawLimit int64) int64 {
	return scaledLimit(rawLimit, filter.DecodedLimitFactor)
}

// scaledLimit returns factor times the raw body limit, the configuration
// default when unset, or the largest multiple that fits.
func scaledLimit(rawLimit, factor int64) int64 {
	if rawLimit <= 0 {
		rawLimit = defaultMaxRequestBodyBytes
	}
	return min(rawLimit, math.MaxInt64/factor) * factor
}

// Filter is a compiled validation.json-schema Policy. It is immutable
// apart from its decoder pool and safe for concurrent Handle calls.
type Filter struct {
	schema *Schema
	status int
	limits bodyLimits
	// decoders pools strict decoders; a Decoder drops oversized scratch
	// after each call, so the pool never pins a large body (07 req 73).
	decoders *sync.Pool
}

var _ filter.Filter = (*Filter)(nil)

func newFilter(s *Schema, status int, limits bodyLimits) *Filter {
	opts := jsonval.Options{MapObjects: true, MaxCost: limits.decoded}
	return &Filter{
		schema:   s,
		status:   status,
		limits:   limits,
		decoders: &sync.Pool{New: func() any { return jsonval.NewDecoder(opts) }},
	}
}

// Schema returns the compiled schema.
func (f *Filter) Schema() *Schema { return f.schema }

// Handle validates the client request body in onRequestBody (07 reqs 78 to
// 82) and continues in every other Phase. A refused body is Respond with
// 400 RZ-RT-009. A decoded body that is oversized, whose validation may
// hold more than its working-set limit or take more than its work limit,
// whose reservation would exceed its reservation limit, or that nests as
// deep as the schema's nesting limit where the states do not bound it, is
// CannotDecide with filter.ErrTooLarge, and one whose reservation the
// budget refuses with filter.ErrBudget, both of which the executor answers
// under either failureMode (07 req 15). A body the message cannot supply,
// a request whose context ends before validation, or an internal validator
// error (07 req 13), is CannotDecide with the class default.
//
// Validation runs in two passes (package documentation "Working set"):
// the decision pass, whose working set is reserved with the decoded
// values, then, for a body that fails, the location pass that names the
// failing keyword, when its working set fits the same limits, its work the
// work limit with the first pass's, and the budget its reservation; a
// failing body that does not get one is refused with DetailMismatch alone.
func (f *Filter) Handle(ctx context.Context, p phase.Phase, x filter.Exchange) filter.Result {
	if p != phase.OnRequestBody {
		return filter.Next()
	}
	msg := x.Message()
	if msg == nil {
		return filter.Undecided("", errNoMessage)
	}
	if !jsonContentType(msg.Header()["Content-Type"]) {
		return f.reject(DetailContentType)
	}
	body, err := msg.Body(ctx)
	if err != nil {
		return filter.Undecided("", fmt.Errorf("validation.json-schema: request body: %w", err))
	}
	if len(body) == 0 {
		return f.reject(DetailEmpty)
	}
	v, cost, err := f.decode(body)
	if err != nil {
		if errors.Is(err, jsonval.ErrTooLarge) {
			return filter.Undecided("", errDecodedTooLarge)
		}
		return f.reject(DetailInvalid)
	}
	scan := bodyScan{ws: &f.schema.work, numbers: f.schema.numbers, workLimit: f.limits.work}
	if f.limits.work <= 0 {
		scan.workLimit = maxWorkLimit
	}
	scan.scan(v)
	if scan.bad {
		return f.reject(DetailNumber)
	}
	if err := f.limits.refusal(&scan); err != nil {
		return filter.Undecided("", err)
	}
	// The decoded values, with what their objects' hash tables, their
	// arrays' spare capacity and their strings' size classes hold beyond
	// the decoder's charge, and the decision pass's working set
	// (architecture R-77, package documentation) are reserved together,
	// within the reservation limit, and released when validation ends, the
	// decoded tree's last use (07 req 82). The work is bounded, never
	// reserved.
	decoded := addCap(cost, scan.extra)
	n := addCap(decoded, scan.decideMemory())
	if f.limits.reserve > 0 && n > f.limits.reserve {
		return filter.Undecided("", errReservationTooLarge)
	}
	// The library takes no context: a request that has ended is not
	// validated (07 section 9 item 21).
	if err := ctx.Err(); err != nil {
		return filter.Undecided("", err)
	}
	if err := x.Reserve(n); err != nil {
		if !errors.Is(err, filter.ErrBudget) {
			err = fmt.Errorf("%w: %w", filter.ErrBudget, err)
		}
		return filter.Undecided("", err)
	}
	defer x.Release(n)
	if f.schema.valid(v) {
		return filter.Next()
	}
	return f.locate(ctx, x, v, &scan, decoded)
}

// locate runs the location pass for v, a body the decision pass refused,
// and returns the refusal: with the failing keyword when the pass fits its
// limits (scan holds the body's bounds, decoded what its decoded values
// reserve), DetailMismatch alone otherwise; an internal validator error is
// cannot decide (07 req 13).
func (f *Filter) locate(ctx context.Context, x filter.Exchange, v any, scan *bodyScan, decoded int64) filter.Result {
	switch {
	case f.limits.workingSet > 0 && scan.locateMemory() > f.limits.workingSet,
		f.limits.reserve > 0 && addCap(decoded, scan.locateMemory()) > f.limits.reserve,
		scan.locateOver(), ctx.Err() != nil:
		return f.reject(DetailMismatch)
	}
	if extra := scan.locateMemory() - scan.decideMemory(); extra > 0 {
		if err := x.Reserve(extra); err != nil {
			return f.reject(DetailMismatch)
		}
		defer x.Release(extra)
	}
	// Should the passes disagree, which only the library could make them
	// do, validate reports the document root ("") and the decision stands.
	loc, _, err := f.schema.validate(v, func() bool { return ctx.Err() != nil })
	if err != nil {
		return filter.Undecided("", err)
	}
	return f.reject(mismatchDetail(loc))
}

// decode decodes body strictly with a pooled decoder.
func (f *Filter) decode(body []byte) (any, int64, error) {
	d, _ := f.decoders.Get().(*jsonval.Decoder)
	if d == nil {
		d = jsonval.NewDecoder(jsonval.Options{MapObjects: true, MaxCost: f.limits.decoded})
	}
	v, cost, err := d.Decode(body)
	f.decoders.Put(d)
	return v, cost, err
}

// reject returns 400 RZ-RT-009 with detail.
func (f *Filter) reject(detail string) filter.Result {
	return filter.Reply(&filter.Response{Status: f.status, Code: CodeRejected, Detail: detail})
}

// jsonContentType applies 07 req 78 to the Content-Type field lines: one
// line naming application/json or a */*+json type, whose parameters are
// all charset=utf-8 (name and value case-insensitive, the value optionally
// quoted); empty parameters are allowed as RFC 9110 allows them.
func jsonContentType(lines []string) bool {
	if len(lines) != 1 || !jsonval.IsJSONMediaType(lines[0]) {
		return false
	}
	_, params, found := strings.Cut(lines[0], ";")
	for found {
		var p string
		p, params, found = strings.Cut(params, ";")
		if p = strings.Trim(p, " \t"); p != "" && !charsetUTF8(p) {
			return false
		}
	}
	return true
}

// charsetUTF8 reports whether the parameter p is charset=utf-8.
func charsetUTF8(p string) bool {
	name, value, ok := strings.Cut(p, "=")
	if !ok || !strings.EqualFold(name, "charset") {
		return false
	}
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		value = value[1 : len(value)-1]
	}
	return strings.EqualFold(value, "utf-8")
}
