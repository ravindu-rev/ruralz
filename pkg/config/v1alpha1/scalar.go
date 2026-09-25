// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Duration is a non-negative duration in Go syntax, such as 50ms, 1m30s or
// 24h. It encodes in the canonical form of time.Duration.String, so 90s
// becomes 1m30s.
type Duration time.Duration

// MarshalJSON encodes the duration as its canonical string.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON decodes a Go duration string.
func (d *Duration) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("duration must be a string such as \"50ms\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	if v < 0 {
		return fmt.Errorf("invalid duration %q: must not be negative", s)
	}
	*d = Duration(v)
	return nil
}

// ByteSize is a number of bytes, written as an integer or a Kubernetes
// quantity such as 64Ki, 10Mi or 1Gi. It encodes as an integer, so 10Mi
// becomes 10485760.
type ByteSize int64

// MarshalJSON encodes the size as an integer number of bytes.
func (b ByteSize) MarshalJSON() ([]byte, error) {
	return []byte(strconv.FormatInt(int64(b), 10)), nil
}

// UnmarshalJSON decodes an integer or a quantity string.
func (b *ByteSize) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) > 0 && data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		v, err := ParseByteSize(s)
		if err != nil {
			return err
		}
		*b = v
		return nil
	}
	v, err := strconv.ParseInt(string(data), 10, 64)
	if err != nil {
		return fmt.Errorf("byte size must be an integer or a quantity such as \"10Mi\": %s", data)
	}
	if v < 0 {
		return fmt.Errorf("byte size %d must not be negative", v)
	}
	*b = ByteSize(v)
	return nil
}

// quantityMultiplier maps a Kubernetes quantity suffix to its multiplier.
func quantityMultiplier(suffix string) (int64, bool) {
	switch suffix {
	case "":
		return 1, true
	case "k":
		return 1_000, true
	case "M":
		return 1_000_000, true
	case "G":
		return 1_000_000_000, true
	case "T":
		return 1_000_000_000_000, true
	case "P":
		return 1_000_000_000_000_000, true
	case "E":
		return 1_000_000_000_000_000_000, true
	case "Ki":
		return 1 << 10, true
	case "Mi":
		return 1 << 20, true
	case "Gi":
		return 1 << 30, true
	case "Ti":
		return 1 << 40, true
	case "Pi":
		return 1 << 50, true
	case "Ei":
		return 1 << 60, true
	default:
		return 0, false
	}
}

// ParseByteSize parses an integer or a Kubernetes quantity with a decimal
// (k, M, G, T, P, E) or binary (Ki, Mi, Gi, Ti, Pi, Ei) suffix. A fraction,
// as in 1.5Gi, is allowed when the result is a whole number of bytes.
func ParseByteSize(s string) (ByteSize, error) {
	num := strings.TrimRight(s, "kKMGTPEi")
	suffix := s[len(num):]
	mult, ok := quantityMultiplier(suffix)
	if !ok || num == "" {
		return 0, fmt.Errorf("invalid byte size %q: want an integer or a quantity such as 10Mi", s)
	}
	whole, frac, hasFrac := strings.Cut(num, ".")
	if whole == "" || !allDigits(whole) || (hasFrac && (frac == "" || !allDigits(frac))) {
		return 0, fmt.Errorf("invalid byte size %q: want an integer or a quantity such as 10Mi", s)
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || w > math.MaxInt64/mult {
		return 0, fmt.Errorf("byte size %q is too large", s)
	}
	total := w * mult
	if hasFrac {
		// Scale the fraction exactly: frac/10^len(frac) * mult must be whole.
		scale := int64(1)
		for range frac {
			if scale > math.MaxInt64/10 {
				return 0, fmt.Errorf("byte size %q has too many fraction digits", s)
			}
			scale *= 10
		}
		f, err := strconv.ParseInt(frac, 10, 64)
		if err != nil || (f != 0 && mult > math.MaxInt64/f) {
			return 0, fmt.Errorf("byte size %q is too large", s)
		}
		if (f*mult)%scale != 0 {
			return 0, fmt.Errorf("byte size %q is not a whole number of bytes", s)
		}
		add := f * mult / scale
		if total > math.MaxInt64-add {
			return 0, fmt.Errorf("byte size %q is too large", s)
		}
		total += add
	}
	return ByteSize(total), nil
}

func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// IntOrString holds either an integer or a string, such as a port number or
// a port name.
type IntOrString struct {
	// Int is the value when IsString is false.
	Int int32
	// Str is the value when IsString is true.
	Str string
	// IsString reports which of Int and Str holds the value.
	IsString bool
}

// MarshalJSON encodes whichever value is set.
func (v IntOrString) MarshalJSON() ([]byte, error) {
	if v.IsString {
		return json.Marshal(v.Str)
	}
	return json.Marshal(v.Int)
}

// UnmarshalJSON decodes an integer or a string.
func (v *IntOrString) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) > 0 && data[0] == '"' {
		*v = IntOrString{IsString: true}
		return json.Unmarshal(data, &v.Str)
	}
	var n int32
	if err := json.Unmarshal(data, &n); err != nil {
		return errors.New("value must be an integer or a string")
	}
	*v = IntOrString{Int: n}
	return nil
}

// Decimal is a non-negative decimal number written as a string, such as a
// price, so values never drift through floating point.
// +ruralz:pattern=^[0-9]+(\.[0-9]+)?$
type Decimal string

// SecretProvider names where a secret value is resolved.
type SecretProvider string

// Secret providers.
const (
	// SecretProviderEnv reads a process environment variable named by name.
	SecretProviderEnv SecretProvider = "env"
	// SecretProviderFile reads the file at the absolute path name; key selects a JSON member.
	SecretProviderFile SecretProvider = "file"
	// SecretProviderKubernetes reads a Secret in the Node's namespace; key is the data key.
	SecretProviderKubernetes SecretProvider = "kubernetes"
	// SecretProviderVault reads the secret path name from Vault; key is the field.
	SecretProviderVault SecretProvider = "vault"
)

// SecretRef points to a secret held outside the Bundle.
type SecretRef struct {
	// Provider resolves the secret: env and file are Planned (M1), kubernetes and vault Planned (M2).
	// +ruralz:required
	Provider SecretProvider `json:"provider"`
	// Name is the variable, absolute file path, Kubernetes Secret or Vault path.
	// +ruralz:required
	// +ruralz:minLength=1
	Name string `json:"name"`
	// Key selects a JSON member, Secret data key or Vault field; unused for env.
	Key string `json:"key,omitempty"`
}

// SecretValue is a secret field: only a secretRef is accepted, a literal is
// RZ-CFG-012, and the value is never rendered.
type SecretValue struct {
	// SecretRef points to the secret.
	// +ruralz:required
	SecretRef SecretRef `json:"secretRef"`
}

// PolicyRef names a Policy in the same rendered Bundle.
type PolicyRef struct {
	// Name is the metadata.name of a Policy.
	// +ruralz:required
	// +ruralz:ref=Policy
	Name string `json:"name"`
}
