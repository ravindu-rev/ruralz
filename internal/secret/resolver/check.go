// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package resolver

import (
	"bytes"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/secret"
)

// Fixed reasons of the kind checks (spec 01 requirement 44, spec 06
// requirements 13, 51, 76 and 80). None repeats any part of the value.
var (
	errNoCertificate    = errors.New("value holds no PEM CERTIFICATE block")
	errBadCertificate   = errors.New("a PEM CERTIFICATE block does not parse")
	errNoPoolCert       = errors.New("value holds no PEM CERTIFICATE block that parses")
	errNoPrivateKey     = errors.New("value holds no PEM private key block")
	errBadPrivateKey    = errors.New("PEM private key does not parse (unencrypted PKCS #8, PKCS #1 or SEC 1 expected)")
	errNoCRL            = errors.New("value holds no PEM X509 CRL block")
	errBadCRL           = errors.New("a PEM X509 CRL block does not parse")
	errShortAPIKey      = fmt.Errorf("API key is shorter than %d bytes after trimming", MinAPIKeyBytes)
	errBadURL           = errors.New("the State Store URL does not parse")
	errURLNoCredentials = errors.New("the State Store URL names a non-loopback host and carries no credentials")
	errUnknownKind      = errors.New("unsupported secret kind")
	errWithheld         = errors.New("check failed (reason withheld: it repeats part of the value)")
	errCheckPanicked    = errors.New("consumer check panicked (panic value withheld)")
)

// PEM block types.
const (
	pemCertificate = "CERTIFICATE"
	pemCRL         = "X509 CRL"
	pemPrivateKey  = "PRIVATE KEY"
)

// sizeCap returns the size cap of a value of kind k (architecture R-9).
func sizeCap(k secret.Kind) int64 {
	if k == secret.KindPEMCRL {
		return MaxCRLBytes
	}
	return MaxValueBytes
}

// runCheck runs the size cap, the default check of u's kind and u's
// consumer check on b. A consumer check error is repeated only when its
// text does not contain part of b; a consumer check panic is
// errCheckPanicked.
func runCheck(u useCheck, b []byte) error {
	if limit := sizeCap(u.kind); int64(len(b)) > limit {
		return tooLarge(limit)
	}
	if err := checkKind(u.kind, b); err != nil {
		return err
	}
	if u.check == nil {
		return nil
	}
	err := callCheck(u.check, b)
	if err == nil || errors.Is(err, errCheckPanicked) {
		return err
	}
	return sanitize(err, b)
}

// callCheck runs check on a copy of b, cleared afterwards. A panic becomes
// errCheckPanicked and its value is dropped, so a faulty consumer check
// neither stops the resolver goroutine nor prints the value it was given
// (spec 01 section 6, secret leak tests).
func callCheck(check func([]byte) error, b []byte) (err error) {
	c := bytes.Clone(b)
	defer func() {
		clear(c)
		if recover() != nil {
			err = errCheckPanicked
		}
	}()
	return check(c)
}

// checkKind runs the default check of kind k on b; its errors are fixed
// reasons.
func checkKind(k secret.Kind, b []byte) error {
	switch k {
	case secret.KindOpaque:
		return nil
	case secret.KindPEMCertificate:
		return checkCertificates(b)
	case secret.KindPEMPrivateKey:
		return checkPrivateKey(b)
	case secret.KindPEMCertPool:
		return checkCertPool(b)
	case secret.KindPEMCRL:
		return checkCRLs(b)
	case secret.KindAPIKey:
		if len(trimSpace(b)) < MinAPIKeyBytes {
			return errShortAPIKey
		}
		return nil
	case secret.KindStateStoreURL:
		return checkStateStoreURL(b)
	default:
		return errUnknownKind
	}
}

// pemBlocks calls fn with every PEM block of b, in order, until fn returns
// false. Text outside blocks is ignored, as crypto/tls does. The decoded
// bytes of each block are cleared once fn returns, so fn must not keep
// them.
func pemBlocks(b []byte, fn func(*pem.Block) bool) {
	for {
		var blk *pem.Block
		blk, b = pem.Decode(b)
		if blk == nil {
			return
		}
		more := fn(blk)
		clear(blk.Bytes)
		if !more {
			return
		}
	}
}

// checkCertificates requires at least one CERTIFICATE block and every one
// to parse (a certificate chain).
func checkCertificates(b []byte) error {
	var n int
	var err error
	pemBlocks(b, func(blk *pem.Block) bool {
		if blk.Type != pemCertificate {
			return true
		}
		n++
		if _, perr := x509.ParseCertificate(blk.Bytes); perr != nil {
			err = errBadCertificate
			return false
		}
		return true
	})
	switch {
	case err != nil:
		return err
	case n == 0:
		return errNoCertificate
	}
	return nil
}

// checkCertPool requires at least one CERTIFICATE block that parses (a CA
// bundle; spec 06 requirement 51: invalid when no certificate parses).
func checkCertPool(b []byte) error {
	var ok bool
	pemBlocks(b, func(blk *pem.Block) bool {
		if blk.Type != pemCertificate {
			return true
		}
		_, err := x509.ParseCertificate(blk.Bytes)
		ok = err == nil
		return !ok
	})
	if !ok {
		return errNoPoolCert
	}
	return nil
}

