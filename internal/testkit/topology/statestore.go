// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package topology

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"slices"
	"sync"

	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/statestore/statestoretest/redisserver"
	"github.com/ravindu-rev/ruralz/internal/statestore/statestoretest/respproxy"
	"github.com/ravindu-rev/ruralz/internal/testkit/faultproxy"
)

// StateStoreUser is the ACL user of the State Stores the harness runs,
// whose password is a canary.
const StateStoreUser = "ruralz"

// StateStoreOptions configure a State Store.
type StateStoreOptions struct {
	// Kind is the redisserver kind and flavor; default redisserver.Process,
	// a standalone redis-server process. A container kind runs its image as
	// it is, without TLS or an ACL user.
	Kind redisserver.Kind
	// Replica adds a replica to a standalone process primary, for the
	// scripted failover of Promote.
	Replica bool
	// Config holds extra redis-server directives, such as
	// "maxmemory-policy noeviction"; process kinds only.
	Config []string
	// RESPProxy puts a counting RESP proxy (respproxy) in front of each
	// Node instead of a plain fault proxy. The proxy parses the commands,
	// so the State Store then runs without TLS. On a process cluster kind
	// each primary also announces a counting proxy of its own, shared by
	// every Node (Proxies), so the commands a cluster's slot map and
	// redirects send to the servers directly cross one too.
	RESPProxy bool
	// Host, when set, runs the servers of a process kind on that host
	// rather than this machine (redisserver.StartOn), as the target chaos
	// profile places its State Store on RH-1S; the Nodes' proxies stay on
	// this machine. A process cluster on a host takes no RESPProxy, whose
	// announced proxies its replicas would reach from the host.
	Host redisserver.Host
}

// StateStore is a State Store of the topology and the proxies its Nodes
// reach it through. Its methods are safe for concurrent use.
type StateStore struct {
	t     *Topology
	name  string
	opts  StateStoreOptions
	dep   *redisserver.Deployment
	caPEM []byte // the servers' CA under TLS

	mu    sync.Mutex
	links []*storeLink
	// announced holds the counting proxy each process cluster primary
	// announces with StateStoreOptions.RESPProxy, by primary.
	announced map[int]*respproxy.Proxy
}

// storeLink is one Node's proxies in front of a State Store.
type storeLink struct {
	node string
	// fault holds one proxy per URL endpoint; resp the same proxies as
	// counting RESP proxies, with StateStoreOptions.RESPProxy.
	fault []*faultproxy.Proxy
	resp  []*respproxy.Proxy
	// url is the State Store URL through the proxies.
	url string
}

// StartStateStore starts the State Store name for t's test, with TLS and
// an ACL user whose password is a canary unless Config.Transport says
// otherwise, on this machine or on StateStoreOptions.Host. Like
// redisserver.Start, it skips the test when the kind cannot run here and
// its flavor is not selected, and fails it on a broken setup. Start State
// Stores before the Nodes that use them: a Node trusts the CAs the harness
// knew when it started.
func (t *Topology) StartStateStore(name string, o StateStoreOptions) *StateStore {
	t.t.Helper()
	if o.Kind == "" {
		o.Kind = redisserver.Process
	}
	t.mu.Lock()
	_, dup := t.stores[name]
	t.mu.Unlock()
	if dup || name == "" {
		t.t.Fatalf("topology: State Store name %q is empty or taken", name)
	}
	s := &StateStore{t: t, name: name, opts: o}
	var ro redisserver.Options
	if o.Kind.Flavor() == redisserver.FlavorProcess {
		ro.TLS = !t.cfg.Transport.CleartextStateStore && !o.RESPProxy
		if !t.cfg.Transport.NoStateStoreUser {
			ro.Users = []redisserver.User{{Name: StateStoreUser, Password: t.Canary()}}
		}
		ro.Replica = o.Replica
		ro.Config = o.Config
		if o.RESPProxy && o.Kind.Topology() == statestore.TopologyCluster {
			ro.Announce = s.announce
		}
	}
	var dep *redisserver.Deployment
	if o.Host != nil {
		dep = redisserver.StartOn(t.t, o.Host, o.Kind, ro)
	} else {
		dep = redisserver.Start(t.t, o.Kind, ro)
	}
	// redisserver stops its servers in cleanups of its own; the teardown
	// must run before them to scan the keyspace.
	t.armTeardown()
	s.dep = dep
	if f := dep.CAFile(); f != "" {
		pem, err := os.ReadFile(f) //nolint:gosec // G304: the CA file redisserver wrote
		if err != nil {
			t.t.Fatalf("topology: State Store %s: %v", name, err)
		}
		s.caPEM = pem
	}
	t.mu.Lock()
	t.stores[name] = s
	t.mu.Unlock()
	if err := t.writeBundle(); err != nil {
		t.t.Fatalf("%v", err)
	}
	return s
}

