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
	// schema compares numbers (proposed, package documentation).
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
	// errWorkingSetTooLarge: the decoded body and the working set its
	// validation may hold cost more than filter.DecodedLimitFactor times
	// the raw limit (413 RZ-RT-003, proposed).
	errWorkingSetTooLarge = fmt.Errorf("validation.json-schema: validating the body may hold more than its decoded limit: %w", filter.ErrTooLarge)
	// errWorkTooLarge: validating the body may take more than WorkPerByte
	// work units per byte of the raw limit (413 RZ-RT-003, proposed).
	errWorkTooLarge = fmt.Errorf("validation.json-schema: validating the body may take more than its work limit: %w", filter.ErrTooLarge)
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
// wrong type and an empty schema.
func (Factory) Build(_ context.Context, env filter.BuildEnv) (filter.Filter, error) {
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
	maxCost, maxWork := decodedLimit(env.Limits.MaxRequestBodyBytes), workLimit(env.Limits.MaxRequestBodyBytes)
	prev, _ := env.Previous.(*Filter)
	if prev != nil && prev.schema.sum == p.sum && prev.status == status && prev.maxCost == maxCost && prev.maxWork == maxWork {
		return prev, nil
	}
	var schema *Schema
	switch {
	case prev != nil && prev.schema.sum == p.sum:
		schema = prev.schema
	case env.Shared != nil:
		v, err := env.Shared.Get(sharedKey{p.sum}, func() (any, error) { return compileShared(p) })
		if err != nil {
			if _, coded := errcode.CodeOf(err); !coded {
				err = errcode.Wrap(CodeConfig, err)
			}
			return nil, err
		}
		schema, _ = v.(*Schema)
		if schema == nil || schema.sum != p.sum {
			return nil, errcode.Errorf(CodeConfig, "validation.json-schema: shared schema has type %T", v)
		}
	default:
		s, err := compileShared(p)
		if err != nil {
			return nil, err
		}
		schema = s
	}
	return newFilter(schema, status, maxCost, maxWork), nil
}

// compileShared compiles a prepared document, wrapping failures with
// RZ-CFG-005.
func compileShared(p *prepared) (*Schema, error) {
	s, issues := compilePrepared(p, refusingLoader{})
	if len(issues) > 0 {
		return nil, errcode.Wrap(CodeConfig, &CompileError{Issues: issues})
	}
	return s, nil
}

// defaultMaxRequestBodyBytes is the limits.maxRequestBodyBytes a Gateway
// gets when it sets none (10 MiB, the configuration default; 07 section 9
// item 24), used when the snapshot leaves BuildEnv.Limits unset.
const defaultMaxRequestBodyBytes = 10 << 20

// decodedLimit returns the decode cost budget for a raw body limit:
// filter.DecodedLimitFactor times the limit (architecture R-63). An unset
// limit is the configuration default, so a snapshot that forgets Limits
// never decodes without a budget; a limit too large to multiply gets the
// largest budget.
func decodedLimit(rawLimit int64) int64 {
	if rawLimit <= 0 {
		rawLimit = defaultMaxRequestBodyBytes
	}
	return min(rawLimit, math.MaxInt64/filter.DecodedLimitFactor) * filter.DecodedLimitFactor
}

// Filter is a compiled validation.json-schema Policy. It is immutable
// apart from its decoder pool and safe for concurrent Handle calls.
type Filter struct {
	schema *Schema
	status int
	// maxCost bounds the decoded values with the working set (bytes);
	// maxWork the work of validation (work units).
	maxCost, maxWork int64
	// decoders pools strict decoders; a Decoder drops oversized scratch
	// after each call, so the pool never pins a large body (07 req 73).
	decoders *sync.Pool
}

var _ filter.Filter = (*Filter)(nil)

func newFilter(s *Schema, status int, maxCost, maxWork int64) *Filter {
	opts := jsonval.Options{MapObjects: true, MaxCost: maxCost}
	return &Filter{
		schema:   s,
		status:   status,
		maxCost:  maxCost,
		maxWork:  maxWork,
		decoders: &sync.Pool{New: func() any { return jsonval.NewDecoder(opts) }},
	}
}

// Schema returns the compiled schema.
func (f *Filter) Schema() *Schema { return f.schema }

// Handle validates the client request body in onRequestBody (07 reqs 78 to
// 82) and continues in every other Phase. A refused body is Respond with
// 400 RZ-RT-009; a decoded body that, with the working set its validation
// may hold, is oversized, whose validation may take more than its work
// limit, or whose reservation is refused, is CannotDecide with
// filter.ErrTooLarge or filter.ErrBudget, which the executor answers
// under either failureMode (07 req 15); a body the message cannot supply,
// or an internal validator error (07 req 13), is CannotDecide with the
// class default.
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
	scan := bodyScan{ws: &f.schema.work, numbers: f.schema.numbers, memLimit: f.maxCost - cost, workLimit: f.maxWork}
	if f.maxCost <= 0 {
		// No decode budget (a Filter built outside Factory.Build).
		scan.memLimit = math.MaxInt64 - 1
	}
	if f.maxWork <= 0 {
		scan.workLimit = maxWorkLimit
	}
	scan.scan(v)
	switch {
	case scan.bad:
		return f.reject(DetailNumber)
	case scan.memoryOver():
		return filter.Undecided("", errWorkingSetTooLarge)
	case scan.workOver():
		return filter.Undecided("", errWorkTooLarge)
	}
	// The decoded values and the validator's working set (proposed,
	// package documentation) are reserved together and released when
	// validation ends, the decoded tree's last use (07 req 82). The work
	// is bounded, never reserved.
	n := cost + scan.memory()
	if err := x.Reserve(n); err != nil {
		if !errors.Is(err, filter.ErrBudget) {
			err = fmt.Errorf("%w: %w", filter.ErrBudget, err)
		}
		return filter.Undecided("", err)
	}
	defer x.Release(n)
	loc, ok, err := f.schema.validate(v)
	if err != nil {
		return filter.Undecided("", err)
	}
	if !ok {
		return f.reject(mismatchDetail(loc))
	}
	return filter.Next()
}

// decode decodes body strictly with a pooled decoder.
func (f *Filter) decode(body []byte) (any, int64, error) {
	d, _ := f.decoders.Get().(*jsonval.Decoder)
	if d == nil {
		d = jsonval.NewDecoder(jsonval.Options{MapObjects: true, MaxCost: f.maxCost})
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
