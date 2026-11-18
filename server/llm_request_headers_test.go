package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/Autumn-27/artex/db"
)

func requestHeadersDatabase(t *testing.T) *db.DB {
	t.Helper()
	dsn := os.Getenv("ARTEX_PG_DSN")
	if dsn == "" {
		t.Skip("set ARTEX_PG_DSN to a test database")
	}
	d, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestDeepSeekSearchUsesProfileUserAgent(t *testing.T) {
	d := requestHeadersDatabase(t)
	previous, err := d.ActiveProfile()
	if err != nil {
		t.Fatal(err)
	}
	seen := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.UserAgent()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"type":"content_block_start","content_block":{"type":"web_search_tool_result","content":[{"type":"web_search_result","url":"https://example.com","title":"Example"}]}}`+"\n\n")
	}))
	defer upstream.Close()
	p := &db.LLMProfile{Name: "search-ua-test", Format: "anthropic", Model: "test", BaseURL: upstream.URL, APIKey: "test-key", UserAgent: "custom/1.0"}
	id, err := d.SaveProfile(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if previous != nil {
			if err := d.SetActiveProfile(previous.ID); err != nil {
				t.Error(err)
			}
		}
		if _, err := d.Exec(`DELETE FROM llm_profiles WHERE id=$1`, id); err != nil {
			t.Error(err)
		}
	})
	if err := d.SetActiveProfile(id); err != nil {
		t.Fatal(err)
	}
	s := &Server{m: &Manager{pg: d, webSearchOn: true, webSearchBackend: "deepseek"}}
	for _, ua := range []string{"custom/1.0", "changed/2.0", ""} {
		p.ID, p.UserAgent = id, ua
		if _, err := d.SaveProfile(p); err != nil {
			t.Fatal(err)
		}
		cfg := s.m.WebSearchOpts()
		if !cfg.Enabled || cfg.DeepSeekUserAgent != ua || cfg.DeepSeekBaseURL != upstream.URL {
			t.Fatalf("runtime search config=%+v", cfg)
		}
		w := callUserAgentHandler(t, s.testWebSearch, map[string]any{"web_search_backend": "deepseek"})
		var result struct {
			OK bool `json:"ok"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 200 || !result.OK {
			t.Fatalf("probe failed: %s err=%v", w.Body, err)
		}
		want := ua
		if want == "" {
			want = "Go-http-client/1.1"
		}
		if got := <-seen; got != want {
			t.Fatalf("wire UA=%q want %q", got, want)
		}
	}
}

func TestLLMTestSessionHeaderFallback(t *testing.T) {
	d := requestHeadersDatabase(t)
	id, err := d.SaveProfile(&db.LLMProfile{Name: "session-header-test", Format: "openai", Model: "test", APIKey: "test-key", SessionHeaderKey: "x-stored-session"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM llm_profiles WHERE id=$1`, id) })
	s := &Server{m: &Manager{pg: d}}
	for _, tc := range []struct {
		name    string
		value   any
		include bool
		want    string
	}{
		{"omitted", nil, false, "x-stored-session"},
		{"null", nil, true, "x-stored-session"},
		{"clear", "", true, ""},
		{"replace", "x-new-session", true, "x-new-session"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seen := make(chan http.Header, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen <- r.Header.Clone()
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`)
			}))
			defer upstream.Close()
			body := map[string]any{"provider": "openai", "model": "test", "base_url": upstream.URL, "profile_id": id, "streaming": false}
			if tc.include {
				body["session_header_key"] = tc.value
			}
			w := callUserAgentHandler(t, s.testLLM, body)
			var result struct {
				OK bool `json:"ok"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 200 || !result.OK {
				t.Fatalf("connection test: %s err=%v", w.Body, err)
			}
			headers := <-seen
			for _, key := range []string{"x-stored-session", "x-new-session"} {
				if got := headers.Get(key); (got != "") != (key == tc.want) {
					t.Errorf("header %s=%q, expected selected header %q", key, got, tc.want)
				}
			}
		})
	}
}
