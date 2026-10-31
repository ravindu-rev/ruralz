// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redisserver

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tclog "github.com/testcontainers/testcontainers-go/log"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
	tcvalkey "github.com/testcontainers/testcontainers-go/modules/valkey"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/ravindu-rev/ruralz/internal/statestore"
)

const (
	// containerTimeout bounds starting a container deployment, image
	// pulls and cluster formation included; it only ends a broken test.
	containerTimeout = 5 * time.Minute
	// respPort is the port every image serves RESP on.
	respPort = "6379/tcp"
	// clusterPort and clusterBusPort are a cluster container's ports.
	clusterPort, clusterBusPort = 6379, 16379
	// memberPort is the port a sentinel kind's server container serves,
	// and sentinelPort its sentinel's, both reached at the container IP.
	memberPort, sentinelPort = 6379, 26379
	// sentinelConfFile is the configuration file of a sentinel container,
	// in the image's working directory, which the entrypoint gives the
	// redis user, so the sentinel can rewrite it.
	sentinelConfFile = "/data/sentinel.conf"
)

// dockerHealth returns nil when a Docker daemon answers.
func dockerHealth(ctx context.Context) (err error) {
	// The provider lookup panics on some hosts without Docker.
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("docker provider: %v", r)
		}
	}()
	p, err := testcontainers.ProviderDocker.GetProvider()
	if err != nil {
		return fmt.Errorf("docker provider: %w", err)
	}
	if err := p.Health(ctx); err != nil {
		return fmt.Errorf("docker health: %w", err)
	}
	return nil
}

// containerUser is the ACL user of the cluster and sentinel container
// kinds, which clients reach at container IPs: the driver refuses a
// non-loopback State Store URL without credentials (RZ-CFG-042).
func containerUser() User {
	return User{Name: "ruralz", Password: "ruralz-test-container"} //nolint:gosec // G101: a test container's password
}

// containerUserArgs returns the redis-server arguments that add the
// user beside the default user, which replication and the sentinel's
// links keep using.
func containerUserArgs(u User) []string {
	return []string{"--user", u.Name, "reset", "on", ">" + u.Password, "~*", "&*", "+@all"}
}

// startContainer starts a container-flavor deployment.
func startContainer(tb testing.TB, k Kind) (*Deployment, error) {
	ctx, cancel := context.WithTimeout(tb.Context(), containerTimeout)
	defer cancel()
	d := &Deployment{kind: k, acc: access{scheme: "redis"}}
	if k.Topology() != statestore.TopologyStandalone {
		u := containerUser()
		d.acc.user, d.acc.dial.user, d.acc.dial.password = &u, u.Name, u.Password
	}
	logger := testcontainers.WithLogger(tclog.TestLogger(tb))
	switch k.Topology() {
	case statestore.TopologyCluster:
		if err := startContainerCluster(ctx, tb, d, logger); err != nil {
			return nil, err
		}
		return d, nil
	case statestore.TopologySentinel:
		if err := startContainerSentinel(ctx, tb, d, logger); err != nil {
			return nil, err
		}
		return d, nil
	case statestore.TopologyStandalone:
	}
	var ctr testcontainers.Container
	var err error
	switch k {
	case Redis8:
		c, e := tcredis.Run(ctx, k.Image(), logger)
		if c != nil {
			ctr = c
		}
		err = e
	case Valkey9:
		c, e := tcvalkey.Run(ctx, k.Image(), logger)
		if c != nil {
			ctr = c
		}
		err = e
	default:
		c, e := testcontainers.Run(ctx, k.Image(), logger, testcontainers.WithExposedPorts(respPort),
			testcontainers.WithWaitStrategy(wait.ForListeningPort(respPort)))
		if c != nil {
			ctr = c
		}
		err = e
	}
	if ctr != nil {
		tb.Cleanup(func() { terminate(tb, ctr) })
	}
	if err != nil {
		return nil, fmt.Errorf("redisserver: run %s: %w", k.Image(), err)
	}
	addr, err := ctr.PortEndpoint(ctx, respPort, "")
	if err != nil {
		return nil, fmt.Errorf("redisserver: %s endpoint: %w", k, err)
	}
	s := &Server{name: string(k), addr: addr, client: addr, acc: d.acc, ctr: ctr}
	if err := waitServing(ctx, s, k); err != nil {
		return nil, err
	}
	d.nodes = []*Server{s}
	return d, nil
}

