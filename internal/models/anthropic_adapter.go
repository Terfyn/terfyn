package models

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/Terfyn/terfyn/internal/models/anthropic"
	"github.com/Terfyn/terfyn/internal/spec"
)

// anthropicClient adapts [anthropic.Client] to [ModelClient] (issue #69, #158).
type anthropicClient struct {
	inner *anthropic.Client
}

// NewAnthropicClientFromConfig builds a client using apiKeyFrom (e.g. env:ANTHROPIC_API_KEY),
// and optionally workspaceIdFrom (e.g. env:ANTHROPIC_WORKSPACE_ID) for the
// anthropic-workspace-id header required by identity-linked keys.
func NewAnthropicClientFromConfig(cfg spec.ModelProviderConfig) (*anthropicClient, error) {
	key, err := ResolveAPIKeyFrom(cfg.APIKeyFrom)
	if err != nil {
		return nil, err
	}
	var workspaceID string
	if cfg.WorkspaceIDFrom != "" {
		workspaceID, err = ResolveAPIKeyFrom(cfg.WorkspaceIDFrom)
		if err != nil {
			return nil, err
		}
	}
	return &anthropicClient{
		inner: &anthropic.Client{APIKey: key, WorkspaceID: workspaceID, HTTPClient: http.DefaultClient},
	}, nil
}

func (a *anthropicClient) Generate(ctx context.Context, req GenerateRequest) (GenerateResponse, error) {
	if a == nil || a.inner == nil {
		return GenerateResponse{}, fmt.Errorf("models: anthropic client not configured")
	}
	innerReq, err := mapToAnthropicRequest(req)
	if err != nil {
		return GenerateResponse{}, err
	}
	out, err := a.inner.Generate(ctx, innerReq)
	if err != nil {
		return GenerateResponse{}, annotateAnthropicRequestError(err, innerReq)
	}
	resp := mapFromAnthropicResponse(out)
	resp.Meta.CostUSD = estimateAnthropicCostUSD(req.Model, resp.Meta.PromptTokens, resp.Meta.CompletionTokens)
	return resp, nil
}

// annotateAnthropicRequestError attaches redacted structural diagnostics to a provider 4xx (issue
// #524). A 4xx means the provider rejected the request we sent, and a bare "Invalid request data"
// carries no clue why; the appended context (message count, trailing block types, tool_use/tool_result
// pairing) makes it diagnosable. Non-4xx errors (5xx, transport, context cancel) are returned
// unchanged — they are not about request construction.
func annotateAnthropicRequestError(err error, req anthropic.Request) error {
	var apiErr *anthropic.APIError
	if !errors.As(err, &apiErr) || !apiErr.IsClientError() {
		return err
	}
	return fmt.Errorf("%w [request: %s]", err, diagnoseAnthropicRequest(req.Messages))
}
