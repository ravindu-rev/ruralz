// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package launch

import (
	"context"
	"errors"
	"io/fs"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/cli/adminclient"
	"github.com/ravindu-rev/ruralz/internal/cli/command"
)

// Admin is the CLI's admin client of its child: plain HTTP to the
// loopback admin port, with the generated operator token.
type Admin struct {
	client *adminclient.Client
}

// NewAdmin returns the admin client of the child whose admin port url
// names, authenticating with ws's operator token.
func NewAdmin(sio command.IO, url string, ws *Workspace, o Options) (*Admin, error) {
	c, err := adminclient.New(adminclient.Config{
		URL: url, TokenFile: ws.TokenFile(), Timeout: adminclient.DefaultTimeout,
	}, adminclient.Options{Stderr: sio.Stderr, Clock: o.timer(), Rand: o.Rand, Version: o.version()})
	if err != nil {
		return nil, err
	}
	return &Admin{client: c}, nil
}

// URL returns the child's admin base URL.
func (a *Admin) URL() string { return a.client.URL() }

// Readyz returns one /readyz answer of the child.
func (a *Admin) Readyz(ctx context.Context) (adminclient.Readiness, error) {
	return a.client.Readyz(ctx)
}

// Close closes what the client's calls left open.
func (a *Admin) Close() { a.client.Close() }

// Reasons returns the reasons of a /readyz answer on one line: each
// "<reason>[ <code>][: <detail>]", joined by "; ".
func Reasons(r adminclient.Readiness) string {
	if len(r.Reasons) == 0 {
		if r.Status == "" {
			return "no reason given"
		}
		return r.Status
	}
	parts := make([]string, 0, len(r.Reasons))
	for _, rr := range r.Reasons {
		s := printableText(rr.Reason)
		if rr.Code != "" {
			s += " " + printableText(rr.Code)
		}
		if rr.Detail != "" {
			s += ": " + printableText(rr.Detail)
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, "; ")
}

// CheckHolder reads the holder.json the child writes in its data
// directory on taking the lock and reports whether there was one; a
// version other than the CLI's own exits 2
// (docs/reference/01-cli-and-api-surface.md "Local ruralzd launched by
// the CLI", Binary), and so does a record that does not decode.
func CheckHolder(ws *Workspace, binary string, o Options) (bool, error) {
	h, err := ws.Holder()
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, command.NoResultf("%s: read holder.json: %w", binary, err)
	case h.Version != o.version():
		return true, command.NoResultf("%s is version %s, but this ruralz is %s; launch the ruralzd of the same release with --ruralzd PATH",
			binary, printableText(h.Version), o.version())
	default:
		return true, nil
	}
}
