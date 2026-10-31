// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package mtls

import (
	"context"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/auth"
	"github.com/ravindu-rev/ruralz/internal/identity"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Type is the Policy type this package implements.
const Type = v1alpha1.PolicyTypeAuthMTLS

// Codes Build returns, each registered in internal/errcode.
const (
	// CodeSchema is a config that is not a valid AuthMTLSConfig, such as a
	// config.subjects rule with both or neither of subject and uriSan.
	CodeSchema = "RZ-CFG-005"
	// CodeResolution is a caCertificate or crl reference that is unresolved,
	// or whose value is refused (newTrustState).
	CodeResolution = "RZ-CFG-026"
)

// Factory builds auth.mtls Filters. The zero value is usable and consults
// identity.NopRevoker.
type Factory struct {
	revoker identity.Revoker
}

var _ filter.Factory = (*Factory)(nil)

// NewFactory returns the auth.mtls Factory; revoker is consulted after
// every verification (nil means identity.NopRevoker, the M1 wiring).
func NewFactory(revoker identity.Revoker) *Factory {
	return &Factory{revoker: revoker}
}

// Build compiles one auth.mtls Policy: config.subjects (a rule with both
// or neither of subject and uriSan is RZ-CFG-005), the trust state of
// config.caCertificate and config.crl (RZ-CFG-026 when a reference is
// unresolved or its value refused), and the snapshot's credential index.
// The trust holder of env.Previous is carried over when it follows the
// same references, so its watches and parsed CRLs survive the swap; the
// verification cache of each connection is still per snapshot, keyed by
// the new Filter. A ctx that has ended is its error, without a code.
func (f *Factory) Build(ctx context.Context, env filter.BuildEnv) (filter.Filter, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cfg, ok := env.Config.(*v1alpha1.AuthMTLSConfig)
	if !ok || cfg == nil {
		return nil, errcode.Errorf(CodeSchema, "auth.mtls: config is %T, want *v1alpha1.AuthMTLSConfig", env.Config)
	}
	rs, err := compileRules(cfg.Subjects)
	if err != nil {
		return nil, err
	}
	if env.Secrets == nil {
		return nil, errcode.Errorf(CodeResolution, "auth.mtls: no resolved secrets")
	}
	ca := secret.RefOf(cfg.CACertificate.SecretRef)
	var crl *secret.Ref
	if cfg.CRL != nil {
		r := secret.RefOf(cfg.CRL.SecretRef)
		crl = &r
	}
	var policy string
	if env.Policy != nil {
		policy = env.Policy.Metadata.Name
	}
	tr := carried(env.Previous, ca, crl)
	if tr == nil {
		clk := env.Clock
		if clk == nil {
			clk = clock.Real()
		}
		if tr, err = newHolder(env.Secrets, ca, crl, clk, env.Status, policy); err != nil {
			return nil, errcode.Wrap(CodeResolution, err)
		}
	}
	ix, err := auth.AcquireIndex(env)
	if err != nil {
		tr.release()
		return nil, err
	}
	revoker := f.revoker
	if revoker == nil {
		revoker = identity.NopRevoker{}
	}
	return &Filter{
		Decider: auth.NewAuth(env.Metrics, ""),
		policy:  policy,
		rules:   rs,
		index:   ix,
		revoker: revoker,
		trust:   tr,
	}, nil
}

// carried returns the trust holder of prev, with a reference taken, when
// prev is an auth.mtls Filter whose holder follows ca and crl and is still
// open; nil otherwise.
func carried(prev filter.Filter, ca secret.Ref, crl *secret.Ref) *holder {
	p, ok := prev.(*Filter)
	if !ok || p == nil || p.trust == nil || !p.trust.follows(ca, crl) || !p.trust.retain() {
		return nil
	}
	return p.trust
}

// compileRules compiles config.subjects; a rule with both or neither of
// subject and uriSan is RZ-CFG-005.
func compileRules(subjects []v1alpha1.CertificateSubject) (rules, error) {
	var r rules
	for i, s := range subjects {
		switch {
		case (s.Subject == "") == (s.URISAN == ""):
			return rules{}, errcode.Errorf(CodeSchema, "auth.mtls: config.subjects[%d] must set exactly one of subject and uriSan", i)
		case s.Subject != "":
			if r.subjects == nil {
				r.subjects = map[string]struct{}{}
			}
			r.subjects[s.Subject] = struct{}{}
		default:
			if r.uris == nil {
				r.uris = map[string]struct{}{}
			}
			r.uris[s.URISAN] = struct{}{}
		}
	}
	return r, nil
}
