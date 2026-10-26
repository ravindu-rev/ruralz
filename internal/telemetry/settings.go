// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"net"
	"net/url"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/internal/telemetry/tracing"
	"github.com/ravindu-rev/ruralz/internal/tlsconf"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// defaultOTLPPort is the port of an endpoint that names none.
const defaultOTLPPort = "4317"

// codeEndpoint is the schema error of a malformed otlp.endpoint; validation
// reports it first through the field's pattern.
const codeEndpoint = "RZ-CFG-005"

// Settings are the telemetry settings of one Revision, compiled off the
// request path by CompileSettings and installed by Runtime.Apply. A
// Settings holding TLS material follows its secrets' rotations until Apply
// adopts it or Close drops it.
type Settings struct {
	// target is the gRPC target host:port; empty means no OTLP export.
	target string
	// cleartext reports an http endpoint: a reported cleartext hop.
	cleartext bool
	// tls is the client configuration of an https endpoint.
	tls *tlsconf.ClientConfig
	// key identifies the connection settings, so an unchanged Revision
	// keeps its pipeline.
	key string
	// sampling is the root sampling ratio.
	sampling float64
}

// CompileSettings compiles Gateway.spec.telemetry (nil: no telemetry
// block). A malformed endpoint is RZ-CFG-005; TLS errors carry the codes
// of tlsconf (RZ-CFG-026 for an unresolved reference).
func CompileSettings(t *v1alpha1.Telemetry, secrets secret.Store) (Settings, error) {
	s := Settings{sampling: tracing.DefaultTraceSampling}
	if t == nil {
		return s, nil
	}
	if t.TraceSampling != nil {
		s.sampling = min(max(*t.TraceSampling, 0), 1)
	}
	if t.OTLP == nil || t.OTLP.Endpoint == "" {
		return s, nil
	}
	target, scheme, err := parseEndpoint(t.OTLP.Endpoint)
	if err != nil {
		return Settings{}, err
	}
	s.target = target
	s.key = scheme + "://" + target
	if scheme == "http" {
		s.cleartext = true
		return s, nil
	}
	spec := clientSpec(t.OTLP.TLS)
	cfg, err := tlsconf.OTLP(spec, secrets)
	if err != nil {
		return Settings{}, err
	}
	s.tls = cfg
	s.key += "\x00" + specKey(spec)
	return s, nil
}

// Sampling returns the root sampling ratio.
func (s Settings) Sampling() float64 { return s.sampling }

// Close stops following the TLS secrets of settings that Apply did not
// adopt; it is safe to call more than once.
func (s Settings) Close() {
	if s.tls != nil {
		s.tls.Close()
	}
}

// parseEndpoint checks an otlp.endpoint: http or https, a host and an
// optional port, an empty path or "/", no user information, query or
// fragment. It returns the gRPC target host:port and the scheme.
func parseEndpoint(raw string) (target, scheme string, err error) {
	u, perr := url.Parse(raw)
	switch {
	case perr != nil:
		return "", "", errcode.Errorf(codeEndpoint, "telemetry.otlp.endpoint is not a URL")
	case u.Scheme != "http" && u.Scheme != "https":
		return "", "", errcode.Errorf(codeEndpoint, "telemetry.otlp.endpoint must use http or https")
	case u.User != nil, u.RawQuery != "", u.ForceQuery, u.Fragment != "", u.Opaque != "":
		return "", "", errcode.Errorf(codeEndpoint, "telemetry.otlp.endpoint has no user information, query or fragment")
	case u.Path != "" && u.Path != "/":
		return "", "", errcode.Errorf(codeEndpoint, "telemetry.otlp.endpoint has no path")
	case u.Hostname() == "":
		return "", "", errcode.Errorf(codeEndpoint, "telemetry.otlp.endpoint has no host")
	}
	port := u.Port()
	if port == "" {
		if strings.HasSuffix(u.Host, ":") {
			return "", "", errcode.Errorf(codeEndpoint, "telemetry.otlp.endpoint has an empty port")
		}
		port = defaultOTLPPort
	}
	return net.JoinHostPort(u.Hostname(), port), u.Scheme, nil
}

// clientSpec converts telemetry.otlp.tls; nil stays nil (system roots).
func clientSpec(t *v1alpha1.UpstreamTLS) *tlsconf.ClientSpec {
	if t == nil {
		return nil
	}
	ref := func(v *v1alpha1.SecretValue) *secret.Ref {
		if v == nil {
			return nil
		}
		r := secret.RefOf(v.SecretRef)
		return &r
	}
	return &tlsconf.ClientSpec{
		SNI:               t.SNI,
		CACertificate:     ref(t.CACertificate),
		ClientCertificate: ref(t.ClientCertificate),
		ClientKey:         ref(t.ClientKey),
	}
}

// specKey identifies a TLS spec by its server name and references; the
// values behind the references rotate in place.
func specKey(spec *tlsconf.ClientSpec) string {
	if spec == nil {
		return "system-roots"
	}
	ref := func(r *secret.Ref) string {
		if r == nil {
			return "-"
		}
		return r.String()
	}
	return strings.Join([]string{spec.SNI, ref(spec.CACertificate), ref(spec.ClientCertificate), ref(spec.ClientKey)}, "\x00")
}
