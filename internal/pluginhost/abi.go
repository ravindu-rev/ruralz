// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pluginhost

import (
	pluginv1 "github.com/ravindu-rev/ruralz/internal/gen/proto/ruralz/plugin/v1"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// The numeric constants of Plugin ABI v1 (docs/architecture/05-wasm-
// plugin-system.md "Contract"). The ruralz.plugin.v1 proto package
// (api/proto/ruralz/plugin/v1/) defines them and every PDK generates its
// own from it; the host takes them from the generated Go package here, as
// the i32 values Host Functions return and read. The Phase results and
// rz_abi_version's value, which the proto does not define, are the WASM
// document's.

// ABI identification.
const (
	// abiModule is the import module of every Host Function, the value of
	// Plugin.spec.abi.
	abiModule = string(v1alpha1.PluginABIV1)
	// abiVersion is what rz_abi_version returns.
	abiVersion = 1
	// wasiModule is the import module of the WASI shim.
	wasiModule = "wasi_snapshot_preview1"
)

// Host Function return codes: negative only for errors.
const (
	errAbsent      = int32(pluginv1.Err_ERR_ABSENT)
	errPhase       = int32(pluginv1.Err_ERR_PHASE)
	errBudget      = int32(pluginv1.Err_ERR_BUDGET)
	errLimit       = int32(pluginv1.Err_ERR_LIMIT)
	errUnavailable = int32(pluginv1.Err_ERR_UNAVAILABLE)
	errInvalid     = int32(pluginv1.Err_ERR_INVALID)
	errDenied      = int32(pluginv1.Err_ERR_DENIED)
)

// Phase results of a Phase export: CONTINUE, RESPOND and END_STREAM; a
// negative result is "cannot decide".
const (
	resultContinue  int32 = 0
	resultRespond   int32 = 1
	resultEndStream int32 = 2
)

// log levels.
const (
	levelDebug = int32(pluginv1.Level_LEVEL_DEBUG)
	levelInfo  = int32(pluginv1.Level_LEVEL_INFO)
	levelWarn  = int32(pluginv1.Level_LEVEL_WARN)
	levelError = int32(pluginv1.Level_LEVEL_ERROR)
)

// crypto_digest and crypto_hmac algorithms.
const (
	algSHA256 = int32(pluginv1.Alg_ALG_SHA_256)
	algSHA384 = int32(pluginv1.Alg_ALG_SHA_384)
	algSHA512 = int32(pluginv1.Alg_ALG_SHA_512)
)

// request_info fields.
const (
	requestMethod = int32(pluginv1.RequestInfoField_REQUEST_INFO_FIELD_METHOD)
	requestScheme = int32(pluginv1.RequestInfoField_REQUEST_INFO_FIELD_SCHEME)
	requestHost   = int32(pluginv1.RequestInfoField_REQUEST_INFO_FIELD_HOST)
	requestPath   = int32(pluginv1.RequestInfoField_REQUEST_INFO_FIELD_PATH)
	requestQuery  = int32(pluginv1.RequestInfoField_REQUEST_INFO_FIELD_QUERY)
	requestSource = int32(pluginv1.RequestInfoField_REQUEST_INFO_FIELD_SOURCE_ADDRESS)
	requestRoute  = int32(pluginv1.RequestInfoField_REQUEST_INFO_FIELD_ROUTE)
)

// consumer_info fields.
const (
	consumerName   = int32(pluginv1.ConsumerInfoField_CONSUMER_INFO_FIELD_NAME)
	consumerTier   = int32(pluginv1.ConsumerInfoField_CONSUMER_INFO_FIELD_TIER)
	consumerTags   = int32(pluginv1.ConsumerInfoField_CONSUMER_INFO_FIELD_TAGS)
	consumerMethod = int32(pluginv1.ConsumerInfoField_CONSUMER_INFO_FIELD_AUTH_METHOD)
	consumerClaims = int32(pluginv1.ConsumerInfoField_CONSUMER_INFO_FIELD_CLAIMS)
)

// chunk_read directions.
const (
	directionToUpstream = int32(pluginv1.Direction_DIRECTION_CLIENT_TO_UPSTREAM)
	directionToClient   = int32(pluginv1.Direction_DIRECTION_UPSTREAM_TO_CLIENT)
)
