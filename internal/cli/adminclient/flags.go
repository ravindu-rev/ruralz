// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package adminclient

import (
	"strings"
	"time"

	"github.com/ravindu-rev/ruralz/internal/cli/command"
)

// Flags binds the admin client flags of one command and resolves them,
// with the environment variables that stand in for absent flags, into the
// Config of each Node the command calls
// (docs/reference/01-cli-and-api-surface.md "Global flags" and "Bundle
// verbs in detail"). Bind it from Command.Flags; resolve after parsing.
type Flags struct {
	f       *command.Flags
	opts    FlagOptions
	url     string
	shared  credentials
	sides   map[string]*credentials
	caFile  string
	timeout time.Duration
}

// credentials are the values of one set of credential flags.
type credentials struct {
	tokenFile, cert, key string
}

// FlagOptions select the admin flags a command takes besides the shared
// --admin-token-file, --ca-file, --client-cert and --client-key.
type FlagOptions struct {
	// URL binds --admin URL, as ruralz dev tap and ruralz node dump do;
	// a command whose arguments name each Node, as ruralz bundle diff's
	// FROM and TO do, leaves it out.
	URL bool
	// Timeout binds --timeout DURATION, the deadline of each call that
	// does not stream; without it a call has DefaultTimeout.
	Timeout bool
	// Sides are the prefixes of per-Node credential flags, such as "from"
	// and "to" for ruralz bundle diff: --from-admin-token-file,
	// --from-client-cert and --from-client-key fall back to the shared
	// flags.
	Sides []string
}

// Bind defines the selected flags on f.
func (fl *Flags) Bind(f *command.Flags, o FlagOptions) {
	fl.f = f
	fl.opts = o
	fl.sides = make(map[string]*credentials, len(o.Sides))
	if o.URL {
		f.String(&fl.url, "admin", "URL", "",
			"Admin base URL of the Node (default $"+EnvURL+", else "+DefaultURL+")", command.CompleteNone)
	}
	f.String(&fl.shared.tokenFile, "admin-token-file", "PATH", "",
		"Operator admin token file (default $"+EnvTokenFile+")", command.CompleteFile)
	f.String(&fl.caFile, "ca-file", "PATH", "", "Extra trust anchors (PEM), added to the system roots", command.CompleteFile)
	f.String(&fl.shared.cert, "client-cert", "PATH", "", "Client certificate (PEM) for admin mTLS over https", command.CompleteFile)
	f.String(&fl.shared.key, "client-key", "PATH", "", "Unencrypted PEM key of --client-cert", command.CompleteFile)
	for _, side := range o.Sides {
		s := &credentials{}
		fl.sides[side] = s
		arg := strings.ToUpper(side)
		f.String(&s.tokenFile, side+"-admin-token-file", "PATH", "",
			"Operator admin token file for "+arg+" (default --admin-token-file)", command.CompleteFile)
		f.String(&s.cert, side+"-client-cert", "PATH", "",
			"Client certificate for "+arg+" (default --client-cert)", command.CompleteFile)
		f.String(&s.key, side+"-client-key", "PATH", "",
			"Key of --"+side+"-client-cert (default --client-key)", command.CompleteFile)
	}
	if o.Timeout {
		f.Timeout(&fl.timeout, "timeout", DefaultTimeout, "Deadline of each admin call that does not stream")
	}
}

// Config resolves the Config of the Node that --admin names: --admin when
// given, else RURALZ_ADMIN_URL when set and not empty, else DefaultURL.
func (fl *Flags) Config(sio command.IO) Config {
	cfg := Config{URL: DefaultURL, Names: Names{URL: "--admin"}}
	if fl.f.IsSet("admin") {
		cfg.URL = fl.url
	} else if v, ok := sio.LookupEnv(EnvURL); ok && v != "" {
		cfg.URL, cfg.Names.URL = v, EnvURL
	}
	return fl.resolve(sio, cfg, "")
}

// NodeConfig resolves the Config of a Node that an argument names, such
// as ruralz bundle diff's FROM: rawURL is the argument, name its
// placeholder for messages, and side one of FlagOptions.Sides, or "" for
// the shared credentials alone.
func (fl *Flags) NodeConfig(sio command.IO, side, name, rawURL string) Config {
	return fl.resolve(sio, Config{URL: rawURL, Names: Names{URL: name}}, side)
}

// resolve fills the credentials, trust anchors and deadline of cfg. A
// flag always wins, even given empty, which sends no token: a side's
// token file, else --admin-token-file, else RURALZ_ADMIN_TOKEN_FILE when
// set and not empty. The client certificate and key are a side's pair
// when either side flag is given, else the shared pair; New checks that
// they come together.
func (fl *Flags) resolve(sio command.IO, cfg Config, side string) Config {
	s := fl.sides[side]
	cfg.Names.TokenFile = "--admin-token-file"
	switch {
	case s != nil && fl.f.IsSet(side+"-admin-token-file"):
		cfg.TokenFile, cfg.Names.TokenFile = s.tokenFile, "--"+side+"-admin-token-file"
	case fl.f.IsSet("admin-token-file"):
		cfg.TokenFile = fl.shared.tokenFile
	default:
		if v, ok := sio.LookupEnv(EnvTokenFile); ok && v != "" {
			cfg.TokenFile, cfg.Names.TokenFile = v, EnvTokenFile
		}
	}
	cfg.ClientCert, cfg.ClientKey = fl.shared.cert, fl.shared.key
	cfg.Names.ClientCert, cfg.Names.ClientKey = "--client-cert", "--client-key"
	if s != nil && (fl.f.IsSet(side+"-client-cert") || fl.f.IsSet(side+"-client-key")) {
		cfg.ClientCert, cfg.ClientKey = s.cert, s.key
		cfg.Names.ClientCert, cfg.Names.ClientKey = "--"+side+"-client-cert", "--"+side+"-client-key"
	}
	cfg.CAFile = fl.caFile
	cfg.Timeout = DefaultTimeout
	if fl.opts.Timeout {
		cfg.Timeout = fl.timeout
	}
	return cfg
}
