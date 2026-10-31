// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package compose

import (
	"net"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// The tests of this file read the suite's files without a Docker daemon:
// what the Compose files, the Dockerfiles and the Collector configuration
// promise, and that they agree with the image definition and the example
// Bundles they serve.

// composeTopLevel are the top-level members a project file may hold.
func composeTopLevel() []string { return []string{"services", "volumes", "networks"} }

// loadProject parses one of the suite's Compose files.
func loadProject(t *testing.T, root, file string) (*tree.Node, []service) {
	t.Helper()
	doc := parseYAML(t, suitePath(root, file))
	for _, k := range keys(doc) {
		if !slices.Contains(composeTopLevel(), k) {
			t.Errorf("%s: top-level member %q, want only %v", file, k, composeTopLevel())
		}
	}
	return doc, services(file, doc)
}

func TestRuralzdServicesAreHardened(t *testing.T) {
	root := repoRoot(t)
	for _, file := range composeFiles() {
		t.Run(file, func(t *testing.T) {
			doc, svcs := loadProject(t, root, file)
			n := 0
			for _, s := range svcs {
				if !s.isRuralzd() {
					continue
				}
				n++
				checkHardened(t, doc, s)
			}
			if n == 0 {
				t.Errorf("%s runs no ruralzd service (image ${%s} or ${%s})", file, varImage, varOldImage)
			}
		})
	}
}

// checkHardened checks one ruralzd service against the hardening of
// "Compose suite": read-only root, no capabilities, no new privileges, the
// data directory on a named volume, a stop grace period above the 30 s
// Drain exit bound, and nothing more the image could need.
func checkHardened(t *testing.T, doc *tree.Node, s service) {
	t.Helper()
	if got := s.get("user"); got != nodeUser {
		t.Errorf("%s %s: user %q, want %q", s.file, s.name, got, nodeUser)
	}
	if got := s.get("read_only"); got != "true" {
		t.Errorf("%s %s: read_only %q, want true", s.file, s.name, got)
	}
	if got := s.list("cap_drop"); !slices.Equal(got, []string{"ALL"}) {
		t.Errorf("%s %s: cap_drop %v, want [ALL]", s.file, s.name, got)
	}
	if got := s.list("security_opt"); !slices.Contains(got, noNewPrivileges) {
		t.Errorf("%s %s: security_opt %v, want %q", s.file, s.name, got, noNewPrivileges)
	}
	if got := s.get("stop_grace_period"); got != stopGracePeriod {
		t.Errorf("%s %s: stop_grace_period %q, want %q", s.file, s.name, got, stopGracePeriod)
	}
	for _, k := range []string{"cap_add", "privileged", "tmpfs", "sysctls", "devices", "pid", "ipc", "userns_mode", "restart"} {
		if s.has(k) {
			t.Errorf("%s %s: sets %s; the image needs nothing beyond the hardened defaults", s.file, s.name, k)
		}
	}
	data, ok := s.mountAt(nodeDataDir)
	if !ok || data.typ != "volume" || data.source != dataVolume {
		t.Errorf("%s %s: %s mounted %+v, want the named volume %q", s.file, s.name, nodeDataDir, data, dataVolume)
	}
	if node(doc, "volumes", dataVolume) == nil {
		t.Errorf("%s: no top-level volume %q", s.file, dataVolume)
	}
	for _, m := range s.mounts() {
		if m.typ == "bind" && !m.readOnly {
			t.Errorf("%s %s: bind mount %s is writable; every bind mount of a Node is read-only", s.file, s.name, m.target)
		}
	}
	env := node(s.n, "environment")
	if node(env, "RURALZ_BIND_LOOPBACK") != nil {
		t.Errorf("%s %s: sets RURALZ_BIND_LOOPBACK; a container Node binds every interface", s.file, s.name)
	}
	if got := scalar(node(env, "RURALZ_ADMIN_TLS_DIR")); got != nodeAdminDir+"/tls" {
		t.Errorf("%s %s: RURALZ_ADMIN_TLS_DIR %q, want %s/tls (admin TLS)", s.file, s.name, got, nodeAdminDir)
	}
	if got := scalar(node(env, "RURALZ_ADMIN_TOKEN_FILE")); got != nodeAdminDir+"/token" {
		t.Errorf("%s %s: RURALZ_ADMIN_TOKEN_FILE %q, want %s/token (the operator token)", s.file, s.name, got, nodeAdminDir)
	}
	if got := scalar(node(env, "RURALZ_CONFIG")); got != nodeBundleDir {
		t.Errorf("%s %s: RURALZ_CONFIG %q, want %s", s.file, s.name, got, nodeBundleDir)
	}
	for _, target := range []string{nodeBundleDir, nodeAdminDir} {
		if m, ok := s.mountAt(target); !ok || m.typ != "bind" {
			t.Errorf("%s %s: %s mounted %+v, want a read-only bind mount", s.file, s.name, target, m)
		}
	}
}

func TestQuickstartProjectsShareTheUpstreamNamespace(t *testing.T) {
	root := repoRoot(t)
	if got := quickstartBundleEndpoint(t, root); got != quickstartEndpoint {
		t.Fatalf("the quickstart Bundle's Endpoint is %q, want %q, which the projects' mock Upstream answers", got, quickstartEndpoint)
	}
	for _, file := range []string{quickstartFile, upgradeFile, airgapFile} {
		t.Run(file, func(t *testing.T) {
			_, svcs := loadProject(t, root, file)
			up := serviceNamed(t, svcs, svcUpstream)
			if got := up.argAfter("-upstream"); got != quickstartEndpoint {
				t.Errorf("%s upstream: -upstream %q, want %q", file, got, quickstartEndpoint)
			}
			if got := up.argAfter("-upstream-routes"); got != "/etc/mock/routes.json" {
				t.Errorf("%s upstream: -upstream-routes %q, want /etc/mock/routes.json", file, got)
			}
			for _, s := range svcs {
				if !s.isRuralzd() {
					continue
				}
				if got := s.get("network_mode"); got != "service:"+svcUpstream {
					t.Errorf("%s %s: network_mode %q, want service:%s", file, s.name, got, svcUpstream)
				}
				if got := s.get("environment", "RURALZ_FETCH_ALLOW"); got != "127.0.0.1" {
					t.Errorf("%s %s: RURALZ_FETCH_ALLOW %q, want 127.0.0.1 for the loopback Endpoint", file, s.name, got)
				}
				if s.has("ports") || s.has("networks") {
					t.Errorf("%s %s: sets ports or networks, which belong to the namespace's owner", file, s.name)
				}
			}
			if file == airgapFile {
				return
			}
			published := up.list("ports")
			for _, p := range []int{portHTTP, portAdmin, portControl} {
				want := "127.0.0.1::" + strconv.Itoa(p)
				if !slices.Contains(published, want) {
					t.Errorf("%s upstream: ports %v, want %q", file, published, want)
				}
			}
		})
	}
}

// quickstartBundleEndpoint returns the address of the quickstart Bundle's
// one Endpoint.
func quickstartBundleEndpoint(t *testing.T, root string) string {
	t.Helper()
	for _, doc := range parseYAMLDocs(t, root+"/"+quickstartBundleDir+"/ruralz.yaml") {
		if scalar(node(doc, "kind")) != "Upstream" {
			continue
		}
		eps := node(doc, "spec", "endpoints")
		if eps == nil || len(eps.Items) != 1 {
			t.Fatalf("the quickstart Upstream has endpoints %v, want one", eps)
		}
		return scalar(node(eps.Items[0], "address"))
	}
	t.Fatal("the quickstart Bundle has no Upstream")
	return ""
}

func TestUpgradeContainersShareTheirState(t *testing.T) {
	_, svcs := loadProject(t, repoRoot(t), upgradeFile)
	old, cur := serviceNamed(t, svcs, svcOld), serviceNamed(t, svcs, svcNew)
	if old.imageVar() != varOldImage || cur.imageVar() != varImage {
		t.Errorf("images ${%s} and ${%s}, want ${%s} upgraded to ${%s}", old.imageVar(), cur.imageVar(), varOldImage, varImage)
	}
	// SO_REUSEPORT, the lock and handover.sock need one network namespace,
	// one data directory and one effective user, and the handover the same
	// RURALZ_CONFIG.
	for _, field := range []string{"network_mode", "user"} {
		if old.get(field) != cur.get(field) {
			t.Errorf("%s %q and %q differ", field, old.get(field), cur.get(field))
		}
	}
	if !slices.Equal(old.mounts(), cur.mounts()) {
		t.Errorf("volumes %+v and %+v differ", old.mounts(), cur.mounts())
	}
	oldEnv, curEnv := node(old.n, "environment"), node(cur.n, "environment")
	for _, k := range keys(oldEnv) {
		if scalar(node(oldEnv, k)) != scalar(node(curEnv, k)) {
			t.Errorf("environment %s differs: %q and %q", k, scalar(node(oldEnv, k)), scalar(node(curEnv, k)))
		}
	}
	if len(keys(oldEnv)) != len(keys(curEnv)) {
		t.Errorf("environments %v and %v differ", keys(oldEnv), keys(curEnv))
	}
	up := serviceNamed(t, svcs, svcUpstream)
	if got := up.get("sysctls", "net.ipv4.tcp_migrate_req"); got != "1" {
		t.Errorf("upstream: net.ipv4.tcp_migrate_req %q, want 1 in the shared network namespace", got)
	}
}

func TestAirgapProjectIsInternal(t *testing.T) {
	doc, svcs := loadProject(t, repoRoot(t), airgapFile)
	nets := node(doc, "networks")
	if got := keys(nets); !slices.Equal(got, []string{"airgap"}) {
		t.Fatalf("networks %v, want only airgap", got)
	}
	if got := scalar(node(nets, "airgap", "internal")); got != "true" {
		t.Errorf("network airgap: internal %q, want true", got)
	}
	for _, s := range svcs {
		if s.has("ports") {
			t.Errorf("%s publishes ports; nothing leaves an internal network", s.name)
		}
		if s.isRuralzd() {
			continue
		}
		if got := s.list("networks"); !slices.Equal(got, []string{"airgap"}) {
			t.Errorf("%s: networks %v, want [airgap]", s.name, got)
		}
	}
	node := serviceNamed(t, svcs, svcRuralzd)
	if m, ok := node.mountAt(nodeResolvConf); !ok || m.typ != "bind" || !m.readOnly || !strings.HasSuffix(m.source, "/resolv.conf") {
		t.Errorf("ruralzd: %s mounted %+v, want the suite's resolv.conf naming the DNS sink", nodeResolvConf, m)
	}
	up := serviceNamed(t, svcs, svcUpstream)
	if got := up.argAfter("-dns"); got != "0.0.0.0:53" {
		t.Errorf("upstream: -dns %q, want 0.0.0.0:53, the Node's resolver on loopback", got)
	}
	if got := up.get("sysctls", "net.ipv4.ip_unprivileged_port_start"); got != "0" {
		t.Errorf("upstream: net.ipv4.ip_unprivileged_port_start %q, want 0 for the DNS sink's port 53", got)
	}
	probe := serviceNamed(t, svcs, svcProbe)
	if got := probe.list("profiles"); !slices.Equal(got, []string{svcProbe}) {
		t.Errorf("probe: profiles %v, want [probe], so only docker compose run starts it", got)
	}
	cmd := probe.list("command")
	order := []string{"-ready", "-get", "-sink-check", "-dns-log"}
	last := -1
	for _, f := range order {
		i := slices.Index(cmd, f)
		if i <= last {
			t.Fatalf("probe command %v: want %v in that order, so the DNS log is read after everything else", cmd, order)
		}
		last = i
	}
	wants := map[string]string{
		"-ready":           "https://" + svcUpstream + ":" + strconv.Itoa(portAdmin) + "/readyz",
		"-get":             "http://" + svcUpstream + ":" + strconv.Itoa(portHTTP) + "/",
		"-sink-check":      svcUpstream + ":53",
		"-sink-check-name": sinkCheckName,
		"-dns-log":         "http://" + svcUpstream + ":" + strconv.Itoa(portControl) + "/dns",
		"-ca":              "/etc/probe/ca.crt",
	}
	for f, want := range wants {
		if got := probe.argAfter(f); got != want {
			t.Errorf("probe: %s %q, want %q", f, got, want)
		}
	}
}

func TestRESTProjectResolvesTheBundleNames(t *testing.T) {
	root := repoRoot(t)
	_, svcs := loadProject(t, root, restFile)
	names := restBundleNames(t, root)
	aliases := map[string][]string{}
	for _, s := range svcs {
		aliases[s.name] = s.list("networks", "default", "aliases")
	}
	for _, n := range []struct{ service, host string }{
		{svcIDP, names.jwksHost},
		{svcIDP, names.tokenHost},
		{svcUpstream, names.ordersHost},
		{svcUpstream, names.inventoryHost},
	} {
		if !slices.Contains(aliases[n.service], n.host) {
			t.Errorf("service %s: aliases %v, want %q, which the REST example Bundle names", n.service, aliases[n.service], n.host)
		}
	}
	idp := serviceNamed(t, svcs, svcIDP)
	if got := idp.argAfter("-idp"); got != "0.0.0.0:"+names.jwksPort {
		t.Errorf("idp: -idp %q, want port %s of the Bundle's JWKS URL", got, names.jwksPort)
	}
	if got := idp.argAfter("-idp-issuer"); got != issuer {
		t.Errorf("idp: -idp-issuer %q, want %q", got, issuer)
	}
	if got := idp.argAfter("-idp-client"); got != oauthClientID {
		t.Errorf("idp: -idp-client %q, want %q", got, oauthClientID)
	}
	up := serviceNamed(t, svcs, svcUpstream)
	if got := up.argAfter("-upstream"); got != "0.0.0.0:"+names.ordersPort || names.ordersPort != names.inventoryPort {
		t.Errorf("upstream: -upstream %q, want port %s of orders and inventory (%s)", got, names.ordersPort, names.inventoryPort)
	}
	if got := serviceNamed(t, svcs, svcValkey).imageVar(); got != varValkeyImage {
		t.Errorf("valkey: image ${%s}, want ${%s}", got, varValkeyImage)
	}
	col := serviceNamed(t, svcs, svcCollector)
	if got := col.imageVar(); got != varCollectorImage {
		t.Errorf("otel-collector: image ${%s}, want ${%s}", got, varCollectorImage)
	}
	if got := envOTLPEndpoint(t, root); got != "https://"+svcCollector+":4317" {
		t.Errorf("the compose Environment's OTLP endpoint is %q, want https://%s:4317", got, svcCollector)
	}
	// A Node refuses a State Store URL without credentials for a host
	// other than a loopback literal (RZ-CFG-042), so the Valkey service
	// requires the password the Node's URL carries.
	password := "${" + varValkeyPassword + ":?the State Store password}"
	node := serviceNamed(t, svcs, svcRuralzd)
	if got, want := node.get("environment", "RURALZ_STATE_STORE_URL"), "redis://:"+password+"@"+svcValkey+":6379"; got != want {
		t.Errorf("ruralzd: RURALZ_STATE_STORE_URL %q, want %q", got, want)
	}
	if got := serviceNamed(t, svcs, svcValkey).argAfter("--requirepass"); got != password {
		t.Errorf("valkey: --requirepass %q, want %q", got, password)
	}
	for _, target := range []string{nodeSecretRoot, nodeCABundle} {
		if m, ok := node.mountAt(target); !ok || m.typ != "bind" || !m.readOnly {
			t.Errorf("ruralzd: %s mounted %+v, want a read-only bind mount", target, m)
		}
	}
	published := node.list("ports")
	for _, p := range []int{portHTTP, portHTTPS, portAdmin} {
		if want := "127.0.0.1::" + strconv.Itoa(p); !slices.Contains(published, want) {
			t.Errorf("ruralzd: ports %v, want %q", published, want)
		}
	}
}

// bundleNames are the hosts and ports the REST example Bundle reaches.
type bundleNames struct {
	jwksHost, jwksPort, tokenHost string
	ordersHost, ordersPort        string
	inventoryHost, inventoryPort  string
}

// restBundleNames reads the names out of examples/shop-rest-bundle.
func restBundleNames(t *testing.T, root string) bundleNames {
	t.Helper()
	var out bundleNames
	for _, doc := range parseYAMLDocs(t, root+"/"+restBundleDir+"/policies/common.yaml") {
		switch scalar(node(doc, "metadata", "name")) {
		case "jwt-default":
			issuers := node(doc, "spec", "config", "issuers")
			if issuers == nil || len(issuers.Items) != 1 || scalar(node(issuers.Items[0], "issuer")) != issuer {
				t.Fatalf("jwt-default issuers %v, want the one issuer %s", issuers, issuer)
			}
			out.jwksHost, out.jwksPort = urlHostPort(t, scalar(node(issuers.Items[0], "jwksUrl")))
		case "upstream-oauth":
			out.tokenHost, _ = urlHostPort(t, scalar(node(doc, "spec", "config", "tokenUrl")))
		}
	}
	for _, doc := range parseYAMLDocs(t, root+"/"+restBundleDir+"/upstreams/services.yaml") {
		switch scalar(node(doc, "metadata", "name")) {
		case "orders":
			out.ordersHost = scalar(node(doc, "spec", "discovery", "service"))
			out.ordersPort = scalar(node(doc, "spec", "discovery", "port"))
		case "inventory":
			eps := node(doc, "spec", "endpoints")
			if eps == nil || len(eps.Items) != 1 {
				t.Fatalf("inventory endpoints %v, want one", eps)
			}
			host, port, err := net.SplitHostPort(scalar(node(eps.Items[0], "address")))
			if err != nil {
				t.Fatal(err)
			}
			out.inventoryHost, out.inventoryPort = host, port
		}
	}
	if out.jwksHost == "" || out.tokenHost == "" || out.ordersHost == "" || out.inventoryHost == "" {
		t.Fatalf("names %+v: the REST example Bundle changed shape", out)
	}
	return out
}

// urlHostPort returns the host and port of an https URL, 443 by default.
func urlHostPort(t *testing.T, raw string) (string, string) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" {
		t.Fatalf("URL %q: %v, want an https URL", raw, err)
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	return u.Hostname(), port
}

