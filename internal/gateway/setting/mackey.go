// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package setting

import (
	"fmt"
	"io"
	"log/slog"
	"os"
)

// MAC key size bounds (spec 08 requirement 68; R-49).
const (
	// MinMACKeyBytes is the shortest accepted MAC key.
	MinMACKeyBytes = 32
	// MaxMACKeyBytes bounds the read of the MAC key file.
	MaxMACKeyBytes = 64 << 10
)

// redacted replaces the key in every text form (R-9).
const redacted = "[REDACTED]"

// MACKey is the State Store entry MAC key (HMAC-SHA-256, spec 08
// requirement 68), passed through statestore/manager into
// statestore.Deps.MACKey. The zero value is unset. fmt verbs, JSON and
// slog print "[REDACTED]", never the key.
type MACKey struct {
	b []byte
}

// IsSet reports whether a key was configured.
func (k MACKey) IsSet() bool { return len(k.b) > 0 }

// Len returns the key length in bytes.
func (k MACKey) Len() int { return len(k.b) }

// Bytes returns a copy of the key; nil when unset.
func (k MACKey) Bytes() []byte {
	if len(k.b) == 0 {
		return nil
	}
	return append([]byte(nil), k.b...)
}

// String returns "[REDACTED]".
func (MACKey) String() string { return redacted }

// GoString returns "[REDACTED]".
func (MACKey) GoString() string { return redacted }

// Format writes "[REDACTED]" for every verb.
func (MACKey) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, redacted) }

// MarshalJSON returns the JSON string "[REDACTED]".
func (MACKey) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

// MarshalText returns "[REDACTED]".
func (MACKey) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// LogValue returns "[REDACTED]" (slog.LogValuer).
func (MACKey) LogValue() slog.Value { return slog.StringValue(redacted) }

// readMACKey reads RURALZ_STATE_STORE_MAC_KEY_FILE at start (R-49): a
// regular file (symbolic links followed) owned by the effective user or
// root that grants nothing to group or others, of at most MaxMACKeyBytes
// bytes. The key is the content without one final "\n" or "\r\n", so a
// key written by a shell tool and the same key mounted without a newline
// give every Node the same HMAC (spec 08 requirement 68: a different key
// turns every shared entry into a silent miss); it must then hold at
// least MinMACKeyBytes bytes. The open never blocks on a FIFO or device,
// and the checks use the opened file, so the checked file is the read
// file. Errors never quote the content.
func readMACKey(path string) (MACKey, error) {
	f, err := openKeyFile(path)
	if err != nil {
		return MACKey{}, fmt.Errorf("%w: %w", ErrMACKeyFile, err)
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return MACKey{}, fmt.Errorf("%w: %w", ErrMACKeyFile, err)
	}
	if !fi.Mode().IsRegular() {
		return MACKey{}, fmt.Errorf("%w: %s has mode %v", ErrMACKeyFile, path, fi.Mode())
	}
	if err := checkOwner(fi, os.Geteuid()); err != nil {
		return MACKey{}, fmt.Errorf("%w: %s is %w", ErrMACKeyOwner, path, err)
	}
	if !ownerOnly(fi.Mode()) {
		return MACKey{}, fmt.Errorf("%w: %s has mode %v; chmod 0600 or 0400", ErrMACKeyMode, path, fi.Mode().Perm())
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxMACKeyBytes+1))
	if err != nil {
		return MACKey{}, fmt.Errorf("%w: %w", ErrMACKeyFile, err)
	}
	if len(b) > MaxMACKeyBytes {
		clear(b)
		return MACKey{}, fmt.Errorf("%w: %s", ErrMACKeyLong, path)
	}
	key, trimmed := trimFinalNewline(b)
	if len(key) < MinMACKeyBytes {
		clear(b)
		note := ""
		if trimmed {
			note = " besides its final newline"
		}
		return MACKey{}, fmt.Errorf("%w: %s holds %d bytes%s", ErrMACKeyShort, path, len(key), note)
	}
	return MACKey{b: key}, nil
}

// trimFinalNewline returns b without one final "\n" or "\r\n", and
// whether it removed one.
func trimFinalNewline(b []byte) ([]byte, bool) {
	n := len(b)
	if n == 0 || b[n-1] != '\n' {
		return b, false
	}
	n--
	if n > 0 && b[n-1] == '\r' {
		n--
	}
	return b[:n], true
}
