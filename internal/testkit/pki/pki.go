// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package pki issues the test certificates the harness needs (11 section
// 3): a test CA (and intermediates), server certificates for Node
// listeners, the admin listener, mock Upstreams, the mock IdP, the OTLP
// sink and TLS State Stores, client certificates for auth.mtls, CRLs, PEM
// files, and an SSL_CERT_FILE bundle so child processes trust the test CA.
//
// Keys are ECDSA P-256 by default, or RSA 2,048-bit. Everything is
// standard library crypto; nothing here is fit for production use.
package pki

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// KeyType selects the key algorithm.
type KeyType int

// Key types.
const (
	// ECDSAP256 is an ECDSA key on P-256 (the default).
	ECDSAP256 KeyType = iota
	// RSA2048 is a 2,048-bit RSA key.
	RSA2048
)

// String returns the key type name.
func (k KeyType) String() string {
	switch k {
	case ECDSAP256:
		return "ECDSA-P256"
	case RSA2048:
		return "RSA-2048"
	}
	return fmt.Sprintf("KeyType(%d)", int(k))
}

// Default validity: from one hour ago (tolerating clock skew between
// processes) for 30 days.
const (
	DefaultBackdate = time.Hour
	DefaultValidity = 30 * 24 * time.Hour
)

// CAOptions configures NewCA and CA.Intermediate.
type CAOptions struct {
	// CommonName defaults to "Ruralz Test CA".
	CommonName string
	KeyType    KeyType
	// NotBefore and NotAfter default to now - DefaultBackdate and
	// NotBefore + DefaultValidity.
	NotBefore, NotAfter time.Time
}

// CA is a certificate authority.
type CA struct {
	Cert *x509.Certificate
	Key  crypto.Signer
	// CertPEM is the CA certificate in PEM.
	CertPEM []byte
	// KeyPEM is the CA key in PKCS #8 PEM.
	KeyPEM []byte
	// Parent is the issuing CA of an intermediate; nil for a root.
	Parent *CA
}

// LeafOptions configures CA.Issue.
type LeafOptions struct {
	CommonName  string
	DNSNames    []string
	IPAddresses []net.IP
	// URIs are URI SANs, for example "spiffe://example.org/ns/a".
	URIs []string
	// Server and Client select the extended key usages; with neither set
	// the certificate is for both.
	Server, Client bool
	KeyType        KeyType
	// NotBefore and NotAfter default like CAOptions'.
	NotBefore, NotAfter time.Time
}

// Leaf is an issued end-entity certificate.
type Leaf struct {
	Cert *x509.Certificate
	Key  crypto.Signer
	// CertPEM is the leaf certificate alone.
	CertPEM []byte
	// ChainPEM is the leaf followed by every intermediate up to (not
	// including) the root, the form TLS servers present.
	ChainPEM []byte
	// KeyPEM is the key in PKCS #8 PEM.
	KeyPEM []byte
	// Issuer is the CA that signed the leaf.
	Issuer *CA
}

// NewCA creates a self-signed root CA.
func NewCA(o CAOptions) (*CA, error) { return newCA(o, nil) }

// Intermediate creates a CA signed by ca.
func (ca *CA) Intermediate(o CAOptions) (*CA, error) { return newCA(o, ca) }

func newCA(o CAOptions, parent *CA) (*CA, error) {
	if o.CommonName == "" {
		o.CommonName = "Ruralz Test CA"
		if parent != nil {
			o.CommonName = "Ruralz Test Intermediate CA"
		}
	}
	key, err := newKey(o.KeyType)
	if err != nil {
		return nil, err
	}
	nb, na := validity(o.NotBefore, o.NotAfter)
	serial, err := newSerial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: o.CommonName, Organization: []string{"Ruralz tests"}},
		NotBefore:             nb,
		NotAfter:              na,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	issuerCert, issuerKey := tmpl, key
	if parent != nil {
		issuerCert, issuerKey = parent.Cert, parent.Key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, issuerCert, key.Public(), issuerKey)
	if err != nil {
		return nil, fmt.Errorf("pki: create CA %q: %w", o.CommonName, err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("pki: parse CA %q: %w", o.CommonName, err)
	}
	keyPEM, err := encodeKey(key)
	if err != nil {
		return nil, err
	}
	return &CA{Cert: cert, Key: key, CertPEM: encodeCert(der), KeyPEM: keyPEM, Parent: parent}, nil
}

