// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package setting reads and validates the RURALZ_* process settings of
// ruralzd once at start (spec 04 requirements 1 to 4; pack section 2;
// OQ-security-and-identity-7 (a) and -22 (a); R-49). A change needs a
// handover or a Drain and restart, and no setting ever changes a Revision.
//
// Every invalid setting is a usage error: Load reports all of them at once
// as *Error values and ruralzd exits 2 (ExitUsage) naming each setting.
// Settings errors carry no RZ code.
//
// Load checks syntax, the configuration mode, the secret root overlap
// rule and reads the State Store entry MAC key. The owners of the other
// settings read their files later: internal/adminauth the admin token
// files and TLS directory, internal/egress.ParseAllow the fetch allow-list
// (whose syntax Load already checked, so both reject the same values), and
// internal/secret/resolver RURALZ_STATE_STORE_URL through its env
// provider, which is why Settings never holds that URL.
package setting

import (
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
)

// Process setting names (pack section 2; spec 04 requirement 2; spec 06
// requirements 85, 88 and 93; R-49).
//
//nolint:gosec // G101: environment variable names, not credentials.
const (
	// EnvConfig selects the configuration source; required.
	EnvConfig = "RURALZ_CONFIG"
	// EnvDataDir is the data directory.
	EnvDataDir = "RURALZ_DATA_DIR"
	// EnvLogLevel is the process log level.
	EnvLogLevel = "RURALZ_LOG_LEVEL"
	// EnvStateStoreURL is the fallback State Store URL, read by the State
	// Store area through the secret resolver's env provider.
	EnvStateStoreURL = "RURALZ_STATE_STORE_URL"
	// EnvAdminTokenFile is the admin operator token file.
	EnvAdminTokenFile = "RURALZ_ADMIN_TOKEN_FILE"
	// EnvAdminMetricsTokenFile is the admin /metrics token file.
	EnvAdminMetricsTokenFile = "RURALZ_ADMIN_METRICS_TOKEN_FILE"
	// EnvAdminTLSDir is the admin server TLS directory.
	EnvAdminTLSDir = "RURALZ_ADMIN_TLS_DIR"
	// EnvSecretRoot is the root under which file secret references
	// resolve.
	EnvSecretRoot = "RURALZ_SECRET_ROOT"
	// EnvFetchAllow is the egress allow-list for Bundle destinations.
	EnvFetchAllow = "RURALZ_FETCH_ALLOW"
	// EnvStateStoreMACKeyFile is the State Store entry MAC key file.
	EnvStateStoreMACKeyFile = "RURALZ_STATE_STORE_MAC_KEY_FILE"
	// EnvTrustPolicyFile is Planned (M2); unused in M1.
	EnvTrustPolicyFile = "RURALZ_TRUST_POLICY_FILE"
	// EnvEnrollmentTokenFile is Planned (M2); unused in M1.
	EnvEnrollmentTokenFile = "RURALZ_ENROLLMENT_TOKEN_FILE"
	// EnvRegistryAuthFile is Planned (M2); unused in M1.
	EnvRegistryAuthFile = "RURALZ_REGISTRY_AUTH_FILE"
)

// Defaults (spec 04 requirement 2; spec 06 requirement 88).
const (
	// DefaultDataDir is the data directory when RURALZ_DATA_DIR is unset;
	// it equals nodedir.DefaultRoot.
	DefaultDataDir = "/var/lib/ruralz"
	// DefaultSecretRoot is the secret root when RURALZ_SECRET_ROOT is
	// unset.
	DefaultSecretRoot = "/etc/ruralz" //nolint:gosec // G101: a directory path, not a credential.
	// DefaultLogLevel is the log level when RURALZ_LOG_LEVEL is unset.
	DefaultLogLevel = LogInfo
)

// Process exit codes of ruralzd (spec 04 requirement 4).
const (
	// ExitOK follows a completed Drain.
	ExitOK = 0
	// ExitRuntime is a fatal runtime error or a refused or timed-out
	// handover.
	ExitRuntime = 1
	// ExitUsage is a usage or settings error.
	ExitUsage = 2
)

