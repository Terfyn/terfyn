package native

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// Broader GitHub write operations (issues / reviews / statuses) built on the same REST client as
// the read + comment ops (github.go): auth from GITHUB_TOKEN, base from GITHUB_API_URL, non-2xx is
// an error. Each is a policy-gated write in a Tool's operations manifest; the effect classes are
// declared on the Tool resource, not here.

// githubIssuesCreate opens an issue: POST /repos/{owner}/{repo}/issues.
func githubIssuesCreate(ctx context.Context, with map[string]any) (map[string]any, error) {
	owner, repo, err := githubOwnerRepo(with, "issues.create")
	if err != nil {
		return nil, err
	}
	title, err := stringFromWith(with, "title")
	if err != nil {
		return nil, fmt.Errorf("native: issues.create %w", err)
	}
	payload := map[string]any{"title": title}
	if body, ok := tryStringFromWith(with, "body"); ok {
		payload["body"] = body
	}
	path := fmt.Sprintf("/repos/%s/%s/issues", url.PathEscape(owner), url.PathEscape(repo))
	b, err := githubPOSTJSON(ctx, path, payload, maxGitHubJSONBody)
	if err != nil {
		return nil, err
	}
	obj, err := decodeGitHubObject(b, "issues.create")
	if err != nil {
		return nil, err
	}
	return pickGitHubFields(obj, "number", "id", "html_url", "state"), nil
}

// githubPullRequestCreate opens a pull request: POST /repos/{owner}/{repo}/pulls.
// head is the branch with the change, base the branch to merge into. The GitHub API
// names the action "create" (the UI says "open"). Either title or issue is required:
// passing an issue number converts that issue into the PR. This is the deliverable of
// an issue-fixing workflow — the fix lands as a reviewable PR.
func githubPullRequestCreate(ctx context.Context, with map[string]any) (map[string]any, error) {
	owner, repo, err := githubOwnerRepo(with, "pull_request.create")
	if err != nil {
		return nil, err
	}
	head, err := stringFromWith(with, "head")
	if err != nil {
		return nil, fmt.Errorf("native: pull_request.create %w", err)
	}
	base, err := stringFromWith(with, "base")
	if err != nil {
		return nil, fmt.Errorf("native: pull_request.create %w", err)
	}
	payload := map[string]any{"head": head, "base": base}
	if title, ok := tryStringFromWith(with, "title"); ok {
		payload["title"] = title
	} else if issue, ok := with["issue"]; ok && issue != nil && issue != "" {
		payload["issue"] = issue // convert an existing issue into the PR
	} else {
		return nil, fmt.Errorf("native: pull_request.create requires title or issue")
	}
	if body, ok := tryStringFromWith(with, "body"); ok {
		payload["body"] = body
	}
	if draft, ok := with["draft"].(bool); ok {
		payload["draft"] = draft
	}
	if v, ok := with["maintainer_can_modify"].(bool); ok {
		payload["maintainer_can_modify"] = v
	}
	path := fmt.Sprintf("/repos/%s/%s/pulls", url.PathEscape(owner), url.PathEscape(repo))
	b, err := githubPOSTJSON(ctx, path, payload, maxGitHubJSONBody)
	if err != nil {
		return nil, err
	}
	obj, err := decodeGitHubObject(b, "pull_request.create")
	if err != nil {
		return nil, err
	}
	return pickGitHubFields(obj, "number", "id", "html_url", "state"), nil
}

// githubPullRequestUpdate edits a pull request: PATCH /repos/{owner}/{repo}/pulls/{number}.
// Any of title, body, base, or state (open | closed) may be set; state=closed closes it.
func githubPullRequestUpdate(ctx context.Context, with map[string]any) (map[string]any, error) {
	owner, repo, err := githubOwnerRepo(with, "pull_request.update")
	if err != nil {
		return nil, err
	}
	number, err := stringFromWith(with, "number")
	if err != nil {
		return nil, fmt.Errorf("native: pull_request.update %w", err)
	}
	payload, err := githubMutablePatch(with, "title", "body", "base", "state")
	if err != nil {
		return nil, fmt.Errorf("native: pull_request.update %w", err)
	}
	if len(payload) == 0 {
		return nil, fmt.Errorf("native: pull_request.update requires one of title, body, base, state")
	}
	path := fmt.Sprintf("/repos/%s/%s/pulls/%s", url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(number))
	b, err := githubPATCHJSON(ctx, path, payload, maxGitHubJSONBody)
	if err != nil {
		return nil, err
	}
	obj, err := decodeGitHubObject(b, "pull_request.update")
	if err != nil {
		return nil, err
	}
	return pickGitHubFields(obj, "number", "id", "html_url", "state"), nil
}

