package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

func TestKeyDestinationNormalizesAndSeparatesHosts(t *testing.T) {
	same := [][2]agent.Config{
		{agent.ConfigFrom("openai", "", "https://gw.example/v1", "", ""), agent.ConfigFrom("openai", "", "https://gw.example/v1/", "", "")},
		{agent.ConfigFrom("openai", "", "https://gw.example/v1", "", ""), agent.ConfigFrom("openai", "", "https://GW.example/v1/chat/completions", "", "")},
		{agent.ConfigFrom("openai", "", "", "", ""), agent.ConfigFrom("openai-responses", "", "", "", "")}, // both api.openai.com
		{agent.ConfigFrom("anthropic", "", "https://gw.example", "", ""), agent.ConfigFrom("openai", "", "https://gw.example", "", "")},
	}
	for _, pair := range same {
		if !sameKeyDestination(pair[0], pair[1]) {
			t.Errorf("%q vs %q should be the same destination", keyDestination(pair[0]), keyDestination(pair[1]))
		}
	}
	different := [][2]agent.Config{
		{agent.ConfigFrom("openai", "", "https://gw.example/v1", "", ""), agent.ConfigFrom("openai", "", "https://attacker.example/v1", "", "")},
		{agent.ConfigFrom("openai", "", "", "", ""), agent.ConfigFrom("openai", "", "https://attacker.example", "", "")},
		{agent.ConfigFrom("anthropic", "", "", "", ""), agent.ConfigFrom("openai", "", "", "", "")}, // default hosts differ
	}
	for _, pair := range different {
		if sameKeyDestination(pair[0], pair[1]) {
			t.Errorf("%q vs %q must count as a destination change", keyDestination(pair[0]), keyDestination(pair[1]))
		}
	}
}

// One authenticated request used to be enough to receive a stored provider key:
// leave api_key blank, pass profile_id, point base_url anywhere.
func TestStoredLLMKeyIsNeverSentToANewDestination(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	t.Cleanup(func() { m.Close() })
	s := &Server{m: m}

	var legitCalls, attackerCalls atomic.Int32
	okReply := func(counter *atomic.Int32) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			counter.Add(1)
			w.Header().Set("Content-Type", "application/json")
			if strings.HasSuffix(r.URL.Path, "/models") {
				_, _ = w.Write([]byte(`{"data":[{"id":"test"}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`))
		}
	}
	legit := httptest.NewServer(okReply(&legitCalls))
	defer legit.Close()
	attacker := httptest.NewServer(okReply(&attackerCalls))
	defer attacker.Close()

	id, err := m.pg.SaveProfile(&db.LLMProfile{Name: t.Name(), Format: "openai", Model: "test", BaseURL: legit.URL, APIKey: "stored-secret"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.pg.Exec(`DELETE FROM llm_profiles WHERE id=$1`, id) })

	for name, handler := range map[string]http.HandlerFunc{"test": s.testLLM, "models": s.pgListModels} {
		w := callUserAgentHandler(t, handler, map[string]any{"provider": "openai", "model": "test", "base_url": attacker.URL, "profile_id": id, "streaming": false})
		if !strings.Contains(w.Body.String(), "重新填写 API Key") {
			t.Errorf("%s: stored key lent to a new base_url: status=%d body=%s", name, w.Code, w.Body)
		}
		w = callUserAgentHandler(t, handler, map[string]any{"provider": "openai", "model": "test", "base_url": legit.URL + "/", "profile_id": id, "streaming": false})
		if !strings.Contains(w.Body.String(), `"ok":true`) {
			t.Errorf("%s: stored key refused for its own destination: status=%d body=%s", name, w.Code, w.Body)
		}
	}
	if n := attackerCalls.Load(); n != 0 {
		t.Fatalf("the new destination received %d requests carrying the stored key", n)
	}
	if legitCalls.Load() == 0 {
		t.Fatal("the stored destination was never called")
	}

	// Saving: moving the base URL without re-entering the key is refused; the
	// stored row keeps its destination.
	w := callUserAgentHandler(t, s.pgSaveProfile, map[string]any{"id": id, "name": t.Name(), "format": "openai", "model": "test", "base_url": attacker.URL})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "重新填写 API Key") {
		t.Fatalf("save moved the base URL keeping the stored key: status=%d body=%s", w.Code, w.Body)
	}
	if p, err := m.pg.ProfileByID(id); err != nil || p == nil || p.BaseURL != legit.URL {
		t.Fatalf("refused save changed the profile: %+v %v", p, err)
	}
}
