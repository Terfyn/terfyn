package native

import (
	"context"
	"fmt"
	"net/url"

	"github.com/Terfyn/terfyn/internal/jsonnum"
)

// GitHub list (read) operations. Each is a GET returning a JSON array, decoded to a
// slice under a named key. Optional filters are passed through as query parameters.

// githubPullRequestList lists pull requests: GET /repos/{owner}/{repo}/pulls.
// Optional filters: state (open|closed|all), head, base.
func githubPullRequestList(ctx context.Context, with map[string]any) (map[string]any, error) {
	owner, repo, err := githubOwnerRepo(with, "pull_request.list")
	if err != nil {
		return nil, err
	}
	q := githubListQuery(with, "state", "head", "base")
	path := fmt.Sprintf("/repos/%s/%s/pulls%s", url.PathEscape(owner), url.PathEscape(repo), q)
	arr, err := githubGETArray(ctx, path, "pull_request.list")
	if err != nil {
		return nil, err
	}
	return map[string]any{"pull_requests": arr}, nil
}

// githubIssuesList lists issues: GET /repos/{owner}/{repo}/issues.
// Optional filters: state (open|closed|all), labels (comma-separated).
func githubIssuesList(ctx context.Context, with map[string]any) (map[string]any, error) {
	owner, repo, err := githubOwnerRepo(with, "issues.list")
	if err != nil {
		return nil, err
	}
	q := githubListQuery(with, "state", "labels")
	path := fmt.Sprintf("/repos/%s/%s/issues%s", url.PathEscape(owner), url.PathEscape(repo), q)
	arr, err := githubGETArray(ctx, path, "issues.list")
	if err != nil {
		return nil, err
	}
	return map[string]any{"issues": arr}, nil
}

// githubListQuery builds a "?k=v&…" string from the given optional string fields.
func githubListQuery(with map[string]any, fields ...string) string {
	vals := url.Values{}
	for _, f := range fields {
		if v, ok := tryStringFromWith(with, f); ok {
			vals.Set(f, v)
		}
	}
	if len(vals) == 0 {
		return ""
	}
	return "?" + vals.Encode()
}

// githubGETArray performs a GitHub GET expected to return a JSON array.
func githubGETArray(ctx context.Context, path, op string) ([]any, error) {
	b, err := githubGET(ctx, path, githubAcceptJSON, maxGitHubJSONBody)
	if err != nil {
		return nil, err
	}
	var arr []any
	if err := jsonnum.Unmarshal(b, &arr); err != nil {
		return nil, fmt.Errorf("native: %s decode: %w", op, err)
	}
	return arr, nil
}
