// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package adminauth authenticates the Ruralz Gateway admin server (port
// 9901; the Ruralz Control admin port 9902 reuses it in M2): the settings
// RURALZ_ADMIN_TOKEN_FILE, RURALZ_ADMIN_METRICS_TOKEN_FILE and
// RURALZ_ADMIN_TLS_DIR (OQ-security-and-identity-7 (a)), bearer tokens
// compared in constant time over SHA-256 digests, the loopback rule for
// cleartext, client certificates admitted by the TLS directory's ca.crt,
// and 401 problem documents with RZ-AUTH-001 or RZ-AUTH-002 (spec 06
// requirements 93 to 96, spec 04 requirement 70).
//
// Authorization matrix: /healthz and /readyz are open; /metrics needs the
// metrics token, the operator token or an admitted client certificate on
// every interface, loopback included; every other path (/debug/*,
// /config/dump, /tap, and unknown paths, which the admin mux answers with
// 404 once authorized) needs the operator token or an admitted client
// certificate and is off while no operator token is configured. A token is
// accepted only over TLS or from a loopback peer.
//
// Token files are read at start, and a changed file is re-read on the next
// request that presents a token (or on Reload), so a rotated token is
// picked up without a restart. A rotation to an empty, invalid,
// unreadable or non-regular file keeps the last valid token; removing the
// file revokes its token until a valid file appears again (see tokenFile).
// The TLS directory is read by internal/tlsconf.Admin at start; this
// package checks that tls.crt and tls.key exist and parses ca.crt, whose
// certificates are the only anchors a verified client chain may end at.
//
// Audit: the /tap and /config/dump access records of spec 06 requirement
// 96 go to Options.Logger, which ruralzd must set; AuditEnabled reports
// whether it is.
package adminauth

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"slices"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/problem"
)

// RZ codes of admin authentication failures (spec 06 requirement 95).
const (
	// CodeMissing is a request that presented no credential.
	CodeMissing = "RZ-AUTH-001"
	// CodeInvalid is a request whose credential is invalid, sent over
	// cleartext from a non-loopback peer, or not admitted for the path.
	CodeInvalid = "RZ-AUTH-002"
)

// Challenge is the WWW-Authenticate value of every admin 401.
const Challenge = `Bearer realm="ruralz-admin"`

// Authentication failures, the causes of the RZ errors Authenticate
// returns. Messages never contain a token.
var (
	// ErrNoCredential: no bearer token and no client certificate.
	ErrNoCredential = errors.New("adminauth: no admin credential presented")
	// ErrMultipleAuthorization: more than one Authorization field.
	ErrMultipleAuthorization = errors.New("adminauth: more than one Authorization field")
	// ErrMalformedToken: an empty, oversized or non-b64token bearer token.
	ErrMalformedToken = errors.New("adminauth: malformed bearer token")
	// ErrCleartext: a bearer token over cleartext from a non-loopback peer.
	ErrCleartext = errors.New("adminauth: bearer token over cleartext from a non-loopback peer")
	// ErrInvalidToken: a bearer token that matches no configured token.
	ErrInvalidToken = errors.New("adminauth: bearer token matches no admin token")
	// ErrCertificate: a client certificate not admitted by ca.crt.
	ErrCertificate = errors.New("adminauth: client certificate not admitted")
	// ErrNotPermitted: a valid credential that does not admit the path
	// (the metrics token outside /metrics).
	ErrNotPermitted = errors.New("adminauth: credential does not admit this path")
	// ErrOperatorOff: an operator path while no operator token is
	// configured.
	ErrOperatorOff = errors.New("adminauth: operator paths are off without " + EnvTokenFile)
)

// Class is the authorization class of an admin path.
type Class uint8

// Classes.
const (
	// ClassOpen paths answer without a credential.
	ClassOpen Class = iota
	// ClassMetrics is /metrics.
	ClassMetrics
	// ClassOperator is every other path.
	ClassOperator
)

// String returns open, metrics or operator.
func (c Class) String() string {
	switch c {
	case ClassOpen:
		return "open"
	case ClassMetrics:
		return "metrics"
	case ClassOperator:
		return "operator"
	}
	return ""
}

// ClassOf returns the class of a request path, compared exactly (the
// admin mux never rewrites paths): /healthz and /readyz are open, /metrics
// is metrics, anything else is operator.
func ClassOf(path string) Class {
	switch path {
	case "/healthz", "/readyz":
		return ClassOpen
	case "/metrics":
		return ClassMetrics
	}
	return ClassOperator
}

