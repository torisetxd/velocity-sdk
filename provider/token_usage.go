package provider

const UsageMetadataKey = "usage"

type TokenUsage struct {
	InputTokens      int `json:"input_tokens,omitempty"`
	OutputTokens     int `json:"output_tokens,omitempty"`
	TotalTokens      int `json:"total_tokens,omitempty"`
	CachedTokens     int `json:"cached_tokens,omitempty"`
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
}

type tokenUsageDetails struct {
	CachedTokens     int `json:"cached_tokens"`
	CacheReadTokens  int `json:"cache_read_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
}

type tokenUsagePayload struct {
	PromptTokens             int               `json:"prompt_tokens"`
	CompletionTokens         int               `json:"completion_tokens"`
	InputTokens              int               `json:"input_tokens"`
	OutputTokens             int               `json:"output_tokens"`
	TotalTokens              int               `json:"total_tokens"`
	CacheReadInputTokens     int               `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int               `json:"cache_creation_input_tokens"`
	PromptTokensDetails      tokenUsageDetails `json:"prompt_tokens_details"`
	InputTokensDetails       tokenUsageDetails `json:"input_tokens_details"`
}

func (u tokenUsagePayload) normalized() *TokenUsage {
	usage := TokenUsage{
		InputTokens:      firstPositive(u.InputTokens, u.PromptTokens),
		OutputTokens:     firstPositive(u.OutputTokens, u.CompletionTokens),
		TotalTokens:      u.TotalTokens,
		CachedTokens:     firstPositive(u.PromptTokensDetails.CachedTokens, u.InputTokensDetails.CachedTokens, u.PromptTokensDetails.CacheReadTokens, u.InputTokensDetails.CacheReadTokens, u.CacheReadInputTokens),
		CacheWriteTokens: firstPositive(u.PromptTokensDetails.CacheWriteTokens, u.InputTokensDetails.CacheWriteTokens, u.CacheCreationInputTokens),
	}
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.InputTokens + usage.OutputTokens
	}
	if usage == (TokenUsage{}) {
		return nil
	}
	return &usage
}

func TokenUsageMetadata(usage *TokenUsage) map[string]any {
	if usage == nil {
		return nil
	}
	values := map[string]any{}
	if usage.InputTokens > 0 {
		values["input_tokens"] = usage.InputTokens
	}
	if usage.OutputTokens > 0 {
		values["output_tokens"] = usage.OutputTokens
	}
	if usage.TotalTokens > 0 {
		values["total_tokens"] = usage.TotalTokens
	}
	if usage.CachedTokens > 0 {
		values["cached_tokens"] = usage.CachedTokens
	}
	if usage.CacheWriteTokens > 0 {
		values["cache_write_tokens"] = usage.CacheWriteTokens
	}
	if len(values) == 0 {
		return nil
	}
	return map[string]any{UsageMetadataKey: values}
}

func firstPositive(values ...int) int {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}