// envOTLPEndpoint returns the OTLP endpoint variable of the compose
// Environment.
func envOTLPEndpoint(t *testing.T, root string) string {
	t.Helper()
	for _, doc := range parseYAMLDocs(t, suitePath(root, "testdata/environments.yaml")) {
		if scalar(node(doc, "metadata", "name")) == composeEnv {
			return scalar(node(doc, "spec", "variables", "OTEL_EXPORTER_OTLP_ENDPOINT"))
		}
	}
	t.Fatalf("testdata/environments.yaml declares no Environment %s", composeEnv)
	return ""
}

func TestMockServicesRunUnprivileged(t *testing.T) {
	root := repoRoot(t)
	for _, file := range composeFiles() {
		_, svcs := loadProject(t, root, file)
		for _, s := range svcs {
			if s.imageVar() != varMockImage {
				continue
			}
			if s.get("user") != nodeUser || s.get("read_only") != "true" || !slices.Equal(s.list("cap_drop"), []string{"ALL"}) {
				t.Errorf("%s %s: user %q, read_only %q, cap_drop %v; the mock image needs no privilege either", file, s.name, s.get("user"), s.get("read_only"), s.list("cap_drop"))
			}
			if s.list("command")[0] != "serve" && s.list("command")[0] != "probe" {
				t.Errorf("%s %s: command %v, want mock serve or mock probe", file, s.name, s.list("command"))
			}
		}
	}
}

