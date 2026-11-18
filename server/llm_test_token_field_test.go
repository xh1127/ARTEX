package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Autumn-27/artex/db"
)

func TestConnectionTestMaxTokensField(t *testing.T) {
	d := requestHeadersDatabase(t)
	id, err := d.SaveProfile(&db.LLMProfile{Name: t.Name(), Format: "openai", Model: "test", APIKey: "test-key", MaxTokensField: "max_completion_tokens"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM llm_profiles WHERE id=$1`, id) })
	s := &Server{m: &Manager{pg: d}}
	for _, tc := range []struct {
		name, value, want string
		include, fresh    bool
	}{
		{"saved-fallback", "", "max_completion_tokens", false, false},
		{"explicit-new-field", "max_completion_tokens", "max_completion_tokens", true, false},
		{"explicit-default", "", "max_tokens", true, false},
		{"invalid-normalized", "unknown", "max_tokens", true, false},
		{"unsaved-profile", "max_completion_tokens", "max_completion_tokens", true, true},
		{"legacy-default", "", "max_tokens", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				var body map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				other := "max_tokens"
				if tc.want == other {
					other = "max_completion_tokens"
				}
				if body[tc.want] == nil || body[other] != nil {
					http.Error(w, "unsupported output token field", http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`)
			}))
			defer upstream.Close()
			body := map[string]any{"provider": "openai", "model": "test", "base_url": upstream.URL, "streaming": false}
			if tc.fresh {
				body["api_key"] = "test-key"
			} else {
				body["profile_id"] = id
			}
			if tc.include {
				body["max_tokens_field"] = tc.value
			}
			w := callUserAgentHandler(t, s.testLLM, body)
			var result struct {
				OK bool `json:"ok"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != http.StatusOK || !result.OK || calls != 1 {
				t.Fatalf("connection test failed: calls=%d response=%s err=%v", calls, w.Body, err)
			}
		})
	}
}
