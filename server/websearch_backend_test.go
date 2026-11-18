package server

import "testing"

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
