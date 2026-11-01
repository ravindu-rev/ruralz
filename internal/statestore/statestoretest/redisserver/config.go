// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redisserver

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore"
)

// Settings of the sentinels of the sentinel kinds: a primary is down for
// a sentinel after sentinelDownAfter without a reply, and a failover times
// out after sentinelFailoverTimeout. Deployment.Failover forces one with
// SENTINEL FAILOVER, which needs neither, so they only bound a failover a
// killed primary starts.
const (
	sentinelDownAfter       = 5 * time.Second
	sentinelFailoverTimeout = 10 * time.Second
)

// Options configure a deployment. Every field is for the process flavor:
// the container flavor runs its images as they are, but for the ACL user
// of its cluster and sentinel kinds (containerUser).
type Options struct {
	// TLS serves TLS only (port 0 and tls-port), replication, the
	// cluster bus and a sentinel's links included, with a certificate for
	// localhost, 127.0.0.1 and ::1 from a fresh testkit/pki CA. URLs are
	// rediss://, and Deployment.CAFile and ClientTLS trust the CA. Clients
	// present no certificate (tls-auth-clients no).
	TLS bool
	// Users are ACL users, of the sentinel too. The URLs, the admin
	// connections, a replica's link to its primary and a sentinel's links
	// to its servers authenticate as the first. Without a user named
	// "default", the default user is off, so every client must
	// authenticate.
	Users []User
	// Replica adds a replica of the standalone primary, for a scripted
	// failover (Deployment.Promote). The sentinel kinds always have one,
	// and the cluster kinds one per primary.
	Replica bool
	// Announce, when set, returns the address clients dial for a primary
	// ("host:port"), such as a proxy forwarding to addr. URLs name it,
	// and a cluster node announces it (cluster-announce-ip and
	// cluster-announce-port), so redirects and CLUSTER SLOTS name it too;
	// "" keeps addr. It is called once per primary and start attempt,
	// before the primary starts. A cluster's replicas announce their own
	// addresses, so a promoted replica is reached directly.
	Announce func(node int, addr string) string
	// Config holds extra configuration directives, one per element, such
	// as "maxmemory 64mb". They follow the generated directives, so a
	// single-valued one overrides.
	Config []string
}

// User is one ACL user.
type User struct {
	Name     string
	Password string
	// Rules are the user's ACL rules; default "~*", "&*" and "+@all".
	Rules []string
}

// validate checks o for kind k.
func (o Options) validate(k Kind) error {
	if k.Flavor() == FlavorContainer {
		if o.TLS || len(o.Users) > 0 || o.Replica || o.Announce != nil || len(o.Config) > 0 {
			return fmt.Errorf("redisserver: %s runs its image as it is and takes no Options", k)
		}
		return nil
	}
	switch k.Topology() {
	case statestore.TopologyCluster:
		if o.Replica {
			return fmt.Errorf("redisserver: %s has a replica per primary already", k)
		}
	case statestore.TopologySentinel:
		if o.Replica {
			return fmt.Errorf("redisserver: %s has its replica already", k)
		}
		if o.Announce != nil {
			return fmt.Errorf("redisserver: %s takes no Announce: its sentinel names the servers", k)
		}
	case statestore.TopologyStandalone:
	}
	seen := map[string]bool{}
	for _, u := range o.Users {
		switch {
		case u.Name == "" || strings.ContainsFunc(u.Name, isSpaceOrControl):
			return fmt.Errorf("redisserver: user name %q", u.Name)
		case u.Password == "":
			return fmt.Errorf("redisserver: user %q has no password", u.Name)
		case seen[u.Name]:
			return fmt.Errorf("redisserver: user %q twice", u.Name)
		}
		seen[u.Name] = true
	}
	for _, line := range o.Config {
		if strings.ContainsAny(line, "\r\n") {
			return fmt.Errorf("redisserver: Config directive %q spans lines", line)
		}
	}
	return nil
}

func isSpaceOrControl(r rune) bool { return r <= ' ' || r == 0x7f }

// directive is one configuration line: a name and its arguments.
type directive []string

// tlsFiles are the PEM files a TLS server reads.
type tlsFiles struct {
	ca, cert, key string
}

// nodeConfig describes one redis-server process.
type nodeConfig struct {
	// bind is the address the server listens on; "" is loopback.
	bind    string
	port    int
	busPort int // cluster bus port; 0 outside a cluster
	dir     string
	// base is the deployment's directory, which holds dir and the TLS
	// files, and which a Host shares with a server it runs in a container.
	base     string
	tls      *tlsFiles
	users    []User
	primary  string // "host:port" of the primary of a replica
	announce string // "host:port" a cluster node announces
	// linkAuth gives a primary the replica link credentials too, so it
	// syncs once a sentinel demotes it.
	linkAuth bool
	// sentinel, when set, makes the process a sentinel (redis-server
	// --sentinel) monitoring that primary.
	sentinel *sentinelConfig
	extra    []string
}

// sentinelConfig is what a sentinel monitors.
type sentinelConfig struct {
	masterSet string
	primary   string // "host:port"
}

// listener returns the port or TLS directives of n: TLS replication makes
// replicas, and a sentinel's links to its servers, use TLS too.
func (n nodeConfig) listener() []directive {
	port := strconv.Itoa(n.port)
	if n.tls == nil {
		return []directive{{"port", port}}
	}
	return []directive{
		{"port", "0"},
		{"tls-port", port},
		{"tls-cert-file", n.tls.cert},
		{"tls-key-file", n.tls.key},
		{"tls-ca-cert-file", n.tls.ca},
		{"tls-auth-clients", "no"},
		{"tls-replication", "yes"},
	}
}

