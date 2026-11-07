// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pluginhost

import (
	"errors"
	"fmt"

	"github.com/ravindu-rev/ruralz/internal/errcode"
)

// RZ-PLG codes of a Phase call and of the pools
// (docs/architecture/05-wasm-plugin-system.md "Failure semantics and the
// RZ-PLG registry").
const (
	// CodeTrap is a guest trap, or a host panic recovered at the boundary.
	CodeTrap = "RZ-PLG-001"
	// CodeDeadline is a call past its wall-clock deadline.
	CodeDeadline = "RZ-PLG-002"
	// CodeMemory is the linear memory limit reached.
	CodeMemory = "RZ-PLG-003"
	// CodeCapRefused is no idle instance within the wait while the pool
	// manager was refused at the Node cap or the pool share.
	CodeCapRefused = "RZ-PLG-004"
	// CodeNoInstance is no idle instance within the wait.
	CodeNoInstance = "RZ-PLG-005"
	// CodeMisuse is Host Function misuse, a sticky error whatever the guest
	// returns: wrong Phase, exhausted budget, second blocking State Store
	// call, ERR_DENIED.
	CodeMisuse = "RZ-PLG-006"
	// CodeInvalidResult is an invalid guest result.
	CodeInvalidResult = "RZ-PLG-007"
	// CodeBackground is a background instantiation or rz_configure
	// failure.
	CodeBackground = "RZ-PLG-008"
	// CodeBreakerOpen is the fault breaker open: the guest was not called.
	CodeBreakerOpen = "RZ-PLG-009"
	// CodeParkedBound is the parked bound reached before the State Store
	// call, and the guest returned cannot-decide.
	CodeParkedBound = "RZ-PLG-010"
	// CodeUndecided is the guest's cannot-decide without a preceding
	// ERR_UNAVAILABLE or parked-bound refusal.
	CodeUndecided = "RZ-PLG-011"
)

// RZ-CFG codes of Plugin activation: one per failure kind (the online
// stage, ingest and Node activation), RZ-CFG-028 keeping a digest
// mismatch, an unavailable artifact and an artifact that disagrees with
// its Plugin.spec (docs/architecture/02-configuration-model.md "Error
// codes").
const (
	// CodeArtifact is an artifact that is unavailable, does not hash to
	// its digest, or disagrees with its Plugin.spec: ABI, exports, Phases,
	// an import the host does not implement, or a declared memory above
	// limits.memoryBytes.
	CodeArtifact = "RZ-CFG-028"
	// CodeSignature is a Plugin signature that fails verification.
	CodeSignature = "RZ-CFG-033"
	// CodeCompile is an artifact wazero does not compile.
	CodeCompile = "RZ-CFG-047"
	// CodeStructure is an artifact over the size or structure limits.
	CodeStructure = "RZ-CFG-048"
	// CodeCompiledLimit is a Revision whose accounted compiled code alone
	// exceeds the compiled-code limit per Node.
	CodeCompiledLimit = "RZ-CFG-049"
	// CodeCapability is an artifact whose imports request a Capability
	// spec.capabilities does not grant.
	CodeCapability = "RZ-CFG-050"
	// CodeConfigRejected is rz_configure refusing a Policy's config.
	CodeConfigRejected = "RZ-CFG-051"
	// CodeReservation is a Revision whose Plugin pools can never fit the
	// Node cap: a warm reservation that cannot fit, or a limits.memoryBytes
	// above the pool share.
	CodeReservation = "RZ-CFG-052"
)

// failure is an activation failure: its RZ-CFG code and whether it is
// transient, which a NACK carries beside the code and the file-mode loader
// retries (docs/architecture/05-wasm-plugin-system.md "Failure semantics
// and the RZ-PLG registry"): a registry outage, a timeout and Node-local
// compiled-code pressure are transient, until the online stage reproduces
// them; every other failure reproduces from the Revision.
type failure struct {
	code      string
	transient bool
	err       error
}

func (f *failure) Error() string { return f.err.Error() }
func (f *failure) Unwrap() error { return f.err }

// Transient reports a failure the Node retries, which a NACK flags.
func (f *failure) Transient() bool { return f.transient }

// fail returns a deterministic activation failure of code.
func fail(code, format string, args ...any) error {
	return errcode.Wrap(code, &failure{code: code, err: fmt.Errorf(format, args...)})
}

// failTransient returns a transient activation failure of code.
func failTransient(code, format string, args ...any) error {
	return errcode.Wrap(code, &failure{code: code, transient: true, err: fmt.Errorf(format, args...)})
}

// IsTransient reports whether err is a transient activation failure: the
// Node retries it with backoff, and a NACK carries class transient.
func IsTransient(err error) bool {
	var t interface{ Transient() bool }
	return errors.As(err, &t) && t.Transient()
}
