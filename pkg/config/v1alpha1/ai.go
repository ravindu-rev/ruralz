// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

// AIDialect is the wire dialect of an LLM provider.
type AIDialect string

// AI dialects.
const (
	// AIDialectOpenAI is the OpenAI API.
	AIDialectOpenAI AIDialect = "openai"
	// AIDialectAnthropic is the Anthropic API.
	AIDialectAnthropic AIDialect = "anthropic"
	// AIDialectGemini is the Gemini API.
	AIDialectGemini AIDialect = "gemini"
	// AIDialectBedrock is Amazon Bedrock.
	AIDialectBedrock AIDialect = "bedrock"
	// AIDialectMistral is the Mistral API.
	AIDialectMistral AIDialect = "mistral"
	// AIDialectOllama is Ollama; baseUrl is required and credentials are optional.
	AIDialectOllama AIDialect = "ollama"
)

// AIProviderSpec is the spec of an AIProvider. Planned (M3).
type AIProviderSpec struct {
	// Dialect is the provider's wire dialect.
	// +ruralz:required
	Dialect AIDialect `json:"dialect"`
	// BaseURL overrides the dialect's default base URL; required for ollama.
	BaseURL string `json:"baseUrl,omitempty"`
	// Credentials authenticate to the provider; required except for ollama.
	Credentials *AICredentials `json:"credentials,omitempty"`
	// Region is used for data residency routing.
	Region string `json:"region,omitempty"`
	// Pricing converts provider-reported usage to currency for cost attribution.
	Pricing *Pricing `json:"pricing,omitempty"`
}

// AICredentials authenticate to an LLM provider.
type AICredentials struct {
	// APIKey is the provider API key.
	// +ruralz:secret
	APIKey *SecretValue `json:"apiKey,omitempty"`
}

// Pricing is a provider's price table.
type Pricing struct {
	// Currency is an ISO 4217 code.
	// +ruralz:required
	// +ruralz:pattern=^[A-Z]{3}$
	Currency string `json:"currency"`
	// Version names the price table.
	// +ruralz:required
	Version string `json:"version"`
	// Models are per-model prices.
	// +ruralz:list=map,key=model
	Models []ModelPrice `json:"models,omitempty"`
}

// ModelPrice is the price of one provider model, per million tokens.
type ModelPrice struct {
	// Model is the provider model identifier.
	// +ruralz:required
	Model string `json:"model"`
	// InputPerMillionTokens is the input token price.
	InputPerMillionTokens *Decimal `json:"inputPerMillionTokens,omitempty"`
	// OutputPerMillionTokens is the output token price.
	OutputPerMillionTokens *Decimal `json:"outputPerMillionTokens,omitempty"`
	// CachedInputPerMillionTokens is the cached input token price.
	CachedInputPerMillionTokens *Decimal `json:"cachedInputPerMillionTokens,omitempty"`
}

// AIModelStrategy selects among AIModel candidates.
type AIModelStrategy string

// AIModel strategies.
const (
	// AIModelStrategyWeighted splits by candidate weight.
	AIModelStrategyWeighted AIModelStrategy = "weighted"
	// AIModelStrategyLatency prefers the fastest candidate.
	AIModelStrategyLatency AIModelStrategy = "latency"
	// AIModelStrategyCost prefers the cheapest candidate.
	AIModelStrategyCost AIModelStrategy = "cost"
	// AIModelStrategyFallback follows list order (Provider Fallback).
	AIModelStrategyFallback AIModelStrategy = "fallback"
)

// AIModelSpec is the spec of an AIModel. Planned (M3).
type AIModelSpec struct {
	// Strategy selects among candidates.
	// +ruralz:required
	Strategy AIModelStrategy `json:"strategy"`
	// Candidates are provider models in fallback order.
	// +ruralz:required
	// +ruralz:minItems=1
	// +ruralz:list=orderedMap,key=name
	// +ruralz:impact=ai
	Candidates []AIModelCandidate `json:"candidates"`
	// Limits cap tokens per request.
	Limits *AIModelLimits `json:"limits,omitempty"`
	// Cache configures the Prompt Cache and Semantic Cache parameters.
	Cache *AIModelCache `json:"cache,omitempty"`
}

// AIModelCandidate is one provider model.
type AIModelCandidate struct {
	// Name identifies the candidate.
	// +ruralz:required
	Name string `json:"name"`
	// Provider is the metadata.name of an AIProvider.
	// +ruralz:required
	// +ruralz:ref=AIProvider
	Provider string `json:"provider"`
	// Model is the provider's model identifier.
	// +ruralz:required
	Model string `json:"model"`
	// Weight applies to the weighted strategy only.
	// +ruralz:minimum=0
	Weight *int32 `json:"weight,omitempty"`
	// When skips the candidate when false.
	// +ruralz:cel=request,source,route,consumer,auth,now,ai:bool
	When string `json:"when,omitempty"`
}

// AIModelLimits cap tokens per request.
type AIModelLimits struct {
	// MaxInputTokens caps estimated input tokens (RZ-AI-003).
	// +ruralz:minimum=1
	MaxInputTokens *int64 `json:"maxInputTokens,omitempty"`
	// MaxOutputTokens caps output tokens; required when a Route with ai.token-budget reaches this model (RZ-CFG-034).
	// +ruralz:minimum=1
	MaxOutputTokens *int64 `json:"maxOutputTokens,omitempty"`
}

// PromptCacheMode is how provider Prompt Cache markers are handled.
type PromptCacheMode string

// Prompt Cache modes.
const (
	// PromptCacheModePassthrough keeps Prompt Cache markers.
	PromptCacheModePassthrough PromptCacheMode = "passthrough"
	// PromptCacheModeDisabled removes Prompt Cache markers.
	PromptCacheModeDisabled PromptCacheMode = "disabled"
)

// AIModelCache configures caching for an AIModel.
type AIModelCache struct {
	// Prompt configures the provider-side Prompt Cache.
	Prompt *PromptCache `json:"prompt,omitempty"`
	// Semantic holds Semantic Cache parameters; a Route enables it with an ai.semantic-cache Policy.
	Semantic *SemanticCache `json:"semantic,omitempty"`
}

// PromptCache configures the provider-side Prompt Cache.
type PromptCache struct {
	// Mode is passthrough or disabled.
	Mode *PromptCacheMode `json:"mode,omitempty"`
}

// SemanticCache holds Semantic Cache parameters.
type SemanticCache struct {
	// SimilarityThreshold is the minimum similarity, 0 to 1, for a hit.
	// +ruralz:minimum=0
	// +ruralz:maximum=1
	SimilarityThreshold *float64 `json:"similarityThreshold,omitempty"`
	// TTL is how long an entry lives.
	TTL *Duration `json:"ttl,omitempty"`
}
