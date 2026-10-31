// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package config

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/cli/command"
)

// errBuiltStandIns refuses a run on the stand-ins for a built binary: the
// stand-ins link into the suite, never into a built binary.
var errBuiltStandIns = errors.New("a built binary links no stand-in")

// ruralzBinary is the built ruralz (docs/engineering/03-testing-and-
// quality-strategy.md "Conformance suites"): the commands cliPath runs in
// process, ruralz bundle validate in text and with --output json, and for
// a single render without an error ruralz bundle build --offline --output
// json for the digest, run as a process over an empty environment, so a
// render without --env substitutes only defaults. A run with an
// environments file runs in the directory that holds the Bundle and names
// both relative to it, as cliPath does.
type ruralzBinary struct {
	path string
}

func (ruralzBinary) binary() string  { return binaryCLI }
func (ruralzBinary) inProcess() bool { return false }

func (b ruralzBinary) validate(ctx context.Context, r run) (outcome, error) {
	if r.standIns {
		return outcome{}, errBuiltStandIns
	}
	if r.environments == "" {
		return b.commands(ctx, "", r.bundle, nil, true)
	}
	dir := filepath.Dir(r.bundle)
	envs, err := filepath.Rel(dir, r.environments)
	if err != nil {
		return outcome{}, err
	}
	flags := []string{"--environments", envs}
	if r.env != "" {
		flags = append(flags, "--env", r.env)
	}
	return b.commands(ctx, dir, "."+string(filepath.Separator)+filepath.Base(r.bundle), flags, r.env != "")
}

// commands runs ruralz bundle validate on arg with flags in dir, in text
// and in JSON, and, when digest is set and validate finds no error, ruralz
// bundle build --offline for the Revision digest.
func (b ruralzBinary) commands(ctx context.Context, dir, arg string, flags []string, digest bool) (outcome, error) {
	validate := append([]string{"bundle", "validate"}, flags...)
	text, code, err := b.execute(ctx, dir, append(slices.Clone(validate), arg))
	if err != nil {
		return outcome{}, err
	}
	js, codeJSON, err := b.execute(ctx, dir, append(append(slices.Clone(validate), "--output", "json"), arg))
	if err != nil {
		return outcome{}, err
	}
	if code != codeJSON {
		return outcome{}, fmt.Errorf("ruralz bundle validate exits %d, and %d with --output json", code, codeJSON)
	}
	o := outcome{text: text, json: js}
	if !digest || code != command.ExitOK {
		return o, nil
	}
	out, _, err := b.execute(ctx, dir, append(append([]string{"bundle", "build", "--offline", "--output", "json"}, flags...), arg))
	if err != nil {
		return outcome{}, err
	}
	var built struct {
		Digest string `json:"digest"`
	}
	if err := json.Unmarshal(out, &built); err != nil {
		return outcome{}, fmt.Errorf("ruralz bundle build prints %q: %w", out, err)
	}
	if built.Digest == "" {
		return outcome{}, fmt.Errorf("ruralz bundle build prints %q: no digest", out)
	}
	o.digest = built.Digest
	return o, nil
}

// execute runs the built ruralz with args in dir over an empty
// environment and returns its standard output and exit code, 0 or 1, the
// codes of a result; any other code is an error carrying its standard
// error.
func (b ruralzBinary) execute(ctx context.Context, dir string, args []string) ([]byte, int, error) {
	cmd := exec.CommandContext(ctx, b.path, args...) //nolint:gosec // G204: the ruralz the suite built.
	cmd.Dir = dir
	cmd.Env = []string{}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			return nil, 0, fmt.Errorf("run ruralz %s: %w", strings.Join(args, " "), err)
		}
		code = ee.ExitCode()
	}
	if code != command.ExitOK && code != command.ExitNegative {
		return nil, code, fmt.Errorf("ruralz %s: exit %d: %s", strings.Join(args, " "), code, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), code, nil
}

// TestBuiltRuralzVersion checks the built ruralz's identity: ruralz version
// reports the build metadata internal/testkit/binaries injects and the
// schema level of every apiVersion it serves.
func TestBuiltRuralzVersion(t *testing.T) {
	s := mustSuite(t)
	out, code, err := ruralzBinary{path: builtBinaries.Ruralz}.execute(t.Context(), s.root, []string{"version", "--output", "json"})
	if err != nil || code != command.ExitOK {
		t.Fatalf("ruralz version: exit %d, %v", code, err)
	}
	var v struct {
		Version      string         `json:"version"`
		APIVersions  []string       `json:"apiVersions"`
		SchemaLevels map[string]int `json:"schemaLevels"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("ruralz version prints %q: %v", out, err)
	}
	if v.Version == "" || len(v.APIVersions) == 0 {
		t.Fatalf("ruralz version prints %q", out)
	}
	for _, api := range v.APIVersions {
		if level, ok := v.SchemaLevels[api]; !ok || level != s.cli.HubIndex().Level() {
			t.Errorf("ruralz version: schemaLevels[%s] = %d, %v; want %d", api, level, ok, s.cli.HubIndex().Level())
		}
	}
}