// Kind is the kind of an admitted credential.
type Kind uint8

// Credential kinds.
const (
	// KindNone is an open path's principal.
	KindNone Kind = iota
	// KindMetrics is the metrics token.
	KindMetrics
	// KindOperator is the operator token.
	KindOperator
	// KindCertificate is a client certificate admitted by ca.crt.
	KindCertificate
)

// String returns none, metrics, operator or certificate.
func (k Kind) String() string {
	switch k {
	case KindNone:
		return "none"
	case KindMetrics:
		return "metrics"
	case KindOperator:
		return "operator"
	case KindCertificate:
		return "certificate"
	}
	return ""
}

// Principal is who a request was admitted as.
type Principal struct {
	// Kind is the credential kind.
	Kind Kind
	// Subject is the client certificate subject (RFC 4514); empty for
	// tokens.
	Subject string
}

// Options are the dependencies of an Authenticator.
type Options struct {
	// Logger receives the access records of /tap and /config/dump (info),
	// token rotations (info), rotation failures and revocations (warn) and
	// denials (debug); a logger handed out by internal/telemetry. ruralzd
	// must set it: the access records are the audit trail spec 06
	// requirement 96 requires. A nil Logger logs nothing, the audit trail
	// included, and AuditEnabled then reports false.
	Logger *slog.Logger
	// RequestID returns the problem document's requestId; nil leaves it
	// empty.
	RequestID func(*http.Request) string
}

// Authenticator admits admin requests. It is safe for concurrent use.
type Authenticator struct {
	settings Settings
	operator *tokenFile // nil without RURALZ_ADMIN_TOKEN_FILE
	metrics  *tokenFile // nil without RURALZ_ADMIN_METRICS_TOKEN_FILE
	// anchors holds the DER of every ca.crt certificate; empty without
	// ca.crt in RURALZ_ADMIN_TLS_DIR.
	anchors   map[string]struct{}
	log       *slog.Logger
	requestID func(*http.Request) string
	deny001   problem.Problem
	deny002   problem.Problem
}

