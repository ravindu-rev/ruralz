// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redis

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"slices"
	"strings"

	"github.com/redis/rueidis"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/statestore"
)

// Codes of a resolved stateStore.url a Node refuses
// (docs/architecture/02-configuration-model.md "Gateway" and "Error
// codes").
const (
	// codeURL rejects a URL that does not parse or does not fit its
	// topology.
	codeURL = "RZ-CFG-026"
	// codeTransport rejects a URL the transport rules refuse: a refused
	// scheme or parameter, conflicting parameters, a cleartext scheme
	// beside the connection's TLS block, or no credentials outside loopback
	// (docs/architecture/08-security-and-identity.md "Transport
	// security").
	codeTransport = "RZ-CFG-042"
)

// Configuration errors of Open, each wrapped in an errcode.Error. None
// carries the URL, which may hold credentials.
var (
	// ErrWrongDriver reports a configuration naming another driver
	// (RZ-CFG-026).
	ErrWrongDriver = errors.New("redis: the configuration names another State Store driver")
	// ErrURL reports a URL that does not parse (RZ-CFG-026).
	ErrURL = errors.New("redis: stateStore.url does not parse")
	// ErrTopology reports a URL that does not fit stateStore.topology
	// (RZ-CFG-026): addr under standalone, a database under cluster,
	// master_set outside sentinel or a sentinel URL without one.
	ErrTopology = errors.New("redis: stateStore.url does not fit stateStore.topology")
	// ErrScheme reports a scheme other than redis, rediss, valkey or
	// valkeys (RZ-CFG-042).
	ErrScheme = errors.New("redis: stateStore.url scheme is not redis, rediss, valkey or valkeys")
	// ErrParameter reports a URL parameter rueidis does not read, whose
	// name the error never repeats, or one that would break a design rule
	// (RZ-CFG-042): skip_verify, client_cache other than 0, protocol
	// other than 3, max_retries other than 0.
	ErrParameter = errors.New("redis: stateStore.url sets a parameter Nodes refuse")
	// ErrConflict reports parameters that contradict each other or the
	// configuration (RZ-CFG-042): one given twice, a database in both the
	// path and db, or a cleartext scheme beside the connection's TLS
	// block.
	ErrConflict = errors.New("redis: stateStore.url has conflicting parameters")
	// ErrCredentials reports a URL naming a non-loopback host without a
	// user name or a password (TB-4, RZ-CFG-042).
	ErrCredentials = errors.New("redis: a non-loopback stateStore.url carries no credentials")
)

// singleValued are the URL parameters rueidis reads once: a second value
// would be silently ignored.
func singleValued() []string {
	return []string{"db", "dial_timeout", "write_timeout", "skip_verify", "protocol", "client_cache", "max_retries", "client_name", "master_set"}
}

// known reports a parameter rueidis.ParseURL reads: the single-valued
// ones and addr. rueidis ignores any other, so a misspelled one would pass
// unnoticed.
func known(p string) bool { return p == "addr" || slices.Contains(singleValued(), p) }

// endpoint is a validated stateStore.url.
type endpoint struct {
	// opt is rueidis.ParseURL's result before the driver's overrides:
	// InitAddress (the URL host, then the addr seeds), credentials,
	// SelectDB, Dialer.Timeout, ConnWriteTimeout, ClientName and, for
	// sentinel, Sentinel.MasterSet.
	opt rueidis.ClientOption
	// tls is true for rediss:// and valkeys://.
	tls bool
	// unauthenticated is true for a loopback URL without credentials.
	unauthenticated bool
}

// refused returns why a Node refuses a parameter of q, or "": a parameter
// rueidis does not read, unnamed since its name comes from the URL, or a
// known one that would break a design rule, named.
func refused(q url.Values) string {
	for p := range q {
		if !known(p) {
			return "a parameter rueidis does not read"
		}
	}
	switch {
	case q.Has("skip_verify"):
		// Every TLS hop verifies the server certificate; no field disables it.
		return "parameter skip_verify"
	case q.Has("client_cache") && q.Get("client_cache") != "0":
		// Client-side caching stays off.
		return "parameter client_cache"
	case q.Has("protocol") && q.Get("protocol") != "3":
		// The State client speaks RESP3.
		return "parameter protocol"
	case q.Has("max_retries") && q.Get("max_retries") != "0":
		// No call makes a second round trip (pack section 8.7 rule 1).
		return "parameter max_retries"
	default:
		return ""
	}
}