// Usage is the text ruralzd writes to stderr with a usage error.
const Usage = `usage: ruralzd

ruralzd takes no arguments or flags. It reads these environment settings
once at start:

  RURALZ_CONFIG                    rendered Bundle directory (required)
  RURALZ_DATA_DIR                  data directory (default /var/lib/ruralz)
  RURALZ_LOG_LEVEL                 debug, info, warn or error (default info)
  RURALZ_SECRET_ROOT               root of file secrets (default /etc/ruralz)
  RURALZ_FETCH_ALLOW               addresses and CIDRs Bundle fetches may reach
  RURALZ_STATE_STORE_URL           State Store URL when the Gateway names none
  RURALZ_STATE_STORE_MAC_KEY_FILE  State Store entry MAC key: owner-only (0600 or
                                   0400), 32 bytes or more; one final newline
                                   is not part of the key
  RURALZ_ADMIN_TOKEN_FILE          admin operator token
  RURALZ_ADMIN_METRICS_TOKEN_FILE  admin /metrics token
  RURALZ_ADMIN_TLS_DIR             admin TLS directory (tls.crt, tls.key, ca.crt)
`

// Errors of Load, each wrapped in an *Error naming the setting.
var (
	// ErrUsage reports a command-line argument; ruralzd takes none.
	ErrUsage = errors.New("ruralzd accepts no arguments or flags")
	// ErrRequired reports an unset required setting.
	ErrRequired = errors.New("required setting is not set")
	// ErrPlanned reports a configuration mode that is Planned (M2).
	ErrPlanned = errors.New("configuration mode is Planned (M2)")
	// ErrSource reports a RURALZ_CONFIG URL with an unknown scheme.
	ErrSource = errors.New("not a directory path or a known configuration source")
	// ErrLogLevel reports a log level other than debug, info, warn or
	// error.
	ErrLogLevel = errors.New("log level must be debug, info, warn or error")
	// ErrNotAbsolute reports a path setting that must be absolute.
	ErrNotAbsolute = errors.New("not an absolute path")
	// ErrInsideSecretRoot reports a protected path that is
	// RURALZ_SECRET_ROOT or lies inside it.
	ErrInsideSecretRoot = errors.New("must lie outside " + EnvSecretRoot)
	// ErrFetchAllow reports an unparsable RURALZ_FETCH_ALLOW entry.
	ErrFetchAllow = errors.New("invalid allow-list entry")
	// ErrMACKeyFile reports a MAC key file that is not a readable regular
	// file.
	ErrMACKeyFile = errors.New("MAC key file is not a readable regular file")
	// ErrMACKeyOwner reports a MAC key file owned by a user other than
	// the effective user or root.
	ErrMACKeyOwner = errors.New("MAC key file must be owned by the effective user or root")
	// ErrMACKeyMode reports a MAC key file readable or writable by group
	// or others.
	ErrMACKeyMode = errors.New("MAC key file must be accessible by its owner only")
	// ErrMACKeyShort reports a MAC key below MinMACKeyBytes, not counting
	// one final newline.
	ErrMACKeyShort = errors.New("MAC key is shorter than 32 bytes")
	// ErrMACKeyLong reports a MAC key above MaxMACKeyBytes.
	ErrMACKeyLong = errors.New("MAC key file is larger than 64 KiB")
)

// Error is one invalid setting. Setting is the variable name, or empty
// for a command-line usage error.
type Error struct {
	// Setting names the variable.
	Setting string
	// Err is the cause; it matches one of the package's Err values.
	Err error
}

// Error returns "<setting>: <cause>".
func (e *Error) Error() string {
	if e.Setting == "" {
		return e.Err.Error()
	}
	return e.Setting + ": " + e.Err.Error()
}

// Unwrap returns the cause.
func (e *Error) Unwrap() error { return e.Err }