// stateStore returns the State Store name.
func (t *Topology) stateStore(name string) (*StateStore, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.stores[name]
	return s, ok
}

// announce is the redisserver.Options.Announce of a process cluster with
// StateStoreOptions.RESPProxy: it starts the counting proxy primary node
// announces, forwarding to addr, in place of an earlier start attempt's.
func (s *StateStore) announce(node int, addr string) string {
	p, err := respproxy.Start(s.t.ctx, respproxy.Config{Fault: faultproxy.Config{Upstream: addr}})
	if err != nil {
		s.t.t.Errorf("topology: State Store %s: the proxy primary %d announces: %v", s.name, node, err)
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.announced == nil {
		s.announced = map[int]*respproxy.Proxy{}
	}
	if old := s.announced[node]; old != nil {
		_ = old.Close()
	}
	s.announced[node] = p
	return p.Addr()
}

// Proxies returns the counting proxies the primaries of a process cluster
// announce with StateStoreOptions.RESPProxy, in primary order; none for
// any other State Store. Every command any Node sends the cluster crosses
// one of them.
func (s *StateStore) Proxies() []*respproxy.Proxy {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*respproxy.Proxy, 0, len(s.announced))
	for i := range len(s.announced) {
		if p := s.announced[i]; p != nil {
			out = append(out, p)
		}
	}
	return out
}

// Name returns the State Store's name.
func (s *StateStore) Name() string { return s.name }

// Deployment returns the running servers, for faults from redis commands,
// kills and restarts.
func (s *StateStore) Deployment() *redisserver.Deployment { return s.dep }

// URL returns the State Store URL that reaches the servers directly, with
// the ACL user's credentials.
func (s *StateStore) URL() string { return s.dep.URL() }

// CAPEM returns the servers' CA, carrying a canary, for a Bundle's
// stateStore.tls.caCertificate; nil without TLS.
func (s *StateStore) CAPEM() []byte {
	if s.caPEM == nil {
		return nil
	}
	return s.t.WithCanary(s.caPEM)
}

// link starts n's proxies in front of every endpoint of the State Store
// URL: the server, a cluster's seeds or the sentinels. A cluster's
// redirects and a sentinel's primary name the servers themselves, so a
// proxy covers the seed or sentinel connections there.
func (s *StateStore) link(_ context.Context, n *Node) (*storeLink, error) {
	l := &storeLink{node: n.name}
	proxy := func(addr string) (string, error) {
		fc := faultproxy.Config{Upstream: addr}
		if s.opts.RESPProxy {
			p, err := respproxy.Start(s.t.ctx, respproxy.Config{Fault: fc})
			if err != nil {
				return "", fmt.Errorf("topology: %s proxy for %s: %w", n.name, s.name, err)
			}
			l.resp = append(l.resp, p)
			l.fault = append(l.fault, p.Proxy)
			return p.Addr(), nil
		}
		p, err := faultproxy.Start(s.t.ctx, fc)
		if err != nil {
			return "", fmt.Errorf("topology: %s proxy for %s: %w", n.name, s.name, err)
		}
		l.fault = append(l.fault, p)
		return p.Addr(), nil
	}
	u, err := proxiedURL(s.dep.URL(), proxy)
	if err != nil {
		l.close()
		return nil, fmt.Errorf("topology: State Store %s: %w", s.name, err)
	}
	l.url = u
	s.mu.Lock()
	s.links = append(s.links, l)
	s.mu.Unlock()
	return l, nil
}

