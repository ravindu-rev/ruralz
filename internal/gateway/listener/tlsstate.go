// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package listener

import (
	"crypto/tls"
	"slices"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/internal/tlsconf"
)

// codeSchema is the RZ code of listener settings the configuration schema
// refuses; validation catches them first.
const codeSchema = "RZ-CFG-005"

// tlsState is one https listener's TLS state for one snapshot: the state
// every handshake reads (tlsconf.ListenerState) and the settings it was
// built from, which decide whether the next snapshot carries it over.
type tlsState struct {
	st      *tlsconf.ListenerState
	minimum uint16
	certs   []snapshot.Certificate
	mtls    bool
}

// newTLSState resolves the listener's certificates from secrets and builds
// its state: an unresolved or unparsable certificate or key is RZ-CFG-026,
// a missing tls block or an unknown minimum version RZ-CFG-005.
func newTLSState(t *snapshot.ListenerTLS, secrets secret.Store) (*tlsState, error) {
	if t == nil {
		return nil, errcode.Errorf(codeSchema, "an https listener needs tls")
	}
	minimum, err := minVersion(t.MinVersion)
	if err != nil {
		return nil, err
	}
	specs := make([]tlsconf.CertSpec, len(t.Certificates))
	for i, c := range t.Certificates {
		specs[i] = tlsconf.CertSpec{Name: c.Name, Certificate: c.Certificate, PrivateKey: c.PrivateKey}
	}
	ix, err := tlsconf.BuildCertIndex(specs, secrets)
	if err != nil {
		return nil, err
	}
	st, err := tlsconf.NewListenerState(tlsconf.ListenerOptions{MinVersion: minimum, Certs: ix, RequestClientCert: t.RequestClientCert})
	if err != nil {
		ix.Close()
		return nil, err
	}
	return &tlsState{st: st, minimum: t.MinVersion, certs: slices.Clone(t.Certificates), mtls: t.RequestClientCert}, nil
}

// same reports whether t has the settings s was built from.
func (s *tlsState) same(t *snapshot.ListenerTLS) bool {
	return t != nil && s.minimum == t.MinVersion && s.mtls == t.RequestClientCert && slices.Equal(s.certs, t.Certificates)
}

// close stops following secret rotations; handshakes still reading the
// state complete with the last certificates.
func (s *tlsState) close() { s.st.Certs().Close() }

// minVersion maps the compiled tls.minVersion to its configuration value:
// 0 and TLS 1.3 give "1.3" (the default), TLS 1.2 gives "1.2".
func minVersion(v uint16) (string, error) {
	switch v {
	case 0, tls.VersionTLS13:
		return tlsconf.Version13, nil
	case tls.VersionTLS12:
		return tlsconf.Version12, nil
	default:
		return "", errcode.Errorf(codeSchema, "tls.minVersion %#04x is not TLS 1.2 or 1.3", v)
	}
}