// ExitCode maps an error of Load (or of the Node's start) to the process
// exit code: ExitOK for nil, ExitUsage when err holds an *Error, else
// ExitRuntime.
func ExitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	var e *Error
	if errors.As(err, &e) {
		return ExitUsage
	}
	return ExitRuntime
}

// Mode is the configuration mode chosen at boot from RURALZ_CONFIG (spec
// 04 requirement 1). M1 serves ModeFile only.
type Mode uint8

// Configuration modes.
const (
	// ModeFile watches a rendered Bundle directory.
	ModeFile Mode = iota + 1
	// ModeOCI pulls Revisions from an OCI registry (oci://), Planned (M2).
	ModeOCI
	// ModeControl follows the Control Stream (ruralz-control://), Planned
	// (M2).
	ModeControl
)

// String returns "file", "oci" or "control".
func (m Mode) String() string {
	switch m {
	case ModeFile:
		return "file"
	case ModeOCI:
		return "oci"
	case ModeControl:
		return "control"
	default:
		return fmt.Sprintf("Mode(%d)", uint8(m))
	}
}

// LogLevel is RURALZ_LOG_LEVEL.
type LogLevel string

// Log levels (spec 04 requirement 2).
const (
	LogDebug LogLevel = "debug"
	LogInfo  LogLevel = "info"
	LogWarn  LogLevel = "warn"
	LogError LogLevel = "error"
)