// proxiedURL returns the State Store URL raw with its host and each addr
// parameter (a cluster's seeds, a sentinel deployment's sentinels)
// replaced by the address proxy returns for it; the credentials, database
// and master_set stay.
func proxiedURL(raw string, proxy func(addr string) (string, error)) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("URL: %w", err)
	}
	if u.Host, err = proxy(u.Host); err != nil {
		return "", err
	}
	q := u.Query()
	if seeds := q["addr"]; len(seeds) > 0 {
		for i, a := range seeds {
			if seeds[i], err = proxy(a); err != nil {
				return "", err
			}
		}
		q["addr"] = seeds
		u.RawQuery = q.Encode()
	}
	return u.String(), nil
}

// Promote makes the replica the primary (redisserver.Deployment.Promote),
// the scripted failover of a standalone process kind, and switches every
// Node's proxy to it for new connections.
func (s *StateStore) Promote(ctx context.Context) error {
	if err := s.dep.Promote(ctx); err != nil {
		return fmt.Errorf("topology: State Store %s: %w", s.name, err)
	}
	primary := s.dep.Primary().ClientAddr()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range s.links {
		if len(l.fault) > 0 {
			l.fault[0].SetUpstream(primary)
		}
	}
	return nil
}

// close closes a link's proxies.
func (l *storeLink) close() {
	for _, p := range l.resp {
		_ = p.Close()
	}
	for _, p := range l.fault {
		_ = p.Close()
	}
}

// closeLinks closes every Node's proxies and the proxies a cluster's
// primaries announce.
func (s *StateStore) closeLinks() {
	s.mu.Lock()
	links := slices.Clone(s.links)
	announced := slices.Collect(maps.Values(s.announced))
	s.mu.Unlock()
	for _, l := range links {
		l.close()
	}
	for _, p := range announced {
		_ = p.Close()
	}
}

// dumpKeyspace writes every key of every primary and its value, raw, to
// w: strings, hashes, lists, sets, sorted sets and streams, read with the
// commands of their type.
func (s *StateStore) dumpKeyspace(ctx context.Context, w io.Writer) error {
	bw := bufio.NewWriter(w)
	for _, srv := range s.dep.Nodes() {
		cursor := "0"
		for {
			v, err := srv.Do(ctx, "SCAN", cursor, "COUNT", "1000")
			if err != nil {
				return fmt.Errorf("topology: State Store %s: SCAN: %w", s.name, err)
			}
			if len(v.Elems) != 2 {
				return fmt.Errorf("topology: State Store %s: SCAN answered %v", s.name, v)
			}
			for _, k := range v.Elems[1].Elems {
				if err := dumpKey(ctx, srv, k.Str, bw); err != nil {
					return fmt.Errorf("topology: State Store %s: %w", s.name, err)
				}
			}
			if cursor = v.Elems[0].Str; cursor == "0" {
				break
			}
		}
	}
	return bw.Flush()
}

// dumpKey writes one key and its value.
func dumpKey(ctx context.Context, srv *redisserver.Server, key string, w *bufio.Writer) error {
	typ, err := srv.Do(ctx, "TYPE", key)
	if err != nil {
		return err
	}
	var read []string
	switch typ.Str {
	case "string":
		read = []string{"GET", key}
	case "hash":
		read = []string{"HGETALL", key}
	case "list":
		read = []string{"LRANGE", key, "0", "-1"}
	case "set":
		read = []string{"SMEMBERS", key}
	case "zset":
		read = []string{"ZRANGE", key, "0", "-1", "WITHSCORES"}
	case "stream":
		read = []string{"XRANGE", key, "-", "+"}
	default:
		// The key expired since the SCAN ("none"), or holds a module type.
	}
	_, _ = w.WriteString(key + "\n")
	if read == nil {
		return nil
	}
	v, err := srv.Do(ctx, read...)
	if err != nil {
		return err
	}
	writeValue(w, v)
	return nil
}

// writeValue writes every scalar of a reply, raw, one per line.
func writeValue(w *bufio.Writer, v redisserver.Value) {
	if len(v.Elems) > 0 {
		for _, e := range v.Elems {
			writeValue(w, e)
		}
		return
	}
	_, _ = w.WriteString(v.Str + "\n")
}