// checkPrivateKey parses the first private key block, as
// tls.X509KeyPair picks it: PKCS #8, PKCS #1 or SEC 1, unencrypted.
func checkPrivateKey(b []byte) error {
	err := errNoPrivateKey
	pemBlocks(b, func(blk *pem.Block) bool {
		if blk.Type != pemPrivateKey && !strings.HasSuffix(blk.Type, " "+pemPrivateKey) {
			return true
		}
		err = parsePrivateKey(blk.Bytes)
		return false
	})
	return err
}

// parsePrivateKey reports whether der is a private key in one of the
// forms crypto/tls accepts.
func parsePrivateKey(der []byte) error {
	if _, err := x509.ParsePKCS8PrivateKey(der); err == nil {
		return nil
	}
	if _, err := x509.ParsePKCS1PrivateKey(der); err == nil {
		return nil
	}
	if _, err := x509.ParseECPrivateKey(der); err == nil {
		return nil
	}
	return errBadPrivateKey
}

// checkCRLs requires at least one X509 CRL block and every one to parse;
// the signature is verified by auth.mtls against its CA pool.
func checkCRLs(b []byte) error {
	var n int
	var err error
	pemBlocks(b, func(blk *pem.Block) bool {
		if blk.Type != pemCRL {
			return true
		}
		n++
		if _, perr := x509.ParseRevocationList(blk.Bytes); perr != nil {
			err = errBadCRL
			return false
		}
		return true
	})
	switch {
	case err != nil:
		return err
	case n == 0:
		return errNoCRL
	}
	return nil
}

// checkStateStoreURL applies the credentials rule of spec 06 requirement
// 80 and spec 08 requirement 8 (RZ-CFG-026 interim): a URL whose hosts are
// not all loopback literals (127.0.0.0/8 in IPv4 form, ::1, localhost;
// never decided by DNS) must carry a user name or a password. Surrounding
// SP, HTAB, CR and LF are ignored; the topology fit is the consumer's
// check.
func checkStateStoreURL(b []byte) error {
	u, err := url.Parse(string(trimSpace(b)))
	if err != nil || u.Scheme == "" || u.Opaque != "" {
		return errBadURL
	}
	if hasCredentials(u) {
		return nil
	}
	hosts := []string{u.Hostname()}
	for _, a := range u.Query()["addr"] {
		h, _, err := net.SplitHostPort(a)
		if err != nil {
			h = strings.TrimSuffix(strings.TrimPrefix(a, "["), "]")
		}
		hosts = append(hosts, h)
	}
	for _, h := range hosts {
		if !isLoopbackLiteral(h) {
			return errURLNoCredentials
		}
	}
	return nil
}

// hasCredentials reports a user name or a password in u.
func hasCredentials(u *url.URL) bool {
	if u.User == nil {
		return false
	}
	if u.User.Username() != "" {
		return true
	}
	p, set := u.User.Password()
	return set && p != ""
}

// isLoopbackLiteral reports one of the loopback literals spec 08
// requirement 8 lists: localhost (any case), an IPv4 address in
// 127.0.0.0/8 or ::1. An IPv4-mapped IPv6 address (::ffff:127.0.0.1) is
// not one of them, although netip counts it as loopback.
func isLoopbackLiteral(h string) bool {
	if strings.EqualFold(h, "localhost") {
		return true
	}
	a, err := netip.ParseAddr(h)
	return err == nil && !a.Is4In6() && a.IsLoopback()
}

// trimSpace trims leading and trailing SP, HTAB, CR and LF (spec 06
// requirement 13).
func trimSpace(b []byte) []byte { return bytes.Trim(b, " \t\r\n") }

// windowBytes is the length of the value windows sanitize looks for.
const windowBytes = 8

// maxEncodedCheck bounds the values whose encoded forms sanitize also
// looks for; larger values (certificate bundles, CRLs) are PEM text whose
// raw windows already cover their base64.
const maxEncodedCheck = 64 << 10

// sanitize returns err when its text repeats no part of value, else a
// fixed reason. "Part" is the whole trimmed value when it is shorter than
// windowBytes, else any windowBytes-long window of it, looked for in the
// raw value and, for values up to maxEncodedCheck bytes, in its hex,
// base64 and URL-escaped forms (the forms of the spec 06 section 6.6
// canary test).
func sanitize(err error, value []byte) error {
	if leaks(err.Error(), value) {
		return errWithheld
	}
	return err
}

// leaks reports whether msg contains part of value (see sanitize).
func leaks(msg string, value []byte) bool {
	v := trimSpace(value)
	if len(v) == 0 {
		return false
	}
	forms := [][]byte{v}
	if len(v) <= maxEncodedCheck {
		s := string(v)
		forms = append(forms,
			[]byte(hex.EncodeToString(v)),
			[]byte(strings.ToUpper(hex.EncodeToString(v))),
			[]byte(base64.StdEncoding.EncodeToString(v)),
			[]byte(base64.URLEncoding.EncodeToString(v)),
			[]byte(url.QueryEscape(s)),
			[]byte(url.PathEscape(s)),
		)
	}
	var windows map[string]struct{}
	for _, f := range forms {
		if len(f) <= windowBytes {
			if strings.Contains(msg, string(f)) {
				return true
			}
			continue
		}
		if len(msg) < windowBytes {
			continue
		}
		if windows == nil {
			windows = make(map[string]struct{}, len(msg)-windowBytes+1)
			for i := 0; i+windowBytes <= len(msg); i++ {
				windows[msg[i:i+windowBytes]] = struct{}{}
			}
		}
		for i := 0; i+windowBytes <= len(f); i++ {
			if _, ok := windows[string(f[i:i+windowBytes])]; ok {
				return true
			}
		}
	}
	return false
}
