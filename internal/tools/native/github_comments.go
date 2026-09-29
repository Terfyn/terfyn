package native

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/Terfyn/terfyn/internal/jsonnum"
)

// AgenticReviewMarker is embedded in automated PR review issue comments so synchronize
// runs can find and update the same comment instead of posting anew.
const AgenticReviewMarker = "<!-- agentic-review -->"

const (
	commentStrategyAppend  = "append"
	commentStrategyReplace = "replace"
)

// githubCommentStrategy returns append or replace for live post_comment calls.
// Default is replace so PR synchronize events do not spam issue comments.
// upsert: true is an alias for replace.
func githubCommentStrategy(with map[string]any) (string, error) {
	if upsert, ok := with["upsert"]; ok {
		b, err := scalarToBool(upsert)
		if err != nil {
			return "", fmt.Errorf("native: post_comment upsert: %w", err)
		}
		if b {
			return commentStrategyReplace, nil
		}
	}
	if s, ok := tryStringFromWith(with, "comment_strategy"); ok {
		switch strings.ToLower(s) {
		case commentStrategyAppend:
			return commentStrategyAppend, nil
		case commentStrategyReplace:
			return commentStrategyReplace, nil
		default:
			return "", fmt.Errorf("native: post_comment comment_strategy must be %q or %q, got %q",
				commentStrategyAppend, commentStrategyReplace, s)
		}
	}
	return commentStrategyReplace, nil
}

func scalarToBool(v any) (bool, error) {
	switch x := v.(type) {
	case bool:
		return x, nil
	case string:
		s := strings.TrimSpace(strings.ToLower(x))
		switch s {
		case "true", "1", "yes":
			return true, nil
		case "false", "0", "no":
			return false, nil
		default:
			return false, fmt.Errorf("unsupported bool string %q", x)
		}
	default:
		return false, fmt.Errorf("unsupported type %T", v)
	}
}

func ensureAgenticReviewMarker(body string) string {
	if strings.Contains(body, AgenticReviewMarker) {
		return body
	}
	body = strings.TrimRight(body, "\n")
	if body == "" {
		return AgenticReviewMarker
	}
	return body + "\n\n" + AgenticReviewMarker
}

// githubPullRequestPostComment posts or updates an issue comment on a PR.
// strategy is append (always POST) or replace (PATCH existing marker comment or POST once).
func githubPullRequestPostComment(ctx context.Context, owner, repo, number, body, strategy string) (map[string]any, error) {
	switch strategy {
	case commentStrategyAppend:
		return githubPullRequestCreateComment(ctx, owner, repo, number, body)
	case commentStrategyReplace:
		return githubPullRequestReplaceComment(ctx, owner, repo, number, body)
	default:
		return nil, fmt.Errorf("native: post_comment unknown strategy %q", strategy)
	}
}

func githubPullRequestCreateComment(ctx context.Context, owner, repo, number, body string) (map[string]any, error) {
	path := fmt.Sprintf("/repos/%s/%s/issues/%s/comments", url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(number))
	payload := map[string]string{"body": body}
	b, err := githubPOSTJSON(ctx, path, payload, maxGitHubJSONBody)
	if err != nil {
		return nil, err
	}
	return decodeGitHubCommentResponse(b, false)
}

func githubPullRequestReplaceComment(ctx context.Context, owner, repo, number, body string) (map[string]any, error) {
	body = ensureAgenticReviewMarker(body)
	existingID, err := githubFindAgenticReviewCommentID(ctx, owner, repo, number)
	if err != nil {
		return nil, err
	}
	if existingID != "" {
		return githubUpdateIssueComment(ctx, owner, repo, existingID, body)
	}
	out, err := githubPullRequestCreateComment(ctx, owner, repo, number, body)
	if err != nil {
		return nil, err
	}
	out["created"] = true
	out["updated"] = false
	return out, nil
}

