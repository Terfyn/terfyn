package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Terfyn/terfyn/internal/jsonnum"
)

// Connector is an MCP session that can exchange JSON-RPC over a transport (stdio or HTTP).
type Connector interface {
	RoundTrip(ctx context.Context, method string, params any) (json.RawMessage, error)
	Notify(ctx context.Context, method string, params map[string]any) error
}

// Initialize performs the MCP initialize + notifications/initialized handshake.
func Initialize(ctx context.Context, c Connector) error {
	params := map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo": map[string]any{
			"name":    "terfyn",
			"version": "0",
		},
	}
	if _, err := c.RoundTrip(ctx, "initialize", params); err != nil {
		return err
	}
	return c.Notify(ctx, "notifications/initialized", map[string]any{})
}

// CallTool invokes tools/call and maps the MCP result into a plain map for §13.2 output.
func CallTool(ctx context.Context, c Connector, name string, arguments map[string]any) (map[string]any, error) {
	if arguments == nil {
		arguments = map[string]any{}
	}
	raw, err := c.RoundTrip(ctx, "tools/call", map[string]any{
		"name":      name,
		"arguments": arguments,
	})
	if err != nil {
		return nil, err
	}
	return parseCallToolResult(raw)
}

func parseCallToolResult(raw json.RawMessage) (map[string]any, error) {
	var envelope struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("mcp: decode tools/call result: %w", err)
	}
	if envelope.IsError {
		return nil, rpcErrorf("tools/call isError=true: %s", string(raw))
	}
	if len(envelope.Content) == 0 {
		return map[string]any{}, nil
	}
	first := envelope.Content[0]
	if first.Type == "text" && first.Text != "" {
		var obj map[string]any
		if jsonnum.Unmarshal([]byte(first.Text), &obj) == nil {
			return obj, nil
		}
		return map[string]any{"text": first.Text}, nil
	}
	return map[string]any{"content": envelope.Content}, nil
}
