// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package topology

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/mockidp"
	"github.com/ravindu-rev/ruralz/internal/testkit/mockup"
	"github.com/ravindu-rev/ruralz/internal/testkit/otlpsink"
)

// Tests for what the leak scan reads and leaves out, and for the State
// Store hops and URLs the harness writes.

// Every admin response is captured for the scan, except /debug/pprof/
// profiles.
func TestAdminResponsesAreCaptured(t *testing.T) {
	topo := standinTopology(t)
	n := addNodes(t, topo, 1, nodeOptions(t, topo))[0]
	file := filepath.Join(topo.captures.dir, "admin-"+n.Name())
	read := func() []byte {
		b, _ := os.ReadFile(file) //nolint:gosec // G304: the topology's capture file
		return b
	}
	if _, err := n.Admin(t.Context(), "/debug/snapshots"); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(read(), []byte(`"snapshots"`)) {
		t.Fatalf("the /debug/snapshots answer was not captured: %q", read())
	}
	before := read()
	if _, err := n.Admin(t.Context(), "/debug/pprof/heap"); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(read(), before) {
		t.Error("a /debug/pprof/ answer was captured")
	}
	if _, err := n.Admin(t.Context(), "/unknown"); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(read()[len(before):], []byte("404 page not found")) {
		t.Error("a 404 answer was not captured")
	}
}

// protoField encodes one length-delimited protobuf field.
func protoField(field uint64, b []byte) []byte {
	out := binary.AppendUvarint(nil, field<<3|2)
	out = binary.AppendUvarint(out, uint64(len(b)))
	return append(out, b...)
}

// exportLog sends the sink an OTLP logs export whose one record's body is
// body, as gRPC over HTTP/2 encoded by hand: the gRPC and OpenTelemetry
// modules link only into internal/telemetry and the sink.
func exportLog(t *testing.T, topo *Topology, sink *otlpsink.Sink, body string) {
	t.Helper()
	// ExportLogsServiceRequest.resource_logs (1) > ResourceLogs.scope_logs
	// (2) > ScopeLogs.log_records (2) > LogRecord.body (5) >
	// AnyValue.string_value (1).
	msg := protoField(1, protoField(2, protoField(2, protoField(5, protoField(1, []byte(body))))))
	frame := binary.BigEndian.AppendUint32([]byte{0}, uint32(len(msg))) //nolint:gosec // G115: a message of a few bytes
	frame = append(frame, msg...)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second) // a hang guard only
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sink.URL()+"/opentelemetry.proto.collector.logs.v1.LogsService/Export", bytes.NewReader(frame))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/grpc")
	req.Header.Set("TE", "trailers")
	tr := &http.Transport{TLSClientConfig: topo.ClientTLS(), ForceAttemptHTTP2: true}
	defer tr.CloseIdleConnections()
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if st := resp.Trailer.Get("Grpc-Status"); st != "0" {
		t.Fatalf("export answered grpc-status %q (%s)", st, resp.Trailer.Get("Grpc-Message"))
	}
}

// The scan reads the OTLP records as the exporter sent them.
func TestLeakScanReadsOTLPRecords(t *testing.T) {
	topo := standinTopology(t)
	sink, err := topo.StartOTLPSink(otlpsink.Config{})
	if err != nil {
		t.Fatal(err)
	}
	exportLog(t, topo, sink, "a log line with "+topo.Canary())
	if errs := topo.dumpCaptures(t.Context(), nil, nil, []*otlpsink.Sink{sink}); len(errs) > 0 {
		t.Fatal(errs)
	}
	findings, errs := topo.scan(nil)
	if len(errs) > 0 || len(findings) != 1 || findings[0].Source != "captures/otlp-1" {
		t.Fatalf("findings %v, errors %v; want the log record in the OTLP records", findings, errs)
	}
	// Clean up the planted leak so the teardown's own scan passes.
	sink.Reset()
	if err := os.Remove(filepath.Join(topo.captures.dir, "otlp-1")); err != nil {
		t.Fatal(err)
	}
}

