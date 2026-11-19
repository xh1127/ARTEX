package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/notify"
)

func mcpEnv(t *testing.T, kv map[string]string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(kv)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestMaskMCPEnvHidesEveryValue(t *testing.T) {
	raw := mcpEnv(t, map[string]string{"Authorization": "Bearer sk-live-abcdef123456", "X-API-Key": "k1", "EMPTY": ""})
	masked := jsonStrMap(maskMCPEnv(raw))
	if len(masked) != 3 {
		t.Fatalf("keys changed: %v", masked)
	}
	for k, v := range masked {
		if k == "EMPTY" {
			if v != "" {
				t.Errorf("empty value was masked into %q", v)
			}
			continue
		}
		if !notify.IsMasked(v) || strings.Contains(v, "sk-live") || v == "k1" {
			t.Errorf("%s not masked: %q", k, v)
		}
	}
	list := maskedMCPServers([]*db.MCPServer{{ID: 1, Name: "s", Env: raw}})
	if string(list[0].Env) == string(raw) || jsonStrMap(raw)["X-API-Key"] != "k1" {
		t.Fatal("masking must return copies and leave the stored rows untouched")
	}
}

func TestMergeMaskedMCPEnv(t *testing.T) {
	stored := &db.MCPServer{
		ID: 7, Name: "remote", Transport: "http", URL: "https://mcp.example/api",
		Env: mcpEnv(t, map[string]string{"Authorization": "Bearer real-token-123456", "X-Trace": "on"}),
	}
	maskedEnv := maskMCPEnv(stored.Env)

	// Unedited save (also what the enable toggle sends): stored values restored.
	in := &db.MCPServer{ID: 7, Name: "remote", Transport: "http", URL: "https://mcp.example/api/v2", Env: maskedEnv}
	if err := mergeMaskedMCPEnv(in, stored); err != nil {
		t.Fatal(err)
	}
	if got := jsonStrMap(in.Env); got["Authorization"] != "Bearer real-token-123456" || got["X-Trace"] != "on" {
		t.Fatalf("stored values not restored: %v", got)
	}

	// One value retyped, one left masked.
	mixed := jsonStrMap(maskedEnv)
	mixed["X-Trace"] = "off"
	in = &db.MCPServer{ID: 7, Transport: "http", URL: stored.URL, Env: mcpEnv(t, mixed)}
	if err := mergeMaskedMCPEnv(in, stored); err != nil {
		t.Fatal(err)
	}
	if got := jsonStrMap(in.Env); got["Authorization"] != "Bearer real-token-123456" || got["X-Trace"] != "off" {
		t.Fatalf("mixed edit merged wrong: %v", got)
	}

	// Masked headers must not follow the URL to another origin.
	for name, target := range map[string]string{"other host": "https://attacker.example/api", "downgrade": "http://mcp.example/api"} {
		in = &db.MCPServer{ID: 7, Transport: "http", URL: target, Env: maskedEnv}
		if err := mergeMaskedMCPEnv(in, stored); err == nil {
			t.Errorf("%s: stored headers were kept for %s", name, target)
		}
	}
	// Nor from a stdio server's environment into a remote server's headers.
	stdio := &db.MCPServer{ID: 8, Transport: "stdio", Command: "npx", Env: mcpEnv(t, map[string]string{"TOKEN": "process-secret-1"})}
	in = &db.MCPServer{ID: 8, Transport: "sse", URL: "https://attacker.example/sse", Env: maskMCPEnv(stdio.Env)}
	if err := mergeMaskedMCPEnv(in, stdio); err == nil {
		t.Error("a stdio server's env was kept as headers for a new remote URL")
	}
	// stdio → stdio keeps values (the command is the operator's own code).
	in = &db.MCPServer{ID: 8, Transport: "stdio", Command: "uvx", Env: maskMCPEnv(stdio.Env)}
	if err := mergeMaskedMCPEnv(in, stdio); err != nil || jsonStrMap(in.Env)["TOKEN"] != "process-secret-1" {
		t.Errorf("stdio env not preserved: err=%v env=%s", err, in.Env)
	}

	// A mask with nothing behind it is an error, never stored as a literal.
	in = &db.MCPServer{Transport: "http", URL: "https://mcp.example", Env: maskedEnv}
	if err := mergeMaskedMCPEnv(in, nil); err == nil {
		t.Error("masked values accepted on create")
	}
	renamed := mcpEnv(t, map[string]string{"X-New-Name": notify.MaskedValue("whatever-secret")})
	in = &db.MCPServer{ID: 7, Transport: "http", URL: stored.URL, Env: renamed}
	if err := mergeMaskedMCPEnv(in, stored); err == nil {
		t.Error("a masked value under a key with no stored value was accepted")
	}

	// Nothing masked: taken as submitted, whatever is stored.
	plain := mcpEnv(t, map[string]string{"Authorization": "Bearer brand-new"})
	in = &db.MCPServer{ID: 7, Transport: "http", URL: "https://elsewhere.example", Env: plain}
	if err := mergeMaskedMCPEnv(in, stored); err != nil || string(in.Env) != string(plain) {
		t.Errorf("plain env altered: err=%v env=%s", err, in.Env)
	}
}

func TestSameURLOrigin(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"https://h.example/a", "https://H.example/b?x=1", true},
		{"https://h.example", "https://h.example:8443", false},
		{"https://h.example", "http://h.example", false},
		{"https://h.example", "", false},
		{"", "", false},
		{"not a url", "https://h.example", false},
	} {
		if got := sameURLOrigin(tc.a, tc.b); got != tc.want {
			t.Errorf("sameURLOrigin(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}
