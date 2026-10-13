package provider

import "context"

type Capabilities struct {
	SupportsReasoning           bool
	SupportsReasoningDetails    bool
	SupportsImages              bool
	SupportsToolCalls           bool
	SupportsPromptCache         bool
	PromptCacheEnabledByDefault bool
}

type CapabilityReporter interface {
	Capabilities() Capabilities
}

type ToolResultImagesConverter interface {
	SetToolResultImagesAsUser(bool)
}

type UsageReporter interface {
	FetchUsage(ctx context.Context) (UsageInfo, error)
}

type ModelFetcher interface {
	FetchModels(ctx context.Context) ([]ModelInfo, error)
}
