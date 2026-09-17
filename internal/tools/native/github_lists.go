package native

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// GitHub list (read) operations. Each is a GET returning a JSON array, decoded to a
// slice under a named key. Optional filters are passed through as query parameters.
// List ops follow Link rel="next" up to githubListMaxPages (100 items/page), matching
// githubFindAgenticReviewCommentID. A still-present next link after the bound is
// reported as truncated rather than presented as a complete list.

const (
	githubListPerPage  = 100
	githubListMaxPages = 10
)

// githubPullRequestList lists pull requests: GET /repos/{owner}/{repo}/pulls.
// Optional filters: state (open|closed|all), head, base.
func githubPullRequestList(ctx context.Context, with map[string]any) (map[string]any, error) {
	owner, repo, err := githubOwnerRepo(with, "pull_request.list")
	if err != nil {
		return nil, err
	}
	q := githubListQuery(with, "state", "head", "base")
	path := fmt.Sprintf("/repos/%s/%s/pulls%s", url.PathEscape(owner), url.PathEscape(repo), q)
	arr, truncated, err := githubGETArray(ctx, path, "pull_request.list")
	if err != nil {
		return nil, err
	}
	out := map[string]any{"pull_requests": arr}
	if truncated {
		out["truncated"] = true
	}
	return out, nil
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
	arr, truncated, err := githubGETArray(ctx, path, "issues.list")
	if err != nil {
		return nil, err
	}
	out := map[string]any{"issues": arr}
	if truncated {
		out["truncated"] = true
	}
	return out, nil
}

// githubListQuery builds a "?k=v&…" string from the given optional string fields
// plus per_page=githubListPerPage so GitHub does not silently default to 30.
func githubListQuery(with map[string]any, fields ...string) string {
	vals := url.Values{}
	for _, f := range fields {
		if v, ok := tryStringFromWith(with, f); ok {
			vals.Set(f, v)
		}
	}
	vals.Set("per_page", strconv.Itoa(githubListPerPage))
	return "?" + vals.Encode()
}

// githubGETArray performs GitHub GETs expected to return JSON arrays, following
// Link rel="next" until the list ends or githubListMaxPages is reached.
func githubGETArray(ctx context.Context, path, op string) ([]any, bool, error) {
	current := path
	var all []any
	for page := 1; page <= githubListMaxPages; page++ {
		b, hdr, err := githubRequest(ctx, "GET", current, githubAcceptJSON, maxGitHubJSONBody)
		if err != nil {
			return nil, false, err
		}
		var arr []any
		if err := json.Unmarshal(b, &arr); err != nil {
			return nil, false, fmt.Errorf("native: %s decode: %w", op, err)
		}
		all = append(all, arr...)
		next, ok := githubAllowFollow(parseGitHubLinkNext(hdr.Get("Link")))
		if !ok {
			return all, false, nil
		}
		if page == githubListMaxPages {
			return all, true, nil
		}
		current = next
	}
	return all, true, nil
}

// parseGitHubLinkNext returns the URL from a GitHub Link header's rel="next" entry.
func parseGitHubLinkNext(link string) string {
	for _, part := range strings.Split(link, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		lt := strings.IndexByte(part, '<')
		gt := strings.IndexByte(part, '>')
		if lt < 0 || gt <= lt {
			continue
		}
		target := strings.TrimSpace(part[lt+1 : gt])
		if target != "" && linkRelIsNext(part[gt+1:]) {
			return target
		}
	}
	return ""
}

func linkRelIsNext(params string) bool {
	for _, p := range strings.Split(params, ";") {
		p = strings.TrimSpace(p)
		k, v, ok := strings.Cut(p, "=")
		if !ok {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(k), "rel") {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if strings.EqualFold(v, "next") {
			return true
		}
	}
	return false
}

// githubAllowFollow accepts a path or an absolute URL whose host matches GITHUB_API_URL.
func githubAllowFollow(next string) (string, bool) {
	next = strings.TrimSpace(next)
	if next == "" {
		return "", false
	}
	if strings.HasPrefix(next, "/") {
		return next, true
	}
	u, err := url.Parse(next)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", false
	}
	base, err := url.Parse(githubAPIBase())
	if err != nil || base.Host == "" {
		return "", false
	}
	if !strings.EqualFold(u.Host, base.Host) {
		return "", false
	}
	return next, true
}
