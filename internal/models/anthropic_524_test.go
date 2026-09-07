package models

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Terfyn/terfyn/internal/models/anthropic"
)

// A recoverable tool failure is answered by a tool_result carrying is_error:true so the model reads
// it as an error observation to correct from, not a successful output (issue #524).
func TestAnthropic_ToolResult_IsError_WireEncoding(t *testing.T) {
	t.Parallel()
	_, msgs, err := mapAnthropicMessages([]ChatMessage{
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "toolu_1", Name: "edit", Arguments: json.RawMessage(`{}`)}}},
		{Role: "user", ToolResults: []ToolResult{{ToolCallID: "toolu_1", Content: `{"error":"no match"}`, IsError: true}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// assistant tool_use, then user tool_result.
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2: %+v", len(msgs), msgs)
	}
	raw, err := json.Marshal(msgs[1].Blocks[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"is_error":true`) {
		t.Fatalf("tool_result missing is_error: %s", raw)
	}

	// A successful result must NOT set is_error.
	ok, err := json.Marshal(anthropic.ContentBlock{Type: "tool_result", ToolUseID: "toolu_2", Content: "ok"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ok), "is_error") {
		t.Fatalf("successful tool_result should omit is_error: %s", ok)
	}
}

// A dangling tool_use the engine never paired with a result must be answered by a synthesized
// well-formed is_error tool_result rather than left to poison the next request (issue #524).
func TestAnthropic_DanglingToolUse_BackstopAnswered(t *testing.T) {
	t.Parallel()

	t.Run("tail_dangling_gets_synthetic_turn", func(t *testing.T) {
		t.Parallel()
		_, msgs, err := mapAnthropicMessages([]ChatMessage{
			{Role: "user", Content: "go"},
			{Role: "assistant", ToolCalls: []ToolCall{{ID: "toolu_1", Name: "edit", Arguments: json.RawMessage(`{}`)}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		last := msgs[len(msgs)-1]
		if last.Role != "user" || len(last.Blocks) != 1 || last.Blocks[0].Type != "tool_result" {
			t.Fatalf("expected synthetic user tool_result turn, got %+v", last)
		}
		if last.Blocks[0].ToolUseID != "toolu_1" || !last.Blocks[0].IsError {
			t.Fatalf("synthetic result not paired/is_error: %+v", last.Blocks[0])
		}
	})

	t.Run("partial_answer_fills_the_gap", func(t *testing.T) {
		t.Parallel()
		// Two tool calls, only one answered — the other must be backfilled into the same user turn.
		_, msgs, err := mapAnthropicMessages([]ChatMessage{
			{Role: "assistant", ToolCalls: []ToolCall{
				{ID: "toolu_1", Name: "a", Arguments: json.RawMessage(`{}`)},
				{ID: "toolu_2", Name: "b", Arguments: json.RawMessage(`{}`)},
			}},
			{Role: "user", ToolResults: []ToolResult{{ToolCallID: "toolu_1", Content: "ok"}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		user := msgs[len(msgs)-1]
		ids := map[string]bool{}
		for _, b := range user.Blocks {
			if b.Type == "tool_result" {
				ids[b.ToolUseID] = true
			}
		}
		if !ids["toolu_1"] || !ids["toolu_2"] {
			t.Fatalf("both tool_use ids must be answered, got %+v", user.Blocks)
		}
	})
}

// The actual #524 repro is the graceful-finalization shape (#518): at the iteration cap the pending
// tool_use is NOT executed and a finalize-text user turn is appended, leaving [assistant tool_use]
// [user text] — a dangling tool_use the Messages API rejects. This locks the end-to-end contract that
// the backstop and the consecutive-turn merge compose into ONE valid user turn with the synthesized
// is_error tool_result BEFORE the finalize text (the ordering Anthropic requires).
func TestAnthropic_FinalizeShape_ToolResultBeforeText(t *testing.T) {
	t.Parallel()
	const finalize = "You have reached your budget. Return your final answer now."
	_, msgs, err := mapAnthropicMessages([]ChatMessage{
		{Role: "user", Content: "go"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "toolu_1", Name: "edit", Arguments: json.RawMessage(`{}`)}}},
		{Role: "user", Content: finalize},
	})
	if err != nil {
		t.Fatal(err)
	}
	last := msgs[len(msgs)-1]
	if last.Role != "user" {
		t.Fatalf("final turn role = %q, want user: %+v", last.Role, last)
	}
	if len(last.Blocks) != 2 {
		t.Fatalf("final user turn must merge into 2 blocks (tool_result, text), got %+v", last.Blocks)
	}
	if last.Blocks[0].Type != "tool_result" || last.Blocks[0].ToolUseID != "toolu_1" || !last.Blocks[0].IsError {
		t.Fatalf("first block must be the synthesized is_error tool_result: %+v", last.Blocks[0])
	}
	if last.Blocks[1].Type != "text" || last.Blocks[1].Text != finalize {
		t.Fatalf("second block must be the finalize text: %+v", last.Blocks[1])
	}
}

// A tool_result that does not immediately follow its tool_use (an interposed turn) is still recognized
// as answered — the backstop keys off every result id in the conversation, not adjacency — so it never
// synthesizes a duplicate result (a different malformation than the one being fixed).
func TestAnthropic_NonAdjacentResult_NoDuplicate(t *testing.T) {
	t.Parallel()
	_, msgs, err := mapAnthropicMessages([]ChatMessage{
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "toolu_1", Name: "x", Arguments: json.RawMessage(`{}`)}}},
		{Role: "user", ToolResults: []ToolResult{{ToolCallID: "toolu_1", Content: "real result"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var count int
	for _, m := range msgs {
		for _, b := range m.Blocks {
			if b.Type == "tool_result" && b.ToolUseID == "toolu_1" {
				count++
			}
		}
	}
	if count != 1 {
		t.Fatalf("toolu_1 answered %d times, want exactly 1 (no synthesized duplicate)", count)
	}
}

func TestDiagnoseAnthropicRequest(t *testing.T) {
	t.Parallel()
	msgs := []anthropic.ChatMessage{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Blocks: []anthropic.ContentBlock{
			{Type: "text", Text: "thinking"},
			{Type: "tool_use", ID: "toolu_1", Name: "edit"},
		}},
		{Role: "user", Blocks: []anthropic.ContentBlock{
			{Type: "tool_result", ToolUseID: "toolu_1", Content: "ok"},
		}},
	}
	got := diagnoseAnthropicRequest(msgs)
	for _, want := range []string{"messages=3", "tool_use=1", "tool_result=1", "unanswered=[]", "assistant:tool_use"} {
		if !strings.Contains(got, want) {
			t.Fatalf("diagnostic %q missing %q", got, want)
		}
	}
	// It must carry no message content — only structure.
	if strings.Contains(got, "thinking") || strings.Contains(got, "hi") {
		t.Fatalf("diagnostic leaked content: %q", got)
	}

	unpaired := diagnoseAnthropicRequest([]anthropic.ChatMessage{
		{Role: "assistant", Blocks: []anthropic.ContentBlock{{Type: "tool_use", ID: "toolu_9", Name: "x"}}},
	})
	if !strings.Contains(unpaired, "unanswered=[toolu_9]") {
		t.Fatalf("unanswered tool_use not reported: %q", unpaired)
	}
}

func TestAnnotateAnthropicRequestError_4xxOnly(t *testing.T) {
	t.Parallel()
	req := GenerateRequest{
		Model:    "claude-haiku-4-5",
		Messages: []ChatMessage{{Role: "user", Content: "x"}},
		Tools:    []ToolDef{weatherTool()},
	}

	newClient := func(status int) *anthropicClient {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"type":"invalid_request_error","message":"Invalid request data"}`))
		}))
		t.Cleanup(srv.Close)
		return &anthropicClient{inner: &anthropic.Client{APIKey: "sk-ant-mock", BaseURL: srv.URL, HTTPClient: srv.Client()}}
	}

	// 4xx: diagnostic context attached.
	_, err := newClient(http.StatusBadRequest).Generate(context.Background(), req)
	if err == nil {
		t.Fatal("expected error on 400")
	}
	if !strings.Contains(err.Error(), "HTTP 400") || !strings.Contains(err.Error(), "request: messages=") {
		t.Fatalf("400 error not annotated: %v", err)
	}

	// 5xx: not a request-construction error, left unannotated.
	_, err = newClient(http.StatusInternalServerError).Generate(context.Background(), req)
	if err == nil {
		t.Fatal("expected error on 500")
	}
	if strings.Contains(err.Error(), "request: messages=") {
		t.Fatalf("500 error should not be annotated: %v", err)
	}
}
