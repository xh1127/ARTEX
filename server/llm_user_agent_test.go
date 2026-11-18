package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/sidequestion"
)

func callUserAgentHandler(t *testing.T, handler http.HandlerFunc, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	handler(w, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(b)))
	return w
}

func TestLLMUserAgentHTTPHandlers(t *testing.T) {
	s := &Server{}
	for name, handler := range map[string]http.HandlerFunc{"test": s.testLLM, "models": s.pgListModels} {
		t.Run(name, func(t *testing.T) {
			for _, ua := range []string{"custom/1.0", "", "agent\r\nX-Injected: yes"} {
				calls := 0
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					want := ua
					if want == "" {
						want = "Go-http-client/1.1"
					}
					if r.UserAgent() != want {
						t.Errorf("wire UA = %q, want %q", r.UserAgent(), want)
					}
					w.Header().Set("Content-Type", "application/json")
					if name == "models" {
						_, _ = io.WriteString(w, `{"data":[{"id":"test-model"}]}`)
					} else {
						_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`)
					}
				}))
				w := callUserAgentHandler(t, handler, map[string]any{"provider": "openai", "model": "test-model", "base_url": upstream.URL, "api_key": "test-key", "streaming": false, "user_agent": ua})
				upstream.Close()
				if ua == "agent\r\nX-Injected: yes" {
					if w.Code != 400 || calls != 0 {
						t.Fatalf("invalid UA: status=%d calls=%d", w.Code, calls)
					}
					continue
				}
				var result struct {
					OK bool `json:"ok"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				if w.Code != 200 || !result.OK || calls != 1 {
					t.Fatalf("status=%d calls=%d body=%s", w.Code, calls, w.Body.String())
				}
			}
		})
	}
}

func TestLLMUserAgentProfileSaveAndFallback(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	t.Cleanup(func() { m.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	dir := t.TempDir()
	s := New(ctx, m, dir, dir, dir)
	// Keep this profile inactive; saving still exercises the real handler,
	// persistence and cache invalidation without changing the global default.
	id, err := m.pg.SaveProfile(&db.LLMProfile{Name: "ua-handler-test", Format: "openai", Model: "test", APIKey: "test-key", UserAgent: "stored/1.0"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.pg.Exec(`DELETE FROM llm_profiles WHERE id=$1`, id) })
	for _, test := range []struct {
		name    string
		value   any
		include bool
		want    string
		status  int
	}{
		{"omitted", nil, false, "stored/1.0", 200},
		{"replace", " new/2.0 ", true, "new/2.0", 200},
		{"invalid", "bad\nheader", true, "new/2.0", 400},
		{"clear", "", true, "", 200},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := map[string]any{"id": id, "name": "ua-handler-test", "format": "openai", "model": "test"}
			if test.include {
				body["user_agent"] = test.value
			}
			w := callUserAgentHandler(t, s.pgSaveProfile, body)
			if w.Code != test.status {
				t.Fatalf("status=%d: %s", w.Code, w.Body.String())
			}
			p, err := m.pg.ProfileByID(id)
			if err != nil || p == nil {
				t.Fatalf("profile: %v", err)
			}
			if p.UserAgent != test.want || llmProfileDTO(p).UserAgent != test.want {
				t.Fatalf("UA = %q, want %q", p.UserAgent, test.want)
			}
			cfg, ok := s.loadProfileConfig(id)
			if !ok || cfg.UserAgent != test.want {
				t.Fatalf("runtime UA=%q ok=%v", cfg.UserAgent, ok)
			}
		})
	}
	p, err := m.pg.ProfileByID(id)
	if err != nil {
		t.Fatal(err)
	}
	p.UserAgent = "stored/3.0"
	if _, err := m.pg.SaveProfile(p); err != nil {
		t.Fatal(err)
	}
	for name, handler := range map[string]http.HandlerFunc{"test": s.testLLM, "models": s.pgListModels} {
		for _, explicitEmpty := range []bool{false, true} {
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				want := "stored/3.0"
				if explicitEmpty {
					want = "Go-http-client/1.1"
				}
				if r.UserAgent() != want {
					t.Errorf("%s explicitEmpty=%v: UA=%q, want %q", name, explicitEmpty, r.UserAgent(), want)
				}
				w.Header().Set("Content-Type", "application/json")
				if name == "models" {
					_, _ = io.WriteString(w, `{"data":[{"id":"test"}]}`)
				} else {
					_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`)
				}
			}))
			body := map[string]any{"provider": "openai", "model": "test", "base_url": upstream.URL, "profile_id": id, "streaming": false}
			if explicitEmpty {
				body["user_agent"] = ""
			}
			w := callUserAgentHandler(t, handler, body)
			upstream.Close()
			if w.Code != 200 || calls != 1 {
				t.Fatalf("status=%d calls=%d body=%s", w.Code, calls, w.Body.String())
			}
		}
	}
}

func TestSideProviderAcceptsLegacySnapshotWithoutUserAgent(t *testing.T) {
	// Frozen pre-UA snapshot: do not regenerate its identity with sideModel,
	// since that would hide changes to the persisted fingerprint format.
	const stored = `{"model":{"profile_id":0,"name":"example","format":"openai","model":"example-model","identity":"f0f29e0ede38d16e7d8415bd1f6f652db7186afbada67bd322d4af178aa4e0c2","streaming":true,"window_tokens":200000}}`
	var snap sidequestion.Snapshot
	if err := json.Unmarshal([]byte(stored), &snap); err != nil {
		t.Fatal(err)
	}
	cfg := agent.ConfigFrom("openai", "example-model", "http://example.invalid/v1", "test-key", "")
	provider := &scriptedLLMProvider{}
	s := &Server{llmCfg: cfg, llmOn: true, llmDirect: provider}
	if got, err := s.sideProvider(snap.Model); err != nil || got != provider {
		t.Fatalf("unchanged config rejected legacy snapshot: provider=%v err=%v", got, err)
	}
	// DB-backed profiles use the same persisted identity check.
	if got := sideModel(cfg, 42, "example").Identity; got != snap.Model.Identity {
		t.Fatalf("profile identity=%q, want legacy %q", got, snap.Model.Identity)
	}
	s.llmCfg.UserAgent = "custom/1.0"
	if _, err := s.sideProvider(snap.Model); err == nil {
		t.Fatal("custom UA accepted the legacy snapshot")
	}
	s.llmCfg.UserAgent = ""
	if _, err := s.sideProvider(snap.Model); err != nil {
		t.Fatalf("restoring default UA did not restore legacy compatibility: %v", err)
	}
	s.llmCfg.Model = "different-model"
	if _, err := s.sideProvider(snap.Model); err == nil {
		t.Fatal("changed model accepted the legacy snapshot")
	}
}

func TestSideModelIdentityIncludesUserAgent(t *testing.T) {
	cfg := agent.ConfigFrom("openai", "test", "", "key", "")
	cfg.UserAgent = "custom/1.0"
	snapshotModel := sideModel(cfg, 0, "test")
	provider := &scriptedLLMProvider{}
	for _, test := range []struct {
		name, ua string
		allowed  bool
	}{
		{"unchanged", "custom/1.0", true},
		{"changed", "custom/2.0", false},
		{"cleared", "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			current := cfg
			current.UserAgent = test.ua
			s := &Server{llmCfg: current, llmOn: true, llmDirect: provider}
			got, err := s.sideProvider(snapshotModel)
			if test.allowed {
				if err != nil || got != provider {
					t.Fatalf("unchanged UA rejected: provider=%v err=%v", got, err)
				}
			} else if err == nil {
				t.Fatal("stale UA snapshot remained usable")
			}
		})
	}
}