// githubIssuesUpdate edits an issue: PATCH /repos/{owner}/{repo}/issues/{number}.
// Any of title, body, or state (open | closed) may be set; labels replaces the label set.
func githubIssuesUpdate(ctx context.Context, with map[string]any) (map[string]any, error) {
	owner, repo, err := githubOwnerRepo(with, "issues.update")
	if err != nil {
		return nil, err
	}
	number, err := stringFromWith(with, "number")
	if err != nil {
		return nil, fmt.Errorf("native: issues.update %w", err)
	}
	payload, err := githubMutablePatch(with, "title", "body", "state")
	if err != nil {
		return nil, fmt.Errorf("native: issues.update %w", err)
	}
	if labels, ok := with["labels"]; ok && labels != nil {
		payload["labels"] = labels
	}
	if len(payload) == 0 {
		return nil, fmt.Errorf("native: issues.update requires one of title, body, state, labels")
	}
	path := fmt.Sprintf("/repos/%s/%s/issues/%s", url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(number))
	b, err := githubPATCHJSON(ctx, path, payload, maxGitHubJSONBody)
	if err != nil {
		return nil, err
	}
	obj, err := decodeGitHubObject(b, "issues.update")
	if err != nil {
		return nil, err
	}
	return pickGitHubFields(obj, "number", "id", "html_url", "state"), nil
}

// githubMutablePatch collects the given optional string fields present in with into a
// PATCH payload, so an update sends only the fields the caller set. An explicitly
// supplied body (including empty string or whitespace) is preserved so callers can
// clear or update descriptions.
func githubMutablePatch(with map[string]any, fields ...string) (map[string]any, error) {
	payload := map[string]any{}
	for _, f := range fields {
		if f == "body" {
			v, ok := with["body"]
			if !ok || v == nil {
				continue
			}
			s, err := scalarToString(v)
			if err != nil {
				return nil, fmt.Errorf("field %q: %w", f, err)
			}
			payload["body"] = s
			continue
		}
		if v, ok := tryStringFromWith(with, f); ok {
			payload[f] = v
		}
	}
	return payload, nil
}

// githubIssuesComment comments on an issue or PR: POST /repos/{owner}/{repo}/issues/{number}/comments.
func githubIssuesComment(ctx context.Context, with map[string]any) (map[string]any, error) {
	owner, repo, err := githubOwnerRepo(with, "issues.comment")
	if err != nil {
		return nil, err
	}
	number, err := stringFromWith(with, "number", "issue_number")
	if err != nil {
		return nil, fmt.Errorf("native: issues.comment requires non-empty number: %w", err)
	}
	body, err := stringFromWith(with, "body")
	if err != nil {
		return nil, fmt.Errorf("native: issues.comment %w", err)
	}
	path := fmt.Sprintf("/repos/%s/%s/issues/%s/comments", url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(number))
	b, err := githubPOSTJSON(ctx, path, map[string]any{"body": body}, maxGitHubJSONBody)
	if err != nil {
		return nil, err
	}
	obj, err := decodeGitHubObject(b, "issues.comment")
	if err != nil {
		return nil, err
	}
	return pickGitHubFields(obj, "id", "html_url"), nil
}

