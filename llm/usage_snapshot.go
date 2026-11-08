package llm

import (
	"bytes"
	"encoding/json"
)

// Unknown or malformed extensions must not invalidate otherwise valid usage.
func aliasToken(raw json.RawMessage) *int {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	var value int
	if json.Unmarshal(raw, &value) != nil || value < 0 {
		return nil
	}
	return &value
}

// A present zero is authoritative, not a missing field. Aliases are alternatives,
// never additive counters (a gateway may echo the same usage in several places).
func firstTokenField(fields ...*int) int {
	for _, value := range fields {
		if value != nil {
			return *value
		}
	}
	return 0
}

// Read only the confirmed cache-read extension; do not infer input/output.
func gatewayCacheRead(raw json.RawMessage) *int {
	var usage struct {
		Tokens struct {
			CacheRead json.RawMessage `json:"cache_read"`
		} `json:"tokens"`
	}
	if json.Unmarshal(raw, &usage) != nil {
		return nil
	}
	return aliasToken(usage.Tokens.CacheRead)
}

type openaiUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	PromptTokensDetails struct {
		CachedTokens *int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	PromptCacheHitTokens json.RawMessage `json:"prompt_cache_hit_tokens"`
	Opencode             json.RawMessage `json:"opencode"`
}

func (u openaiUsage) norm() Usage {
	return Usage{
		InputTokens: u.PromptTokens, OutputTokens: u.CompletionTokens,
		CacheReadTokens: firstTokenField(u.PromptTokensDetails.CachedTokens, aliasToken(u.PromptCacheHitTokens), gatewayCacheRead(u.Opencode)),
	}
}

// OpenAI usage frames are cumulative snapshots. Merge per field so an omitted,
// repeated or stale field cannot erase a counter. The stream emits the final
// snapshot once; unlike Anthropic streaming, these are not incremental events.
func mergeUsageSnapshot(previous, next Usage) Usage {
	return Usage{
		InputTokens:      max(previous.InputTokens, next.InputTokens),
		OutputTokens:     max(previous.OutputTokens, next.OutputTokens),
		CacheReadTokens:  max(previous.CacheReadTokens, next.CacheReadTokens),
		CacheWriteTokens: max(previous.CacheWriteTokens, next.CacheWriteTokens),
	}
}