// startContainerCluster starts three cluster-enabled containers and forms
// them into a cluster over their container IPs, which members announce
// and only a Linux host reaches, then one more container per primary that
// joins as its replica.
func startContainerCluster(ctx context.Context, tb testing.TB, d *Deployment, logger testcontainers.LoggerOption) error {
	cmd := append([]string{d.kind.server(), "--cluster-enabled", "yes", "--protected-mode", "no", "--save", "", "--appendonly", "no"},
		containerUserArgs(*d.acc.user)...)
	run := func(name string) (clusterNode, error) {
		ctr, err := testcontainers.Run(ctx, d.kind.Image(), logger,
			testcontainers.WithCmd(cmd...),
			testcontainers.WithWaitStrategy(wait.ForLog("Ready to accept connections")))
		if ctr != nil {
			tb.Cleanup(func() { terminate(tb, ctr) })
		}
		if err != nil {
			return clusterNode{}, fmt.Errorf("redisserver: run %s: %w", d.kind.Image(), err)
		}
		ip, err := ctr.ContainerIP(ctx)
		if err != nil {
			return clusterNode{}, fmt.Errorf("redisserver: %s container IP: %w", name, err)
		}
		if ip == "" {
			return clusterNode{}, fmt.Errorf("redisserver: %s container has no IP", name)
		}
		addr := net.JoinHostPort(ip, strconv.Itoa(clusterPort))
		s := &Server{name: name, addr: addr, client: addr, acc: d.acc, ctr: ctr}
		if err := waitServing(ctx, s, d.kind); err != nil {
			return clusterNode{}, err
		}
		return clusterNode{srv: s, host: ip, port: clusterPort, busPort: clusterBusPort}, nil
	}
	var nodes, replicas []clusterNode
	for i := range clusterPrimaries {
		n, err := run(fmt.Sprintf("%s-%d", d.kind, i))
		if err != nil {
			return err
		}
		d.nodes = append(d.nodes, n.srv)
		nodes = append(nodes, n)
	}
	if err := bootstrapCluster(ctx, nodes); err != nil {
		return err
	}
	for i := range clusterPrimaries {
		r, err := run(fmt.Sprintf("%s-%d-replica", d.kind, i))
		if err != nil {
			return err
		}
		d.shardReplicas = append(d.shardReplicas, r.srv)
		replicas = append(replicas, r)
	}
	return attachReplicas(ctx, nodes, replicas)
}

// startContainerSentinel starts a primary, its replica and a sentinel
// monitoring them as MasterSet, generic containers of the kind's image and
// server binary,
// over their container IPs, which the sentinel names and only a Linux host
// reaches, and waits until the sentinel lists the replica as healthy.
func startContainerSentinel(ctx context.Context, tb testing.TB, d *Deployment, logger testcontainers.LoggerOption) error {
	run := func(name string, port int, ready string, opts ...testcontainers.ContainerCustomizer) (*Server, string, error) {
		opts = append([]testcontainers.ContainerCustomizer{logger, testcontainers.WithWaitStrategy(wait.ForLog(ready))}, opts...)
		ctr, err := testcontainers.Run(ctx, d.kind.Image(), opts...)
		if ctr != nil {
			tb.Cleanup(func() { terminate(tb, ctr) })
		}
		if err != nil {
			return nil, "", fmt.Errorf("redisserver: run %s: %w", d.kind.Image(), err)
		}
		ip, err := ctr.ContainerIP(ctx)
		if err != nil {
			return nil, "", fmt.Errorf("redisserver: %s container IP: %w", name, err)
		}
		if ip == "" {
			return nil, "", fmt.Errorf("redisserver: %s container has no IP", name)
		}
		addr := net.JoinHostPort(ip, strconv.Itoa(port))
		s := &Server{name: name, addr: addr, client: addr, acc: d.acc, ctr: ctr}
		return s, ip, waitServing(ctx, s, d.kind)
	}
	server := append([]string{d.kind.server(), "--protected-mode", "no", "--save", "", "--appendonly", "no"}, containerUserArgs(*d.acc.user)...)
	name := string(d.kind)
	p, pip, err := run(name+"-primary", memberPort, "Ready to accept connections", testcontainers.WithCmd(server...))
	if err != nil {
		return err
	}
	r, _, err := run(name+"-replica", memberPort, "Ready to accept connections",
		testcontainers.WithCmd(append(server, "--replicaof", pip, strconv.Itoa(memberPort))...))
	if err != nil {
		return err
	}
	if err := waitReplica(ctx, r); err != nil {
		return err
	}
	s, _, err := run(name+"-sentinel", sentinelPort, "Sentinel ID is",
		testcontainers.WithFiles(testcontainers.ContainerFile{
			Reader: bytes.NewReader(containerSentinelConfig(pip, *d.acc.user)), ContainerFilePath: sentinelConfFile, FileMode: 0o666,
		}),
		testcontainers.WithCmd(d.kind.server(), sentinelConfFile, "--sentinel"))
	if err != nil {
		return err
	}
	if err := waitSentinel(ctx, s, MasterSet, p, r); err != nil {
		return err
	}
	d.nodes, d.replica, d.sentinels, d.masterSet = []*Server{p}, r, []*Server{s}, MasterSet
	return nil
}

// containerSentinelConfig returns the configuration file of a sentinel
// container monitoring the primary at host: protected mode off, as the
// test process connects from outside the container, the sentinel timings,
// and u beside the default user, as the admin connections authenticate
// as u.
func containerSentinelConfig(host string, u User) []byte {
	ds := []directive{{"port", strconv.Itoa(sentinelPort)}, {"protected-mode", "no"}}
	ds = append(ds, sentinelMonitor(MasterSet, host, strconv.Itoa(memberPort))...)
	ds = append(ds, directive{"user", u.Name, "reset", "on", ">" + u.Password, "~*", "&*", "+@all"})
	return render(ds, nil)
}

// terminate stops and removes a container at the end of its test.
func terminate(tb testing.TB, ctr testcontainers.Container) {
	if err := testcontainers.TerminateContainer(ctr); err != nil {
		tb.Errorf("redisserver: %v", err)
	}
}
