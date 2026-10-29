// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redisserver

import (
	"context"
	"errors"
	"maps"
	"slices"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/testkit/pki"
)

// echoHandler answers PING, AUTH (user "u", password "p"), ECHO and an
// error command.
func echoHandler(s *session, args []string) string {
	switch args[0] {
	case "AUTH":
		if len(args) == 3 && args[1] == "u" && args[2] == "p" {
			s.user = "u"
			return "+OK\r\n"
		}
		return "-WRONGPASS invalid username-password pair or user is disabled.\r\n"
	case "PING":
		return "+PONG\r\n"
	case "ECHO":
		return bulk(args[1])
	case "WHOAMI":
		return bulk(s.user)
	}
	return "-ERR unknown command\r\n"
}

// TestConnDo checks the admin round trip: replies, error replies as a
// *ReplyError beside the Value, AUTH first when a password is set, and
// connection reuse.
func TestConnDo(t *testing.T) {
	f := startFake(t, nil, echoHandler)
	ctx := t.Context()
	c, err := dialConn(ctx, f.addr(), dialOptions{user: "u", password: "p"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if v, err := c.Do(ctx, "ECHO", "a\r\nb"); err != nil || v.Str != "a\r\nb" {
		t.Fatalf("ECHO = %v, %v", v, err)
	}
	if v, err := c.Do(ctx, "WHOAMI"); err != nil || v.Str != "u" {
		t.Fatalf("WHOAMI = %v, %v; want the AUTH user", v, err)
	}
	v, err := c.Do(ctx, "BOOM")
	var re *ReplyError
	if !errors.As(err, &re) || re.Code() != "ERR" || v.Kind != '-' {
		t.Fatalf("BOOM = %v, %v; want the error reply", v, err)
	}
	if _, err := c.Do(ctx); err == nil {
		t.Fatal("Do without a command succeeded")
	}
	var names []string
	for _, c := range f.commands() {
		names = append(names, c[0])
	}
	if got := f.commands(); !slices.Equal(names, []string{"AUTH", "ECHO", "WHOAMI", "BOOM"}) || !slices.Equal(got[0], []string{"AUTH", "u", "p"}) {
		t.Errorf("commands = %q, want AUTH with the credentials first", got)
	}

	if _, err := dialConn(ctx, f.addr(), dialOptions{user: "u", password: "wrong"}); !errors.As(err, &re) || re.Code() != "WRONGPASS" {
		t.Errorf("dial with a wrong password = %v, want WRONGPASS", err)
	}
	if _, err := dialConn(ctx, "127.0.0.1:1", dialOptions{}); err == nil {
		t.Error("dial to a closed port succeeded")
	}
}

// TestConnDoHonorsContext checks that a canceled context ends a round
// trip the server never answers, and that the connection's next round
// trip is not cut short by it.
func TestConnDoHonorsContext(t *testing.T) {
	read := make(chan struct{}, 1)
	release := make(chan struct{})
	defer close(release)
	f := startFake(t, nil, func(_ *session, args []string) string {
		if args[0] == "HANG" {
			read <- struct{}{}
			<-release
		}
		return "+PONG\r\n"
	})
	c, err := dialConn(t.Context(), f.addr(), dialOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := c.Do(ctx, "HANG")
		done <- err
	}()
	<-read
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Do after cancel = %v, want context.Canceled", err)
	}

	d, err := dialConn(t.Context(), f.addr(), dialOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	first, stop := context.WithCancel(t.Context())
	if _, err := d.Do(first, "PING"); err != nil {
		t.Fatal(err)
	}
	stop()
	if _, err := d.Do(t.Context(), "PING"); err != nil {
		t.Fatalf("PING after an earlier context ended = %v", err)
	}
}

// TestConnTLS checks admin connections over TLS: the server name comes
// from the address, so the CA's certificate for 127.0.0.1 verifies, and
// another CA fails the handshake.
func TestConnTLS(t *testing.T) {
	ca, err := pki.NewCA(pki.CAOptions{})
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := ca.Server()
	if err != nil {
		t.Fatal(err)
	}
	serverCfg, err := leaf.ServerTLS(nil)
	if err != nil {
		t.Fatal(err)
	}
	f := startFake(t, serverCfg, echoHandler)
	clientCfg, err := pki.ClientTLS(ca, nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err := dialConn(t.Context(), f.addr(), dialOptions{tls: clientCfg})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if v, err := c.Do(t.Context(), "PING"); err != nil || v.Str != "PONG" {
		t.Fatalf("PING over TLS = %v, %v", v, err)
	}
	if clientCfg.ServerName != "" {
		t.Errorf("dialConn changed the caller's configuration: ServerName %q", clientCfg.ServerName)
	}
	other, err := pki.NewCA(pki.CAOptions{})
	if err != nil {
		t.Fatal(err)
	}
	otherCfg, err := pki.ClientTLS(other, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dialConn(t.Context(), f.addr(), dialOptions{tls: otherCfg}); err == nil {
		t.Error("TLS with another CA succeeded")
	}
}

// TestServerHelpers checks Server.Do, Dial and Info against a fake.
func TestServerHelpers(t *testing.T) {
	f := startFake(t, nil, func(_ *session, args []string) string {
		if args[0] == "INFO" {
			return bulk("# Server\r\nredis_version:8.2.1\r\nos:Linux\r\n\r\n# Clients\r\nconnected_clients:1\r\n")
		}
		return "+PONG\r\n"
	})
	s := f.server("fake", access{scheme: "redis"})
	info, err := s.Info(t.Context(), "server")
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"redis_version": "8.2.1", "os": "Linux", "connected_clients": "1"}; !maps.Equal(info, want) {
		t.Errorf("Info = %v, want %v", info, want)
	}
	if s.Output() != "" || s.Name() != "fake" {
		t.Errorf("container-like server: Output %q, Name %q", s.Output(), s.Name())
	}
	if err := s.Kill(t.Context()); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("Kill of a server without a process = %v, want ErrUnsupported", err)
	}
	if err := s.Restart(t.Context()); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("Restart of a server without a process = %v, want ErrUnsupported", err)
	}
	f.close()
	if _, err := s.Do(t.Context(), "PING"); err == nil {
		t.Error("Do to a closed server succeeded")
	}
}

// TestBuildURL checks the State Store URL forms of the configuration
// model: one endpoint, a cluster's seeds as addr parameters, rediss://
// under TLS, and escaped credentials.
func TestBuildURL(t *testing.T) {
	for _, tc := range []struct {
		acc   access
		addrs []string
		want  string
	}{
		{access{scheme: "redis"}, []string{"127.0.0.1:7000"}, "redis://127.0.0.1:7000"},
		{access{scheme: "rediss", user: &User{Name: "u", Password: "p@ss word"}}, []string{"127.0.0.1:7000"}, "rediss://u:p%40ss%20word@127.0.0.1:7000"},
		{access{scheme: "redis"}, []string{"a:1", "b:2", "c:3"}, "redis://a:1?addr=b:2&addr=c:3"},
		{access{scheme: "rediss", user: &User{Name: "default", Password: "x"}}, []string{"[::1]:1", "[::1]:2"}, "rediss://default:x@[::1]:1?addr=[::1]:2"},
	} {
		if got := buildURL(tc.acc, tc.addrs); got != tc.want {
			t.Errorf("buildURL(%v) = %q, want %q", tc.addrs, got, tc.want)
		}
	}
}

// TestParseInfo checks INFO parsing: sections and blank lines skipped,
// CRLF or LF, values holding colons kept whole.
func TestParseInfo(t *testing.T) {
	got := parseInfo("# Server\r\nredis_version:7.0.15\r\n\nexecutable:/usr/bin/redis-server\nno colon\r\n# Cluster\r\ncluster_enabled:1")
	want := map[string]string{"redis_version": "7.0.15", "executable": "/usr/bin/redis-server", "cluster_enabled": "1"}
	if !maps.Equal(got, want) {
		t.Errorf("parseInfo = %v, want %v", got, want)
	}
}
