// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package ociregistry

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tclog "github.com/testcontainers/testcontainers-go/log"
	tcregistry "github.com/testcontainers/testcontainers-go/modules/registry"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/ravindu-rev/ruralz/internal/statestore/statestoretest/redisserver"
	"github.com/ravindu-rev/ruralz/internal/testkit/pki"
)

// Image is the OCI registry image of the container flavor
// (docs/engineering/01-tech-stack-and-libraries.md "CI tooling"),
// distribution 3.1.2, pinned by the digest of its multi-arch index, its
// version tag kept beside it. Never a module default. It serves no
// referrers API, so clients reach referrers through the referrers tag
// schema there.
const Image = "registry:3.1.2@sha256:38907510915c3f1604c7e83d6ab602ab3020d092f73bdaed8696773693ff768c"

// The credentials the container accepts, through htpasswd.
const (
	// ContainerUser is the container's user.
	ContainerUser = "ruralz"
	// ContainerPassword is ContainerUser's password.
	ContainerPassword = "ruralz-test-registry" //nolint:gosec // G101: a test container's password.
	// htpasswd is ContainerUser with the bcrypt hash of ContainerPassword.
	htpasswd = "ruralz:$2a$05$K.RP.vC6.vxd4XeYS201oet4m02BTfP0s../7kqpH9ZXb/8n4wx72\n" //nolint:gosec // G101: a test container's password hash.
)

const (
	// containerTimeout bounds starting the container, the image pull
	// included; it only ends a broken test.
	containerTimeout = 5 * time.Minute
	// registryPort is the port the image serves on.
	registryPort = "5000/tcp"
	// certDir is where the container's certificate and key go.
	certDir = "/certs"
)

// Container is a running registry image serving TLS with a certificate
// for localhost and 127.0.0.1 and requiring ContainerUser's Basic
// credentials.
type Container struct {
	host string
	ca   *pki.CA
}

// Host returns the registry's host and port.
func (c *Container) Host() string { return c.host }

// RootCAs returns a pool holding the CA of the registry's certificate.
func (c *Container) RootCAs() *x509.CertPool { return c.ca.Pool() }

// StartContainer runs Image for tb, terminated when tb ends. The
// registry image is a container kind of the integration tests, so it runs
// with the container flavor of redisserver.EnvStateStore: tb skips when
// the variable leaves that flavor out, and fails when it selects it and
// Docker cannot run the image.
func StartContainer(tb testing.TB) *Container {
	tb.Helper()
	f, err := redisserver.ParseFlavor(os.Getenv(redisserver.EnvStateStore))
	if err != nil {
		tb.Fatal(err)
	}
	if f&redisserver.FlavorContainer == 0 {
		tb.Skipf("ociregistry: %s does not select the container flavor; set it to container (Docker) to run the OCI registry suite", redisserver.EnvStateStore)
	}
	ca, err := pki.NewCA(pki.CAOptions{CommonName: "ociregistry container CA"})
	if err != nil {
		tb.Fatal(err)
	}
	leaf, err := ca.Server("localhost", "127.0.0.1")
	if err != nil {
		tb.Fatal(err)
	}
	dir := tb.TempDir()
	certPath, keyPath, err := leaf.WriteFiles(dir, "tls")
	if err != nil {
		tb.Fatal(err)
	}
	htpasswdPath := filepath.Join(dir, "htpasswd")
	if err := os.WriteFile(htpasswdPath, []byte(htpasswd), 0o600); err != nil {
		tb.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(tb.Context(), containerTimeout)
	defer cancel()
	ctr, err := tcregistry.Run(ctx, Image,
		testcontainers.WithLogger(tclog.TestLogger(tb)),
		tcregistry.WithHtpasswdFile(htpasswdPath),
		testcontainers.WithFiles(
			testcontainers.ContainerFile{HostFilePath: certPath, ContainerFilePath: certDir + "/tls.crt", FileMode: 0o644},
			testcontainers.ContainerFile{HostFilePath: keyPath, ContainerFilePath: certDir + "/tls.key", FileMode: 0o644},
		),
		testcontainers.WithEnv(map[string]string{
			"REGISTRY_HTTP_TLS_CERTIFICATE": certDir + "/tls.crt",
			"REGISTRY_HTTP_TLS_KEY":         certDir + "/tls.key",
		}),
		testcontainers.WithWaitStrategy(wait.ForHTTP("/v2/").WithPort(registryPort).
			WithTLS(true, &tls.Config{RootCAs: ca.Pool(), MinVersion: tls.VersionTLS12}).
			WithStatusCodeMatcher(func(status int) bool { return status == 200 || status == 401 }).
			WithStartupTimeout(containerTimeout)),
	)
	if ctr != nil {
		tb.Cleanup(func() {
			if err := testcontainers.TerminateContainer(ctr); err != nil {
				tb.Errorf("ociregistry: terminate the registry container: %v", err)
			}
		})
	}
	if err != nil {
		tb.Fatalf("ociregistry: run %s: %v", Image, err)
	}
	host, err := ctr.PortEndpoint(ctx, registryPort, "")
	if err != nil {
		tb.Fatalf("ociregistry: registry endpoint: %v", err)
	}
	return &Container{host: host, ca: ca}
}
