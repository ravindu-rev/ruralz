// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package bundle

import (
	"github.com/ravindu-rev/ruralz/internal/cli/command"
	"github.com/ravindu-rev/ruralz/internal/config/loader"
)

// Output formats of the --output flag.
const (
	outputText = "text"
	outputJSON = "json"
	outputYAML = "yaml"
)

// bundleFlags are the Bundle flags --env and --environments of every
// Bundle reader (docs/reference/01-cli-and-api-surface.md "Global flags").
// Bind them from Command.Flags; check after parsing.
type bundleFlags struct {
	f            *command.Flags
	env          string
	environments string
}

// bind defines --env and --environments on f.
func (b *bundleFlags) bind(f *command.Flags) {
	b.f = f
	f.String(&b.env, "env", "NAME", "",
		"Render with this Environment's overlay and variables, read from --environments", command.CompleteNone)
	f.String(&b.environments, "environments", "FILE", "",
		"Local Environment resources", command.CompleteFile)
}

// hasEnvironments reports whether --environments names a file.
func (b *bundleFlags) hasEnvironments() bool { return b.environments != "" }

// envSource returns the --environments file as a loader source; the zero
// Source without one.
func (b *bundleFlags) envSource() loader.Source {
	if b.environments == "" {
		return loader.Source{}
	}
	return loader.Source{Path: b.environments}
}

// check refuses an empty --environments, and checks --env with
// checkEnvFlag.
func (b *bundleFlags) check() error {
	if b.f.IsSet("environments") && b.environments == "" {
		return command.Usagef("--environments needs a file")
	}
	return b.checkEnvFlag("env", b.env)
}

// checkEnvFlag refuses the Environment flag name when it is given empty,
// or without --environments: the Environment's overlay and variables come
// from that file, since fetching Environments from Ruralz Control is
// Planned (M2).
func (b *bundleFlags) checkEnvFlag(name, value string) error {
	if !b.f.IsSet(name) {
		return nil
	}
	if value == "" {
		return command.Usagef("--%s needs an Environment name", name)
	}
	if b.environments == "" {
		return command.Usagef("--%s needs --environments: fetching Environments from Ruralz Control is Planned (M2)", name)
	}
	return nil
}

// oneEnvironment refuses --environments without --env for the verb, a
// command that renders one Environment: only ruralz bundle validate runs
// once per Environment of the file.
func (b *bundleFlags) oneEnvironment(verb string) error {
	if b.environments != "" && b.env == "" {
		return command.Usagef("--environments needs --env: ruralz bundle %s renders one Environment", verb)
	}
	return nil
}
