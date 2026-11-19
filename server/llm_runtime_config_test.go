package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/llmpool"
)

func newProfileRuntimeTestServer(t *testing.T) (*Server, int64) {
	t.Helper()
	d := requestHeadersDatabase(t)
	previous, err := d.ActiveProfile()
	if err != nil {
		t.Fatal(err)
	}
	id, err := d.SaveProfile(&db.LLMProfile{Name: t.Name(), Format: "openai", Model: "test", APIKey: "key", UserAgent: "old/1.0"})
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
	s := &Server{m: &Manager{pg: d, dir: t.TempDir()}, profChatAgents: map[int64]*agent.ChatAgent{}, provByProfile: map[int64]*provEntry{}, ctx: context.Background()}
	if err := s.reapplyActiveProfile(); err != nil {
		t.Fatal(err)
	}
	return s, id
}

func TestSettingsRefreshBoundChats(t *testing.T) {
	for _, globalReady := range []bool{true, false} {
		for _, setting := range []string{"web_search_enabled", "global_proxy"} {
			name := setting
			if !globalReady {
				name += "-without-global"
			}
			t.Run(name, func(t *testing.T) {
				s, id := newProfileRuntimeTestServer(t)
				for _, key := range []string{settingWebSearchOn, settingWebSearchBackend, settingGlobalProxy} {
					value, exists, err := s.m.pg.GetSetting(key)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() {
						if exists {
							if err := s.m.pg.SetSetting(key, value); err != nil {
								t.Error(err)
							}
						} else if _, err := s.m.pg.Exec(`DELETE FROM settings WHERE key=$1`, key); err != nil {
							t.Error(err)
						}
						if key == settingGlobalProxy {
							s.m.globalProxy = value
							s.m.syncBrowserMCPProxy()
						}
					})
				}
				if !globalReady {
					if _, err := s.m.pg.Exec(`UPDATE llm_profiles SET is_default=false WHERE id=$1`, id); err != nil {
						t.Fatal(err)
					}
					s.llmOn, s.llmProv, s.llmDirect, s.chatAgent = false, nil, nil, nil
				}
				s.m.webSearchOn, s.m.webSearchBackend = true, "ddgs"
				before := s.chatAgentForProfile(id)
				var value any = false
				if setting == "global_proxy" {
					value = "http://127.0.0.1:9999"
				}
				w := callUserAgentHandler(t, s.putSettings, map[string]any{setting: value})
				if w.Code != http.StatusOK {
					t.Fatalf("settings rejected: %s", w.Body)
				}
				after := s.chatAgentForProfile(id)
				if after == nil || after == before {
					t.Fatal("settings did not rebuild bound chat")
				}
				v := reflect.ValueOf(after).Elem()
				if setting == "web_search_enabled" {
					if v.FieldByName("webSearch").FieldByName("Enabled").Bool() {
						t.Fatal("search remains enabled after successful disable")
					}
				} else if proxy := v.FieldByName("proxyAddr").String(); proxy != value {
					t.Fatalf("chat proxy=%q want %q", proxy, value)
				}
			})
		}
	}
}

func TestGlobalProfileApplyRejectsStaleSnapshot(t *testing.T) {
	s, id := newProfileRuntimeTestServer(t)
	stale, _ := s.loadLLMConfig()
	p, err := s.m.pg.ProfileByID(id)
	if err != nil {
		t.Fatal(err)
	}
	p.UserAgent = "new/2.0"
	if _, err := s.m.pg.SaveProfile(p); err != nil {
		t.Fatal(err)
	}
	s.invalidateProfileAgents()
	if err := s.reapplyActiveProfile(); err != nil {
		t.Fatal(err)
	}
	// An earlier request resumes after the newer save has finished.
	if err := s.applyLLM(stale); err != nil {
		t.Fatal(err)
	}
	prov, cfg, ready := s.globalProvider()
	cached, _, _ := s.providerForProfile(id)
	if !ready || cfg.UserAgent != "new/2.0" || prov != cached || s.llmDirect != cached {
		t.Fatalf("stale apply replaced latest provider: ready=%v UA=%q shared=%v", ready, cfg.UserAgent, prov == cached)
	}
}

