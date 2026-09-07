package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClient_Generate_messagesAPI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("x-api-key") != "sk-ant-test" {
			t.Errorf("x-api-key %q", r.Header.Get("x-api-key"))
		}
		if r.Header.Get("anthropic-version") != apiVersion {
			t.Errorf("anthropic-version %q", r.Header.Get("anthropic-version"))
		}
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		var req struct {
			Model    string `json:"model"`
			System   string `json:"system"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
			MaxTokens int `json:"max_tokens"`
		}
		if err := json.Unmarshal(b, &req); err != nil {
			t.Fatal(err)
		}
		if req.Model != "claude-sonnet-4-20250514" {
			t.Errorf("model %q", req.Model)
		}
		if req.System != "Be brief." {
			t.Errorf("system %q", req.System)
		}
		if len(req.Messages) != 1 || req.Messages[0].Role != "user" || req.Messages[0].Content != `{"q":1}` {
			t.Fatalf("messages %+v", req.Messages)
		}
		if req.MaxTokens != defaultMaxTok {
			t.Errorf("max_tokens %d", req.MaxTokens)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"{\"ok\":true}"}],"usage":{"input_tokens":10,"output_tokens":20}}`))
	}))
	defer srv.Close()

	c := &Client{APIKey: "sk-ant-test", BaseURL: srv.URL, HTTPClient: srv.Client()}
	resp, err := c.Generate(context.Background(), Request{
		Model:  "claude-sonnet-4-20250514",
		System: "Be brief.",
		Messages: []ChatMessage{
			{Role: "user", Content: `{"q":1}`},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != `{"ok":true}` {
		t.Fatalf("text %q", resp.Text)
	}
	if resp.InputTokens != 10 || resp.OutputTokens != 20 {
		t.Fatalf("usage in=%d out=%d", resp.InputTokens, resp.OutputTokens)
	}
	if resp.StopReason != stopEndTurn {
		t.Fatalf("stop %q", resp.StopReason)
	}
}

func TestClient_Generate_concatTextBlocks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}`))
	}))
	defer srv.Close()

	c := &Client{APIKey: "k", BaseURL: srv.URL, HTTPClient: srv.Client()}
	resp, err := c.Generate(context.Background(), Request{
		Model:    "m",
		Messages: []ChatMessage{{Role: "user", Content: "x"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "ab" {
		t.Fatalf("got %q", resp.Text)
	}
}

func TestClient_Generate_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"bad key"}}`))
	}))
	defer srv.Close()

	c := &Client{APIKey: "bad", BaseURL: srv.URL, HTTPClient: srv.Client()}
	_, err := c.Generate(context.Background(), Request{
		Model:    "m",
		Messages: []ChatMessage{{Role: "user", Content: "x"}},
	})
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("got %v", err)
	}
	// The error is a typed *APIError carrying the status so a caller can tell a rejected request (4xx)
	// from a transient 5xx and attach diagnostics on the former (issue #524).
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error is not *APIError: %T", err)
	}
	if apiErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("StatusCode = %d, want 401", apiErr.StatusCode)
	}
	if apiErr.IsClientError() != true {
		t.Fatalf("401 must be a client error")
	}
}

func TestClient_Generate_omitsToolsWhenEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatal(err)
		}
		if _, ok := got["tools"]; ok {
			t.Fatalf("tools present: %v", got["tools"])
		}
		if _, ok := got["tool_choice"]; ok {
			t.Fatalf("tool_choice present: %v", got["tool_choice"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`))
	}))
	defer srv.Close()

	c := &Client{APIKey: "k", BaseURL: srv.URL, HTTPClient: srv.Client()}
	resp, err := c.Generate(context.Background(), Request{
		Model:    "m",
		Messages: []ChatMessage{{Role: "user", Content: "x"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "ok" {
		t.Fatalf("text %q", resp.Text)
	}
}

func TestClient_Generate_workspaceIDHeader(t *testing.T) {
	for _, tc := range []struct {
		name      string
		workspace string
		want      string // expected anthropic-workspace-id header value
	}{
		{name: "set", workspace: "wrkspc_123", want: "wrkspc_123"},
		{name: "unset", workspace: "", want: ""},
		{name: "trimmed", workspace: "  wrkspc_9  ", want: "wrkspc_9"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("anthropic-workspace-id"); got != tc.want {
					t.Errorf("anthropic-workspace-id = %q, want %q", got, tc.want)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`))
			}))
			defer srv.Close()

			c := &Client{APIKey: "k", BaseURL: srv.URL, WorkspaceID: tc.workspace, HTTPClient: srv.Client()}
			if _, err := c.Generate(context.Background(), Request{
				Model:    "m",
				Messages: []ChatMessage{{Role: "user", Content: "x"}},
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
