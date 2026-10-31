// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package adminclient

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/cli/command"
)

// Tests for the "Base URL" rule of docs/reference/01-cli-and-api-surface.md
// "Admin API conventions": http or https URLs with a host, the port
// defaulting by scheme, userinfo, a query or a fragment a usage error
// (exit 2), and a path prefix kept with admin paths appended to it.

// TestBaseURLRefusals covers each refused form: a usage error naming
// where the URL came from, and never repeating userinfo.
func TestBaseURLRefusals(t *testing.T) {
	cases := []struct {
		name, raw, want string
	}{
		{"empty", "", `--admin "": want an http or https URL, such as http://127.0.0.1:9901`},
		{"no scheme", "node-a.shop.example:9901", `--admin "node-a.shop.example:9901": want an http or https URL`},
		{"address without scheme", "127.0.0.1:9901", "--admin is not a valid admin URL: first path segment in URL cannot contain colon"},
		{"other scheme", "ftp://node-a:9901", `--admin "ftp://node-a:9901": want an http or https URL`},
		{"opaque", "http:node-a", `--admin "http:node-a": an admin URL needs a host`},
		{"no host", "http://", `--admin "http://": an admin URL needs a host`},
		{"port without host", "http://:9901", `--admin "http://:9901": an admin URL needs a host`},
		{"userinfo with password", "https://operator:s3cret@node-a:9901", "--admin: an admin URL takes no userinfo"},
		{"user name", "http://operator@node-a:9901", "--admin: an admin URL takes no userinfo"},
		{"empty userinfo", "http://@node-a:9901", "--admin: an admin URL takes no userinfo"},
		{"query", "http://node-a:9901/?sample=1", `--admin "http://node-a:9901/?sample=1": an admin URL takes no query`},
		{"empty query", "http://node-a:9901?", `--admin "http://node-a:9901?": an admin URL takes no query`},
		{"fragment", "http://node-a:9901/#top", `--admin "http://node-a:9901/#top": an admin URL takes no fragment`},
		{"empty fragment", "http://node-a:9901#", `--admin "http://node-a:9901#": an admin URL takes no fragment`},
		{"port zero", "http://node-a:0", `--admin "http://node-a:0": port 0 is not between 1 and 65535`},
		{"port too large", "http://node-a:65536", `--admin "http://node-a:65536": port 65536 is not between 1 and 65535`},
		{"port overflow", "http://node-a:99999999999999999999", `--admin "http://node-a:99999999999999999999": port 99999999999999999999 is not between 1 and 65535`},
		{"space in host", "http://node a:9901", `--admin is not a valid admin URL: invalid character " " in host name`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := New(Config{URL: c.raw}, Options{})
			if _, ok := errors.AsType[*command.UsageError](err); !ok {
				t.Fatalf("New(%q) = %v, want a usage error", c.raw, err)
			}
			if code := command.Code(t.Context(), err); code != command.ExitNoResult {
				t.Errorf("New(%q) exits %d, want 2", c.raw, code)
			}
			if !strings.HasPrefix(err.Error(), c.want) {
				t.Errorf("New(%q) = %q, want prefix %q", c.raw, err, c.want)
			}
			if strings.Contains(err.Error(), "s3cret") {
				t.Errorf("New(%q) repeats the password: %q", c.raw, err)
			}
		})
	}
}

// TestBaseURLName checks that a refusal names where the URL came from.
func TestBaseURLName(t *testing.T) {
	for _, name := range []string{"FROM", EnvURL} {
		_, err := New(Config{URL: "ftp://x", Names: Names{URL: name}}, Options{})
		if err == nil || !strings.HasPrefix(err.Error(), name+` "ftp://x"`) {
			t.Errorf("New with name %s = %v", name, err)
		}
	}
}

// TestEndpointURLs covers the accepted forms and the URLs of the admin
// paths below them: the scheme lowercased, a path prefix kept, escaped,
// with or without a trailing slash, and the /tap query appended.
func TestEndpointURLs(t *testing.T) {
	cases := []struct {
		raw, path, query, want string
	}{
		{"http://127.0.0.1:9901", PathConfigDump, "", "http://127.0.0.1:9901/config/dump"},
		{"http://127.0.0.1:9901/", PathReadyz, "", "http://127.0.0.1:9901/readyz"},
		{"HTTP://Node-A.shop.example:9901", PathConfigDump, "", "http://Node-A.shop.example:9901/config/dump"},
		{"https://node-a.shop.example:9901", PathTap, "sample=0.5", "https://node-a.shop.example:9901/tap?sample=0.5"},
		{"http://[::1]:9901", PathReadyz, "", "http://[::1]:9901/readyz"},
		{"http://node-a", PathConfigDump, "", "http://node-a/config/dump"},
		{"https://proxy.shop.example/ruralz/node-a", PathConfigDump, "", "https://proxy.shop.example/ruralz/node-a/config/dump"},
		{"https://proxy.shop.example/ruralz/node-a/", PathTap, "sample=1", "https://proxy.shop.example/ruralz/node-a/tap?sample=1"},
		{"https://proxy.shop.example/nodes%2Fa/", PathReadyz, "", "https://proxy.shop.example/nodes%2Fa/readyz"},
	}
	for _, c := range cases {
		cl, _ := newTestClient(t, Config{URL: c.raw})
		if got := cl.endpoint(c.path, c.query).String(); got != c.want {
			t.Errorf("endpoint of %q, %s = %q, want %q", c.raw, c.path, got, c.want)
		}
	}
	// URL names the Node as given, with the scheme lowercased.
	for raw, want := range map[string]string{
		"https://proxy.shop.example/ruralz/node-a/": "https://proxy.shop.example/ruralz/node-a/",
		"HTTP://127.0.0.1:9901":                     "http://127.0.0.1:9901",
	} {
		if cl, _ := newTestClient(t, Config{URL: raw}); cl.URL() != want {
			t.Errorf("URL of %q = %q, want %q", raw, cl.URL(), want)
		}
	}
}

// TestDefaultPortByScheme checks the address dialed for a URL without a
// port: 80 for http and 443 for https.
func TestDefaultPortByScheme(t *testing.T) {
	for raw, want := range map[string]string{
		"http://node-a.shop.example":        "node-a.shop.example:80",
		"https://node-a.shop.example":       "node-a.shop.example:443",
		"http://node-a.shop.example:9901":   "node-a.shop.example:9901",
		"https://proxy.shop.example/node-a": "proxy.shop.example:443",
	} {
		cl, _ := newTestClient(t, Config{URL: raw, Timeout: DefaultTimeout})
		refused := errors.New("refused by the test")
		var asked []string
		cl.dial = func(_ context.Context, _, addr string) (net.Conn, error) {
			asked = append(asked, addr)
			return nil, refused
		}
		_, err := cl.ConfigDump(t.Context())
		if !errors.Is(err, refused) || len(asked) != 1 || asked[0] != want {
			t.Errorf("%s dialed %q (%v), want %q", raw, asked, err, want)
		}
	}
}