func TestCollectorImageIsPinned(t *testing.T) {
	root := repoRoot(t)
	lines := dockerfileLines(t, suitePath(root, "collector.Dockerfile"))
	from := regexp.MustCompile(`^FROM otel/opentelemetry-collector:([0-9]+\.[0-9]+\.[0-9]+)@sha256:[0-9a-f]{64}$`)
	var version string
	for _, l := range lines {
		if strings.HasPrefix(l, "FROM ") {
			m := from.FindStringSubmatch(l)
			if m == nil {
				t.Fatalf("collector.Dockerfile: %q, want the Collector pinned by the digest of its index", l)
			}
			version = m[1]
		}
	}
	if version == "" {
		t.Fatal("collector.Dockerfile has no FROM line")
	}
	if want := techStackCollectorVersion(t, root); version != want {
		t.Errorf("collector.Dockerfile pins %s, the tech stack's CI tooling row %s", version, want)
	}
	if !slices.Contains(lines, "COPY --chmod=0644 collector.yaml /etc/otelcol/config.yaml") {
		t.Errorf("collector.Dockerfile: %v, want collector.yaml copied to /etc/otelcol/config.yaml", lines)
	}
	ignore := dockerfileLines(t, suitePath(root, "collector.Dockerfile.dockerignore"))
	if !slices.Equal(ignore, []string{"*", "!collector.yaml"}) {
		t.Errorf("collector.Dockerfile.dockerignore: %v, want only collector.yaml admitted", ignore)
	}
}

