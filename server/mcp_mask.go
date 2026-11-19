package server

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/notify"
)

// An MCP server's env map is its process environment (stdio) or its request
// headers (http / sse) — in practice mostly credentials: Authorization, X-API-Key,
// provider tokens. GET /api/mcp used to return it verbatim, the one place stored
// credentials were echoed to the browser while LLM keys and notify secrets are
// only ever hinted.
//
// Every value is masked on the way out (notify's "__masked__:…last6" form, so a
// value stays recognizable). No attempt is made to guess which values are secret:
// a wrong guess would silently keep leaking one. A value that comes back still
// masked means "keep what is stored".

// maskMCPEnv returns env with every non-empty value masked.
func maskMCPEnv(raw json.RawMessage) json.RawMessage {
	env := jsonStrMap(raw)
	if len(env) == 0 {
		return raw
	}
	for k, v := range env {
		if v != "" {
			env[k] = notify.MaskedValue(v)
		}
	}
	return encodeJSON(env)
}

// maskedMCPServers returns copies safe to send to the client.
func maskedMCPServers(in []*db.MCPServer) []*db.MCPServer {
	out := make([]*db.MCPServer, 0, len(in))
	for _, m := range in {
		c := *m
		c.Env = maskMCPEnv(m.Env)
		out = append(out, &c)
	}
	return out
}

// mergeMaskedMCPEnv replaces masked values in incoming.Env with the stored ones.
//
// Stored values are only reused for the server they were saved with. For a
// remote transport that means the same origin: the values are request headers,
// and keeping them while pointing the URL elsewhere would send them there. A
// stdio server gets no such check — its command is arbitrary code the operator
// chose, which could read anything anyway — so there the mask only keeps the
// values off the screen.
func mergeMaskedMCPEnv(incoming, existing *db.MCPServer) error {
	env := jsonStrMap(incoming.Env)
	var masked []string
	for k, v := range env {
		if notify.IsMasked(v) {
			masked = append(masked, k)
		}
	}
	if len(masked) == 0 {
		return nil
	}
	sort.Strings(masked)
	if existing == nil {
		return fmt.Errorf("%s 的值是掩码，新建时请填写实际的值", strings.Join(masked, "、"))
	}
	if incoming.Transport != "stdio" && (existing.Transport == "stdio" || !sameURLOrigin(existing.URL, incoming.URL)) {
		return fmt.Errorf("地址已变更：已保存的 %s 只会发往原来的地址，请重新填写", strings.Join(masked, "、"))
	}
	stored := jsonStrMap(existing.Env)
	for _, k := range masked {
		v, ok := stored[k]
		if !ok {
			return fmt.Errorf("%s 没有已保存的值，请填写实际的值", k)
		}
		env[k] = v
	}
	incoming.Env = encodeJSON(env)
	return nil
}

// sameURLOrigin reports whether two URLs share scheme and host (incl. port).
func sameURLOrigin(a, b string) bool {
	ua, errA := url.Parse(strings.TrimSpace(a))
	ub, errB := url.Parse(strings.TrimSpace(b))
	if errA != nil || errB != nil || ua.Host == "" || ub.Host == "" {
		return false
	}
	return strings.EqualFold(ua.Scheme, ub.Scheme) && strings.EqualFold(ua.Host, ub.Host)
}

// findMCPByID returns the MCP server row with the given id, or nil.
func (s *Server) findMCPByID(id int64) (*db.MCPServer, error) {
	all, err := s.m.pg.ListMCP()
	if err != nil {
		return nil, err
	}
	for _, m := range all {
		if m.ID == id {
			return m, nil
		}
	}
	return nil, nil
}
