// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package upstreamsigv4

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Type is the Policy type this package implements.
const Type = v1alpha1.PolicyTypeAuthUpstreamSigV4

// Codes, each registered in internal/errcode.
const (
	// CodeConfig is a config the Factory cannot use: the wrong config
	// type.
	CodeConfig = "RZ-CFG-005"
	// CodeUpstreamAuth is an upstream credential that was not obtained or
	// signed: the code of every CannotDecide the Filter returns (401).
	CodeUpstreamAuth = "RZ-AUTH-020"
)

// Errors of the Factory and the Filter; their messages carry no secret.
var (
	// ErrNoCredentialSource reports a Factory without a credential source:
	// a fault of the Node's wiring, not a finding about the Revision.
	ErrNoCredentialSource = errors.New("auth.upstream-sigv4: the Node has no AWS credential source")
	errNoRequest          = errors.New("no outgoing request in onUpstreamRequest")
	errNotRecorded        = errors.New("the attempt reached the signing hook without its credentials")
	errNoCredentials      = errors.New("the credential source returned no access key ID or secret access key")
)

// Credentials are one set of AWS credentials. The secrets are
// secret.Value, so they never print.
type Credentials struct {
	// AccessKeyID is the access key ID, which the signature names.
	AccessKeyID string
	// SecretAccessKey is the secret access key the signing key derives
	// from.
	SecretAccessKey secret.Value
	// SessionToken is the session token of temporary credentials, sent
	// and signed as X-Amz-Security-Token; unset for long-term keys.
	SessionToken secret.Value
}

// String returns the access key ID and secret.Redacted, never a secret.
func (c Credentials) String() string { return "AccessKeyID=" + c.AccessKeyID + " " + secret.Redacted }

// GoString returns String.
func (c Credentials) GoString() string { return c.String() }

// LogValue returns String for log/slog.
func (c Credentials) LogValue() slog.Value { return slog.StringValue(c.String()) }

// CredentialSource yields the credentials one Policy signs with.
// Implementations are safe for concurrent use. A source that also
// implements io.Closer is closed by the Close of the Filter built on it.
type CredentialSource interface {
	// Credentials returns the credentials to sign with now, waiting
	// within ctx when the source must obtain them first.
	Credentials(ctx context.Context) (Credentials, error)
}

// Factory builds auth.upstream-sigv4 Filters (filter.Factory).
type Factory struct {
	// Credentials returns the credential source of the Policy env
	// compiles; a nil Credentials refuses every Build with
	// ErrNoCredentialSource. Its error carries its own RZ-CFG code.
	Credentials func(env filter.BuildEnv) (CredentialSource, error)
}

var _ filter.Factory = Factory{}

// Build returns the Filter of env's Policy: a config of the wrong type is
// RZ-CFG-005. The signing time is env.Clock's, else the real clock's. A
// ctx that has ended is its error, unchanged, as is a Factory without a
// credential source and the error of its Credentials.
func (fa Factory) Build(ctx context.Context, env filter.BuildEnv) (filter.Filter, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cfg, ok := env.Config.(*v1alpha1.AuthUpstreamSigV4Config)
	if !ok || cfg == nil {
		return nil, errcode.Errorf(CodeConfig, "auth.upstream-sigv4: config is %T, want *v1alpha1.AuthUpstreamSigV4Config", env.Config)
	}
	if fa.Credentials == nil {
		return nil, ErrNoCredentialSource
	}
	src, err := fa.Credentials(env)
	if err != nil {
		return nil, fmt.Errorf("auth.upstream-sigv4: %w", err)
	}
	if src == nil {
		return nil, ErrNoCredentialSource
	}
	clk := env.Clock
	if clk == nil {
		clk = clock.Real()
	}
	return &Filter{p: paramsOf(cfg), signer: v4.NewSigner(), clock: clk, src: src}, nil
}