// techStackCollectorVersion reads the Collector version of the CI tooling
// table of the tech stack document.
func techStackCollectorVersion(t *testing.T, root string) string {
	t.Helper()
	data, err := os.ReadFile(root + "/" + techStackDoc) //nolint:gosec // G304: a document of the repository
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile("(?m)^\\| `otel/opentelemetry-collector` image \\| ([0-9]+\\.[0-9]+\\.[0-9]+),").FindSubmatch(data)
	if m == nil {
		t.Fatalf("%s has no otel/opentelemetry-collector row", techStackDoc)
	}
	return string(m[1])
}

func TestCollectorConfiguration(t *testing.T) {
	root := repoRoot(t)
	cfg := parseYAML(t, suitePath(root, "collector.yaml"))
	grpc := node(cfg, "receivers", "otlp", "protocols", "grpc")
	if got := scalar(node(grpc, "endpoint")); got != "0.0.0.0:4317" {
		t.Errorf("OTLP gRPC endpoint %q, want 0.0.0.0:4317", got)
	}
	_, svcs := loadProject(t, root, restFile)
	col := serviceNamed(t, svcs, svcCollector)
	tlsMount, ok1 := col.mountAt("/etc/otelcol-tls")
	outMount, ok2 := col.mountAt("/var/lib/otelcol-out")
	if !ok1 || !tlsMount.readOnly || !ok2 || outMount.readOnly {
		t.Fatalf("otel-collector mounts %+v, want a read-only /etc/otelcol-tls and a writable /var/lib/otelcol-out", col.mounts())
	}
	for _, k := range []string{"cert_file", "key_file"} {
		if got := scalar(node(grpc, "tls", k)); !strings.HasPrefix(got, tlsMount.target+"/") {
			t.Errorf("OTLP TLS %s %q, want a file under %s", k, got, tlsMount.target)
		}
	}
	for signal, file := range signalFiles() {
		exp := "file/" + signal
		if got := scalar(node(cfg, "exporters", exp, "path")); got != outMount.target+"/"+file {
			t.Errorf("exporter %s path %q, want %s/%s", exp, got, outMount.target, file)
		}
		pipe := node(cfg, "service", "pipelines", signal)
		if !slices.Equal(scalars(node(pipe, "receivers")), []string{"otlp"}) || !slices.Equal(scalars(node(pipe, "exporters")), []string{exp}) {
			t.Errorf("pipeline %s: receivers %v, exporters %v; want otlp to %s", signal, scalars(node(pipe, "receivers")), scalars(node(pipe, "exporters")), exp)
		}
	}
	if got := col.get("user"); got != "${"+varUID+":?the runner's user}:${"+varGID+":?the runner's group}" {
		t.Errorf("otel-collector: user %q, want the runner's user and group so the suite reads the files", got)
	}
}

