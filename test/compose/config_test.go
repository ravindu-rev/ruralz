// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build compose

package compose

import (
	"context"
	"encoding/json"
	"os/exec"
	"slices"
	"testing"
)

// composeConfig is the part of docker compose config --format json the
// suite checks: compose's own reading of a project file.
type composeConfig struct {
	Services map[string]struct {
		Image           string   `json:"image"`
		User            string   `json:"user"`
		ReadOnly        bool     `json:"read_only"`
		CapDrop         []string `json:"cap_drop"`
		SecurityOpt     []string `json:"security_opt"`
		StopGracePeriod string   `json:"stop_grace_period"`
		NetworkMode     string   `json:"network_mode"`
		Volumes         []struct {
			Type     string `json:"type"`
			Source   string `json:"source"`
			Target   string `json:"target"`
			ReadOnly bool   `json:"read_only"`
		} `json:"volumes"`
		Networks map[string]*struct {
			Aliases []string `json:"aliases"`
		} `json:"networks"`
	} `json:"services"`
	Networks map[string]struct {
		Internal bool `json:"internal"`
	} `json:"networks"`
}

// TestComposeConfig reads every project file with docker compose config,
// which needs the compose plugin but no daemon, so compose itself
// validates the files and reads the hardening of every ruralzd service as
// the untagged tests do.
func TestComposeConfig(t *testing.T) {
	docker, err := exec.LookPath("docker")
	if err != nil {
		t.Skipf("no docker CLI: %v", err)
	}
	if out, err := exec.CommandContext(t.Context(), docker, "compose", "version").CombinedOutput(); err != nil { //nolint:gosec // G204: the docker CLI with fixed arguments
		if theSuite != nil {
			t.Fatalf("docker compose version: %v\n%s", err, out)
		}
		t.Skipf("no docker compose plugin: %v", err)
	}
	s := &suite{docker: docker, root: repoRoot(t)}
	env := []string{
		varImage + "=ruralzd:under-test", varOldImage + "=ruralzd:previous", varMockImage + "=ruralz-compose-mock:test",
		varRun + "=" + t.TempDir(), varValkeyImage + "=valkey:test", varValkeyPassword + "=password",
		varCollectorImage + "=ruralz-compose-collector:test",
		varUID + "=1001", varGID + "=1001",
	}
	for _, file := range composeFiles() {
		t.Run(file, func(t *testing.T) {
			out, err := s.dockerCmd(context.WithoutCancel(t.Context()), env, "compose", "--project-name", "rzc-config", "--file", suitePath(s.root, file),
				"--project-directory", t.TempDir(), "--profile", svcProbe, "config", "--format", "json")
			if err != nil {
				t.Fatal(err)
			}
			var cfg composeConfig
			if err := json.Unmarshal(out, &cfg); err != nil {
				t.Fatalf("docker compose config of %s: %v", file, err)
			}
			n := 0
			for name, svc := range cfg.Services {
				if svc.Image != "ruralzd:under-test" && svc.Image != "ruralzd:previous" {
					continue
				}
				n++
				if svc.User != nodeUser || !svc.ReadOnly || !slices.Equal(svc.CapDrop, []string{"ALL"}) ||
					!slices.Contains(svc.SecurityOpt, noNewPrivileges) || svc.StopGracePeriod != stopGracePeriod {
					t.Errorf("%s %s as compose reads it: user %q, read_only %v, cap_drop %v, security_opt %v, stop_grace_period %q",
						file, name, svc.User, svc.ReadOnly, svc.CapDrop, svc.SecurityOpt, svc.StopGracePeriod)
				}
				data := false
				for _, v := range svc.Volumes {
					switch {
					case v.Target == nodeDataDir:
						data = v.Type == "volume" && v.Source == dataVolume
					case v.Type == "bind" && !v.ReadOnly:
						t.Errorf("%s %s: bind mount %s is writable", file, name, v.Target)
					}
				}
				if !data {
					t.Errorf("%s %s: volumes %+v, want the named volume %s at %s", file, name, svc.Volumes, dataVolume, nodeDataDir)
				}
			}
			if n == 0 {
				t.Errorf("compose reads no ruralzd service in %s", file)
			}
			switch file {
			case airgapFile:
				if !cfg.Networks["airgap"].Internal {
					t.Errorf("compose reads network airgap as %+v, want internal", cfg.Networks["airgap"])
				}
			case restFile:
				for svc, alias := range map[string]string{svcIDP: idpHost, svcUpstream: "orders.shop.internal"} {
					net := cfg.Services[svc].Networks["default"]
					if net == nil || !slices.Contains(net.Aliases, alias) {
						t.Errorf("compose reads %s's default network as %+v, want the alias %s", svc, net, alias)
					}
				}
			}
		})
	}
}