// githubFindAgenticReviewCommentID returns the id of the issue comment carrying
// AgenticReviewMarker, or "" when the PR has none. It walks the comments with the
// shared Link-following walker (githubWalkArray) and stops at the first match. If the
// page cap is reached with comments remaining and no match, it fails instead of
// returning "": "not found" would make replace create a second marker comment.
func githubFindAgenticReviewCommentID(ctx context.Context, owner, repo, number string) (string, error) {
	path := fmt.Sprintf("/repos/%s/%s/issues/%s/comments?per_page=%d",
		url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(number), githubListPerPage)
	var found string
	more, err := githubWalkArray(ctx, path, "pull_request.post_comment list comments", func(page []any) bool {
		for _, raw := range page {
			c, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			id, body, ok := commentIDAndBody(c)
			if ok && strings.Contains(body, AgenticReviewMarker) {
				found = id
				return false
			}
		}
		return true
	})
	if err != nil {
		return "", err
	}
	if found != "" {
		return found, nil
	}
	if more {
		return "", fmt.Errorf("native: pull_request.post_comment: no %s comment in the first %d comments and more remain; refusing to post a possibly duplicate comment (pass comment_id, or comment_strategy: append)",
			AgenticReviewMarker, githubListMaxLimit)
	}
	return "", nil
}

func commentIDAndBody(c map[string]any) (id, body string, ok bool) {
	rawID, okID := c["id"]
	if !okID || rawID == nil {
		return "", "", false
	}
	idStr, err := scalarToString(rawID)
	if err != nil || idStr == "" {
		return "", "", false
	}
	rawBody, okBody := c["body"]
	if !okBody || rawBody == nil {
		return "", "", false
	}
	bodyStr, err := scalarToString(rawBody)
	if err != nil {
		return "", "", false
	}
	return idStr, bodyStr, true
}

func githubUpdateIssueComment(ctx context.Context, owner, repo, commentID, body string) (map[string]any, error) {
	path := fmt.Sprintf("/repos/%s/%s/issues/comments/%s",
		url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(commentID))
	payload := map[string]string{"body": body}
	b, err := githubPATCHJSON(ctx, path, payload, maxGitHubJSONBody)
	if err != nil {
		return nil, err
	}
	out, err := decodeGitHubCommentResponse(b, true)
	if err != nil {
		return nil, err
	}
	out["updated"] = true
	out["created"] = false
	return out, nil
}

func decodeGitHubCommentResponse(b []byte, updated bool) (map[string]any, error) {
	var out map[string]any
	if err := jsonnum.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("native: pull_request.post_comment decode: %w", err)
	}
	if out == nil {
		out = map[string]any{}
	}
	out["simulated"] = false
	out["updated"] = updated
	out["created"] = !updated
	return out, nil
}

func githubPATCHJSON(ctx context.Context, path string, payload any, maxResp int64) ([]byte, error) {
	return githubJSONRequest(ctx, http.MethodPatch, path, payload, maxResp)
}

func githubJSONRequest(ctx context.Context, method, path string, payload any, maxResp int64) ([]byte, error) {
	token, err := githubToken()
	if err != nil {
		return nil, err
	}
	var body io.Reader
	if payload != nil {
		bodyBytes, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("native: github encode body: %w", err)
		}
		body = bytes.NewReader(bodyBytes)
	}
	fullURL, err := githubPathURL(path)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, fullURL, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", githubUserAgent)
	req.Header.Set("Accept", githubAcceptJSON)
	if body != nil {
		req.Header.Set("Content-Type", githubAcceptJSON)
	}
	req.Header.Set("X-GitHub-Api-Version", githubAPIVersion)

	cli := defaultGitHubHTTPClient()
	resp, err := cli.Do(req)
	if err != nil {
		return nil, fmt.Errorf("native: github request: %w", err)
	}
	defer resp.Body.Close()

	b, err := readGitHubResponseBody(resp, maxResp)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("native: github HTTP %s: %s", resp.Status, truncateRunes(string(b), 512))
	}
	return b, nil
}

// commentIDFromWith returns an explicit issue comment id for replace strategy.
func commentIDFromWith(with map[string]any) (string, bool) {
	return tryStringFromWith(with, "comment_id")
}

func githubPullRequestReplaceCommentByID(ctx context.Context, owner, repo, commentID, body string) (map[string]any, error) {
	body = ensureAgenticReviewMarker(body)
	return githubUpdateIssueComment(ctx, owner, repo, commentID, body)
}

// parseCommentID validates a numeric GitHub comment id string.
func parseCommentID(id string) error {
	if _, err := strconv.ParseInt(strings.TrimSpace(id), 10, 64); err != nil {
		return fmt.Errorf("native: post_comment comment_id must be a numeric id: %w", err)
	}
	return nil
}
