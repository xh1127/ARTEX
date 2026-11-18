package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Autumn-27/norma/agentcore"
	actool "github.com/Autumn-27/norma/tool"
)

func (o WebSearchOpts) sdkConfig() actool.WebSearchConfig {
	return actool.WebSearchConfig{
		Backend: o.Backend, BraveAPIKey: o.BraveKey, TavilyAPIKey: o.TavilyKey, Proxy: o.Proxy,
		DeepSeekBaseURL: o.DeepSeekBaseURL, DeepSeekAPIKey: o.DeepSeekAPIKey, DeepSeekModel: o.DeepSeekModel,
	}
}

// configureWebSearch keeps all agent roles on the same search implementation.
// The SDK does not expose a search HTTP client/header hook, so DeepSeek uses a
// local runner while retaining the SDK tool's schema and permission checks.
//
// A build failure is logged rather than swallowed: the tool simply vanishing is
// indistinguishable from "the backend is off", and the most likely cause — an
// invalid User-Agent inherited from the active LLM profile — would otherwise
// leave the operator guessing.
func configureWebSearch(opts *agentcore.Options, search WebSearchOpts) {
	if !search.Enabled {
		return
	}
	t, err := newWebSearch(search)
	if err != nil {
		log.Printf("[web_search] 已启用但构建失败，本轮不提供该工具（backend=%q）: %v", search.Backend, err)
		return
	}
	if t == nil { // a backend that reports success but yields no tool
		log.Printf("[web_search] 已启用但未返回工具（backend=%q）", search.Backend)
		return
	}
	opts.Tools = append(opts.Tools, t)
}

func newWebSearch(o WebSearchOpts) (actool.CoreTool, error) {
	t, err := actool.NewWebSearch(o.sdkConfig())
	if err != nil || !strings.EqualFold(strings.TrimSpace(o.Backend), "deepseek") {
		return t, err
	}
	ua, err := NormalizeUserAgent(o.DeepSeekUserAgent)
	if err != nil {
		return nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil // Search has its own proxy setting; empty means direct.
	if proxy := strings.TrimSpace(o.Proxy); proxy != "" {
		u, err := url.Parse(proxy)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5") {
			return nil, fmt.Errorf("web_search: proxy must be an http, https or socks5 URL")
		}
		transport.Proxy = http.ProxyURL(u)
	}
	endpoint := strings.TrimRight(strings.TrimSpace(o.DeepSeekBaseURL), "/")
	switch {
	case strings.HasSuffix(endpoint, "/v1/messages"):
	case strings.HasSuffix(endpoint, "/v1"):
		endpoint += "/messages"
	default:
		endpoint += "/v1/messages"
	}
	return &deepSeekSearchTool{
		CoreTool: t, client: &http.Client{Transport: transport}, endpoint: endpoint,
		apiKey: strings.TrimSpace(o.DeepSeekAPIKey), model: strings.TrimSpace(o.DeepSeekModel), userAgent: ua,
	}, nil
}

// ProbeWebSearch uses the same backend and headers as the live web_search tool.
// Enabled is intentionally ignored: settings can be tested before enabling them.
func ProbeWebSearch(ctx context.Context, o WebSearchOpts, query string, limit int) ([]actool.SearchResult, error) {
	if !strings.EqualFold(strings.TrimSpace(o.Backend), "deepseek") {
		return actool.WebSearchProbe(ctx, o.sdkConfig(), query, limit)
	}
	t, err := newWebSearch(o)
	if err != nil {
		return nil, err
	}
	ds, ok := t.(*deepSeekSearchTool)
	if !ok || ds == nil {
		return nil, fmt.Errorf("web_search: deepseek backend did not build a search runner")
	}
	if limit <= 0 {
		limit = 3
	}
	return ds.search(ctx, query, min(limit, 20))
}

type deepSeekSearchTool struct {
	actool.CoreTool
	client                             *http.Client
	endpoint, apiKey, model, userAgent string
}

