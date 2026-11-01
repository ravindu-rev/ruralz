// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redisserver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/statestore"
)

// Host is a machine other than the test's own that runs the servers of a
// process-flavor deployment (StartOn), as the target chaos profile places
// its State Store on RH-1S, the benchmark lab's State Store host
// (docs/engineering/03-testing-and-quality-strategy.md "Scale profiles"):
// test/chaos implements it over the benchmark harness's inventory and
// test/bench/labhost. Its paths are absolute, slash-separated paths of the
// host.
type Host interface {
	// Addr returns the host's IP address, which the servers bind and
	// announce, and the test and the clients dial.
	Addr() string
	// Names returns the host's DNS names, which the servers' certificate
	// covers beside Addr and loopback.
	Names() []string
	// Port returns a TCP port of Addr that was free on the host a moment
	// ago.
	Port(ctx context.Context) (int, error)
	// MkdirTemp creates a fresh directory on the host in dir, the host's
	// working directory when dir is empty, named after pattern as
	// os.MkdirTemp names it, and returns its path.
	MkdirTemp(ctx context.Context, dir, pattern string) (string, error)
	// WriteFile writes data to the file name on the host, mode 0600.
	WriteFile(ctx context.Context, name string, data []byte) error
	// RemoveAll removes dir and everything under it.
	RemoveAll(ctx context.Context, dir string) error
	// Start starts s on the host and returns once it wrote a line
	// matching s.Ready and accepts TCP connections on s.Port of Addr; the
	// error of a server that exits before, as one whose port another
	// process took, carries the end of its output.
	Start(ctx context.Context, s HostServer) (HostProcess, error)
}

// HostServer is one server a Host starts.
type HostServer struct {
	// Name names the server, such as "redis-server-cluster-0".
	Name string
	// Dir is the deployment directory, which holds every file of the
	// server and which a server in a container shares.
	Dir string
	// Port is the server's TCP port of the host's address.
	Port int
	// Args follow the host's server command: the server's configuration
	// file and, for a sentinel, --sentinel.
	Args []string
	// Ready is a regular expression matching the line the server writes
	// once it serves: a port accepting connections alone could be another
	// process's.
	Ready string
}

// HostProcess is a server a Host started.
type HostProcess interface {
	// Signal sends sig to the server process, such as SIGSTOP and SIGCONT
	// for the frozen State Store fault; signaling an exited server fails.
	Signal(sig os.Signal) error
	// Kill sends SIGKILL to the server process and waits until it exited;
	// killing an exited server is not an error.
	Kill(ctx context.Context) error
	// Close ends the server if it still runs and releases what the Host
	// holds for it.
	Close() error
	// Output returns the end of what the server wrote.
	Output() string
}

// StartOn starts a deployment of process kind k on h for tb; it stops,
// and its directory on h is removed, when tb ends. Every Options field
// applies as on this machine, but Announce for a cluster, whose replicas
// would replicate through the announced addresses from h. StartOn fails
// tb on another kind, invalid Options, an address of h that is not an IP
// address, or a failed start; h decides the server command, so no
// redis-server is looked up here and nothing skips.
func StartOn(tb testing.TB, h Host, k Kind, o Options) *Deployment {
	tb.Helper()
	d, err := startOn(tb, h, k, o)
	if err != nil {
		tb.Fatalf("redisserver: start %s on %s: %v", k, h.Addr(), err)
	}
	return d
}

func startOn(tb testing.TB, h Host, k Kind, o Options) (*Deployment, error) {
	if k.Flavor() != FlavorProcess {
		return nil, fmt.Errorf("redisserver: %s is not a process kind, the kinds a Host runs", k)
	}
	if err := o.validate(k); err != nil {
		return nil, err
	}
	if o.Announce != nil && k.Topology() == statestore.TopologyCluster {
		return nil, errors.New("redisserver: a cluster on a Host takes no Announce: its replicas would reach their primaries at the announced addresses from the host")
	}
	if net.ParseIP(h.Addr()) == nil {
		return nil, fmt.Errorf("redisserver: the host's address %q is not an IP address", h.Addr())
	}
	return startProcess(&launcher{tb: tb, host: h, ports: h.Port}, k, o)
}