func TestGlobalProfileApplyUsesLatestActivation(t *testing.T) {
	s, _ := newProfileRuntimeTestServer(t)
	stale, _ := s.loadLLMConfig()
	id, err := s.m.pg.SaveProfile(&db.LLMProfile{Name: t.Name() + "-new", Format: "openai", Model: "new-model", APIKey: "new-key", UserAgent: "new/2.0"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.m.pg.Exec(`DELETE FROM llm_profiles WHERE id=$1`, id) })
	w := callUserAgentHandler(t, s.pgActivateProfile, map[string]any{"id": id})
	if w.Code != http.StatusOK {
		t.Fatalf("activation failed: %s", w.Body)
	}
	if err := s.applyLLM(stale); err != nil {
		t.Fatal(err)
	}
	prov, cfg, ready := s.globalProvider()
	cached, _, _ := s.providerForProfile(id)
	if !ready || cfg.Model != "new-model" || cfg.UserAgent != "new/2.0" || prov != cached || s.llmProf != t.Name()+"-new" {
		t.Fatalf("new activation reverted: ready=%v model=%q UA=%q name=%q", ready, cfg.Model, cfg.UserAgent, s.llmProf)
	}
}

func TestGlobalProfileApplyPreservesEnvironmentFallback(t *testing.T) {
	d := requestHeadersDatabase(t)
	previous, err := d.ActiveProfile()
	if err != nil {
		t.Fatal(err)
	}
	if previous != nil {
		if _, err := d.Exec(`UPDATE llm_profiles SET is_default=false WHERE id=$1`, previous.ID); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = d.SetActiveProfile(previous.ID) })
	}
	s := &Server{m: &Manager{pg: d, dir: t.TempDir()}}
	cfg := agent.ConfigFrom("openai", "env-model", "", "env-key", "")
	if err := s.applyLLM(cfg); err != nil {
		t.Fatal(err)
	}
	s.invalidateProfileAgents()
	if err := s.reapplyActiveProfile(); err != nil {
		t.Fatal(err)
	}
	if _, current, ready := s.globalProvider(); !ready || current != cfg {
		t.Fatal("settings refresh lost environment configuration")
	}
}

func TestGlobalProfileApplyRetriesInvalidatedBuild(t *testing.T) {
	s, id := newProfileRuntimeTestServer(t)
	stale, _ := s.loadLLMConfig()
	s.m.mu.Lock() // Block construction at ProxyAddr, after taking the old provider.
	locked := true
	defer func() {
		if locked {
			s.m.mu.Unlock()
		}
	}()
	done := make(chan error, 1)
	go func() { done <- s.applyLLM(stale) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		stack := make([]byte, 1<<20)
		n := runtime.Stack(stack, true)
		if strings.Contains(string(stack[:n]), "(*Manager).ProxyAddr(") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("constructor did not reach barrier")
		}
		time.Sleep(time.Millisecond)
	}
	p, err := s.m.pg.ProfileByID(id)
	if err != nil {
		t.Fatal(err)
	}
	p.UserAgent = "new/2.0"
	if _, err := s.m.pg.SaveProfile(p); err != nil {
		t.Fatal(err)
	}
	s.invalidateProfileAgents()
	s.m.mu.Unlock()
	locked = false
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("apply did not finish")
	}
	_, cfg, ready := s.globalProvider()
	if !ready || cfg.UserAgent != "new/2.0" {
		t.Fatalf("invalidated build installed: ready=%v UA=%q", ready, cfg.UserAgent)
	}
}

func TestReapplyInvalidActiveProfileDisablesOldRuntime(t *testing.T) {
	s, id := newProfileRuntimeTestServer(t)
	p, err := s.m.pg.ProfileByID(id)
	if err != nil {
		t.Fatal(err)
	}
	p.Proxy = "ftp://invalid.example"
	if _, err := s.m.pg.SaveProfile(p); err != nil {
		t.Fatal(err)
	}
	s.invalidateProfileAgents()
	if err := s.reapplyActiveProfile(); err == nil {
		t.Fatal("unusable active config silently accepted")
	}
	if _, _, ready := s.globalProvider(); ready || s.chatAgentRef() != nil || s.llmDirect != nil {
		t.Fatal("invalid active config retained a usable old provider")
	}
	p.Proxy = ""
	if _, err := s.m.pg.SaveProfile(p); err != nil {
		t.Fatal(err)
	}
	s.invalidateProfileAgents()
	if err := s.reapplyActiveProfile(); err != nil {
		t.Fatal(err)
	}
	if _, _, ready := s.globalProvider(); !ready {
		t.Fatal("corrected config did not recover")
	}
}