// Slog returns the slog level for the telemetry logger factory.
func (l LogLevel) Slog() slog.Level {
	switch l {
	case LogDebug:
		return slog.LevelDebug
	case LogWarn:
		return slog.LevelWarn
	case LogError:
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// Settings are the validated process settings. Paths are absolute and
// cleaned; an empty optional path is unset.
type Settings struct {
	// Mode is ModeFile.
	Mode Mode
	// Config is the rendered Bundle directory (RURALZ_CONFIG, made
	// absolute). It need not exist yet: the watcher keeps polling.
	Config string
	// DataDir is RURALZ_DATA_DIR (made absolute) or DefaultDataDir.
	DataDir string
	// LogLevel is RURALZ_LOG_LEVEL or DefaultLogLevel.
	LogLevel LogLevel
	// SecretRoot is RURALZ_SECRET_ROOT or DefaultSecretRoot.
	SecretRoot string
	// FetchAllow is RURALZ_FETCH_ALLOW verbatim, syntax-checked; pass it
	// to egress.ParseAllow.
	FetchAllow string
	// AdminTokenFile is RURALZ_ADMIN_TOKEN_FILE.
	AdminTokenFile string
	// AdminMetricsTokenFile is RURALZ_ADMIN_METRICS_TOKEN_FILE.
	AdminMetricsTokenFile string
	// AdminTLSDir is RURALZ_ADMIN_TLS_DIR.
	AdminTLSDir string
	// MACKeyFile is RURALZ_STATE_STORE_MAC_KEY_FILE.
	MACKeyFile string
	// MACKey is the content of MACKeyFile without one final newline, read
	// at start; unset without the setting. Its text forms are redacted.
	MACKey MACKey
	// EnrollmentTokenFile is RURALZ_ENROLLMENT_TOKEN_FILE (M2, unused);
	// kept for the secret root rule.
	EnrollmentTokenFile string
	// Unused names the set Planned (M2) settings, which the Node logs at
	// warn as unused in file mode without OCI.
	Unused []string
}

// ProtectedPath is a process setting whose path the secret root must not
// contain (spec 04 requirement 3; spec 06 requirement 88; R-49).
type ProtectedPath struct {
	// Setting names the variable.
	Setting string
	// Path is the effective absolute path.
	Path string
}

// Protected returns every set protected path, including the effective
// data directory, in a fixed order; the secret resolver repeats the
// check (spec 01 requirement 45).
func (s *Settings) Protected() []ProtectedPath {
	out := make([]ProtectedPath, 0, 6)
	for _, p := range []ProtectedPath{
		{EnvDataDir, s.DataDir},
		{EnvAdminTLSDir, s.AdminTLSDir},
		{EnvAdminTokenFile, s.AdminTokenFile},
		{EnvAdminMetricsTokenFile, s.AdminMetricsTokenFile},
		{EnvStateStoreMACKeyFile, s.MACKeyFile},
		{EnvEnrollmentTokenFile, s.EnrollmentTokenFile},
	} {
		if p.Path != "" {
			out = append(out, p)
		}
	}
	return out
}

// Load parses the command-line arguments (os.Args[1:]) and the settings
// read through lookupEnv (nil selects os.LookupEnv), and reads the MAC key
// file. An empty variable counts as unset. Any argument is a usage error
// reported alone; otherwise every invalid setting is reported, joined, as
// *Error values.
func Load(args []string, lookupEnv func(string) (string, bool)) (Settings, error) {
	if len(args) > 0 {
		return Settings{}, &Error{Err: fmt.Errorf("%w (got %q)", ErrUsage, args[0])}
	}
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	get := func(name string) string {
		v, _ := lookupEnv(name)
		return v
	}
	l := loader{}
	s := Settings{
		Mode:       ModeFile,
		DataDir:    DefaultDataDir,
		LogLevel:   DefaultLogLevel,
		SecretRoot: DefaultSecretRoot,
	}
	s.Config = l.config(get(EnvConfig))
	if v := get(EnvDataDir); v != "" {
		s.DataDir = l.makeAbs(EnvDataDir, v)
	}
	if v := get(EnvLogLevel); v != "" {
		s.LogLevel = l.logLevel(v)
	}
	if v := get(EnvSecretRoot); v != "" {
		s.SecretRoot = l.absolute(EnvSecretRoot, v)
	}
	if v := get(EnvFetchAllow); v != "" {
		l.fetchAllow(v)
		s.FetchAllow = v
	}
	s.AdminTokenFile = l.absolute(EnvAdminTokenFile, get(EnvAdminTokenFile))
	s.AdminMetricsTokenFile = l.absolute(EnvAdminMetricsTokenFile, get(EnvAdminMetricsTokenFile))
	s.AdminTLSDir = l.absolute(EnvAdminTLSDir, get(EnvAdminTLSDir))
	s.MACKeyFile = l.absolute(EnvStateStoreMACKeyFile, get(EnvStateStoreMACKeyFile))
	for _, name := range []string{EnvTrustPolicyFile, EnvEnrollmentTokenFile, EnvRegistryAuthFile} {
		if get(name) != "" {
			s.Unused = append(s.Unused, name)
		}
	}
	if v := get(EnvEnrollmentTokenFile); v != "" {
		// Unused in M1, but still never inside the secret root (spec 06
		// requirement 88); its syntax is not checked until M2.
		s.EnrollmentTokenFile = l.makeAbs(EnvEnrollmentTokenFile, v)
	}
	if s.SecretRoot != "" {
		for _, p := range s.Protected() {
			if within(p.Path, s.SecretRoot) {
				l.fail(p.Setting, fmt.Errorf("%w: %s=%s is %s=%s or inside it",
					ErrInsideSecretRoot, p.Setting, p.Path, EnvSecretRoot, s.SecretRoot))
			}
		}
	}
	if s.MACKeyFile != "" {
		key, err := readMACKey(s.MACKeyFile)
		if err != nil {
			l.fail(EnvStateStoreMACKeyFile, err)
		}
		s.MACKey = key
	}
	if err := errors.Join(l.errs...); err != nil {
		clear(s.MACKey.b)
		return Settings{}, err
	}
	return s, nil
}

// loader collects the errors of one Load.
type loader struct {
	errs []error
}

func (l *loader) fail(setting string, err error) {
	l.errs = append(l.errs, &Error{Setting: setting, Err: err})
}

// config parses RURALZ_CONFIG (spec 04 requirement 1): a path selects file
// mode; oci:// and ruralz-control:// are Planned (M2); any other URL is
// refused. Only the scheme is echoed in errors.
func (l *loader) config(v string) string {
	if v == "" {
		l.fail(EnvConfig, fmt.Errorf("%w; set it to a rendered Bundle directory", ErrRequired))
		return ""
	}
	if scheme, ok := urlScheme(v); ok {
		switch strings.ToLower(scheme) {
		case "oci":
			l.fail(EnvConfig, fmt.Errorf("%w: oci:// selects OCI pull mode; M1 serves file mode only", ErrPlanned))
		case "ruralz-control":
			l.fail(EnvConfig, fmt.Errorf("%w: ruralz-control:// selects Control mode; M1 serves file mode only", ErrPlanned))
		default:
			l.fail(EnvConfig, fmt.Errorf("%w: scheme %q", ErrSource, scheme))
		}
		return ""
	}
	return l.makeAbs(EnvConfig, v)
}

// urlScheme returns the scheme of v when v starts with an RFC 3986
// scheme followed by "://".
func urlScheme(v string) (string, bool) {
	scheme, _, ok := strings.Cut(v, "://")
	if !ok || scheme == "" {
		return "", false
	}
	for i := range len(scheme) {
		c := scheme[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case i > 0 && (c >= '0' && c <= '9' || c == '+' || c == '-' || c == '.'):
		default:
			return "", false
		}
	}
	return scheme, true
}

// logLevel parses RURALZ_LOG_LEVEL; the four names are lowercase.
func (l *loader) logLevel(v string) LogLevel {
	switch lv := LogLevel(v); lv {
	case LogDebug, LogInfo, LogWarn, LogError:
		return lv
	}
	l.fail(EnvLogLevel, fmt.Errorf("%w, got %q", ErrLogLevel, v))
	return ""
}

// absolute requires an absolute path and cleans it; "" stays "".
func (l *loader) absolute(setting, v string) string {
	if v == "" {
		return ""
	}
	if !filepath.IsAbs(v) {
		l.fail(setting, fmt.Errorf("%w: %q", ErrNotAbsolute, v))
		return ""
	}
	return filepath.Clean(v)
}

// makeAbs makes a path absolute against the working directory, like
// ruralz node drain does for --data-dir (spec 10 requirement 94).
func (l *loader) makeAbs(setting, v string) string {
	p, err := filepath.Abs(v)
	if err != nil {
		l.fail(setting, fmt.Errorf("%w: %w", ErrNotAbsolute, err))
		return ""
	}
	return p
}

// fetchAllow checks RURALZ_FETCH_ALLOW with egress.ParseAllow's grammar
// (spec 06 requirement 85): comma-separated entries, blanks trimmed, each
// an IP address or CIDR prefix without a zone, or env-proxy. A value of
// only blanks allows nothing.
func (l *loader) fetchAllow(v string) {
	if strings.TrimSpace(v) == "" {
		return
	}
	for i, raw := range strings.Split(v, ",") {
		e := strings.TrimSpace(raw)
		if err := checkAllowEntry(e); err != nil {
			l.fail(EnvFetchAllow, fmt.Errorf("%w %d (%q): %w", ErrFetchAllow, i+1, echo(e), err))
		}
	}
}

// envProxyEntry opts Bundle fetches into the proxy variables.
const envProxyEntry = "env-proxy"

// checkAllowEntry checks one allow-list entry.
func checkAllowEntry(e string) error {
	switch {
	case e == envProxyEntry:
		return nil
	case e == "":
		return errors.New("empty entry")
	case strings.Contains(e, "%"):
		return errors.New("addresses with a zone are not allowed")
	case strings.Contains(e, "/"):
		if _, err := netip.ParsePrefix(e); err != nil {
			return errors.New("not a CIDR prefix")
		}
	default:
		if _, err := netip.ParseAddr(e); err != nil {
			return errors.New("not an IP address, a CIDR prefix or " + envProxyEntry)
		}
	}
	return nil
}

// echo bounds an entry quoted in an error.
func echo(e string) string {
	const maxEcho = 64
	if len(e) > maxEcho {
		return e[:maxEcho] + "..."
	}
	return e
}
