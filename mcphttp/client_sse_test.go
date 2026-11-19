package mcphttp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLegacySSEClientDiscoverAndCall(t *testing.T) {
	var mu sync.Mutex
	var stream http.ResponseWriter
	var flusher http.Flusher
	sessions := make(map[string]bool)
	messages := make(chan []byte, 8)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sse":
			id := "test-session"
			mu.Lock()
			sessions[id] = true
			stream, flusher = w, w.(http.Flusher)
			mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			fmt.Fprintf(w, "event: endpoint\ndata: %s/message?sessionId=%s\n\n", "http://"+r.Host, id)
			flusher.Flush()
			for {
				select {
				case payload := <-messages:
					fmt.Fprintf(w, "event: message\ndata: %s\n\n", payload)
					flusher.Flush()
				case <-r.Context().Done():
					return
				}
			}
		case "/message":
			mu.Lock()
			knownSession := sessions["test-session"]
			mu.Unlock()
			if r.URL.Query().Get("sessionId") != "test-session" || !knownSession {
				http.Error(w, "unknown session", http.StatusBadRequest)
				return
			}
			var req rpcRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusAccepted)
			if req.ID == 0 {
				return
			}
			var result any
			switch req.Method {
			case "initialize":
				result = map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "test", "version": "1"}}
			case "tools/list":
				result = map[string]any{"tools": []any{map[string]any{"name": "echo", "description": "echo input", "inputSchema": map[string]any{"type": "object"}}}}
			case "tools/call":
				result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "ok"}}}
			default:
				result = map[string]any{}
			}
			resultJSON, err := json.Marshal(result)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			payload, _ := json.Marshal(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(resultJSON)})
			mu.Lock()
			streamReady := stream != nil
			mu.Unlock()
			if streamReady {
				messages <- payload
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := NewSSE(ctx, "test", srv.URL+"/sse", map[string]string{"Authorization ": "Bearer test"}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	tools, err := c.Tools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || !strings.Contains(tools[0].Name(), "echo") {
		t.Fatalf("unexpected tools: %#v", tools)
	}
	got, err := c.Call(ctx, "echo", map[string]any{"value": "x"})
	if err != nil || got != "ok" {
		t.Fatalf("Call() = %q, %v", got, err)
	}
}

// silentSSE accepts the GET and opens the stream but never announces an
// endpoint — what a Streamable-HTTP server looks like when configured as "sse".
func silentSSE(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestLegacySSEHandshakeHonorsContext(t *testing.T) {
	srv := silentSSE(t)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := NewSSE(ctx, "silent", srv.URL+"/sse", nil, false)
	if err == nil || !strings.Contains(err.Error(), "handshake") {
		t.Fatalf("err=%v, want a handshake error", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("handshake ignored ctx: took %s", elapsed)
	}
}

func TestLegacySSEHandshakeTimesOutWithoutDeadline(t *testing.T) {
	old := sseHandshakeTimeout
	sseHandshakeTimeout = 200 * time.Millisecond
	defer func() { sseHandshakeTimeout = old }()
	srv := silentSSE(t)
	start := time.Now()
	_, err := NewSSE(context.Background(), "silent", srv.URL+"/sse", nil, false)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err=%v, want a handshake timeout", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("handshake not bounded: took %s", elapsed)
	}
}

// Once the stream ends, every call must fail promptly. The old one-slot error
// channel woke only the first waiter; later calls waited forever (with a ctx
// without deadline) while holding the call lock, wedging the client.
func TestLegacySSEStreamEndFailsEveryCall(t *testing.T) {
	responses := make(chan []byte, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sse":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: endpoint\ndata: /message\n\n")
			w.(http.Flusher).Flush()
			// Deliver the initialize response, then end the stream.
			select {
			case payload := <-responses:
				fmt.Fprintf(w, "event: message\ndata: %s\n\n", payload)
				w.(http.Flusher).Flush()
			case <-r.Context().Done():
			}
		case "/message":
			var req rpcRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			w.WriteHeader(http.StatusAccepted)
			if req.Method == "initialize" {
				result, _ := json.Marshal(map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}, "serverInfo": map[string]any{"name": "t", "version": "1"}})
				payload, _ := json.Marshal(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(result)})
				responses <- payload
			}
		}
	}))
	defer srv.Close()

	c, err := NewSSE(context.Background(), "ending", srv.URL+"/sse", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	select {
	case <-c.streamDone:
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not end")
	}
	for i := 1; i <= 2; i++ {
		done := make(chan error, 1)
		go func() {
			_, err := c.Tools(context.Background()) // no deadline: must not rely on it
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil {
				t.Fatalf("call %d succeeded on a closed stream", i)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("call %d hung after the SSE stream ended", i)
		}
	}
}

// Every call posts the configured headers (Authorization included) to the
// announced endpoint, so it must stay on the SSE server's own origin.
func TestReadSSEEndpointStaysOnOrigin(t *testing.T) {
	const base = "https://mcp.example:8443/sse"
	for _, tc := range []struct {
		data, want string
		ok         bool
	}{
		{"/message?sessionId=1", "https://mcp.example:8443/message?sessionId=1", true},
		{"message?sessionId=1", "https://mcp.example:8443/message?sessionId=1", true},
		{"https://MCP.example:8443/message", "https://MCP.example:8443/message", true},
		{"https://attacker.example/collect", "", false},
		{"http://mcp.example:8443/message", "", false}, // scheme downgrade
		{"//attacker.example/message", "", false},      // scheme-relative
		{"https://mcp.example:9999/message", "", false},
	} {
		got, err := readSSEEndpoint(bufio.NewReader(strings.NewReader("event: endpoint\ndata: "+tc.data+"\n\n")), base)
		if tc.ok && (err != nil || got != tc.want) {
			t.Errorf("%q: got %q err=%v, want %q", tc.data, got, err, tc.want)
		}
		if !tc.ok && err == nil {
			t.Errorf("%q: cross-origin endpoint accepted as %q", tc.data, got)
		}
	}
}