// Filter is a compiled auth.upstream-sigv4 Policy: in onUpstreamRequest it
// records the credentials of the attempt, and at the transport-time
// signing hook it signs the attempt's request with them (filter.Signer).
// It is safe for concurrent use.
type Filter struct {
	p      params
	signer *v4.Signer
	clock  clock.Clock
	src    CredentialSource
	// records pools the per-leg records of the credentials.
	records sync.Pool
	closed  atomic.Bool
}

var (
	_ filter.Signer   = (*Filter)(nil)
	_ filter.Finisher = (*Filter)(nil)
	_ filter.Closer   = (*Filter)(nil)
)

// record is the Policy's per-leg state: the credentials of the leg's
// current attempt, which Handle records and Sign signs with.
type record struct {
	creds aws.Credentials
	set   bool
}

// Handle records the attempt's credentials in onUpstreamRequest and
// continues; every other Phase continues untouched. Credentials the
// source cannot give within ctx, and a leg view without an outgoing
// request, are CannotDecide with RZ-AUTH-020.
func (f *Filter) Handle(ctx context.Context, p phase.Phase, x filter.Exchange) filter.Result {
	if p != phase.OnUpstreamRequest {
		return filter.Next()
	}
	if l := x.Leg(); l == nil || l.Request == nil {
		return filter.Undecided(CodeUpstreamAuth, fmt.Errorf("auth.upstream-sigv4: %w", errNoRequest))
	}
	c, err := f.src.Credentials(ctx)
	if err != nil {
		return filter.Undecided(CodeUpstreamAuth, fmt.Errorf("auth.upstream-sigv4: credentials: %w", err))
	}
	if c.AccessKeyID == "" || c.SecretAccessKey.Len() == 0 {
		return filter.Undecided(CodeUpstreamAuth, fmt.Errorf("auth.upstream-sigv4: %w", errNoCredentials))
	}
	slot := x.PolicyState()
	rec, _ := (*slot).(*record)
	if rec == nil {
		if rec, _ = f.records.Get().(*record); rec == nil {
			rec = new(record)
		}
		*slot = rec
	}
	rec.creds = aws.Credentials{
		AccessKeyID:     c.AccessKeyID,
		SecretAccessKey: string(c.SecretAccessKey.Reveal()),
		SessionToken:    string(c.SessionToken.Reveal()),
	}
	rec.set = true
	return filter.Next()
}

// Sign signs the attempt's outgoing request with the credentials Handle
// recorded, at the clock's now (package documentation "Signing at the
// transport-time hook"). An attempt without them, a leg view without a
// request and a request that cannot be signed, such as a streamed body
// under a signed payload, are CannotDecide with RZ-AUTH-020.
func (f *Filter) Sign(ctx context.Context, x filter.Exchange) filter.Result {
	l := x.Leg()
	if l == nil || l.Request == nil {
		return filter.Undecided(CodeUpstreamAuth, fmt.Errorf("auth.upstream-sigv4: %w", errNoRequest))
	}
	rec, _ := (*x.PolicyState()).(*record)
	if rec == nil || !rec.set {
		return filter.Undecided(CodeUpstreamAuth, fmt.Errorf("auth.upstream-sigv4: %w", errNotRecorded))
	}
	if err := f.p.sign(ctx, f.signer, l.Request, rec.creds, f.clock.Now()); err != nil {
		return filter.Undecided(CodeUpstreamAuth, fmt.Errorf("auth.upstream-sigv4: %w", err))
	}
	return filter.Next()
}

// Finish drops the leg's credentials when the leg ends and returns its
// record to the pool.
func (f *Filter) Finish(_ context.Context, x filter.Exchange) {
	slot := x.PolicyState()
	if rec, ok := (*slot).(*record); ok && rec != nil {
		*rec = record{}
		f.records.Put(rec)
	}
	*slot = nil
}

// Close closes the credential source when it is an io.Closer. It is
// idempotent.
func (f *Filter) Close() error {
	if !f.closed.CompareAndSwap(false, true) {
		return nil
	}
	if c, ok := f.src.(io.Closer); ok {
		return c.Close()
	}
	return nil
}
