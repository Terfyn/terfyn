package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Terfyn/terfyn/internal/spec"
)

func mockMCPExecutable(t *testing.T) string {
	t.Helper()
	name := "mockmcp"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	out := filepath.Join(t.TempDir(), name)
	cmd := exec.Command("go", "build", "-o", out, "./testdata/mockmcp")
	cmd.Dir = "."
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build mockmcp: %v\n%s", err, b)
	}
	return out
}

func TestCall_stdio_mockSubprocess(t *testing.T) {
	bin := mockMCPExecutable(t)
	ctx := context.Background()
	out, meta, err := Call(ctx, &spec.ToolMCP{
		Transport: "stdio",
		Command:   bin,
	}, nil, "any", map[string]any{"repo": "acme/api", "n": float64(3)})
	if err != nil {
		t.Fatal(err)
	}
	if meta.DurationMs < 0 {
		t.Fatalf("meta %+v", meta)
	}
	if out["repo"] != "acme/api" || out["n"] != int64(3) {
		t.Fatalf("output %+v", out)
	}
}

func TestCall_stdio_retryOnTransportFailure(t *testing.T) {
	ctx := context.Background()
	_, _, err := Call(ctx, &spec.ToolMCP{
		Transport: "stdio",
		Command:   "/nonexistent/binary/mcp-missing-xyz",
	}, &spec.ToolRetry{MaxAttempts: 2, Backoff: "fixed"}, "x", nil)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestCall_http_mockServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("want POST, got %s", r.Method)
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		var msg map[string]any
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		method, _ := msg["method"].(string)
		switch method {
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
			return
		case "initialize":
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Mcp-Session-Id", "test-session")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      msg["id"],
				"result": map[string]any{
					"protocolVersion": "2024-11-05",
					"capabilities":    map[string]any{},
					"serverInfo":      map[string]any{"name": "mock-http", "version": "1"},
				},
			})
			return
		case "tools/call":
			params, _ := msg["params"].(map[string]any)
			args, _ := params["arguments"].(map[string]any)
			if args == nil {
				args = map[string]any{}
			}
			text, _ := json.Marshal(args)
			result := map[string]any{
				"content": []map[string]any{
					{"type": "text", "text": string(text)},
				},
				"isError": false,
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      msg["id"],
				"result":  result,
			})
			return
		default:
			t.Fatalf("unexpected method %q", method)
		}
	}))
	defer srv.Close()

	ctx := context.Background()
	out, meta, err := Call(ctx, &spec.ToolMCP{
		Transport: "http",
		URL:       srv.URL + "/mcp",
	}, nil, "any", map[string]any{"repo": "acme/api", "n": float64(3)})
	if err != nil {
		t.Fatal(err)
	}
	if meta.DurationMs < 0 {
		t.Fatalf("meta %+v", meta)
	}
	if out["repo"] != "acme/api" || out["n"] != int64(3) {
		t.Fatalf("output %+v", out)
	}
}

func TestCall_http_sseInitializeResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg map[string]any
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		method, _ := msg["method"].(string)
		id := msg["id"]
		switch method {
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
			return
		case "initialize":
			w.Header().Set("Content-Type", "text/event-stream")
			line, _ := json.Marshal(map[string]any{
				"jsonrpc": "2.0",
				"id":      id,
				"result": map[string]any{
					"protocolVersion": "2024-11-05",
					"capabilities":    map[string]any{},
					"serverInfo":      map[string]any{"name": "sse-mock", "version": "1"},
				},
			})
			_, _ = fmt.Fprintf(w, "data: %s\n\n", line)
			return
		case "tools/call":
			params, _ := msg["params"].(map[string]any)
			args, _ := params["arguments"].(map[string]any)
			if args == nil {
				args = map[string]any{}
			}
			text, _ := json.Marshal(args)
			result := map[string]any{
				"content": []map[string]any{
					{"type": "text", "text": string(text)},
				},
				"isError": false,
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      id,
				"result":  result,
			})
			return
		default:
			t.Fatalf("unexpected method %q", method)
		}
	}))
	defer srv.Close()

	ctx := context.Background()
	out, _, err := Call(ctx, &spec.ToolMCP{
		Transport: "http",
		URL:       srv.URL,
	}, nil, "t", map[string]any{"k": "v"})
	if err != nil {
		t.Fatal(err)
	}
	if out["k"] != "v" {
		t.Fatalf("output %+v", out)
	}
}

func TestCall_http_retryOnTransportFailure(t *testing.T) {
	ctx := context.Background()
	_, _, err := Call(ctx, &spec.ToolMCP{
		Transport: "http",
		URL:       "http://127.0.0.1:9/nope",
	}, &spec.ToolRetry{MaxAttempts: 2, Backoff: "fixed"}, "x", nil)
	if err == nil {
		t.Fatal("expected error")
	}
}