// bindAddr returns the address n listens on: bind, or loopback.
func (n nodeConfig) bindAddr() string {
	if n.bind == "" {
		return loopback
	}
	return n.bind
}

// directives returns n's configuration, Config last.
func (n nodeConfig) directives() ([]directive, error) {
	if n.sentinel != nil {
		return n.sentinelDirectives()
	}
	ds := []directive{
		{"bind", n.bindAddr()},
		{"dir", n.dir},
		{"save", ""},
		{"appendonly", "no"},
		// A replica's full sync starts at once instead of after the
		// default 5 s wait for more replicas.
		{"repl-diskless-sync-delay", "0"},
	}
	ds = append(ds, n.listener()...)
	ds = append(ds, userDirectives(n.users)...)
	if n.primary != "" {
		host, p, err := net.SplitHostPort(n.primary)
		if err != nil {
			return nil, fmt.Errorf("redisserver: primary address %q: %w", n.primary, err)
		}
		ds = append(ds, directive{"replicaof", host, p})
	}
	if (n.primary != "" || n.linkAuth) && len(n.users) > 0 {
		ds = append(ds, directive{"masteruser", n.users[0].Name}, directive{"masterauth", n.users[0].Password})
	}
	if n.busPort != 0 {
		ds = append(ds,
			directive{"cluster-enabled", "yes"},
			directive{"cluster-config-file", "nodes.conf"},
			directive{"cluster-port", strconv.Itoa(n.busPort)},
		)
		if n.tls != nil {
			ds = append(ds, directive{"tls-cluster", "yes"})
		}
		if n.announce != "" {
			host, p, err := net.SplitHostPort(n.announce)
			if err != nil {
				return nil, fmt.Errorf("redisserver: announced address %q: %w", n.announce, err)
			}
			ds = append(ds, directive{"cluster-announce-ip", host}, directive{"cluster-announce-port", p})
			if n.tls != nil {
				ds = append(ds, directive{"cluster-announce-tls-port", p})
			}
		}
	}
	return ds, nil
}

// sentinelDirectives returns a sentinel's configuration: its listener and
// users, then the monitor of the primary with a quorum of 1, its timings
// and, with users, the first user's credentials for its links.
func (n nodeConfig) sentinelDirectives() ([]directive, error) {
	host, p, err := net.SplitHostPort(n.sentinel.primary)
	if err != nil {
		return nil, fmt.Errorf("redisserver: monitored address %q: %w", n.sentinel.primary, err)
	}
	ds := []directive{{"bind", n.bindAddr()}, {"dir", n.dir}}
	ds = append(ds, n.listener()...)
	ds = append(ds, userDirectives(n.users)...)
	ds = append(ds, sentinelMonitor(n.sentinel.masterSet, host, p)...)
	if len(n.users) > 0 {
		set := n.sentinel.masterSet
		ds = append(ds, directive{"sentinel", "auth-user", set, n.users[0].Name}, directive{"sentinel", "auth-pass", set, n.users[0].Password})
	}
	return ds, nil
}

// sentinelMonitor returns the directives that make a sentinel monitor the
// primary at host:port as set, with a quorum of 1 and the sentinel
// timings.
func sentinelMonitor(set, host, port string) []directive {
	ms := func(d time.Duration) string { return strconv.FormatInt(d.Milliseconds(), 10) }
	return []directive{
		{"sentinel", "monitor", set, host, port, "1"},
		{"sentinel", "down-after-milliseconds", set, ms(sentinelDownAfter)},
		{"sentinel", "failover-timeout", set, ms(sentinelFailoverTimeout)},
	}
}

// userDirectives turns users into user directives; the default user is
// off unless listed.
func userDirectives(users []User) []directive {
	if len(users) == 0 {
		return nil
	}
	var ds []directive
	listed := false
	for _, u := range users {
		rules := u.Rules
		if len(rules) == 0 {
			rules = []string{"~*", "&*", "+@all"}
		}
		ds = append(ds, append(directive{"user", u.Name, "reset", "on", ">" + u.Password}, rules...))
		listed = listed || u.Name == "default"
	}
	if !listed {
		ds = append(ds, directive{"user", "default", "off"})
	}
	return ds
}

// render writes directives and then the verbatim extra lines as a
// configuration file.
func render(ds []directive, extra []string) []byte {
	var b strings.Builder
	for _, d := range ds {
		for i, a := range d {
			if i > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(quote(a))
		}
		b.WriteByte('\n')
	}
	for _, line := range extra {
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

// quote returns s as one argument of a configuration line: bare when it
// holds only printable ASCII other than space and quotes, else in double
// quotes with backslash escapes, which Redis's argument splitter reads
// back exactly.
func quote(s string) string {
	bare := s != ""
	for i := range len(s) {
		if c := s[i]; c <= ' ' || c >= 0x7f || c == '"' || c == '\'' {
			bare = false
			break
		}
	}
	if bare {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	for i := range len(s) {
		switch c := s[i]; {
		case c == '\\' || c == '"':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c == '\n':
			b.WriteString(`\n`)
		case c == '\r':
			b.WriteString(`\r`)
		case c == '\t':
			b.WriteString(`\t`)
		case c < ' ' || c >= 0x7f:
			_, _ = fmt.Fprintf(&b, `\x%02x`, c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// errNoReplica is returned by Promote without a replica.
var errNoReplica = errors.New("redisserver: no replica to promote")