func TestActivateProfileRejectsUnusableConfig(t *testing.T) {
	for _, tc := range []struct {
		name, key, proxy string
	}{
		{"missing-key", "", ""},
		{"blank-key", "   ", ""},
		{"invalid-proxy", "key", "ftp://invalid.example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, oldID := newProfileRuntimeTestServer(t)
			oldProvider, oldCfg, _ := s.globalProvider()
			id, err := s.m.pg.SaveProfile(&db.LLMProfile{Name: t.Name() + "-candidate", Format: "openai", Model: "new-model", APIKey: tc.key, Proxy: tc.proxy})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _, _ = s.m.pg.Exec(`DELETE FROM llm_profiles WHERE id=$1`, id) })
			w := callUserAgentHandler(t, s.pgActivateProfile, map[string]any{"id": id})
			if w.Code != http.StatusBadRequest {
				t.Fatalf("unusable config accepted: status=%d body=%s", w.Code, w.Body)
			}
			active, err := s.m.pg.ActiveProfile()
			if err != nil || active == nil || active.ID != oldID {
				t.Fatalf("rejected activation changed persisted selection: %v %v", active, err)
			}
			prov, cfg, ready := s.globalProvider()
			if !ready || prov != oldProvider || cfg != oldCfg {
				t.Fatal("rejected activation changed running provider")
			}
		})
	}
}

func TestReapplyProfileReadFailurePreservesRuntime(t *testing.T) {
	s, id := newProfileRuntimeTestServer(t)
	oldProvider, oldCfg, _ := s.globalProvider()
	oldChat, oldDirect, oldName := s.chatAgentRef(), s.llmDirect, s.llmProf
	originalDB := s.m.pg
	dsn, err := url.Parse(os.Getenv("ARTEX_PG_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	query := dsn.Query()
	query.Set("statement_timeout", "150")
	dsn.RawQuery = query.Encode()
	timedSQL, err := sql.Open("pgx", dsn.String())
	if err != nil {
		t.Fatal(err)
	}
	defer timedSQL.Close()
	s.m.pg = &db.DB{DB: timedSQL}
	defer func() { s.m.pg = originalDB }()
	tx, err := originalDB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`LOCK TABLE llm_profiles IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	s.invalidateProfileAgents()
	if err := s.reapplyActiveProfile(); !errors.Is(err, errLLMConfigRead) {
		t.Fatalf("expected a temporary read failure, got %v", err)
	}
	// The second read path must preserve its cause too, rather than report an
	// unusable profile when the cache is empty and ProfileByID times out.
	if _, _, err := s.providerForProfileErr(id); !errors.Is(err, errLLMConfigRead) {
		t.Fatalf("provider lookup lost the read error: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if active, err := s.m.pg.ActiveProfile(); err != nil || active == nil || active.ID != id {
		t.Fatalf("database did not recover: %v %v", active, err)
	}
	provider, cfg, ready := s.globalProvider()
	if !ready || provider != oldProvider || cfg != oldCfg || s.chatAgentRef() != oldChat || s.llmDirect != oldDirect || s.llmProf != oldName {
		t.Fatal("temporary read failure discarded the healthy runtime")
	}
	// A subsequent successful edit must still replace the retained runtime.
	p, err := s.m.pg.ProfileByID(id)
	if err != nil {
		t.Fatal(err)
	}
	p.UserAgent = "recovered/2.0"
	if _, err := s.m.pg.SaveProfile(p); err != nil {
		t.Fatal(err)
	}
	s.invalidateProfileAgents()
	if err := s.reapplyActiveProfile(); err != nil {
		t.Fatal(err)
	}
	if _, cfg, ready := s.globalProvider(); !ready || cfg.UserAgent != p.UserAgent {
		t.Fatal("configuration updates did not recover")
	}
}

func TestSaveProfileRejectsInvalidActiveConfig(t *testing.T) {
	for name, invalid := range map[string]map[string]any{
		"invalid-proxy": {"proxy": "ftp://invalid.example"},
		"blank-key":     {"api_key": "   "},
	} {
		t.Run(name, func(t *testing.T) {
			s, id := newProfileRuntimeTestServer(t)
			before, err := s.m.pg.ProfileByID(id)
			if err != nil {
				t.Fatal(err)
			}
			oldProvider, oldCfg, _ := s.globalProvider()
			body := map[string]any{"id": id, "name": before.Name, "format": "openai", "model": "new-model"}
			for key, value := range invalid {
				body[key] = value
			}
			w := callUserAgentHandler(t, s.pgSaveProfile, body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("invalid save returned %d: %s", w.Code, w.Body)
			}
			after, err := s.m.pg.ProfileByID(id)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("rejected save changed profile: %v", err)
			}
			if provider, cfg, ready := s.globalProvider(); !ready || provider != oldProvider || cfg != oldCfg {
				t.Fatal("rejected save changed running model")
			}
		})
	}
}

func TestSaveProfilePreservesKeyAndAppliesValidEdit(t *testing.T) {
	s, id := newProfileRuntimeTestServer(t)
	w := callUserAgentHandler(t, s.pgSaveProfile, map[string]any{
		"id": id, "name": t.Name(), "format": "openai", "model": "new-model", "api_key": "", "user_agent": "edited/2.0",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("save failed: %s", w.Body)
	}
	p, err := s.m.pg.ProfileByID(id)
	if err != nil || p.APIKey != "key" {
		t.Fatalf("saved key was lost: %v", err)
	}
	if _, cfg, ready := s.globalProvider(); !ready || cfg.Model != "new-model" || cfg.APIKey != "key" || cfg.UserAgent != "edited/2.0" {
		t.Fatal("valid edit was not applied with the stored key")
	}
}

func TestSaveProfileReportsReapplyFailure(t *testing.T) {
	s, activeID := newProfileRuntimeTestServer(t)
	id, err := s.m.pg.SaveProfile(&db.LLMProfile{Name: t.Name() + "-other", Format: "openai", Model: "test", APIKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.m.pg.Exec(`DELETE FROM llm_profiles WHERE id=$1`, id) })
	// Simulate an already-invalid active row while editing another valid one.
	if _, err := s.m.pg.Exec(`UPDATE llm_profiles SET proxy='ftp://invalid.example' WHERE id=$1`, activeID); err != nil {
		t.Fatal(err)
	}
	w := callUserAgentHandler(t, s.pgSaveProfile, map[string]any{"id": id, "name": t.Name() + "-other", "format": "openai", "model": "new-model"})
	// Committed but not hot-applied: a 200 carrying a warning. A 500 made the UI
	// treat the save as failed — the failure must stay visible, not be fatal.
	var resp struct {
		ID      int64  `json:"id"`
		Warning string `json:"warning"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &resp) != nil || resp.ID != id || !strings.Contains(resp.Warning, "已保存") {
		t.Fatalf("hot reload failure hidden or reported as failed save: status=%d body=%s", w.Code, w.Body)
	}
	if p, err := s.m.pg.ProfileByID(id); err != nil || p == nil || p.Model != "new-model" {
		t.Fatalf("edit was not committed: %+v %v", p, err)
	}

	// Create path: the response must carry the new id, otherwise the client keeps
	// an id-less form and a retry inserts a second row.
	w = callUserAgentHandler(t, s.pgSaveProfile, map[string]any{"name": t.Name() + "-new", "format": "openai", "model": "m", "api_key": "k"})
	resp.ID, resp.Warning = 0, ""
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.ID > 0 {
		t.Cleanup(func() { _, _ = s.m.pg.Exec(`DELETE FROM llm_profiles WHERE id=$1`, resp.ID) })
	}
	if w.Code != http.StatusOK || resp.ID <= 0 || !strings.Contains(resp.Warning, "已保存") {
		t.Fatalf("create with failed hot reload: status=%d body=%s", w.Code, w.Body)
	}
}

func preserveRuntimeSettings(t *testing.T, s *Server, keys ...string) {
	t.Helper()
	for _, key := range keys {
		value, exists, err := s.m.pg.GetSetting(key)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			var err error
			if exists {
				err = s.m.pg.SetSetting(key, value)
			} else {
				_, err = s.m.pg.Exec(`DELETE FROM settings WHERE key=$1`, key)
			}
			if err != nil {
				t.Error(err)
			}
		})
	}
}

