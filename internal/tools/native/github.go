package native

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	githubAcceptJSON     = "application/vnd.github+json"
	githubAcceptDiff     = "application/vnd.github.diff"
	githubAPIVersion     = "2022-11-28"
	githubUserAgent      = "terfyn/terfyn (native-github-read)"
	maxGitHubJSONBody    = 8 << 20  // 8 MiB
	maxGitHubDiffBody    = 32 << 20 // 32 MiB
	defaultGitHubAPIBase = "https://api.github.com"
)

func githubAPIBase() string {
	u := strings.TrimSpace(os.Getenv("GITHUB_API_URL"))
	if u == "" {
		return defaultGitHubAPIBase
	}
	return strings.TrimSuffix(u, "/")
}

func githubToken() (string, error) {
	t := strings.TrimSpace(os.Getenv("GITHUB_TOKEN"))
	if t == "" {
		return "", fmt.Errorf("native: GITHUB_TOKEN is not set (required for GitHub API operations)")
	}
	return t, nil
}

func githubPullRequestGet(ctx context.Context, with map[string]any) (map[string]any, error) {
	owner, repo, number, err := githubRepoTriplet(with)
	if err != nil {
		return nil, err
	}
	path := fmt.Sprintf("/repos/%s/%s/pulls/%s", url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(number))
	b, err := githubGET(ctx, path, githubAcceptJSON, maxGitHubJSONBody)
	if err != nil {
		return nil, err
	}
	var pr map[string]any
	if err := json.Unmarshal(b, &pr); err != nil {
		return nil, fmt.Errorf("native: pull_request.get decode: %w", err)
	}
	return map[string]any{"pull_request": pr}, nil
}

// githubIssuesGet fetches a single issue (GET /repos/{owner}/{repo}/issues/{number}).
// Unlike pull_request.get (which hits /pulls and 404s on a non-PR number), this reads
// the issues endpoint, so a plain issue — the input to an issue-fixing workflow — can
// be fetched. Note the GitHub issues endpoint also returns PRs (a PR is an issue), so
// it is the more general "get by number".
func githubIssuesGet(ctx context.Context, with map[string]any) (map[string]any, error) {
	owner, repo, number, err := githubRepoTriplet(with)
	if err != nil {
		return nil, err
	}
	path := fmt.Sprintf("/repos/%s/%s/issues/%s", url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(number))
	b, err := githubGET(ctx, path, githubAcceptJSON, maxGitHubJSONBody)
	if err != nil {
		return nil, err
	}
	var issue map[string]any
	if err := json.Unmarshal(b, &issue); err != nil {
		return nil, fmt.Errorf("native: issues.get decode: %w", err)
	}
	return map[string]any{"issue": issue}, nil
}

func githubPullRequestDiff(ctx context.Context, with map[string]any) (map[string]any, error) {
	owner, repo, number, err := githubRepoTriplet(with)
	if err != nil {
		return nil, err
	}
	path := fmt.Sprintf("/repos/%s/%s/pulls/%s", url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(number))
	text, err := githubGETString(ctx, path, githubAcceptDiff, maxGitHubDiffBody)
	if err != nil {
		return nil, err
	}
	return map[string]any{"diff": text}, nil
}

// githubLivePostCommentContext reports whether step inputs request a real GitHub issue comment:
// non-empty owner, repo, number (or pull_number), and body. When true, post_comment uses the REST
// API if GITHUB_TOKEN is set; otherwise the demo stays fully offline (simulated).
func githubLivePostCommentContext(with map[string]any) (owner, repo, number, body string, wantLive bool) {
	owner, ok1 := tryStringFromWith(with, "owner")
	repo, ok2 := tryStringFromWith(with, "repo")
	num, ok3 := tryStringFromWith(with, "number", "pull_number")
	bodyStr, ok4 := tryStringFromWith(with, "body")
	if !ok1 || !ok2 || !ok3 || !ok4 {
		return "", "", "", "", false
	}
	return owner, repo, num, bodyStr, true
}

func tryStringFromWith(with map[string]any, keys ...string) (string, bool) {
	for _, k := range keys {
		v, ok := with[k]
		if !ok || v == nil {
			continue
		}
		s, err := scalarToString(v)
		if err != nil || strings.TrimSpace(s) == "" {
			continue
		}
		return strings.TrimSpace(s), true
	}
	return "", false
}

func githubPOSTJSON(ctx context.Context, path string, payload any, maxResp int64) ([]byte, error) {
	return githubJSONRequest(ctx, http.MethodPost, path, payload, maxResp)
}

func defaultGitHubHTTPClient() *http.Client {
	return &http.Client{Timeout: 60 * time.Second}
}