func (t *deepSeekSearchTool) Call(ctx context.Context, input json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
	var in struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return actool.Result{}, err
	}
	query := strings.TrimSpace(in.Query)
	if query == "" {
		return actool.Errorf("Error: query must not be empty"), nil
	}
	if in.Limit <= 0 {
		in.Limit = 5
	}
	results, err := t.search(ctx, query, min(in.Limit, 20))
	if err != nil {
		return actool.Errorf(fmt.Sprintf("Error searching web via deepseek: %s", err)), nil
	}
	out, _ := json.MarshalIndent(map[string]any{"backend": "deepseek", "query": query, "results": results}, "", "  ")
	text := []rune(string(out))
	if len(text) > 50000 {
		text = append(text[:50000], []rune("\n… (truncated)")...)
	}
	return actool.Text(string(text)), nil
}

func (t *deepSeekSearchTool) search(ctx context.Context, query string, limit int) ([]actool.SearchResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	// Agents create a fresh session per turn; release this runner's idle sockets.
	defer t.client.CloseIdleConnections()
	body, err := json.Marshal(map[string]any{
		"model": t.model, "max_tokens": 1024, "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "Search the web for: " + query + "\n\nUse the web_search tool. Do not answer from memory; reply with at most one short sentence once the search is done."}},
		"tools":    []any{map[string]any{"type": "web_search_20250305", "name": "web_search", "max_uses": min(max((limit+9)/10, 1), 3)}},
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("x-api-key", t.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	if t.userAgent != "" {
		req.Header.Set("User-Agent", t.userAgent)
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("DeepSeek search returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return readDeepSeekSearch(resp.Body, limit)
}

// Search hits are complete content_block_start frames; text/reasoning deltas
// are irrelevant. Large encrypted_content fields require more than 64 KiB.
func readDeepSeekSearch(body io.Reader, limit int) ([]actool.SearchResult, error) {
	const maxBytes = 2 << 20
	r := &io.LimitedReader{R: body, N: maxBytes + 1}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), maxBytes+1)
	var results []actool.SearchResult
	seen := make(map[string]bool)
	var toolError string
	for scanner.Scan() {
		data, ok := strings.CutPrefix(strings.TrimSpace(scanner.Text()), "data:")
		if !ok || strings.TrimSpace(data) == "[DONE]" {
			continue
		}
		var frame struct {
			Type  string `json:"type"`
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
			Block struct {
				Type    string          `json:"type"`
				Content json.RawMessage `json:"content"`
			} `json:"content_block"`
		}
		if json.Unmarshal([]byte(data), &frame) != nil {
			continue
		}
		if frame.Type == "error" {
			return nil, fmt.Errorf("DeepSeek search stream error: %s", frame.Error.Message)
		}
		if frame.Type != "content_block_start" || frame.Block.Type != "web_search_tool_result" {
			continue
		}
		type hit struct {
			Title     string `json:"title"`
			URL       string `json:"url"`
			ErrorCode string `json:"error_code"`
		}
		var hits []hit
		if err := json.Unmarshal(frame.Block.Content, &hits); err != nil {
			// A tool failure may be an object instead of an array of hits.
			var failure hit
			if json.Unmarshal(frame.Block.Content, &failure) != nil || failure.ErrorCode == "" {
				return nil, fmt.Errorf("invalid DeepSeek search results: %w", err)
			}
			hits = []hit{failure}
		}
		for _, h := range hits {
			if h.ErrorCode != "" {
				toolError = h.ErrorCode
				continue
			}
			if h.URL == "" || seen[h.URL] {
				continue
			}
			seen[h.URL] = true
			if strings.TrimSpace(h.Title) == "" {
				h.Title = h.URL
				if u, err := url.Parse(h.URL); err == nil && u.Host != "" {
					h.Title = u.Host
				}
			}
			results = append(results, actool.SearchResult{Title: h.Title, URL: h.URL, Position: len(results) + 1})
			if len(results) >= limit {
				return results, nil
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("could not read DeepSeek search stream: %w", err)
	}
	if r.N == 0 {
		return nil, fmt.Errorf("DeepSeek search response exceeds %d bytes", maxBytes)
	}
	if len(results) == 0 && toolError != "" {
		return nil, fmt.Errorf("DeepSeek web search failed: %s", toolError)
	}
	return results, nil
}