func TestSettingsRejectInvalidPatchWithoutChanges(t *testing.T) {
	for _, tc := range []struct {
		name    string
		invalid map[string]any
		status  int
	}{
		{"global-proxy", map[string]any{"global_proxy": "ftp://invalid.example"}, 400},
		{"search-proxy", map[string]any{"web_search_proxy": "ftp://invalid.example"}, 400},
		{"workers", map[string]any{"workers": 0}, 400},
		{"notify-url", map[string]any{"notify_public_base_url": "ftp://invalid.example"}, 400},
		{"notify-interval", map[string]any{"notify_digest_interval_min": 0}, 400},
		// PostgreSQL rejects NUL in text. This fails after earlier keys have
		// been written inside the transaction, proving rollback of the patch.
		{"database-write", map[string]any{"python_interpreter": "bad\x00path"}, 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, id := newProfileRuntimeTestServer(t)
			keys := []string{settingLLMPoolOn, settingLLMRecord, settingWebSearchOn, settingPythonInterp}
			preserveRuntimeSettings(t, s, keys...)
			for _, key := range keys[:3] {
				if err := s.m.pg.SetBool(key, false); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.reapplyActiveProfile(); err != nil {
				t.Fatal(err)
			}
			oldProvider, _, _ := s.globalProvider()
			oldChat := s.chatAgentForProfile(id)
			body := map[string]any{"llm_pool_enabled": true, "llm_record": true, "web_search_enabled": true}
			for key, value := range tc.invalid {
				body[key] = value
			}
			w := callUserAgentHandler(t, s.putSettings, body)
			if w.Code != tc.status {
				t.Fatalf("status=%d body=%s", w.Code, w.Body)
			}
			for _, key := range keys[:3] {
				if s.m.pg.GetBool(key, true) {
					t.Fatalf("rejected request persisted %s", key)
				}
			}
			provider, _, ready := s.globalProvider()
			searchOn, _, _, _, _ := s.m.WebSearch()
			if !ready || provider != oldProvider || s.chatAgentForProfile(id) != oldChat || s.m.LLMRecordEnabled() || searchOn {
				t.Fatal("rejected request changed runtime state")
			}
		})
	}
}

