// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package property

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/builtin"
	"github.com/ravindu-rev/ruralz/internal/gateway/compile"
	"github.com/ravindu-rev/ruralz/internal/gateway/composition"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream"
	"github.com/ravindu-rev/ruralz/internal/secret/resolver"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/statestore/manager"
	"github.com/ravindu-rev/ruralz/internal/statestore/memory"
	"github.com/ravindu-rev/ruralz/internal/telemetry/aggregate"
)

// nodeParts are the Node-wide parts of one simulated Node's snapshot
// compiler, wired as ruralzd wires them (internal/gateway).
type nodeParts struct {
	// Filters are the Filter tables; nil builds the built-in ones
	// (builtin.New).
	Filters *filter.Registry
	// Stores acquires the State Store handles; nil opens a State Store
	// manager over the memory driver for this Node alone.
	Stores compile.StateStores
	// Root is RURALZ_SECRET_ROOT; empty makes a temporary directory.
	Root string
}

// nodeCompiler is one Node's snapshot compiler.
type nodeCompiler struct {
	comp *compile.Compiler
	// root is RURALZ_SECRET_ROOT, where file secrets live.
	root string
}

// secretFile is the file secret every generated auth.upstream-oauth2
// Policy names, relative to the Node's secret root.
const secretFile = "oauth-client-secret" //nolint:gosec // G101: a file name, not a credential.

// newNodeCompiler builds a Node's snapshot compiler on clk with the real
// configuration pipeline, CEL compiler, secret resolver, Upstream manager,
// composition engine and meter. The test's end closes what it opened.
func newNodeCompiler(t testing.TB, clk *clocktest.Fake, parts nodeParts) *nodeCompiler {
	t.Helper()
	tc := tools(t)
	root := parts.Root
	if root == "" {
		root = t.TempDir()
	}
	if err := os.WriteFile(filepath.Join(root, secretFile), []byte("s3cret"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := resolver.NewResolver(resolver.Config{Root: root, LookupEnv: func(string) (string, bool) { return "", false }, Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	stores := parts.Stores
	if stores == nil {
		mgr := manager.New(context.Background(), manager.Config{Deps: statestore.Deps{Clock: clk}, Openers: manager.Openers{Memory: memory.Open}})
		t.Cleanup(func() { _ = mgr.Close(context.Background()) })
		stores = mgr
	}
	meter, err := aggregate.New(aggregate.Options{Clock: clk, Stripes: 1})
	if err != nil {
		t.Fatal(err)
	}
	filters := parts.Filters
	if filters == nil {
		if filters, err = builtin.New(builtin.Options{Clock: clk}); err != nil {
			t.Fatal(err)
		}
	}
	comp, err := compile.New(compile.Deps{
		Pipeline: tc.pipe, CEL: tc.cel, Filters: filters, Secrets: res, LookupEnv: func(string) (string, bool) { return "", false },
		StateStores: stores, Upstreams: upstream.NewManager(upstream.Options{Clock: clk, Rand: fixedSource{}}),
		Composition: composition.New(composition.Deps{Clock: clk}), Meter: meter, Clock: clk, Rand: fixedSource{}, Workers: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &nodeCompiler{comp: comp, root: root}
}

// secretPath is the absolute path of the Node's file secret.
func (n *nodeCompiler) secretPath() string { return filepath.Join(n.root, secretFile) }

// fixedSource is a deterministic rand.Source: weighted picks and jitter
// never depend on chance.
type fixedSource struct{}

func (fixedSource) Uint64() uint64 { return 0x2545f4914f6cdd1d }