// Root returns the root of ca's chain.
func (ca *CA) Root() *CA {
	for ca.Parent != nil {
		ca = ca.Parent
	}
	return ca
}

// Issue signs a leaf certificate.
func (ca *CA) Issue(o LeafOptions) (*Leaf, error) {
	key, err := newKey(o.KeyType)
	if err != nil {
		return nil, err
	}
	nb, na := validity(o.NotBefore, o.NotAfter)
	serial, err := newSerial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: o.CommonName, Organization: []string{"Ruralz tests"}},
		NotBefore:    nb,
		NotAfter:     na,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		DNSNames:     o.DNSNames,
		IPAddresses:  o.IPAddresses,
	}
	if o.KeyType == RSA2048 {
		tmpl.KeyUsage |= x509.KeyUsageKeyEncipherment
	}
	switch {
	case o.Server && !o.Client:
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	case o.Client && !o.Server:
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	default:
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}
	}
	for _, s := range o.URIs {
		u, err := url.Parse(s)
		if err != nil {
			return nil, fmt.Errorf("pki: URI SAN %q: %w", s, err)
		}
		tmpl.URIs = append(tmpl.URIs, u)
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, key.Public(), ca.Key)
	if err != nil {
		return nil, fmt.Errorf("pki: issue %q: %w", o.CommonName, err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("pki: parse %q: %w", o.CommonName, err)
	}
	keyPEM, err := encodeKey(key)
	if err != nil {
		return nil, err
	}
	certPEM := encodeCert(der)
	chain := bytes.Clone(certPEM)
	for c := ca; c.Parent != nil; c = c.Parent {
		chain = append(chain, c.CertPEM...)
	}
	return &Leaf{Cert: cert, Key: key, CertPEM: certPEM, ChainPEM: chain, KeyPEM: keyPEM, Issuer: ca}, nil
}

// Server issues a server certificate for hosts, each a DNS name or an IP
// address; no hosts means "localhost", 127.0.0.1 and ::1.
func (ca *CA) Server(hosts ...string) (*Leaf, error) {
	if len(hosts) == 0 {
		hosts = []string{"localhost", "127.0.0.1", "::1"}
	}
	o := LeafOptions{CommonName: hosts[0], Server: true}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			o.IPAddresses = append(o.IPAddresses, ip)
		} else {
			o.DNSNames = append(o.DNSNames, h)
		}
	}
	return ca.Issue(o)
}

// Client issues a client certificate with common name cn and optional URI
// SANs.
func (ca *CA) Client(cn string, uris ...string) (*Leaf, error) {
	return ca.Issue(LeafOptions{CommonName: cn, URIs: uris, Client: true})
}

// Pool returns a pool holding the root of ca's chain.
func (ca *CA) Pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(ca.Root().Cert)
	return p
}

