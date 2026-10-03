// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package resolver

import (
	"errors"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/secret"
)

// Fixed reasons of the env provider and of the provider switch.
var (
	errEnvName  = errors.New("env name must be " + EnvStateStoreURL + " or start with " + EnvSecretPrefix)
	errEnvUnset = errors.New("environment variable is not set")
	errEnvEmpty = errors.New("environment variable is empty")
	errPlanned  = errors.New("provider Planned (M2), not supported by this Node")
	errProvider = errors.New("unknown provider")
)

// envAllowed reports whether name may be resolved by the env provider
// (spec 01 requirement 44, spec 06 requirement 88).
func envAllowed(name string) bool {
	return name == EnvStateStoreURL || strings.HasPrefix(name, EnvSecretPrefix)
}

// readEnv resolves an env reference; key is ignored, and the value never
// rotates (spec 01 requirement 46: env needs a restart).
func (r *Resolver) readEnv(ref secret.Ref) ([]byte, error) {
	if !envAllowed(ref.Name) {
		return nil, errEnvName
	}
	v, ok := r.lookupEnv(ref.Name)
	switch {
	case !ok:
		return nil, errEnvUnset
	case v == "":
		return nil, errEnvEmpty
	}
	return []byte(v), nil
}
