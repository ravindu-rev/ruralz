// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redis

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/statestore"
)

// TestParseURLAccepts checks the URL forms of the configuration model:
// standalone with one endpoint and an optional database, cluster with
// seeds as addr parameters, sentinel with sentinel seeds and master_set,
// TLS for rediss:// and valkeys:// (with or without stateStore.tls), a
// user name alone as credentials, and the rueidis parameters that pass.
func TestParseURLAccepts(t *testing.T) {
	for _, tc := range []struct {
		name, url string
		topo      statestore.Topology
		withTLS   bool
		addrs     []string
		tls       bool
		unauth    bool
		db        int
		masterSet string
	}{
		{name: "loopback", url: "redis://127.0.0.1:6379", topo: statestore.TopologyStandalone, addrs: []string{"127.0.0.1:6379"}, unauth: true},
		{name: "localhost default port", url: "redis://localhost", topo: statestore.TopologyStandalone, addrs: []string{"localhost:6379"}, unauth: true},
		{name: "ipv6 loopback", url: "redis://[::1]:6380", topo: statestore.TopologyStandalone, addrs: []string{"[::1]:6380"}, unauth: true},
		{name: "tls with credentials and db", url: "rediss://ruralz:pw@cache.example:6380/2", topo: statestore.TopologyStandalone, addrs: []string{"cache.example:6380"}, tls: true, db: 2}, //nolint:gosec // G101: a test URL with a placeholder password.
		{name: "db parameter", url: "rediss://:pw@cache.example:6380?db=4", topo: statestore.TopologyStandalone, addrs: []string{"cache.example:6380"}, tls: true, db: 4},
		{name: "password only", url: "redis://:pw@10.0.0.5:6379", topo: statestore.TopologyStandalone, addrs: []string{"10.0.0.5:6379"}},
		{name: "user name only", url: "rediss://ruralz@cache.example:6380", topo: statestore.TopologyStandalone, addrs: []string{"cache.example:6380"}, tls: true},
		{name: "loopback user name only", url: "redis://ruralz@127.0.0.1:6379", topo: statestore.TopologyStandalone, addrs: []string{"127.0.0.1:6379"}},
		{name: "valkeys", url: "valkeys://u:pw@cache.example:6380", topo: statestore.TopologyStandalone, addrs: []string{"cache.example:6380"}, tls: true}, //nolint:gosec // G101: a test URL with a placeholder password.
		{name: "valkey", url: "valkey://127.0.0.1:6379", topo: statestore.TopologyStandalone, addrs: []string{"127.0.0.1:6379"}, unauth: true},
		{name: "stateStore.tls beside rediss", url: "rediss://:pw@cache.example:6380", topo: statestore.TopologyStandalone, withTLS: true, addrs: []string{"cache.example:6380"}, tls: true},
		{
			name: "cluster seeds", url: "rediss://:pw@a.example:7000?addr=b.example:7001&addr=c.example:7002", topo: statestore.TopologyCluster,
			addrs: []string{"a.example:7000", "b.example:7001", "c.example:7002"}, tls: true,
		},
		{
			name: "sentinel seeds", url: "rediss://ruralz:pw@s1.example:26379?addr=s2.example:26379&addr=s3.example:26379&master_set=main",
			topo: statestore.TopologySentinel, addrs: []string{"s1.example:26379", "s2.example:26379", "s3.example:26379"}, tls: true, masterSet: "main",
		},
		{
			name: "sentinel on loopback with db", url: "redis://127.0.0.1:26379/1?master_set=main", topo: statestore.TopologySentinel,
			addrs: []string{"127.0.0.1:26379"}, unauth: true, db: 1, masterSet: "main",
		},
		{
			name: "passing parameters", url: "rediss://:pw@a.example:7000?dial_timeout=2s&write_timeout=3s&client_name=gw&client_cache=0&protocol=3&max_retries=0",
			topo: statestore.TopologyStandalone, addrs: []string{"a.example:7000"}, tls: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ep, err := parseURL(tc.url, tc.topo, tc.withTLS)
			if err != nil {
				t.Fatalf("parseURL: %v", err)
			}
			if !slices.Equal(ep.opt.InitAddress, tc.addrs) || ep.tls != tc.tls || ep.unauthenticated != tc.unauth || ep.opt.SelectDB != tc.db ||
				ep.opt.Sentinel.MasterSet != tc.masterSet {
				t.Fatalf("parseURL = addrs %v tls %v unauth %v db %d master set %q; want %v %v %v %d %q",
					ep.opt.InitAddress, ep.tls, ep.unauthenticated, ep.opt.SelectDB, ep.opt.Sentinel.MasterSet,
					tc.addrs, tc.tls, tc.unauth, tc.db, tc.masterSet)
			}
		})
	}
}