// conflict returns the reason two parts of u contradict each other, or "".
func conflict(u *url.URL, q url.Values) string {
	for _, p := range singleValued() {
		if len(q[p]) > 1 {
			return "parameter " + p + " given twice"
		}
	}
	if q.Has("db") && strings.Trim(u.Path, "/") != "" {
		return "a database in both the path and db"
	}
	return ""
}

// configErr wraps err with its code (codeOf) and a fixed detail.
func configErr(err error, detail string) error {
	if detail != "" {
		err = fmt.Errorf("%w: %s", err, detail)
	}
	return errcode.Wrap(codeOf(err), err)
}

// codeOf returns the code of a configuration error: RZ-CFG-042 for the
// transport rules, else RZ-CFG-026.
func codeOf(err error) string {
	for _, t := range []error{ErrScheme, ErrParameter, ErrConflict, ErrCredentials} {
		if errors.Is(err, t) {
			return codeTransport
		}
	}
	return codeURL
}

// parseURL validates raw for topology; withTLS reports the connection's TLS
// block, which needs a TLS scheme. Errors never contain raw.
func parseURL(raw string, topology statestore.Topology, withTLS bool) (endpoint, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return endpoint{}, configErr(ErrURL, "")
	}
	var ep endpoint
	switch strings.ToLower(u.Scheme) {
	case "rediss", "valkeys":
		ep.tls = true
	case "redis", "valkey":
	default:
		return endpoint{}, configErr(ErrScheme, "")
	}
	q := u.Query()
	if why := refused(q); why != "" {
		return endpoint{}, configErr(ErrParameter, why)
	}
	if c := conflict(u, q); c != "" {
		return endpoint{}, configErr(ErrConflict, c)
	}
	if withTLS && !ep.tls {
		return endpoint{}, configErr(ErrConflict, "a TLS block beside a cleartext scheme")
	}
	ep.opt, err = rueidis.ParseURL(raw)
	if err != nil {
		return endpoint{}, configErr(ErrURL, "")
	}
	switch topology {
	case statestore.TopologyCluster:
		if q.Has("master_set") {
			return endpoint{}, configErr(ErrTopology, "master_set under cluster")
		}
		if ep.opt.SelectDB != 0 {
			return endpoint{}, configErr(ErrTopology, "a cluster serves database 0 only")
		}
	case statestore.TopologySentinel:
		if ep.opt.Sentinel.MasterSet == "" {
			return endpoint{}, configErr(ErrTopology, "sentinel needs master_set")
		}
	default:
		if q.Has("master_set") {
			return endpoint{}, configErr(ErrTopology, "master_set under standalone")
		}
		if q.Has("addr") {
			return endpoint{}, configErr(ErrTopology, "addr under standalone")
		}
	}
	loopback := true
	for _, a := range ep.opt.InitAddress {
		host, _, err := net.SplitHostPort(a)
		if err != nil || host == "" {
			return endpoint{}, configErr(ErrURL, "")
		}
		loopback = loopback && isLoopback(host)
	}
	if ep.opt.Username == "" && ep.opt.Password == "" {
		if !loopback {
			return endpoint{}, configErr(ErrCredentials, "")
		}
		ep.unauthenticated = true
	}
	return ep, nil
}

// isLoopback reports one of the loopback literals the secret resolver's
// URL check accepts: localhost (any case), an IPv4 address in 127.0.0.0/8
// or ::1. An IPv4-mapped IPv6 address is not one, and DNS never decides.
func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	a, err := netip.ParseAddr(host)
	return err == nil && !a.Is4In6() && a.IsLoopback()
}
