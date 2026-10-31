// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package source

import (
	"context"
	"strconv"

	"github.com/ravindu-rev/ruralz/internal/config/loader"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// defaultAdminPort is Gateway.spec.admin.port's default, the port of a
// Gateway that names none.
const defaultAdminPort = 9901

// AdminPort returns Gateway.spec.admin.port of the Bundle at the source
// when its Gateway parses: stages A to E without an overlay, ${VAR}
// substituted from Config.Variables, then the Gateway's spec.admin.port,
// or 9901 when it names none. It reports false when the source cannot be
// read, its load stopped, it holds no Gateway, or the port is not an
// integer from 1 to 65535: the Node then binds Last-Known-Good's admin
// port, else 9901 (docs/architecture/03-data-plane.md "Admin endpoints").
// The rest of the Bundle may fail validation; only the Gateway must
// parse.
func (w *Watcher) AdminPort(ctx context.Context) (int, bool) {
	b, err := w.cfg.Pipeline.Parse(ctx, loader.Source{Path: w.cfg.Path})
	if err != nil || b.Stopped() {
		return 0, false
	}
	res, err := b.Render(ctx, nil, w.cfg.Variables)
	if err != nil || res.Stopped {
		return 0, false
	}
	for _, r := range res.Resources {
		if r == nil || r.ID.Kind != v1alpha1.KindGateway {
			continue
		}
		return adminPortOf(r.Root)
	}
	return 0, false
}

// adminPortOf reads spec.admin.port of a Gateway envelope.
func adminPortOf(root *tree.Node) (int, bool) {
	spec, ok := root.Get("spec")
	if !ok || spec.Kind != tree.KindMap {
		return 0, false
	}
	admin, ok := spec.Get("admin")
	if !ok {
		return defaultAdminPort, true
	}
	if admin.Kind != tree.KindMap {
		return 0, false
	}
	port, ok := admin.Get("port")
	if !ok {
		return defaultAdminPort, true
	}
	if port.Kind != tree.KindInt {
		return 0, false
	}
	p, err := strconv.Atoi(port.Text)
	if err != nil || p < 1 || p > 65535 {
		return 0, false
	}
	return p, true
}