// New reads the token files and checks the TLS directory. Any error
// refuses start (exit 2); all are reported: a missing, unreadable,
// non-regular, empty, shorter than 22 bytes, longer than 4096 bytes or
// non-b64token token (after trimming trailing whitespace), a TLS directory
// without tls.crt or tls.key, a ca.crt that holds no certificate or one
// that does not parse.
func New(s Settings, o Options) (*Authenticator, error) {
	a := &Authenticator{
		settings:  s,
		log:       o.Logger,
		requestID: o.RequestID,
		deny001:   problem.New(CodeMissing, ""),
		deny002:   problem.New(CodeInvalid, ""),
	}
	var errs []error
	var err error
	if s.TokenFile != "" {
		if a.operator, err = loadTokenFile(EnvTokenFile, s.TokenFile); err != nil {
			errs = append(errs, err)
		}
	}
	if s.MetricsTokenFile != "" {
		if a.metrics, err = loadTokenFile(EnvMetricsTokenFile, s.MetricsTokenFile); err != nil {
			errs = append(errs, err)
		}
	}
	if s.TLSDir != "" {
		if a.anchors, err = checkTLSDir(s.TLSDir); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return a, nil
}

// Settings returns the settings a was built from.
func (a *Authenticator) Settings() Settings { return a.settings }

// ClientCertificates reports whether client certificates are admitted
// (ca.crt present).
func (a *Authenticator) ClientCertificates() bool { return len(a.anchors) > 0 }

// Configured reports whether any admin credential is configured; without
// one only /healthz and /readyz answer.
func (a *Authenticator) Configured() bool {
	return a.operator != nil || a.metrics != nil || a.ClientCertificates()
}

// AuditEnabled reports whether the /tap and /config/dump uses are logged
// (spec 06 requirement 96), that is whether Options.Logger was set. The
// admin server's owner asserts it.
func (a *Authenticator) AuditEnabled() bool { return a.log != nil }

// Reload re-reads every token file now (for example on SIGHUP). A file
// that fails keeps its last valid token, a removed file revokes its token
// (ErrTokenRevoked); the errors are returned joined.
func (a *Authenticator) Reload() error {
	var errs []error
	for _, tf := range a.tokenFiles() {
		if _, err := tf.reload(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// tokenFiles returns the configured token files.
func (a *Authenticator) tokenFiles() []*tokenFile {
	var out []*tokenFile
	for _, tf := range []*tokenFile{a.operator, a.metrics} {
		if tf != nil {
			out = append(out, tf)
		}
	}
	return out
}

// Authenticate decides whether r may reach its path. It returns the
// principal, or an error carrying CodeMissing or
// CodeInvalid (errcode.CodeOf) around one of the Err values.
func (a *Authenticator) Authenticate(r *http.Request) (Principal, error) {
	class := ClassOf(r.URL.Path)
	if class == ClassOpen {
		return Principal{}, nil
	}
	tok, hasToken, err := bearer(r.Header)
	if err != nil {
		return Principal{}, errcode.Wrap(CodeInvalid, err)
	}
	tokenKind := KindNone
	if hasToken {
		if r.TLS == nil && !Loopback(r.RemoteAddr) {
			return Principal{}, errcode.Wrap(CodeInvalid, ErrCleartext)
		}
		if tokenKind = a.matchToken(r.Context(), tok); tokenKind == KindNone {
			return Principal{}, errcode.Wrap(CodeInvalid, ErrInvalidToken)
		}
	}
	subject, certOK, hasCert := a.certificate(r)

	switch {
	case tokenKind != KindNone && a.admits(class, tokenKind):
		return Principal{Kind: tokenKind}, nil
	case certOK && a.admits(class, KindCertificate):
		return Principal{Kind: KindCertificate, Subject: subject}, nil
	case !hasToken && !hasCert:
		return Principal{}, errcode.Wrap(CodeMissing, ErrNoCredential)
	case class == ClassOperator && a.operator == nil:
		return Principal{}, errcode.Wrap(CodeInvalid, ErrOperatorOff)
	case hasCert && !certOK && tokenKind == KindNone:
		return Principal{}, errcode.Wrap(CodeInvalid, ErrCertificate)
	}
	return Principal{}, errcode.Wrap(CodeInvalid, ErrNotPermitted)
}

// admits applies the authorization matrix to an admitted credential.
func (a *Authenticator) admits(class Class, k Kind) bool {
	switch class {
	case ClassOpen:
		return true
	case ClassMetrics:
		return k == KindMetrics || k == KindOperator || k == KindCertificate
	case ClassOperator:
		return a.operator != nil && (k == KindOperator || k == KindCertificate)
	}
	return false
}

// matchToken refreshes the token files and compares the presented token's
// digest with both configured digests in constant time. When the token
// matches nothing while another request was refreshing a file, that
// refresh may be publishing a rotated token: matchToken waits for it and
// compares again, without starting a second refresh, so a request with
// the new token is not refused right after a rotation.
func (a *Authenticator) matchToken(ctx context.Context, tok string) Kind {
	d := sha256.Sum256([]byte(tok))
	opBusy := a.operator != nil && a.refresh(ctx, EnvTokenFile, a.operator)
	mBusy := a.metrics != nil && a.refresh(ctx, EnvMetricsTokenFile, a.metrics)
	k := a.compare(&d)
	if k == KindNone && (opBusy || mBusy) {
		if opBusy {
			a.operator.wait()
		}
		if mBusy {
			a.metrics.wait()
		}
		k = a.compare(&d)
	}
	return k
}

// compare compares d with both configured digests in constant time.
func (a *Authenticator) compare(d *digest) Kind {
	operator := a.operator != nil && a.operator.match(d)
	metrics := a.metrics != nil && a.metrics.match(d)
	switch {
	case operator:
		return KindOperator
	case metrics:
		return KindMetrics
	}
	return KindNone
}

// Log record keys (snake_case) and messages.
const (
	keyPath       = "path"
	keyPeer       = "peer"
	keyCredential = "credential"
	keySubject    = "subject"
	keySetting    = "setting"
	keyCode       = "code"
	keyError      = "error"
)

// refresh picks up a rotated token file, logging the outcome. It reports
// whether another refresh of tf was in progress.
func (a *Authenticator) refresh(ctx context.Context, env string, tf *tokenFile) (inProgress bool) {
	res, err := tf.tryRefresh()
	if res == busy || a.log == nil {
		return res == busy
	}
	switch res {
	case unchanged, busy:
	case rotated:
		a.log.LogAttrs(ctx, slog.LevelInfo, "admin token rotated", slog.String(keySetting, env))
	case failed:
		a.log.LogAttrs(ctx, slog.LevelWarn, "admin token rotation failed, keeping the last valid token",
			slog.String(keySetting, env), slog.String(keyError, err.Error()))
	case revoked:
		a.log.LogAttrs(ctx, slog.LevelWarn, "admin token file removed, token revoked",
			slog.String(keySetting, env), slog.String(keyError, err.Error()))
	}
	return false
}

// certificate reports the client certificate of r: its subject, whether
// ca.crt admits it (the leaf lists clientAuth and a verified chain from it
// ends at a certificate of ca.crt), and whether one was presented at all.
// Checking the anchor here keeps a TLS configuration that verified the
// chain against other roots from admitting it.
func (a *Authenticator) certificate(r *http.Request) (subject string, ok, presented bool) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return "", false, false
	}
	leaf := r.TLS.PeerCertificates[0]
	if len(a.anchors) == 0 || !slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageClientAuth) {
		return "", false, true
	}
	for _, chain := range r.TLS.VerifiedChains {
		if len(chain) == 0 || !chain[0].Equal(leaf) {
			continue
		}
		if _, ok := a.anchors[string(chain[len(chain)-1].Raw)]; ok {
			return leaf.Subject.String(), true, true
		}
	}
	return "", false, true
}

// bearer extracts the bearer token of h. A missing Authorization field or
// another scheme is no token; more than one field, or an empty, oversized
// or non-b64token token, is an error.
func bearer(h http.Header) (tok string, present bool, err error) {
	vs := h.Values("Authorization")
	switch len(vs) {
	case 0:
		return "", false, nil
	case 1:
	default:
		return "", true, ErrMultipleAuthorization
	}
	scheme, rest, _ := strings.Cut(vs[0], " ")
	if !strings.EqualFold(scheme, "Bearer") {
		return "", false, nil
	}
	tok = strings.Trim(rest, " \t")
	if tok == "" || len(tok) > MaxTokenBytes || !b64token(tok) {
		return "", true, ErrMalformedToken
	}
	return tok, true, nil
}

// Loopback reports whether remoteAddr ("ip:port", or a bare address) is a
// loopback peer: 127.0.0.0/8, ::1 or an IPv4-mapped loopback address.
func Loopback(remoteAddr string) bool {
	var addr netip.Addr
	if ap, err := netip.ParseAddrPort(remoteAddr); err == nil {
		addr = ap.Addr()
	} else if addr, err = netip.ParseAddr(remoteAddr); err != nil {
		return false
	}
	return addr.Unmap().IsLoopback()
}

// principalKey keys the Principal in a request context.
type principalKey struct{}

// PrincipalFrom returns the principal the Middleware admitted the request
// as; false on open paths and outside the Middleware.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// audited reports the paths whose every use is logged at info (spec 06
// requirement 96).
func audited(path string) bool { return path == "/tap" || path == "/config/dump" }

// Middleware authenticates every request before next. A refused request
// gets a 401 problem document (RZ-AUTH-001 or RZ-AUTH-002) with
// WWW-Authenticate: Bearer realm="ruralz-admin"; an admitted one reaches
// next with its Principal in the context (PrincipalFrom) and without its
// Authorization field. /tap and /config/dump uses are logged at info with
// path, peer address and credential kind (and certificate subject), never
// the token.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := a.Authenticate(r)
		if err != nil {
			a.deny(w, r, err)
			return
		}
		if ClassOf(r.URL.Path) == ClassOpen {
			next.ServeHTTP(w, r)
			return
		}
		ctx := context.WithValue(r.Context(), principalKey{}, p)
		if a.log != nil && audited(r.URL.Path) {
			attrs := []slog.Attr{
				slog.String(keyPath, r.URL.Path),
				slog.String(keyPeer, r.RemoteAddr),
				slog.String(keyCredential, p.Kind.String()),
			}
			if p.Subject != "" {
				attrs = append(attrs, slog.String(keySubject, p.Subject))
			}
			a.log.LogAttrs(ctx, slog.LevelInfo, "admin access", attrs...)
		}
		r2 := r.WithContext(ctx)
		r2.Header = r.Header.Clone()
		r2.Header.Del("Authorization")
		next.ServeHTTP(w, r2)
	})
}

// deny writes the 401 problem document for err.
func (a *Authenticator) deny(w http.ResponseWriter, r *http.Request, err error) {
	p := a.deny002
	if code, _ := errcode.CodeOf(err); code == CodeMissing {
		p = a.deny001
	}
	if a.requestID != nil {
		p.RequestID = a.requestID(r)
	}
	if a.log != nil {
		a.log.LogAttrs(r.Context(), slog.LevelDebug, "admin request refused",
			slog.String(keyPath, r.URL.Path), slog.String(keyPeer, r.RemoteAddr),
			slog.String(keyCode, p.Code), slog.String(keyError, err.Error()))
	}
	problem.Write(w, p, http.Header{"Www-Authenticate": {Challenge}})
}