// The mock IdP's token endpoint, the designated destination of a client
// secret, is not scanned; a mock Upstream log that dropped requests makes
// the scan incomplete, which fails the test.
func TestDesignatedDestinationsAndIncompleteLogs(t *testing.T) {
	rtb := &recordingTB{TB: t}
	t.Cleanup(func() {
		got := rtb.recorded()
		if strings.Contains(got, "secret leak") {
			t.Errorf("the token endpoint's records were scanned:\n%s", got)
		}
		if !strings.Contains(got, `leak scan of mock Upstream "tiny" is incomplete`) {
			t.Errorf("no incomplete-scan error for the dropped requests:\n%s", got)
		}
	})
	cfg := standinCfg(t)
	cfg.ArtifactDir = t.TempDir() // the recorded failure keeps its evidence here
	topo := New(rtb, cfg)
	idp, err := topo.StartIDP(mockidp.Config{})
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {"gateway"}, "client_secret": {topo.Canary()}}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, idp.TokenURL(), strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := topo.HTTPClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(idp.TokenRequests()) != 1 {
		t.Fatalf("token request answered %d, %d recorded", resp.StatusCode, len(idp.TokenRequests()))
	}
	tiny, err := topo.StartMockUpstream(mockup.Config{Name: "tiny", LogSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		get(t, topo.HTTPClient(), tiny.URL()+"/")
	}
}

// A State Store URL keeps its credentials, database and master_set, with
// the host and every addr parameter through the Node's proxies.
func TestProxiedURL(t *testing.T) {
	proxies := map[string]string{"127.0.0.1:1": "127.0.0.1:91", "127.0.0.1:2": "127.0.0.1:92", "127.0.0.1:3": "127.0.0.1:93"}
	proxy := func(addr string) (string, error) { return proxies[addr], nil }
	for _, tt := range []struct{ name, raw, want string }{
		{"standalone", "rediss://ruralz:pw@127.0.0.1:1/2", "rediss://ruralz:pw@127.0.0.1:91/2"},
		{"cluster", "rediss://127.0.0.1:1?addr=127.0.0.1:2&addr=127.0.0.1:3", "rediss://127.0.0.1:91?addr=127.0.0.1%3A92&addr=127.0.0.1%3A93"},
		{"sentinel", "redis://ruralz:pw@127.0.0.1:1?addr=127.0.0.1:2&master_set=ruralz", "redis://ruralz:pw@127.0.0.1:91?addr=127.0.0.1%3A92&master_set=ruralz"},
	} {
		got, err := proxiedURL(tt.raw, proxy)
		if err != nil || got != tt.want {
			t.Errorf("%s: proxiedURL(%s) = %s, %v; want %s", tt.name, tt.raw, got, err, tt.want)
		}
		u, err := url.Parse(got)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range u.Query()["addr"] {
			if !strings.HasPrefix(a, "127.0.0.1:9") {
				t.Errorf("%s: seed %s is not a proxy", tt.name, a)
			}
		}
	}
	if _, err := proxiedURL("://bad", proxy); err == nil {
		t.Error("an unparsable URL succeeded")
	}
}

// With the Transport toggles a State Store runs in cleartext without a
// user, and its URL says so.
func TestStateStoreCleartextWithoutUser(t *testing.T) {
	topo := standinTopology(t, func(c *Config) {
		c.Transport.CleartextStateStore = true
		c.Transport.NoStateStoreUser = true
	})
	ss := topo.StartStateStore("plain", StateStoreOptions{})
	if ss.CAPEM() != nil {
		t.Error("a cleartext State Store has a CA")
	}
	o := nodeOptions(t, topo)
	o.StateStoreURLs = map[string]string{refStoreURL: "plain"}
	n := addNodes(t, topo, 1, o)[0]
	v, _ := n.SecretValue(refStoreURL)
	u, err := url.Parse(string(v))
	if err != nil || u.Scheme != "redis" || u.User != nil {
		t.Fatalf("URL %s (%v), want redis:// without credentials", v, err)
	}
	proxies, ok := n.StateStoreProxy("plain")
	if !ok || len(proxies) != 1 || proxies[0].Addr() != u.Host {
		t.Fatalf("proxies %v, want one at the URL's host %s", proxies, u.Host)
	}
	ping(t, topo, u)
	if st := proxies[0].Stats(); st.Accepted < 1 {
		t.Errorf("the proxy accepted %d connections", st.Accepted)
	}
}
