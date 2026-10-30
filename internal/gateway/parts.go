// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"

	"github.com/ravindu-rev/ruralz/internal/gateway/admin"
	"github.com/ravindu-rev/ruralz/internal/gateway/body"
	"github.com/ravindu-rev/ruralz/internal/gateway/reload"
	"github.com/ravindu-rev/ruralz/internal/gateway/sdnotify"
)

// budgetPart is the Node buffer budget as an activation switches it: right
// after the swap the budget takes the Revision's limits.maxBufferedBytes
// for new reservations and keeps the outstanding ones, so the budget is
// carried across Hot Reloads (body.Budget.SetTotal;
// docs/architecture/03-data-plane.md "Bounded resources").
type budgetPart struct{ budget *body.Budget }

// Prepare reads the Revision's limits.maxBufferedBytes; nothing can fail.
func (p budgetPart) Prepare(_ context.Context, next reload.Compiled) (reload.Switch, error) {
	return budgetSwitch{budget: p.budget, total: next.Snapshot().Gateway.Limits.MaxBufferedBytes}, nil
}

// budgetSwitch applies one Revision's total.
type budgetSwitch struct {
	budget *body.Budget
	total  int64
}

// Commit sets the total.
func (s budgetSwitch) Commit(context.Context) error {
	s.budget.SetTotal(s.total)
	return nil
}

// Abort keeps the running total.
func (budgetSwitch) Abort() {}

// errAdminUnbound is the admin probe's error before the admin port binds.
var errAdminUnbound = errors.New("the admin port is not bound")

// adminProbe returns the admin server's watchdog probe: one GET /healthz
// over the admin socket the Node serves, on a connection of its own, which
// answers only while the socket's accept loop, the connection ceiling,
// the server and its handler all run (docs/operations/02-zero-downtime-
// upgrades-and-hot-reload.md "Handover under systemd"). A socket bound to
// every interface is reached on loopback. With admin TLS the probe pins
// the admin server's own certificate, since the address it dials need not
// be one the certificate names; it presents no credential.
func (n *node) adminProbe() sdnotify.Probe {
	var pinned []byte
	if n.adminTLS != nil && len(n.adminTLS.Certificates) > 0 && len(n.adminTLS.Certificates[0].Certificate) > 0 {
		pinned = n.adminTLS.Certificates[0].Certificate[0]
	}
	tlsOn := n.adminTLS != nil
	return func(ctx context.Context) error {
		a := n.adminSock.Addr()
		if a == nil {
			return errAdminUnbound
		}
		return probeHealthz(ctx, probeAddr(a), tlsOn, pinned)
	}
}

// probeAddr returns the address a probe dials for the bound address a:
// loopback for a socket on every interface.
func probeAddr(a net.Addr) string {
	ap, err := netip.ParseAddrPort(a.String())
	if err != nil {
		return a.String()
	}
	if ap.Addr().IsUnspecified() {
		ap = netip.AddrPortFrom(netip.AddrFrom4([4]byte{127, 0, 0, 1}), ap.Port())
	}
	return ap.String()
}

// probeHealthz sends GET /healthz to addr and wants 200. With tlsOn the
// connection must present the certificate pinned.
func probeHealthz(ctx context.Context, addr string, tlsOn bool, pinned []byte) error {
	tr := &http.Transport{DisableKeepAlives: true, Proxy: nil}
	defer tr.CloseIdleConnections()
	scheme := "http"
	if tlsOn {
		scheme = "https"
		tr.TLSClientConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
			// The admin server's own certificate is pinned instead.
			InsecureSkipVerify: true, //nolint:gosec // G402: VerifyConnection pins the admin server's own certificate.
			VerifyConnection: func(cs tls.ConnectionState) error {
				return checkPinned(cs.PeerCertificates, pinned)
			},
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, scheme+"://"+addr+admin.PathHealthz, nil)
	if err != nil {
		return fmt.Errorf("admin probe: %w", err)
	}
	resp, err := (&http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}).Do(req)
	if err != nil {
		return fmt.Errorf("admin probe: %w", err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("admin probe: /healthz answered %d", resp.StatusCode)
	}
	return nil
}

// errNotPinned is a TLS peer that does not present the pinned
// certificate.
var errNotPinned = errors.New("admin probe: the peer does not present the admin server's certificate")

// checkPinned accepts a chain whose leaf is the pinned certificate.
func checkPinned(chain []*x509.Certificate, pinned []byte) error {
	if len(chain) == 0 || len(pinned) == 0 || subtle.ConstantTimeCompare(chain[0].Raw, pinned) != 1 {
		return errNotPinned
	}
	return nil
}
