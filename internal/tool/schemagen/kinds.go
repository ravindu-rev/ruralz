// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"reflect"

	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// resourceTypes lists the ten kinds in the canonical resource order.
func resourceTypes() []reflect.Type {
	return []reflect.Type{
		reflect.TypeFor[v1alpha1.Gateway](),
		reflect.TypeFor[v1alpha1.Upstream](),
		reflect.TypeFor[v1alpha1.Plugin](),
		reflect.TypeFor[v1alpha1.Policy](),
		reflect.TypeFor[v1alpha1.Consumer](),
		reflect.TypeFor[v1alpha1.AIProvider](),
		reflect.TypeFor[v1alpha1.AIModel](),
		reflect.TypeFor[v1alpha1.Route](),
		reflect.TypeFor[v1alpha1.Environment](),
		reflect.TypeFor[v1alpha1.Cluster](),
	}
}

// policyConfigTypes lists the config type of every Policy type; each
// carries a +ruralz:policyType marker naming its type.
func policyConfigTypes() []reflect.Type {
	return []reflect.Type{
		reflect.TypeFor[v1alpha1.AuthJWTConfig](),
		reflect.TypeFor[v1alpha1.AuthAPIKeyConfig](),
		reflect.TypeFor[v1alpha1.AuthBasicConfig](),
		reflect.TypeFor[v1alpha1.AuthMTLSConfig](),
		reflect.TypeFor[v1alpha1.AuthzCELConfig](),
		reflect.TypeFor[v1alpha1.AuthzOPAConfig](),
		reflect.TypeFor[v1alpha1.AuthzCedarConfig](),
		reflect.TypeFor[v1alpha1.AuthzIPConfig](),
		reflect.TypeFor[v1alpha1.AuthzGeoIPConfig](),
		reflect.TypeFor[v1alpha1.RateLimitConfig](),
		reflect.TypeFor[v1alpha1.QuotaConfig](),
		reflect.TypeFor[v1alpha1.ValidationJSONSchemaConfig](),
		reflect.TypeFor[v1alpha1.CORSConfig](),
		reflect.TypeFor[v1alpha1.CacheConfig](),
		reflect.TypeFor[v1alpha1.HeadersConfig](),
		reflect.TypeFor[v1alpha1.TransformRequestConfig](),
		reflect.TypeFor[v1alpha1.TransformResponseConfig](),
		reflect.TypeFor[v1alpha1.AuthUpstreamOAuth2Config](),
		reflect.TypeFor[v1alpha1.AuthUpstreamSigV4Config](),
		reflect.TypeFor[v1alpha1.AITokenBudgetConfig](),
		reflect.TypeFor[v1alpha1.AISemanticCacheConfig](),
		reflect.TypeFor[v1alpha1.AIGuardrailConfig](),
		reflect.TypeFor[v1alpha1.PluginConfig](),
	}
}