func readGitHubResponseBody(resp *http.Response, maxResp int64) ([]byte, error) {
	limited := io.LimitReader(resp.Body, maxResp+1)
	b, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("native: github read body: %w", err)
	}
	if int64(len(b)) > maxResp {
		return nil, fmt.Errorf("native: github response body exceeds limit (%d bytes)", maxResp)
	}
	return b, nil
}

func githubCheckRunsList(ctx context.Context, with map[string]any) (map[string]any, error) {
	owner, err := stringFromWith(with, "owner")
	if err != nil {
		return nil, fmt.Errorf("native: check_runs.list %w", err)
	}
	repo, err := stringFromWith(with, "repo")
	if err != nil {
		return nil, fmt.Errorf("native: check_runs.list %w", err)
	}
	ref, err := stringFromWith(with, "ref", "head_sha", "sha")
	if err != nil {
		return nil, fmt.Errorf("native: check_runs.list requires non-empty ref, head_sha, or sha: %w", err)
	}
	path := fmt.Sprintf("/repos/%s/%s/commits/%s/check-runs", url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(ref))
	b, err := githubGET(ctx, path, githubAcceptJSON, maxGitHubJSONBody)
	if err != nil {
		return nil, err
	}
	var payload map[string]any
	if err := json.Unmarshal(b, &payload); err != nil {
		return nil, fmt.Errorf("native: check_runs.list decode: %w", err)
	}
	return payload, nil
}

func githubRepoTriplet(with map[string]any) (owner, repo, number string, err error) {
	owner, err = stringFromWith(with, "owner")
	if err != nil {
		return "", "", "", fmt.Errorf("native: pull_request.* %w", err)
	}
	repo, err = stringFromWith(with, "repo")
	if err != nil {
		return "", "", "", fmt.Errorf("native: pull_request.* %w", err)
	}
	number, err = stringFromWith(with, "number", "pull_number")
	if err != nil {
		return "", "", "", fmt.Errorf("native: pull_request.* requires non-empty number or pull_number: %w", err)
	}
	return owner, repo, number, nil
}

func stringFromWith(with map[string]any, keys ...string) (string, error) {
	for _, k := range keys {
		v, ok := with[k]
		if !ok || v == nil {
			continue
		}
		s, err := scalarToString(v)
		if err != nil {
			return "", fmt.Errorf("field %q: %w", k, err)
		}
		if strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s), nil
		}
	}
	return "", fmt.Errorf("missing or empty one of %v", keys)
}

func scalarToString(v any) (string, error) {
	switch x := v.(type) {
	case string:
		return x, nil
	case bool:
		return strconv.FormatBool(x), nil
	case int:
		return strconv.Itoa(x), nil
	case int64:
		return strconv.FormatInt(x, 10), nil
	case float64:
		if x == float64(int64(x)) && x >= -9007199254740992 && x <= 9007199254740992 {
			return strconv.FormatInt(int64(x), 10), nil
		}
		return strconv.FormatFloat(x, 'f', -1, 64), nil
	case json.Number:
		return x.String(), nil
	default:
		return "", fmt.Errorf("unsupported type %T", v)
	}
}

func githubGET(ctx context.Context, path, accept string, maxBody int64) ([]byte, error) {
	b, _, err := githubRequest(ctx, http.MethodGet, path, accept, maxBody)
	return b, err
}

func githubGETString(ctx context.Context, path, accept string, maxBody int64) (string, error) {
	b, err := githubRequestBody(ctx, http.MethodGet, path, accept, maxBody)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func githubRequestBody(ctx context.Context, method, path, accept string, maxBody int64) ([]byte, error) {
	b, _, err := githubRequest(ctx, method, path, accept, maxBody)
	return b, err
}

func githubAbsoluteURL(pathOrURL string) string {
	if strings.HasPrefix(pathOrURL, "http://") || strings.HasPrefix(pathOrURL, "https://") {
		return pathOrURL
	}
	return strings.TrimSuffix(githubAPIBase(), "/") + pathOrURL
}

func githubRequest(ctx context.Context, method, path, accept string, maxBody int64) ([]byte, http.Header, error) {
	token, err := githubToken()
	if err != nil {
		return nil, nil, err
	}
	fullURL := githubAbsoluteURL(path)
	req, err := http.NewRequestWithContext(ctx, method, fullURL, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", githubUserAgent)
	if strings.TrimSpace(accept) != "" {
		req.Header.Set("Accept", accept)
	}
	if accept == githubAcceptJSON || accept == "" {
		req.Header.Set("X-GitHub-Api-Version", githubAPIVersion)
	}

	cli := defaultGitHubHTTPClient()
	resp, err := cli.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("native: github request: %w", err)
	}
	defer resp.Body.Close()

	b, err := readGitHubResponseBody(resp, maxBody)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, nil, fmt.Errorf("native: github HTTP %s: %s", resp.Status, truncateRunes(string(b), 512))
	}
	return b, resp.Header.Clone(), nil
}