// CRL returns a PEM certificate revocation list signed by ca revoking
// certs, valid from thisUpdate to nextUpdate (zero values mean now minus
// DefaultBackdate and one day later).
func (ca *CA) CRL(revoked []*x509.Certificate, thisUpdate, nextUpdate time.Time) ([]byte, error) {
	if thisUpdate.IsZero() {
		thisUpdate = time.Now().Add(-DefaultBackdate)
	}
	if nextUpdate.IsZero() {
		nextUpdate = thisUpdate.Add(24 * time.Hour)
	}
	num, err := newSerial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.RevocationList{Number: num, ThisUpdate: thisUpdate, NextUpdate: nextUpdate}
	for _, c := range revoked {
		tmpl.RevokedCertificateEntries = append(tmpl.RevokedCertificateEntries, x509.RevocationListEntry{
			SerialNumber: c.SerialNumber, RevocationTime: thisUpdate,
		})
	}
	der, err := x509.CreateRevocationList(rand.Reader, tmpl, ca.Cert, ca.Key)
	if err != nil {
		return nil, fmt.Errorf("pki: create CRL: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "X509 CRL", Bytes: der}), nil
}

// TLSCertificate returns the leaf and its chain as a tls.Certificate.
func (l *Leaf) TLSCertificate() (tls.Certificate, error) {
	c, err := tls.X509KeyPair(l.ChainPEM, l.KeyPEM)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("pki: key pair: %w", err)
	}
	return c, nil
}

// ServerTLS returns a server configuration presenting l. When clientCAs is
// non-nil, client certificates are required and verified against it.
func (l *Leaf) ServerTLS(clientCAs *CA) (*tls.Config, error) {
	c, err := l.TLSCertificate()
	if err != nil {
		return nil, err
	}
	cfg := &tls.Config{Certificates: []tls.Certificate{c}, MinVersion: tls.VersionTLS12}
	if clientCAs != nil {
		cfg.ClientCAs = clientCAs.Pool()
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return cfg, nil
}

// ClientTLS returns a client configuration trusting roots, presenting l
// when l is non-nil.
func ClientTLS(roots *CA, l *Leaf) (*tls.Config, error) {
	cfg := &tls.Config{RootCAs: roots.Pool(), MinVersion: tls.VersionTLS12}
	if l != nil {
		c, err := l.TLSCertificate()
		if err != nil {
			return nil, err
		}
		cfg.Certificates = []tls.Certificate{c}
	}
	return cfg, nil
}

// WriteFiles writes <name>.crt (the chain) and <name>.key (mode 0600) into
// dir and returns their paths.
func (l *Leaf) WriteFiles(dir, name string) (certPath, keyPath string, err error) {
	certPath = filepath.Join(dir, name+".crt")
	keyPath = filepath.Join(dir, name+".key")
	if err := os.WriteFile(certPath, l.ChainPEM, 0o600); err != nil {
		return "", "", fmt.Errorf("pki: %w", err)
	}
	if err := os.WriteFile(keyPath, l.KeyPEM, 0o600); err != nil {
		return "", "", fmt.Errorf("pki: %w", err)
	}
	return certPath, keyPath, nil
}

// WriteFile writes the CA certificate to path.
func (ca *CA) WriteFile(path string) error {
	if err := os.WriteFile(path, ca.CertPEM, 0o600); err != nil {
		return fmt.Errorf("pki: %w", err)
	}
	return nil
}

// WriteBundle writes the certificates of cas, concatenated, to path: the
// file a child process names in SSL_CERT_FILE to trust them.
func WriteBundle(path string, cas ...*CA) error {
	if len(cas) == 0 {
		return errors.New("pki: WriteBundle needs at least one CA")
	}
	var b bytes.Buffer
	for _, ca := range cas {
		b.Write(ca.CertPEM)
	}
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		return fmt.Errorf("pki: %w", err)
	}
	return nil
}

// Env returns the SSL_CERT_FILE environment entry for a bundle path.
func Env(bundlePath string) string { return "SSL_CERT_FILE=" + bundlePath }

func newKey(t KeyType) (crypto.Signer, error) {
	switch t {
	case ECDSAP256:
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("pki: generate %v key: %w", t, err)
		}
		return k, nil
	case RSA2048:
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			return nil, fmt.Errorf("pki: generate %v key: %w", t, err)
		}
		return k, nil
	}
	return nil, fmt.Errorf("pki: unknown key type %v", t)
}

func newSerial() (*big.Int, error) {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, fmt.Errorf("pki: serial: %w", err)
	}
	return n.Add(n, big.NewInt(1)), nil
}

func validity(nb, na time.Time) (time.Time, time.Time) {
	if nb.IsZero() {
		nb = time.Now().Add(-DefaultBackdate)
	}
	if na.IsZero() {
		na = nb.Add(DefaultValidity)
	}
	return nb.UTC(), na.UTC()
}

func encodeCert(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func encodeKey(k crypto.Signer) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		return nil, fmt.Errorf("pki: marshal key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}
