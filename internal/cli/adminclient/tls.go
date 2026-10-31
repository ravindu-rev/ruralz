// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package adminclient

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/cli/command"
	"github.com/ravindu-rev/ruralz/internal/tlsconf"
)

// protoHTTP11 is the only ALPN protocol the client offers: every call is
// one HTTP/1.1 exchange on a fresh connection.
const protoHTTP11 = "http/1.1"

// clientTLS returns the configuration of an https Node: the Node's client
// TLS policy (tlsconf.Client: TLS 1.2 or newer with the fixed TLS 1.2
// suites, no renegotiation), the server always verified against the
// system roots plus the certificates of cfg.CAFile, ServerName from host,
// ALPN http/1.1, and the client certificate of cfg.ClientCert and
// cfg.ClientKey, presented whenever the Node asks for one.
func clientTLS(cfg Config, names Names, host string) (*tls.Config, error) {
	var roots *x509.CertPool
	if cfg.CAFile != "" {
		pool, err := anchors(names.CAFile, cfg.CAFile)
		if err != nil {
			return nil, err
		}
		roots = pool
	}
	tc := tlsconf.Client(roots)
	tc.ServerName = serverName(host)
	tc.NextProtos = []string{protoHTTP11}
	if cfg.ClientCert != "" {
		pair, err := clientPair(names, cfg.ClientCert, cfg.ClientKey)
		if err != nil {
			return nil, err
		}
		tc.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return pair, nil }
	}
	return tc, nil
}

// anchors returns the system roots plus the certificates of the PEM file
// at path, --ca-file: every CERTIFICATE block must parse and one must be
// present. Without system roots the file's certificates stand alone.
func anchors(name, path string) (*x509.CertPool, error) {
	b, err := os.ReadFile(path) //nolint:gosec // G304: the operator names the --ca-file the CLI reads.
	if err != nil {
		return nil, command.NoResultf("%s: %w", name, err)
	}
	certs, err := pemCertificates(b)
	if err != nil {
		return nil, command.Usagef("%s %s: %w", name, path, err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	for _, c := range certs {
		pool.AddCert(c)
	}
	return pool, nil
}

// pemCertificates parses the CERTIFICATE blocks of b, skipping other
// blocks; one must be present and each must parse.
func pemCertificates(b []byte) ([]*x509.Certificate, error) {
	var out []*x509.Certificate
	for {
		var block *pem.Block
		if block, b = pem.Decode(b); block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("a PEM certificate does not parse: %w", err)
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, errors.New("holds no PEM certificate")
	}
	return out, nil
}

// clientPair reads the client certificate chain and its key: both PEM,
// the key unencrypted, and matching. A file that cannot be read exits 2;
// any other refusal is a usage error.
func clientPair(names Names, certPath, keyPath string) (*tls.Certificate, error) {
	certPEM, err := os.ReadFile(certPath) //nolint:gosec // G304: the operator names the --client-cert the CLI reads.
	if err != nil {
		return nil, command.NoResultf("%s: %w", names.ClientCert, err)
	}
	keyPEM, err := os.ReadFile(keyPath) //nolint:gosec // G304: the operator names the --client-key the CLI reads.
	if err != nil {
		return nil, command.NoResultf("%s: %w", names.ClientKey, err)
	}
	defer clear(keyPEM)
	if _, err := pemCertificates(certPEM); err != nil {
		return nil, command.Usagef("%s %s: %w", names.ClientCert, certPath, err)
	}
	if err := checkKeyPEM(keyPEM); err != nil {
		return nil, command.Usagef("%s %s: %w", names.ClientKey, keyPath, err)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, command.Usagef("%s and %s do not form a key pair: %w", names.ClientCert, names.ClientKey, err)
	}
	return &pair, nil
}

// checkKeyPEM requires an unencrypted PEM private key: the first block
// holding a key is neither PKCS #8 encrypted nor a legacy encrypted block
// (Proc-Type: 4,ENCRYPTED).
func checkKeyPEM(b []byte) error {
	for {
		var block *pem.Block
		if block, b = pem.Decode(b); block == nil {
			return errors.New("holds no PEM private key")
		}
		switch {
		case block.Type == "ENCRYPTED PRIVATE KEY", strings.Contains(block.Headers["Proc-Type"], "ENCRYPTED"):
			return errors.New("holds an encrypted private key; give the key unencrypted")
		case strings.HasSuffix(block.Type, "PRIVATE KEY"):
			return nil
		}
	}
}

// serverName turns a URL host into a ServerName: an IP literal loses its
// zone and stays an IP literal, which crypto/tls verifies against IP SANs
// and never sends as SNI; a name is lowercased without a trailing dot.
func serverName(host string) string {
	if a, err := netip.ParseAddr(host); err == nil {
		return a.WithZone("").String()
	}
	return strings.ToLower(strings.TrimSuffix(host, "."))
}
