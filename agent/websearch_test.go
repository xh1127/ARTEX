package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/permission"
)

const searchResultFrame = `data: {"type":"content_block_start","content_block":{"type":"web_search_tool_result","content":[{"type":"web_search_result","url":"https://example.com","title":"Example"}]}}` + "\n\n"

func TestDeepSeekSearchHeaders(t *testing.T) {
	for _, tc := range []struct {
		name, ua, suffix string
		proxy            bool
	}{
		{"custom", "custom/1.0", "", false},
		{"default", "", "/v1", false},
		{"other-profile", "other/2.0", "/v1/messages/", false},
		{"proxy", "proxied/1.0", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel() // Each profile must keep its own UA under concurrent searches.
			seen := make(chan string, 2)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen <- r.UserAgent()
				if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" || r.Header.Get("x-api-key") != "test-key" || r.Header.Get("anthropic-version") != "2023-06-01" {
					t.Errorf("unexpected request: %s %s headers=%v", r.Method, r.URL, r.Header)
				}
				if tc.proxy && r.URL.Host != "search.invalid" {
					t.Errorf("proxy target=%s", r.URL)
				}
				var body struct {
					Stream bool   `json:"stream"`
					Model  string `json:"model"`
					Tools  []struct {
						Type string `json:"type"`
					} `json:"tools"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !body.Stream || body.Model != "test-model" || len(body.Tools) != 1 || body.Tools[0].Type != "web_search_20250305" {
					t.Errorf("invalid search request: %+v err=%v", body, err)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, searchResultFrame)
			}))
			defer upstream.Close()
			o := WebSearchOpts{Enabled: true, Backend: "deepseek", DeepSeekBaseURL: upstream.URL + tc.suffix, DeepSeekAPIKey: "test-key", DeepSeekModel: "test-model", DeepSeekUserAgent: tc.ua}
			if tc.proxy {
				o.DeepSeekBaseURL, o.Proxy = "http://search.invalid", upstream.URL
			}
			results, err := ProbeWebSearch(t.Context(), o, "example", 3)
			if err != nil || len(results) != 1 || results[0].URL != "https://example.com" {
				t.Fatalf("probe results=%v err=%v", results, err)
			}
			var opts agentcore.Options
			configureWebSearch(&opts, o)
			if opts.EnableWebSearch || len(opts.Tools) != 1 {
				t.Fatalf("search must be registered exactly once: %+v", opts.Tools)
			}
			tool := opts.Tools[0]
			input := json.RawMessage(`{"query":"example","limit":3}`)
			if tool.Name() != "web_search" || !tool.IsReadOnly(input) || !tool.IsConcurrencySafe(input) || tool.CheckPermissions(t.Context(), input, permission.Context{}).Behavior != permission.Ask {
				t.Fatal("search tool contract changed")
			}
			result, err := tool.Call(t.Context(), input, nil)
			if err != nil || result.IsError || !strings.Contains(result.Flatten(), "https://example.com") {
				t.Fatalf("tool result=%v err=%v", result, err)
			}
			want := tc.ua
			if want == "" {
				want = "Go-http-client/1.1"
			}
			for i := 0; i < 2; i++ {
				if got := <-seen; got != want {
					t.Errorf("request %d UA=%q want %q", i, got, want)
				}
			}
		})
	}
}

func TestWebSearchDisabledAndOtherBackends(t *testing.T) {
	var opts agentcore.Options
	configureWebSearch(&opts, WebSearchOpts{Enabled: false, Backend: "deepseek"})
	if len(opts.Tools) != 0 {
		t.Fatal("disabled search registered")
	}
	for _, backend := range []string{"ddgs", "brave-free", "tavily"} {
		tool, err := newWebSearch(WebSearchOpts{Backend: backend, BraveKey: "test", TavilyKey: "test", DeepSeekUserAgent: "bad\nUA"})
		if err != nil {
			t.Fatal(err)
		}
		if _, wrapped := tool.(*deepSeekSearchTool); wrapped {
			t.Fatalf("%s unexpectedly replaced", backend)
		}
	}
}

func TestDeepSeekSearchErrors(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "wrong UA", http.StatusForbidden) }))
	defer upstream.Close()
	o := WebSearchOpts{Backend: "deepseek", DeepSeekBaseURL: upstream.URL, DeepSeekAPIKey: "test", DeepSeekModel: "test"}
	if _, err := ProbeWebSearch(t.Context(), o, "test", 3); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("HTTP failure lost: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := ProbeWebSearch(ctx, o, "test", 3); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	o.DeepSeekUserAgent = "bad\r\nUA"
	if _, err := newWebSearch(o); err == nil {
		t.Fatal("invalid UA accepted")
	}
	o.DeepSeekUserAgent, o.Proxy = "valid/1.0", "://bad"
	if _, err := newWebSearch(o); err == nil {
		t.Fatal("invalid proxy accepted")
	}
}

func TestReadDeepSeekSearch(t *testing.T) {
	large := strings.Replace(searchResultFrame, `"title":"Example"`, `"encrypted_content":"`+strings.Repeat("x", 100000)+`"`, 1)
	results, err := readDeepSeekSearch(strings.NewReader(large+large+searchResultFrame), 3)
	if err != nil || len(results) != 1 || results[0].Title != "example.com" || results[0].Position != 1 {
		t.Fatalf("large/duplicate results=%v err=%v", results, err)
	}
	results, err = readDeepSeekSearch(strings.NewReader(searchResultFrame+`data: {"type":"error","error":{"message":"later error"}}`+"\n"), 1)
	if err != nil || len(results) != 1 {
		t.Fatalf("result limit not respected: %v %v", results, err)
	}
	for _, tc := range []struct{ name, stream, want string }{
		{"stream error", `data: {"type":"error","error":{"message":"unavailable"}}`, "unavailable"},
		{"array error", `data: {"type":"content_block_start","content_block":{"type":"web_search_tool_result","content":[{"error_code":"rate_limit"}]}}`, "rate_limit"},
		{"object error", `data: {"type":"content_block_start","content_block":{"type":"web_search_tool_result","content":{"error_code":"rate_limit"}}}`, "rate_limit"},
		{"size limit", strings.Repeat(": ping\n", (2<<20)/7+1), "exceeds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := readDeepSeekSearch(strings.NewReader(tc.stream), 3); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v want %s", err, tc.want)
			}
		})
	}
}

// configureWebSearch appends its tool AFTER AugmentTools has already wrapped
// everything in guardPanic, so it must apply the wrapper itself — otherwise
// web_search is the one tool a panic can still take the process down through.
func TestWebSearchIsPanicGuarded(t *testing.T) {
	var opts agentcore.Options
	configureWebSearch(&opts, WebSearchOpts{Enabled: true, Backend: "ddgs"})
	if len(opts.Tools) != 1 {
		t.Fatalf("tools=%d", len(opts.Tools))
	}
	if _, ok := opts.Tools[0].(*guardedTool); !ok {
		t.Fatalf("web_search added without guardPanic: %T", opts.Tools[0])
	}
}