// githubPullRequestCreateReview submits a review: POST /repos/{owner}/{repo}/pulls/{number}/reviews.
// event is APPROVE, REQUEST_CHANGES, or COMMENT; GitHub requires a body for the latter two.
func githubPullRequestCreateReview(ctx context.Context, with map[string]any) (map[string]any, error) {
	owner, repo, err := githubOwnerRepo(with, "pull_request.create_review")
	if err != nil {
		return nil, err
	}
	number, err := stringFromWith(with, "number", "pull_number")
	if err != nil {
		return nil, fmt.Errorf("native: pull_request.create_review requires non-empty number: %w", err)
	}
	eventRaw, err := stringFromWith(with, "event")
	if err != nil {
		return nil, fmt.Errorf("native: pull_request.create_review %w", err)
	}
	event := strings.ToUpper(strings.TrimSpace(eventRaw))
	switch event {
	case "APPROVE", "REQUEST_CHANGES", "COMMENT":
	default:
		return nil, fmt.Errorf("native: pull_request.create_review event %q must be APPROVE, REQUEST_CHANGES, or COMMENT", eventRaw)
	}
	body, hasBody := tryStringFromWith(with, "body")
	if event != "APPROVE" && !hasBody {
		return nil, fmt.Errorf("native: pull_request.create_review event %s requires a non-empty body", event)
	}
	payload := map[string]any{"event": event}
	if hasBody {
		payload["body"] = body
	}
	path := fmt.Sprintf("/repos/%s/%s/pulls/%s/reviews", url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(number))
	b, err := githubPOSTJSON(ctx, path, payload, maxGitHubJSONBody)
	if err != nil {
		return nil, err
	}
	obj, err := decodeGitHubObject(b, "pull_request.create_review")
	if err != nil {
		return nil, err
	}
	return pickGitHubFields(obj, "id", "state", "html_url"), nil
}

// githubCommitStatusCreate sets a commit status: POST /repos/{owner}/{repo}/statuses/{sha}.
// state is error, failure, pending, or success.
func githubCommitStatusCreate(ctx context.Context, with map[string]any) (map[string]any, error) {
	owner, repo, err := githubOwnerRepo(with, "commit_status.create")
	if err != nil {
		return nil, err
	}
	sha, err := stringFromWith(with, "sha", "ref", "commit")
	if err != nil {
		return nil, fmt.Errorf("native: commit_status.create requires non-empty sha: %w", err)
	}
	stateRaw, err := stringFromWith(with, "state")
	if err != nil {
		return nil, fmt.Errorf("native: commit_status.create %w", err)
	}
	state := strings.ToLower(strings.TrimSpace(stateRaw))
	switch state {
	case "error", "failure", "pending", "success":
	default:
		return nil, fmt.Errorf("native: commit_status.create state %q must be error, failure, pending, or success", stateRaw)
	}
	payload := map[string]any{"state": state}
	if v, ok := tryStringFromWith(with, "context"); ok {
		payload["context"] = v
	}
	if v, ok := tryStringFromWith(with, "description"); ok {
		payload["description"] = v
	}
	if v, ok := tryStringFromWith(with, "target_url", "url"); ok {
		payload["target_url"] = v
	}
	path := fmt.Sprintf("/repos/%s/%s/statuses/%s", url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(sha))
	b, err := githubPOSTJSON(ctx, path, payload, maxGitHubJSONBody)
	if err != nil {
		return nil, err
	}
	obj, err := decodeGitHubObject(b, "commit_status.create")
	if err != nil {
		return nil, err
	}
	return pickGitHubFields(obj, "id", "state", "context", "target_url"), nil
}

// githubOwnerRepo reads the owner and repo fields shared by every write op.
func githubOwnerRepo(with map[string]any, op string) (owner, repo string, err error) {
	owner, err = stringFromWith(with, "owner")
	if err != nil {
		return "", "", fmt.Errorf("native: %s %w", op, err)
	}
	repo, err = stringFromWith(with, "repo")
	if err != nil {
		return "", "", fmt.Errorf("native: %s %w", op, err)
	}
	return owner, repo, nil
}

func decodeGitHubObject(b []byte, op string) (map[string]any, error) {
	var obj map[string]any
	if err := json.Unmarshal(b, &obj); err != nil {
		return nil, fmt.Errorf("native: %s decode: %w", op, err)
	}
	return obj, nil
}

// pickGitHubFields returns the requested keys that are present, so a tool result is a small,
// predictable subset of the GitHub payload rather than the whole object.
func pickGitHubFields(obj map[string]any, keys ...string) map[string]any {
	out := make(map[string]any, len(keys))
	for _, k := range keys {
		if v, ok := obj[k]; ok {
			out[k] = v
		}
	}
	return out
}
