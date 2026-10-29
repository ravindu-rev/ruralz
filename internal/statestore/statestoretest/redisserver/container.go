// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redisserver

import (
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

// startContainer starts a container-flavor deployment.
func startContainer(tb testing.TB, k Kind) (*Deployment, error) {
	ctx, cancel := context.WithTimeout(tb.Context(), containerTimeout)
	defer cancel()
	d := &Deployment{kind: k, acc: access{scheme: "redis"}}
	logger := testcontainers.WithLogger(tclog.TestLogger(tb))
	if k.Topology() == statestore.TopologyCluster {
		if err := startContainerCluster(ctx, tb, d, logger); err != nil {
			return nil, err
		}
		return d, nil
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
// and only a Linux host reaches.
func startContainerCluster(ctx context.Context, tb testing.TB, d *Deployment, logger testcontainers.LoggerOption) error {
	server := "redis-server"
	if d.kind == Valkey9Cluster {
		server = "valkey-server"
	}
	var nodes []clusterNode
	for i := range 3 {
		ctr, err := testcontainers.Run(ctx, d.kind.Image(), logger,
			testcontainers.WithCmd(server, "--cluster-enabled", "yes", "--protected-mode", "no", "--save", "", "--appendonly", "no"),
			testcontainers.WithWaitStrategy(wait.ForLog("Ready to accept connections")))
		if ctr != nil {
			tb.Cleanup(func() { terminate(tb, ctr) })
		}
		if err != nil {
			return fmt.Errorf("redisserver: run %s: %w", d.kind.Image(), err)
		}
		ip, err := ctr.ContainerIP(ctx)
		if err != nil {
			return fmt.Errorf("redisserver: %s container IP: %w", d.kind, err)
		}
		if ip == "" {
			return fmt.Errorf("redisserver: %s container has no IP", d.kind)
		}
		addr := net.JoinHostPort(ip, strconv.Itoa(clusterPort))
		s := &Server{name: fmt.Sprintf("%s-%d", d.kind, i), addr: addr, client: addr, acc: d.acc, ctr: ctr}
		if err := waitServing(ctx, s, d.kind); err != nil {
			return err
		}
		d.nodes = append(d.nodes, s)
		nodes = append(nodes, clusterNode{srv: s, host: ip, port: clusterPort, busPort: clusterBusPort})
	}
	return bootstrapCluster(ctx, nodes)
}

// terminate stops and removes a container at the end of its test.
func terminate(tb testing.TB, ctr testcontainers.Container) {
	if err := testcontainers.TerminateContainer(ctr); err != nil {
		tb.Errorf("redisserver: %v", err)
	}
}