// signalFiles maps each signal to the file the Collector writes it to.
func signalFiles() map[string]string {
	return map[string]string{"traces": "traces.jsonl", "metrics": "metrics.jsonl", "logs": "logs.jsonl"}
}

func TestMockDockerfile(t *testing.T) {
	lines := dockerfileLines(t, suitePath(repoRoot(t), "mock.Dockerfile"))
	want := []string{"FROM scratch", "COPY --chmod=0755 mock /mock", "USER " + nodeUser, `ENTRYPOINT ["/mock"]`}
	if !slices.Equal(lines, want) {
		t.Errorf("mock.Dockerfile instructions %q, want %q", lines, want)
	}
}

func TestImageDefinitionFitsTheProjects(t *testing.T) {
	lines := dockerfileLines(t, repoRoot(t)+"/"+imageDockerfile)
	stage := -1
	for i, l := range lines {
		if l == "FROM scratch AS ruralzd" {
			stage = i
		}
	}
	if stage < 0 {
		t.Fatalf("%s has no final stage FROM scratch AS ruralzd", imageDockerfile)
	}
	final := lines[stage:]
	for _, want := range []string{
		"USER " + nodeUser,
		"EXPOSE " + strconv.Itoa(portHTTP) + "/tcp " + strconv.Itoa(portHTTPS) + "/tcp " + strconv.Itoa(portAdmin) + "/tcp",
		`ENTRYPOINT ["/usr/local/bin/ruralzd"]`,
		"COPY --from=certs --chown=65532:65532 /work/varlib/ /var/lib/",
	} {
		if !slices.Contains(final, want) {
			t.Errorf("%s final stage lacks %q, which the projects rely on", imageDockerfile, want)
		}
	}
	for _, l := range final {
		if strings.HasPrefix(l, "VOLUME") {
			t.Errorf("%s declares %q; the projects put %s on a named volume", imageDockerfile, l, nodeDataDir)
		}
	}
}

// dockerfileLines returns a Dockerfile's instructions, comments and blank
// lines dropped and continuation lines joined.
func dockerfileLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // G304: a file of the repository
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	var cur strings.Builder
	for _, l := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(l)
		if cur.Len() == 0 && (trimmed == "" || strings.HasPrefix(trimmed, "#")) {
			continue
		}
		if rest, ok := strings.CutSuffix(trimmed, "\\"); ok {
			cur.WriteString(strings.TrimSpace(rest) + " ")
			continue
		}
		cur.WriteString(trimmed)
		out = append(out, strings.Join(strings.Fields(cur.String()), " "))
		cur.Reset()
	}
	return out
}

// serviceNamed returns the service called name.
func serviceNamed(t *testing.T, svcs []service, name string) service {
	t.Helper()
	for _, s := range svcs {
		if s.name == name {
			return s
		}
	}
	t.Fatalf("no service %s", name)
	return service{}
}
