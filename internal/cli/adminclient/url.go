// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package adminclient

import (
	"errors"
	"net/url"
	"strconv"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/cli/command"
)

// URL schemes of an admin base URL.
const (
	schemeHTTP  = "http"
	schemeHTTPS = "https"
)

// parseBase checks an admin base URL by the "Base URL" rule of
// docs/reference/01-cli-and-api-surface.md "Admin API conventions": an
// http or https URL with a host, whose port defaults by scheme, without
// userinfo, a query or a fragment; a path prefix is kept. name is where
// raw came from, such as --admin or FROM. Every refusal is a usage error,
// and none repeats userinfo, which may hold a password.
func parseBase(name, raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		// The parse error alone, without url.Error's copy of raw.
		if ue, ok := errors.AsType[*url.Error](err); ok {
			err = ue.Err
		}
		return nil, command.Usagef("%s is not a valid admin URL: %w", name, err)
	}
	if u.User != nil {
		return nil, command.Usagef("%s: an admin URL takes no userinfo", name)
	}
	switch {
	case u.Scheme != schemeHTTP && u.Scheme != schemeHTTPS:
		return nil, command.Usagef("%s %q: want an http or https URL, such as %s", name, raw, DefaultURL)
	case u.Opaque != "" || u.Hostname() == "":
		return nil, command.Usagef("%s %q: an admin URL needs a host", name, raw)
	case u.ForceQuery || u.RawQuery != "":
		return nil, command.Usagef("%s %q: an admin URL takes no query", name, raw)
	case strings.Contains(raw, "#"):
		return nil, command.Usagef("%s %q: an admin URL takes no fragment", name, raw)
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return nil, command.Usagef("%s %q: port %s is not between 1 and 65535", name, raw, p)
		}
	}
	return u, nil
}