// TestParseURLRefuses checks each reason and its code: RZ-CFG-026 for a
// URL that does not parse or fit its topology; RZ-CFG-042 for a refused
// scheme or parameter, conflicting parameters, a cleartext scheme beside
// stateStore.tls, and a non-loopback URL without credentials (TB-4), with
// the secret resolver's loopback literals. No error carries the URL.
func TestParseURLRefuses(t *testing.T) {
	const fit, transport = "RZ-CFG-026", "RZ-CFG-042"
	for _, tc := range []struct {
		name, url string
		topo      statestore.Topology
		withTLS   bool
		want      error
		code      string
	}{
		{"unparsable", "redis://pw@[bad", statestore.TopologyStandalone, false, ErrURL, fit},
		{"bad database", "redis://:pw@cache.example:6379/x", statestore.TopologyStandalone, false, ErrURL, fit},
		{"addr under standalone", "redis://:pw@a.example:1?addr=b.example:2", statestore.TopologyStandalone, false, ErrTopology, fit},
		{"master_set under standalone", "redis://:pw@s.example:26379?master_set=main", statestore.TopologyStandalone, false, ErrTopology, fit},
		{"master_set under cluster", "redis://:pw@s.example:26379?master_set=main", statestore.TopologyCluster, false, ErrTopology, fit},
		{"sentinel without master_set", "redis://:pw@s.example:26379?addr=t.example:26379", statestore.TopologySentinel, false, ErrTopology, fit},
		{"sentinel with an empty master_set", "redis://:pw@s.example:26379?master_set=", statestore.TopologySentinel, false, ErrTopology, fit},
		{"database under cluster", "redis://:pw@a.example:7000/3", statestore.TopologyCluster, false, ErrTopology, fit},
		{"unix socket", "unix:///run/redis.sock", statestore.TopologyStandalone, false, ErrScheme, transport},
		{"http", "http://:pw@cache.example:6379", statestore.TopologyStandalone, false, ErrScheme, transport},
		{"skip_verify", "rediss://:pw@cache.example:6380?skip_verify=true", statestore.TopologyStandalone, false, ErrParameter, transport},
		{"skip_verify false", "rediss://:pw@cache.example:6380?skip_verify=false", statestore.TopologyStandalone, false, ErrParameter, transport},
		{"client cache", "rediss://:pw@cache.example:6380?client_cache=1", statestore.TopologyStandalone, false, ErrParameter, transport},
		{"resp2", "rediss://:pw@cache.example:6380?protocol=2", statestore.TopologyStandalone, false, ErrParameter, transport},
		{"retries", "rediss://:pw@cache.example:6380?max_retries=3", statestore.TopologyStandalone, false, ErrParameter, transport},
		{"unknown parameter", "rediss://:pw@cache.example:6380?s3cr3t-token=1", statestore.TopologyStandalone, false, ErrParameter, transport},
		{"unknown parameter without value", "rediss://:pw@cache.example:6380?tls", statestore.TopologyStandalone, false, ErrParameter, transport},
		{"misspelled master_set", "rediss://:pw@s.example:26379?master_set=main&master_sett=main", statestore.TopologySentinel, false, ErrParameter, transport},
		{"password parameter", "rediss://cache.example:6380?password=x", statestore.TopologyStandalone, false, ErrParameter, transport},
		{"database twice", "rediss://:pw@cache.example:6380/1?db=2", statestore.TopologyStandalone, false, ErrConflict, transport},
		{"db given twice", "rediss://:pw@cache.example:6380?db=1&db=2", statestore.TopologyStandalone, false, ErrConflict, transport},
		{"master_set given twice", "rediss://:pw@s.example:26379?master_set=a&master_set=b", statestore.TopologySentinel, false, ErrConflict, transport},
		{"dial_timeout given twice", "rediss://:pw@cache.example:6380?dial_timeout=1s&dial_timeout=9s", statestore.TopologyStandalone, false, ErrConflict, transport},
		{"protocol given twice", "rediss://:pw@cache.example:6380?protocol=3&protocol=2", statestore.TopologyStandalone, false, ErrConflict, transport},
		{"stateStore.tls beside redis", "redis://:pw@cache.example:6379", statestore.TopologyStandalone, true, ErrConflict, transport},
		{"stateStore.tls beside valkey", "valkey://127.0.0.1:6379", statestore.TopologyStandalone, true, ErrConflict, transport},
		{"remote without credentials", "redis://10.0.0.5:6379", statestore.TopologyStandalone, false, ErrCredentials, transport},
		{"remote empty password", "redis://:@cache.example:6379", statestore.TopologyStandalone, false, ErrCredentials, transport},
		{"remote seed without credentials", "redis://127.0.0.1:7000?addr=10.0.0.5:7001", statestore.TopologyCluster, false, ErrCredentials, transport},
		{"remote sentinel without credentials", "redis://127.0.0.1:26379?addr=10.0.0.5:26379&master_set=main", statestore.TopologySentinel, false, ErrCredentials, transport},
		{"IPv4-mapped loopback", "redis://[::ffff:127.0.0.1]:6379", statestore.TopologyStandalone, false, ErrCredentials, transport},
		{"name resolving to loopback", "redis://localhost.example:6379", statestore.TopologyStandalone, false, ErrCredentials, transport},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseURL(tc.url, tc.topo, tc.withTLS)
			if !errors.Is(err, tc.want) {
				t.Fatalf("parseURL(%q) = %v, want %v", tc.name, err, tc.want)
			}
			if code, ok := errcode.CodeOf(err); !ok || code != tc.code {
				t.Fatalf("parseURL(%q) code %q, want %s", tc.name, code, tc.code)
			}
			for _, secret := range []string{"pw", "cache.example", "10.0.0.5", "s.example", "s3cr3t-token", "master_sett"} {
				if strings.Contains(tc.url, secret) && strings.Contains(err.Error(), secret) {
					t.Fatalf("error %q carries %q from the URL", err, secret)
				}
			}
		})
	}
}

