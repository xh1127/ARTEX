package server

import (
	"context"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
)

func TestChatCacheCannotReinsertStaleProvider(t *testing.T) {
	m := &Manager{dir: t.TempDir()}
	oldProvider := &scriptedLLMProvider{}
	newProvider := &scriptedLLMProvider{}
	oldCfg := agent.ConfigFrom("openai", "test", "", "key", "")
	oldCfg.UserAgent = "old/1.0"
	newCfg := oldCfg
	newCfg.UserAgent = "new/2.0"
	s := &Server{m: m, profChatAgents: map[int64]*agent.ChatAgent{}, provByProfile: map[int64]*provEntry{1: {prov: oldProvider, cfg: oldCfg}}, ctx: context.Background()}
	m.mu.Lock() // Pause agent construction at ProxyAddr, after it took the old provider.
	locked := true
	defer func() {
		if locked {
			m.mu.Unlock()
		}
	}()
	done := make(chan *agent.ChatAgent, 1)
	go func() { done <- s.chatAgentForProfile(1) }()
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
	s.invalidateProfileAgents() // Same invalidation as saving the new profile.
	s.provCacheMu.Lock()
	s.provByProfile[1] = &provEntry{prov: newProvider, cfg: newCfg}
	s.provCacheMu.Unlock()
	m.mu.Unlock()
	locked = false
	var built *agent.ChatAgent
	select {
	case built = <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("constructor did not finish")
	}
	cached := s.chatAgentForProfile(1)
	if cached != built {
		t.Fatal("constructor returned an agent different from the current cache")
	}
	providerPointer := reflect.ValueOf(cached).Elem().FieldByName("prov").Elem().Pointer()
	if providerPointer != reflect.ValueOf(newProvider).Pointer() {
		t.Fatalf("stale chat agent repopulated the cache after invalidation: provider=%x want=%x", providerPointer, reflect.ValueOf(newProvider).Pointer())
	}
}