func TestSettingsCommitPatchAndRebuildPool(t *testing.T) {
	s, _ := newProfileRuntimeTestServer(t)
	preserveRuntimeSettings(t, s, settingLLMPoolOn, settingLLMRecord, settingWebSearchOn, settingWebSearchBackend, settingBraveKey)
	if err := s.m.pg.SetBool(settingLLMPoolOn, false); err != nil {
		t.Fatal(err)
	}
	if err := s.reapplyActiveProfile(); err != nil {
		t.Fatal(err)
	}
	spareID, err := s.m.pg.SaveProfile(&db.LLMProfile{Name: t.Name() + "-spare", Format: "openai", Model: "spare", APIKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.m.pg.Exec(`DELETE FROM llm_profiles WHERE id=$1`, spareID) })
	w := callUserAgentHandler(t, s.putSettings, map[string]any{
		"llm_pool_enabled": true, "llm_record": true, "web_search_enabled": true, "web_search_backend": " brave-free ", "brave_search_api_key": "test-key",
	})
	if w.Code != 200 {
		t.Fatalf("settings failed: %s", w.Body)
	}
	provider, _, ready := s.globalProvider()
	if _, ok := provider.(*llmpool.Pool); !ok || !ready || !s.m.LLMPoolEnabled() || !s.m.LLMRecordEnabled() {
		t.Fatalf("committed settings did not rebuild runtime: provider=%T ready=%v", provider, ready)
	}
	// A partial patch must keep the previously committed search key/backend.
	w = callUserAgentHandler(t, s.putSettings, map[string]any{"web_search_enabled": false})
	on, backend, key, _, _ := s.m.WebSearch()
	if w.Code != 200 || on || backend != "brave-free" || key != "test-key" {
		t.Fatalf("partial patch lost search settings: status=%d on=%v backend=%q", w.Code, on, backend)
	}
}

// A config that is set but cannot be built used to surface only as
// configured:false — indistinguishable from "nothing configured".
func TestLLMBuildFailureReasonIsReported(t *testing.T) {
	s, activeID := newProfileRuntimeTestServer(t)
	read := func() (bool, string) {
		w := httptest.NewRecorder()
		s.getLLM(w, httptest.NewRequest("GET", "/api/llm", nil))
		var r struct {
			Configured bool   `json:"configured"`
			Error      string `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		return r.Configured, r.Error
	}
	if _, err := s.m.pg.Exec(`UPDATE llm_profiles SET proxy='ftp://invalid.example' WHERE id=$1`, activeID); err != nil {
		t.Fatal(err)
	}
	// Edited behind the handlers' back: drop the provider cache the way every
	// save handler does, or applyLLM reuses the provider built from the old row.
	s.invalidateProfileAgents()
	if err := s.reapplyActiveProfile(); err == nil {
		t.Fatal("an invalid active profile was applied")
	}
	if on, reason := read(); on || reason == "" {
		t.Fatalf("configured=%v error=%q, want false with the build failure reason", on, reason)
	}
	if _, err := s.m.pg.Exec(`UPDATE llm_profiles SET proxy=NULL WHERE id=$1`, activeID); err != nil {
		t.Fatal(err)
	}
	s.invalidateProfileAgents()
	if err := s.reapplyActiveProfile(); err != nil {
		t.Fatal(err)
	}
	if on, reason := read(); !on || reason != "" {
		t.Fatalf("after the fix: configured=%v error=%q, want true and no error", on, reason)
	}
}
