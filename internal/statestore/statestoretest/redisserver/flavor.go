// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redisserver

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/statestore"
)

// The environment of the State Store flavors
// (docs/engineering/03-testing-and-quality-strategy.md "Integration
// tests").
const (
	// EnvStateStore selects the flavor the State Store suites run
	// against: "process" or "container".
	EnvStateStore = "RURALZ_TEST_STATESTORE"
	// EnvRedisServer names the redis-server binary of the process flavor;
	// without it, redis-server is looked up in PATH.
	EnvRedisServer = "RURALZ_TEST_REDIS_SERVER"
)

// Flavor is how State Store servers run.
type Flavor uint8

// Flavors.
const (
	// FlavorNone is an unset EnvStateStore: the suites skip.
	FlavorNone Flavor = iota
	// FlavorProcess runs local redis-server processes.
	FlavorProcess
	// FlavorContainer runs the images of images.go through
	// testcontainers-go, on the linux/amd64 container leg.
	FlavorContainer
)

// String returns the EnvStateStore value of f ("" for FlavorNone).
func (f Flavor) String() string {
	switch f {
	case FlavorProcess:
		return "process"
	case FlavorContainer:
		return "container"
	default:
		return ""
	}
}

// ParseFlavor parses an EnvStateStore value; "" is FlavorNone.
func ParseFlavor(s string) (Flavor, error) {
	switch s {
	case "":
		return FlavorNone, nil
	case "process":
		return FlavorProcess, nil
	case "container":
		return FlavorContainer, nil
	}
	return FlavorNone, fmt.Errorf("redisserver: %s=%q, want process or container", EnvStateStore, s)
}

// Kinds returns the kinds of f, standalone first.
func (f Flavor) Kinds() []Kind {
	switch f {
	case FlavorProcess:
		return []Kind{Process, ProcessCluster}
	case FlavorContainer:
		return []Kind{Redis8, Valkey9, Dragonfly, Redis8Cluster, Valkey9Cluster}
	default:
		return nil
	}
}

// Kind is a State Store deployment the suites run against.
type Kind string

// Kinds of deployment: the rows of the Integration tests dependency
// table.
const (
	// Process is a standalone local redis-server.
	Process Kind = "redis-server"
	// ProcessCluster is a three-primary cluster of local redis-server
	// processes.
	ProcessCluster Kind = "redis-server-cluster"
	// Redis8 is ImageRedis8 through testcontainers-go modules/redis.
	Redis8 Kind = "redis-8"
	// Valkey9 is ImageValkey9 through modules/valkey.
	Valkey9 Kind = "valkey-9"
	// Dragonfly is ImageDragonfly as a generic container.
	Dragonfly Kind = "dragonfly"
	// Redis8Cluster is a three-primary cluster of ImageRedis8 generic
	// containers.
	Redis8Cluster Kind = "redis-8-cluster"
	// Valkey9Cluster is a three-primary cluster of ImageValkey9 generic
	// containers.
	Valkey9Cluster Kind = "valkey-9-cluster"
)

// Flavor returns the flavor k runs in.
func (k Kind) Flavor() Flavor {
	switch k {
	case Process, ProcessCluster:
		return FlavorProcess
	case Redis8, Valkey9, Dragonfly, Redis8Cluster, Valkey9Cluster:
		return FlavorContainer
	default:
		return FlavorNone
	}
}

// Topology returns the shape of k, as stateStore.topology names it.
func (k Kind) Topology() statestore.Topology {
	switch k {
	case ProcessCluster, Redis8Cluster, Valkey9Cluster:
		return statestore.TopologyCluster
	default:
		return statestore.TopologyStandalone
	}
}

// Image returns the image k runs; "" for the process flavor.
func (k Kind) Image() string {
	switch k {
	case Redis8, Redis8Cluster:
		return ImageRedis8
	case Valkey9, Valkey9Cluster:
		return ImageValkey9
	case Dragonfly:
		return ImageDragonfly
	default:
		return ""
	}
}

// Kinds returns the kinds EnvStateStore selects. It skips tb when the
// variable is unset and fails it when the value is invalid.
func Kinds(tb testing.TB) []Kind {
	tb.Helper()
	f, err := ParseFlavor(os.Getenv(EnvStateStore))
	if err != nil {
		tb.Fatal(err)
	}
	if f == FlavorNone {
		tb.Skipf("redisserver: %s is unset; set it to process (a local redis-server) or container (Docker) to run the State Store suites", EnvStateStore)
	}
	return f.Kinds()
}

// host is what the gates read from the machine.
type host struct {
	getenv   func(string) string
	lookPath func(string) (string, error)
	goos     string
	// docker returns nil when a Docker daemon answers.
	docker func(context.Context) error
}

func realHost() host {
	return host{getenv: os.Getenv, lookPath: exec.LookPath, goos: runtime.GOOS, docker: dockerHealth}
}

// redisServer returns the redis-server of the process flavor, or missing,
// why there is none. An EnvRedisServer naming no executable is an error.
func (h host) redisServer() (path, missing string, err error) {
	if p := h.getenv(EnvRedisServer); p != "" {
		found, err := h.lookPath(p)
		if err != nil {
			return "", "", fmt.Errorf("redisserver: %s=%q: %w", EnvRedisServer, p, err)
		}
		return found, "", nil
	}
	if found, err := h.lookPath("redis-server"); err == nil {
		return found, "", nil
	}
	return "", fmt.Sprintf("no redis-server in PATH and %s is unset", EnvRedisServer), nil
}

// containerMissing returns why container kind k cannot run on this host,
// or "": a cluster needs a Linux host, and every kind a Docker daemon.
func (h host) containerMissing(ctx context.Context, k Kind) string {
	if k.Topology() == statestore.TopologyCluster && h.goos != "linux" {
		return "cluster members announce container IPs, which only a Linux host reaches"
	}
	if err := h.docker(ctx); err != nil {
		return fmt.Sprintf("Docker is unavailable: %v", err)
	}
	return ""
}

// gate returns the redis-server of process kind k (path), or why k
// cannot run here: an error when EnvStateStore selects k's flavor or is
// invalid, else a skip with the reason. A process kind runs wherever a
// redis-server is found, selected or not; a container kind runs only
// when selected.
func (h host) gate(ctx context.Context, k Kind) (path, skip string, err error) {
	selected, err := ParseFlavor(h.getenv(EnvStateStore))
	if err != nil {
		return "", "", err
	}
	var why string
	switch k.Flavor() {
	case FlavorProcess:
		if path, why, err = h.redisServer(); err != nil || why == "" {
			return path, "", err
		}
	case FlavorContainer:
		if selected != FlavorContainer {
			return "", fmt.Sprintf("the container flavor runs only with %s=container", EnvStateStore), nil
		}
		if why = h.containerMissing(ctx, k); why == "" {
			return "", "", nil
		}
	default:
		return "", "", fmt.Errorf("redisserver: unknown kind %q", string(k))
	}
	if selected == k.Flavor() {
		return "", "", fmt.Errorf("redisserver: %s=%s selects %s, which cannot run here: %s", EnvStateStore, selected, k, why)
	}
	return "", why, nil
}
