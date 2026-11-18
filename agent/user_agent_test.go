package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
)

func TestUserAgentTransportPreservesRequestAndDefaults(t *testing.T) {
	req := newReq(transcript.WithSessionID(context.Background(), "conv-42"))
	req.Header.Set("User-Agent", "sdk-default/1.0")
	for _, ua := range []string{"custom-agent/1.0", "other-agent/2.0", ""} {
		base := &fakeRT{}
		rt := quotaAwareTransport{base: base, userAgent: ua, sessionHeaderKey: "x-session-id"}
		resp, err := rt.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		want := ua
		if want == "" {
			want = "sdk-default/1.0"
		}
		if got := base.seen.UserAgent(); got != want {
			t.Fatalf("User-Agent = %q, want %q", got, want)
		}
		if base.seen.Header.Get("x-session-id") != "conv-42" {
			t.Fatal("lost session header")
		}
		if req.UserAgent() != "sdk-default/1.0" || req.Header.Get("x-session-id") != "" {
			t.Fatal("transport mutated original request headers")
		}
	}
}

func TestUserAgentValidation(t *testing.T) {
	for _, value := range []string{"agent\r\nInjected: yes", "agent\n", "\ragent", "agent\x00", "agent\x7f", "agent\t", "中文", strings.Repeat("a", 513)} {
		cfg := ConfigFrom("openai", "test", "", "key", "")
		cfg.UserAgent = value
		if _, err := cfg.NewProvider(); err == nil {
			t.Errorf("accepted invalid UA %q", value)
		}
	}
	for _, value := range []string{"", "   ", " custom/1.0 (example; cli) ", strings.Repeat("a", 512)} {
		got, err := NormalizeUserAgent(value)
		if err != nil || got != strings.TrimSpace(value) {
			t.Errorf("normalize %q: got %q, err %v", value, got, err)
		}
	}
}

// Check what a real HTTP server sees after Norma has constructed its request,
// including a retry. Each provider and streaming mode shares the same override.
func TestUserAgentThroughProviders(t *testing.T) {
	fixtures := []struct {
		format string
		json   string
		sse    string
	}{
		{"anthropic",
			`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`,
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"usage\":{\"input_tokens\":1}}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"OK\"}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"},
		{"openai",
			`{"choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`,
			"data: {\"choices\":[{\"delta\":{\"content\":\"OK\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"},
		{"openai-responses",
			`{"id":"resp_1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`,
			"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"delta\":\"OK\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"},
	}
	for _, f := range fixtures {
		for _, stream := range []bool{true, false} {
			for _, ua := range []string{"custom-agent/1.0", ""} {
				t.Run(fmt.Sprintf("%s/stream=%v/ua=%s", f.format, stream, ua), func(t *testing.T) {
					calls := 0
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						calls++
						if got := r.UserAgent(); (ua != "" && got != ua) || (ua == "" && got == "") {
							t.Errorf("wire User-Agent = %q, configured %q", got, ua)
						}
						if r.Header.Get("x-session-id") != "conv-ua-test" {
							t.Error("missing session header")
						}
						if calls == 1 {
							w.WriteHeader(http.StatusInternalServerError)
							_, _ = io.WriteString(w, `{"error":{"message":"try again"}}`)
							return
						}
						if stream {
							w.Header().Set("Content-Type", "text/event-stream")
							_, _ = io.WriteString(w, f.sse)
						} else {
							w.Header().Set("Content-Type", "application/json")
							_, _ = io.WriteString(w, f.json)
						}
					}))
					defer srv.Close()
					cfg := ConfigFrom(f.format, "test", srv.URL, "test-key", "")
					cfg.UserAgent, cfg.SessionHeaderKey = ua, "x-session-id"
					cfg.Retry.ConnectAttempts, cfg.Retry.ConnectInterval = 1, 1
					p, err := cfg.NewProvider()
					if err != nil {
						t.Fatal(err)
					}
					ctx := transcript.WithSessionID(t.Context(), "conv-ua-test")
					req := llm.CompletionRequest{Messages: []llm.Message{{Role: "user", Content: []llm.ContentBlock{{Type: "text", Text: "ping"}}}}, MaxTokens: 32}
					var reply strings.Builder
					if stream {
						for ev, err := range p.Stream(ctx, req) {
							if err != nil {
								t.Fatal(err)
							}
							if ev.Type == llm.SETextDelta {
								reply.WriteString(ev.Text)
							}
						}
					} else {
						msg, _, _, err := p.Complete(ctx, req)
						if err != nil {
							t.Fatal(err)
						}
						for _, block := range msg.Content {
							reply.WriteString(block.Text)
						}
					}
					if reply.String() != "OK" || calls != 2 {
						t.Fatalf("reply=%q calls=%d", reply.String(), calls)
					}
				})
			}
		}
	}
}
