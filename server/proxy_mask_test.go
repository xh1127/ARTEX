package server

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/notify"
)

func TestMaskProxyURLHidesOnlyThePassword(t *testing.T) {
	for in, want := range map[string]string{
		"":                                   "",
		"http://127.0.0.1:7890":              "http://127.0.0.1:7890",
		"socks5://user@proxy.example:1080":   "socks5://user@proxy.example:1080",
		"http://user:s3cret@proxy.example:8": "http://user:" + proxyPasswordMask + "@proxy.example:8",
		"https://u:p%40ss@h.example":         "https://u:" + proxyPasswordMask + "@h.example",
	} {
		got := maskProxyURL(in)
		if got != want {
			t.Errorf("maskProxyURL(%q) = %q, want %q", in, got, want)
		}
		if strings.Contains(got, "s3cret") || strings.Contains(got, "p%40ss") {
			t.Errorf("maskProxyURL(%q) leaked the password: %q", in, got)
		}
	}
}

func TestResolveMaskedProxyKeepsPasswordOnlyForTheSameProxy(t *testing.T) {
	const stored = "http://user:s3cret@proxy.example:8080"
	masked := maskProxyURL(stored)

	// Unedited form value → the stored proxy, password intact.
	if got, err := resolveMaskedProxy(masked, stored); err != nil || got != stored {
		t.Fatalf("round trip: got %q err=%v, want %q", got, err, stored)
	}
	// Unmasked input is taken as typed, whatever is stored.
	for _, in := range []string{"", "http://other.example:1", "http://user:newpass@proxy.example:8080", "  http://127.0.0.1:7890  "} {
		if got, err := resolveMaskedProxy(in, stored); err != nil || got != strings.TrimSpace(in) {
			t.Errorf("resolveMaskedProxy(%q): got %q err=%v, want it unchanged", in, got, err)
		}
	}
	// The mask must not carry the stored password to a different proxy.
	for name, in := range map[string]string{
		"other host":   "http://user:" + proxyPasswordMask + "@attacker.example:8080",
		"other port":   "http://user:" + proxyPasswordMask + "@proxy.example:9999",
		"other scheme": "socks5://user:" + proxyPasswordMask + "@proxy.example:8080",
		"other user":   "http://admin:" + proxyPasswordMask + "@proxy.example:8080",
	} {
		if got, err := resolveMaskedProxy(in, stored); !errors.Is(err, errProxyDestinationChanged) || got != "" {
			t.Errorf("%s: got %q err=%v, want errProxyDestinationChanged", name, got, err)
		}
	}
	// A mask with nothing (or no password) stored cannot be resolved.
	for _, storedValue := range []string{"", "http://proxy.example:8080", "http://user@proxy.example:8080"} {
		if _, err := resolveMaskedProxy(masked, storedValue); !errors.Is(err, errProxyDestinationChanged) {
			t.Errorf("stored=%q: err=%v, want errProxyDestinationChanged", storedValue, err)
		}
	}
}

// End to end through the settings handlers: the password never appears in a
// response, an unedited save keeps it, and it is not lent to another proxy.
func TestSettingsProxyPasswordIsMaskedAndPreserved(t *testing.T) {
	s, _ := newProfileRuntimeTestServer(t)
	preserveRuntimeSettings(t, s, settingGlobalProxy, settingWebSearchProxy)
	const stored = "http://user:s3cret@127.0.0.1:9"
	read := func() map[string]any {
		w := httptest.NewRecorder()
		s.getSettings(w, httptest.NewRequest("GET", "/api/settings", nil))
		if strings.Contains(w.Body.String(), "s3cret") {
			t.Fatalf("GET /api/settings leaked the proxy password: %s", w.Body)
		}
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	w := callUserAgentHandler(t, s.putSettings, map[string]any{"web_search_proxy": stored})
	if w.Code != 200 || strings.Contains(w.Body.String(), "s3cret") {
		t.Fatalf("save: status=%d body=%s", w.Code, w.Body)
	}
	masked, _ := read()["web_search_proxy"].(string)
	if masked != maskProxyURL(stored) {
		t.Fatalf("web_search_proxy=%q, want the masked form", masked)
	}
	// Saving the form back unedited keeps the real password.
	if w := callUserAgentHandler(t, s.putSettings, map[string]any{"web_search_proxy": masked}); w.Code != 200 {
		t.Fatalf("unedited save: status=%d body=%s", w.Code, w.Body)
	}
	if _, _, _, _, got := s.m.WebSearch(); got != stored {
		t.Fatalf("stored proxy after an unedited save = %q, want the original", got)
	}
	// Pointing the masked value at another host is refused and changes nothing.
	moved := strings.Replace(masked, "127.0.0.1:9", "127.0.0.1:10", 1)
	if w := callUserAgentHandler(t, s.putSettings, map[string]any{"web_search_proxy": moved}); w.Code != 400 {
		t.Fatalf("masked password accepted for another proxy: status=%d body=%s", w.Code, w.Body)
	}
	if _, _, _, _, got := s.m.WebSearch(); got != stored {
		t.Fatalf("refused save changed the stored proxy to %q", got)
	}
}

// The notify proxy is a settings value like the others: validated, masked on
// read, applied to the notify package, and cleared by saving it empty.
func TestNotifyProxySettingRoundTrip(t *testing.T) {
	s, _ := newProfileRuntimeTestServer(t)
	preserveRuntimeSettings(t, s, settingNotifyProxy)
	// notify.SetProxy is process-global: never leave a proxy behind for the
	// package's other tests, which deliver to local httptest receivers.
	t.Cleanup(func() { _ = notify.SetProxy("") })

	if w := callUserAgentHandler(t, s.putSettings, map[string]any{"notify_proxy": "ftp://proxy.example:21"}); w.Code != 400 {
		t.Fatalf("invalid notify proxy accepted: status=%d body=%s", w.Code, w.Body)
	}
	const stored = "socks5://user:s3cret@127.0.0.1:9"
	w := callUserAgentHandler(t, s.putSettings, map[string]any{"notify_proxy": stored})
	if w.Code != 200 || strings.Contains(w.Body.String(), "s3cret") {
		t.Fatalf("save: status=%d body=%s", w.Code, w.Body)
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if got, _ := out["notify_proxy"].(string); got != maskProxyURL(stored) {
		t.Fatalf("notify_proxy=%q, want the masked form", got)
	}
	if got := notifyProxySetting(s.m.pg); got != stored {
		t.Fatalf("stored notify proxy=%q, want %q", got, stored)
	}
	// Saved back unedited (masked) → unchanged; saved empty → cleared.
	if w := callUserAgentHandler(t, s.putSettings, map[string]any{"notify_proxy": maskProxyURL(stored)}); w.Code != 200 || notifyProxySetting(s.m.pg) != stored {
		t.Fatalf("unedited save: status=%d stored=%q", w.Code, notifyProxySetting(s.m.pg))
	}
	if w := callUserAgentHandler(t, s.putSettings, map[string]any{"notify_proxy": ""}); w.Code != 200 || notifyProxySetting(s.m.pg) != "" {
		t.Fatalf("clear: status=%d stored=%q", w.Code, notifyProxySetting(s.m.pg))
	}
}
