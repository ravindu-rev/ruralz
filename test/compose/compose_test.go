// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package compose is the Compose suite
// (docs/engineering/03-testing-and-quality-strategy.md "Compose suite"):
// the Docker Compose end-to-end environment in file mode, which tests a
// shipped ruralzd image. `make image-checks` runs it under the test-only
// tag compose, driving docker compose from the runner image, in real time
// with condition waits under deadlines, never fixed sleeps.
//
// # Projects
//
// Each scenario is one Compose file in this directory, run as its own
// project per test, with the variables the suite sets (RURALZ_COMPOSE_*):
//
//   - quickstart.compose.yaml: the image serves the quickstart Bundle,
//     unchanged, from inside the mock Upstream's network namespace;
//   - upgrade.compose.yaml: a Zero-Downtime Upgrade from the old image to
//     the image under test, both containers on one data volume;
//   - airgap.compose.yaml: the air-gapped start on an internal network,
//     checked from a probe client container, with a DNS sink as the Node's
//     only resolver;
//   - rest.compose.yaml: the REST example Bundle, which the CLI on the
//     runner renders with the compose Environment of testdata/, beside a
//     Valkey 9 State Store, the mock Upstream and IdP and an OpenTelemetry
//     Collector; ruralz bundle render --effective --route, the spans the
//     Collector receives, ruralz bundle diff against /config/dump over TLS
//     and the signals reaching the Collector.
//
// Every ruralzd container runs with read_only, cap_drop [ALL],
// no-new-privileges, its data directory on a named volume and a 40 s stop
// grace period, which the untagged tests check in the files and the
// tagged ones in the running containers.
//
// # Inputs
//
// RURALZ_TEST_IMAGE names the image under test; without it every tagged
// test skips, and with it a missing Docker daemon or compose plugin fails
// the suite. RURALZ_TEST_PREVIOUS_IMAGE names the image the upgrade starts
// from, the previous release by digest; empty, the image under test.
// RURALZ_TEST_BIN_DIR names a directory holding the ruralz CLI the suite
// runs, such as a release's linux archive unpacked; empty, the suite
// builds it from the commit with the release flags
// (internal/testkit/binaries). The suite builds its mock image from
// test/compose/mockimage and its Collector image from collector.Dockerfile,
// and removes both at the end. When a test fails, the suite writes each
// project's container list and logs and the Collector's files under
// RURALZ_TEST_ARTIFACT_DIR.
//
// The untagged tests need no daemon: they read the Compose files, the
// Dockerfiles, the Collector configuration and the compose Environment.
package compose

import (
	"path/filepath"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/testkit/binaries"
)

// Compose files of the projects, relative to this directory.
const (
	quickstartFile = "quickstart.compose.yaml"
	upgradeFile    = "upgrade.compose.yaml"
	airgapFile     = "airgap.compose.yaml"
	restFile       = "rest.compose.yaml"
)

// composeFiles lists every project's file.
func composeFiles() []string {
	return []string{quickstartFile, upgradeFile, airgapFile, restFile}
}

// Variables the Compose files read; the suite sets every one.
const (
	varImage          = "RURALZ_COMPOSE_IMAGE"
	varOldImage       = "RURALZ_COMPOSE_OLD_IMAGE"
	varMockImage      = "RURALZ_COMPOSE_MOCK_IMAGE"
	varRun            = "RURALZ_COMPOSE_RUN"
	varValkeyImage    = "RURALZ_COMPOSE_VALKEY_IMAGE"
	varValkeyPassword = "RURALZ_COMPOSE_VALKEY_PASSWORD" //nolint:gosec // G101: the name of the variable, not a credential
	varCollectorImage = "RURALZ_COMPOSE_COLLECTOR_IMAGE"
	varUID            = "RURALZ_COMPOSE_UID"
	varGID            = "RURALZ_COMPOSE_GID"
)

// ruralzdImageVars are the variables a ruralzd service's image is: the
// image under test or the image the upgrade starts from.
func ruralzdImageVars() []string { return []string{varImage, varOldImage} }

// Service names.
const (
	svcUpstream  = "upstream"
	svcRuralzd   = "ruralzd"
	svcOld       = "ruralzd-old"
	svcNew       = "ruralzd-new"
	svcProbe     = "probe"
	svcIDP       = "idp"
	svcValkey    = "valkey"
	svcCollector = "otel-collector"
)

// Ports inside the containers.
const (
	portHTTP    = 8080
	portHTTPS   = 8443
	portAdmin   = 9901
	portControl = 9100
)

// Paths inside a ruralzd container and the hardening every one runs with
// (docs/engineering/04-release-versioning-and-compatibility.md "Packaging
// and audit": the image needs nothing beyond these).
const (
	nodeUser        = "65532:65532"
	nodeDataDir     = "/var/lib/ruralz"
	nodeBundleDir   = "/etc/ruralz-bundle"
	nodeAdminDir    = "/run/ruralz-admin"
	nodeSecretRoot  = "/etc/ruralz" //nolint:gosec // G101: a directory path, not a credential
	nodeCABundle    = "/etc/ssl/certs/ca-certificates.crt"
	nodeResolvConf  = "/etc/resolv.conf"
	stopGracePeriod = "40s"
	noNewPrivileges = "no-new-privileges:true"
	dataVolume      = "data"
	// quickstartEndpoint is the quickstart Bundle's one Endpoint, which
	// the mock Upstream answers inside the network namespace it shares.
	quickstartEndpoint = "127.0.0.1:9000"
)

// Paths of the repository the suite reads.
const (
	quickstartBundleDir = "examples/quickstart"
	restBundleDir       = "examples/shop-rest-bundle"
	imageDockerfile     = "deploy/container/Dockerfile"
	techStackDoc        = "docs/engineering/01-tech-stack-and-libraries.md"
	// suiteDir is this directory.
	suiteDir = "test/compose"
)

// The compose Environment of testdata/ (environments.yaml and its overlay
// overlays/compose/), with which the CLI renders the REST example Bundle.
const (
	composeEnv = "compose"
	// consumerKeyRef is the file reference the overlay gives the partner
	// Consumer's API key, a canary the suite writes under the secret root.
	consumerKeyRef = nodeSecretRoot + "/consumers/partner-acme-primary"
	// clientSecretRef is the example's OAuth client secret reference.
	clientSecretRef = nodeSecretRoot + "/oauth/client-secret"
	// The example's https listener certificate and key.
	tlsCertRef = nodeSecretRoot + "/tls/tls.crt"
	tlsKeyRef  = nodeSecretRoot + "/tls/tls.key"
)

// The REST example's Routes, the names its Bundle resolves and what its
// Policies expect.
const (
	routeSummary  = "orders-summary"
	routePartner  = "partner-orders"
	shopHost      = "api.shop.example"
	idpHost       = "auth.shop.example"
	issuer        = "https://" + idpHost
	audience      = "shop-api"
	partnerOrigin = "https://portal.acme.example"
	oauthClientID = "gateway-edge"
	oauthScope    = "orders.read"
)

// repoRoot returns the repository root.
func repoRoot(t testing.TB) string {
	t.Helper()
	root, err := binaries.RepoRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// suitePath returns the path of a file of this directory.
func suitePath(root, name string) string {
	return filepath.Join(root, filepath.FromSlash(suiteDir), filepath.FromSlash(name))
}
