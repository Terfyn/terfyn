package native

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// dispatchHandler runs a single native operation (excluding shell-command ops).
type dispatchHandler func(ctx context.Context, with map[string]any, start time.Time) (map[string]any, ExecMeta, error)

// dispatchHandlers is the single source of truth for non-shell operations handled by [Registry.Dispatch].
// When adding an operation, register it here and in operationCatalog (see operations.go).
var dispatchHandlers = map[string]dispatchHandler{
	"check_runs.list":            dispatchCheckRunsList,
	"commit":                     dispatchGitCommit,
	"commit_status.create":       dispatchGitHubJSON(githubCommitStatusCreate),
	"create_branch":              dispatchGitCreateBranch,
	"echo":                       dispatchEcho,
	"identity":                   dispatchIdentity,
	"issues.comment":             dispatchGitHubJSON(githubIssuesComment),
	"issues.create":              dispatchGitHubJSON(githubIssuesCreate),
	"issues.get":                 dispatchGitHubJSON(githubIssuesGet),
	"issues.list":                dispatchGitHubJSON(githubIssuesList),
	"issues.update":              dispatchGitHubJSON(githubIssuesUpdate),
	"message.send":               dispatchSlackMessageSend,
	"message.update":             dispatchSlackMessageUpdate,
	"pull_request.create":        dispatchGitHubJSON(githubPullRequestCreate),
	"pull_request.create_review": dispatchGitHubJSON(githubPullRequestCreateReview),
	"pull_request.diff":          dispatchPullRequestDiff,
	"pull_request.fetch":         dispatchPullRequestFetch,
	"pull_request.get":           dispatchPullRequestGet,
	"pull_request.list":          dispatchGitHubJSON(githubPullRequestList),
	"pull_request.post_comment":  dispatchPullRequestPostComment,
	"pull_request.update":        dispatchGitHubJSON(githubPullRequestUpdate),
	"push_branch":                dispatchGitPushBranch,
	"read_file":                  dispatchWorkspaceReadFile,
	"write_file":                 dispatchWorkspaceWriteFile,
	"edit":                       dispatchWorkspaceEdit,
	"run_tests":                  dispatchWorkspaceRunTests,
	"list_dir":                   dispatchWorkspaceListDir,
	"glob":                       dispatchWorkspaceGlob,
	"grep":                       dispatchWorkspaceGrep,
}

// dispatchGitHubJSON adapts a (ctx, with) GitHub write op to the dispatchHandler shape, attaching
// timing metadata. The read ops keep bespoke wrappers; these share one.
func dispatchGitHubJSON(fn func(context.Context, map[string]any) (map[string]any, error)) dispatchHandler {
	return func(ctx context.Context, with map[string]any, start time.Time) (map[string]any, ExecMeta, error) {
		out, err := fn(ctx, with)
		meta := ExecMeta{DurationMs: time.Since(start).Milliseconds()}
		if err != nil {
			return nil, meta, err
		}
		return out, meta, nil
	}
}

func dispatchEcho(_ context.Context, with map[string]any, start time.Time) (map[string]any, ExecMeta, error) {
	meta := ExecMeta{DurationMs: time.Since(start).Milliseconds()}
	return map[string]any{"echo": shallowCopy(with)}, meta, nil
}

func dispatchIdentity(_ context.Context, with map[string]any, start time.Time) (map[string]any, ExecMeta, error) {
	v, ok := with["value"]
	meta := ExecMeta{DurationMs: time.Since(start).Milliseconds()}
	return map[string]any{"value": v, "ok": ok}, meta, nil
}

func dispatchPullRequestFetch(_ context.Context, with map[string]any, start time.Time) (map[string]any, ExecMeta, error) {
	meta := ExecMeta{DurationMs: time.Since(start).Milliseconds()}
	obj, err := prObjectFromWith(with)
	if err != nil {
		return nil, meta, err
	}
	return map[string]any{"pull_request": obj}, meta, nil
}

func prObjectFromWith(with map[string]any) (map[string]any, error) {
	v, ok := with["pr"]
	if !ok || v == nil {
		return nil, fmt.Errorf("native: pull_request.fetch requires JSON object or JSON string field pr")
	}
	switch t := v.(type) {
	case map[string]any:
		return t, nil
	case string:
		raw := strings.TrimSpace(t)
		if raw == "" {
			return nil, fmt.Errorf("native: pull_request.fetch requires JSON object or JSON string field pr")
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(raw), &obj); err != nil {
			return nil, fmt.Errorf("native: pull_request.fetch pr: %w", err)
		}
		return obj, nil
	default:
		return nil, fmt.Errorf("native: pull_request.fetch requires JSON object or JSON string field pr")
	}
}

func dispatchPullRequestPostComment(ctx context.Context, with map[string]any, start time.Time) (map[string]any, ExecMeta, error) {
	meta := ExecMeta{DurationMs: time.Since(start).Milliseconds()}
	owner, repo, num, bodyText, wantLive := githubLivePostCommentContext(with)
	if !wantLive {
		body, _ := with["body"].(string)
		return map[string]any{
			"simulated":    true,
			"body_preview": truncateRunes(body, 240),
		}, meta, nil
	}
	strategy, err := githubCommentStrategy(with)
	if err != nil {
		return nil, meta, err
	}
	var out map[string]any
	if commentID, ok := commentIDFromWith(with); ok {
		if err := parseCommentID(commentID); err != nil {
			return nil, meta, err
		}
		out, err = githubPullRequestReplaceCommentByID(ctx, owner, repo, commentID, bodyText)
	} else {
		out, err = githubPullRequestPostComment(ctx, owner, repo, num, bodyText, strategy)
	}
	if err != nil {
		return nil, meta, err
	}
	return out, meta, nil
}

func dispatchPullRequestGet(ctx context.Context, with map[string]any, start time.Time) (map[string]any, ExecMeta, error) {
	out, err := githubPullRequestGet(ctx, with)
	meta := ExecMeta{DurationMs: time.Since(start).Milliseconds()}
	if err != nil {
		return nil, meta, err
	}
	return out, meta, nil
}

func dispatchPullRequestDiff(ctx context.Context, with map[string]any, start time.Time) (map[string]any, ExecMeta, error) {
	out, err := githubPullRequestDiff(ctx, with)
	meta := ExecMeta{DurationMs: time.Since(start).Milliseconds()}
	if err != nil {
		return nil, meta, err
	}
	return out, meta, nil
}

func dispatchCheckRunsList(ctx context.Context, with map[string]any, start time.Time) (map[string]any, ExecMeta, error) {
	out, err := githubCheckRunsList(ctx, with)
	meta := ExecMeta{DurationMs: time.Since(start).Milliseconds()}
	if err != nil {
		return nil, meta, err
	}
	return out, meta, nil
}
