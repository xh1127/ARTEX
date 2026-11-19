package server

import (
	"testing"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/norma/llm"
)

// The DeepSeek credential lookup compares case-sensitively while the settings
// page's test button and the agent-side builders compare case-insensitively, so
// a stored "DeepSeek" used to pass the test and then resolve no credentials on
// every real search. Normalize on both the write and read paths.
func TestNormalizeWebSearchBackend(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", "ddgs"},
		{"   ", "ddgs"},
		{"ddgs", "ddgs"},
		{"DeepSeek", "deepseek"},
		{"DEEPSEEK", "deepseek"},
		{"  deepseek  ", "deepseek"},
		{"brave-free", "brave-free"},
		{"Tavily", "tavily"},
		{"unknown-backend", "unknown-backend"},
	} {
		if got := normalizeWebSearchBackend(tc.in); got != tc.want {
			t.Errorf("normalizeWebSearchBackend(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Search borrows the credentials of the profile the caller runs on, but only
// when that profile can serve the backend: it speaks the Anthropic Messages API.
// An OpenAI-format profile (or one missing key/base/model) must leave the
// fallback — the globally active profile's credentials — untouched.
func TestMergeSearchCredsOnlyBorrowsAnthropicProfiles(t *testing.T) {
	fallback := agent.WebSearchOpts{
		Enabled: true, Backend: deepSeekWebSearchBackend,
		DeepSeekBaseURL: "https://api.deepseek.com/anthropic", DeepSeekAPIKey: "active-key",
		DeepSeekModel: "deepseek-chat", DeepSeekUserAgent: "active/1.0",
	}
	bound := agent.Config{Format: llm.FormatAnthropic, BaseURL: "https://bound.example/anthropic", APIKey: "bound-key", Model: "bound-model", UserAgent: "bound/2.0"}

	got := mergeSearchCreds(fallback, bound)
	if got.DeepSeekBaseURL != bound.BaseURL || got.DeepSeekAPIKey != bound.APIKey || got.DeepSeekModel != bound.Model || got.DeepSeekUserAgent != bound.UserAgent {
		t.Fatalf("anthropic profile was not borrowed: %+v", got)
	}

	for name, cfg := range map[string]agent.Config{
		"openai format":    {Format: llm.FormatOpenAI, BaseURL: "https://api.openai.com/v1", APIKey: "k", Model: "gpt"},
		"responses format": {Format: llm.FormatOpenAIResponses, BaseURL: "https://api.openai.com/v1", APIKey: "k", Model: "gpt"},
		"missing key":      {Format: llm.FormatAnthropic, BaseURL: "https://bound.example", Model: "m"},
		"missing base":     {Format: llm.FormatAnthropic, APIKey: "k", Model: "m"},
		"missing model":    {Format: llm.FormatAnthropic, BaseURL: "https://bound.example", APIKey: "k"},
	} {
		if got := mergeSearchCreds(fallback, cfg); got != fallback {
			t.Errorf("%s: fallback credentials were replaced: %+v", name, got)
		}
	}

	other := fallback
	other.Backend = "ddgs"
	if got := mergeSearchCreds(other, bound); got != other {
		t.Errorf("non-deepseek backend must be left untouched: %+v", got)
	}
}