// TestCodesAreRegistered: both codes of a refused URL are registered
// configuration codes.
func TestCodesAreRegistered(t *testing.T) {
	for _, code := range []string{codeURL, codeTransport} {
		if c, ok := errcode.Lookup(code); !ok || c.Area != errcode.AreaCFG {
			t.Errorf("%s: registered %v, %+v", code, ok, c)
		}
	}
}

// TestNewRefusesConfiguration checks New's other refusals: another driver
// and an unknown topology (RZ-CFG-026), a short MAC key, a non-loopback
// URL without credentials (RZ-CFG-042), and stateStore.tls without its
// secrets, beside a cleartext URL, or with a certificate but no key.
func TestNewRefusesConfiguration(t *testing.T) {
	ctx := context.Background()
	if _, err := New(ctx, statestore.Config{Driver: statestore.DriverMemory}, statestore.Deps{}, Options{}); !errors.Is(err, ErrWrongDriver) {
		t.Errorf("memory configuration: %v, want ErrWrongDriver", err)
	}
	if _, err := New(ctx, statestore.Config{Driver: statestore.DriverRedis, Topology: 9, URL: "redis://127.0.0.1"}, statestore.Deps{}, Options{}); !errors.Is(err, ErrTopology) {
		t.Errorf("topology 9: %v, want ErrTopology", err)
	}
	_, err := New(ctx, statestore.Config{Driver: statestore.DriverRedis, URL: "redis://127.0.0.1"}, statestore.Deps{MACKey: []byte("short")}, Options{})
	if err == nil || !strings.Contains(err.Error(), "32 bytes") {
		t.Errorf("short MAC key: %v", err)
	}
	_, err = Open(ctx, statestore.Config{Driver: statestore.DriverRedis, URL: "redis://10.0.0.5"}, statestore.Deps{})
	if code, _ := errcode.CodeOf(err); !errors.Is(err, ErrCredentials) || code != codeTransport {
		t.Errorf("Open: %v (%s), want ErrCredentials, %s", err, code, codeTransport)
	}

	ca := certRef("ca.crt")
	crt := certRef("tls.crt")
	// The cache connection's TLS block is named in its errors.
	_, err = New(ctx, statestore.Config{
		Driver: statestore.DriverRedis, Role: statestore.RoleCache, URL: "rediss://127.0.0.1:6379",
		TLS: &statestore.TLS{ClientCertificate: &crt, Secrets: tlsSecrets{}},
	}, statestore.Deps{}, Options{})
	if err == nil || !strings.Contains(err.Error(), "stateStore.cache.tls") {
		t.Errorf("cache TLS error %v, want it to name stateStore.cache.tls", err)
	}
	for _, tc := range []struct {
		name, url string
		tls       *statestore.TLS
		code      string
	}{
		{"cleartext URL", "redis://127.0.0.1:6379", &statestore.TLS{}, codeTransport},
		{"no secrets", "rediss://127.0.0.1:6379", &statestore.TLS{CACertificate: &ca}, "RZ-CFG-026"},
		{"unresolved CA", "rediss://127.0.0.1:6379", &statestore.TLS{CACertificate: &ca, Secrets: tlsSecrets{}}, "RZ-CFG-026"},
		{"certificate without key", "rediss://127.0.0.1:6379", &statestore.TLS{ClientCertificate: &crt, Secrets: tlsSecrets{}}, "RZ-CFG-005"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(ctx, statestore.Config{Driver: statestore.DriverRedis, URL: tc.url, TLS: tc.tls}, statestore.Deps{}, Options{})
			if code, ok := errcode.CodeOf(err); !ok || code != tc.code {
				t.Fatalf("New = %v (%q), want %s", err, code, tc.code)
			}
		})
	}
}

// TestUserNameAloneAuthenticates: a URL with a user name and no password
// carries credentials (an ACL user without a password), so the dialer
// sends AUTH with them before the client takes the connection.
func TestUserNameAloneAuthenticates(t *testing.T) {
	f := newFixture(t, fixtureOptions{urlFor: func(s *fakeServer) string { return "redis://ruralz@" + s.addr }})
	f.ready()
	c := gcraCall(statestore.DigestOf("k"), oneLimit())
	if f.consume(c); c.Err != nil {
		t.Fatalf("call: Err %v", c.Err)
	}
	if f.srv.count("AUTH") == 0 {
		t.Fatal("no AUTH for a user name without a password")
	}
	if f.d.Status().Unauthenticated {
		t.Error("a user name is credentials, yet Status reports unauthenticated")
	}
}
