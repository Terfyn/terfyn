package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Terfyn/terfyn/internal/spec"
	"github.com/Terfyn/terfyn/internal/state/sqlite"
	"github.com/Terfyn/terfyn/internal/tools"
	"github.com/Terfyn/terfyn/internal/trace"
)

// realisticIssue is shaped like a GitHub REST issue (URL fields, user, labels,
// reactions) with a 3-15 KB body, so the payload size matches what GitHub returns.
func realisticIssue(n int) map[string]any {
	api := fmt.Sprintf("https://api.github.com/repos/o/r/issues/%d", n)
	user := map[string]any{"login": "someone", "id": 12345, "type": "User", "site_admin": false}
	for _, k := range []string{"avatar_url", "url", "html_url", "followers_url", "following_url", "gists_url",
		"starred_url", "subscriptions_url", "organizations_url", "repos_url", "events_url", "received_events_url"} {
		user[k] = "https://api.github.com/users/someone/" + k
	}
	bodyLen := 3000 + (n*1237)%12000
	return map[string]any{
		"url": api, "repository_url": "https://api.github.com/repos/o/r", "labels_url": api + "/labels{/name}",
		"comments_url": api + "/comments", "events_url": api + "/events", "timeline_url": api + "/timeline",
		"html_url": fmt.Sprintf("https://github.com/o/r/issues/%d", n), "id": 1000000 + n, "number": n,
		"title": fmt.Sprintf("issue %d: something is broken", n), "user": user, "state": "open",
		"labels":     []any{map[string]any{"name": "bug", "color": "ededed", "url": "https://api.github.com/repos/o/r/labels/bug"}},
		"created_at": "2026-09-01T12:00:00Z", "updated_at": "2026-09-02T12:00:00Z", "comments": 3,
		"author_association": "CONTRIBUTOR",
		"body":               strings.Repeat("lorem ipsum <dolor> & sit amet, ", bodyLen/32+1)[:bodyLen],
		"reactions":          map[string]any{"url": api + "/reactions", "total_count": 0},
	}
}

func realisticIssuePage(page int) []any {
	items := make([]any, 0, 100)
	for i := 1; i <= 100; i++ {
		items = append(items, realisticIssue((page-1)*100+i))
	}
	return items
}

// runGitHubIssuesList runs the native issues.list op through a real workflow step
// (runToolStep -> enforceToolOutput with the limits resolved for the step) against a
// repository with many realistic-size issues, with optional project- and tool-level
// limits. It requires that the step succeeds, that the engine never had to cut the
// output (no tool_output limit_hit), and that the workflow sees truncated: true with
// whole items in GitHub order; it returns the issues as downstream steps receive them.
func runGitHubIssuesList(t *testing.T, projectLimits, toolLimits *spec.ExecutionLimits) []map[string]any {
	t.Helper()
	var api *httptest.Server
	api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := 1
		if p := r.URL.Query().Get("page"); p != "" {
			_, _ = fmt.Sscan(p, &page)
		}
		w.Header().Set("Link", fmt.Sprintf(`<%s/repos/o/r/issues?per_page=100&page=%d>; rel="next"`, api.URL, page+1))
		_ = json.NewEncoder(w).Encode(realisticIssuePage(page))
	}))
	t.Cleanup(api.Close)
	t.Setenv("GITHUB_TOKEN", "tok")
	t.Setenv("GITHUB_API_URL", api.URL)

	ctx := context.Background()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "gh-list.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	graph := demoWorkflowGraph(t)
	graph.Spec.Limits = projectLimits
	graph.Tools["helper"].Spec.Limits = toolLimits
	graph.Workflows["demo"].Spec.Steps = []spec.WorkflowStep{{
		ID:   "list",
		Uses: "tool.helper.issues.list",
		With: map[string]any{"owner": "o", "repo": "r"},
	}}
	graph.Workflows["demo"].Spec.Output = &spec.WorkflowOutput{Value: map[string]any{
		"truncated": "${steps.list.output.truncated}",
	}}

	runID := "run-gh-list"
	started := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	input := startDemoRun(t, st, runID, started)
	ex := &Executor{
		Graph:       graph,
		ProjectRoot: testProjectRoot(t),
		Tools:       tools.NewRegistry(graph),
		Store:       st,
		Trace:       trace.NewRecorder(st),
	}
	if err := ex.Run(ctx, RunInput{
		RunID: runID, WorkflowName: "demo", Env: "dev", StartedAt: started, Input: input,
		Hitl: HitlRunOptions{AutoApprove: true},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := st.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "succeeded" {
		t.Fatalf("status %q err=%q", got.Status, got.ErrorText)
	}
	events, err := st.ListTraceEventsByRunID(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if n := countLimitHitEvents(events, string(spec.LimitKindToolOutput)); n != 0 {
		t.Fatalf("engine had to truncate the list output (%d limit_hit events); the op must fit it itself", n)
	}
	var wfOut map[string]any
	if err := json.Unmarshal([]byte(got.OutputJSON), &wfOut); err != nil {
		t.Fatal(err)
	}
	if wfOut["truncated"] != true {
		t.Fatalf("workflow saw truncated = %v, want true for a list the op could not return in full", wfOut["truncated"])
	}
	// The step output as downstream steps receive it (after enforceToolOutput) is kept
	// raw in the checkpoint; run/step rows are display copies with strings redacted.
	cp, err := st.GetLatestCheckpoint(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Steps map[string]struct {
			Output struct {
				Truncated bool             `json:"truncated"`
				Issues    []map[string]any `json:"issues"`
			} `json:"Output"`
		} `json:"steps"`
	}
	if err := json.Unmarshal([]byte(cp.ContextJSON), &payload); err != nil {
		t.Fatal(err)
	}
	out := payload.Steps["list"].Output
	if !out.Truncated {
		t.Fatal("step output truncated = false")
	}
	if len(out.Issues) == 0 {
		t.Fatal("no issues returned")
	}
	for i, is := range out.Issues {
		want := realisticIssue(i + 1)
		if is["number"] != float64(i+1) || is["body"] != want["body"] {
			t.Fatalf("issue %d is not whole and in order (number %v, body %d bytes)", i+1, is["number"], len(fmt.Sprint(is["body"])))
		}
	}
	return out.Issues
}

// TestRun_githubIssuesListFitsDefaultToolOutputLimit: with the default limits the op
// bounds its own result so the step succeeds without engine truncation.
func TestRun_githubIssuesListFitsDefaultToolOutputLimit(t *testing.T) {
	// Guard against a vacuous test: one raw page of these issues must exceed the default
	// limit (the engine would have to cut bodies or fail the step), as in the bug report.
	if n, err := trace.JSONByteLen(map[string]any{"issues": realisticIssuePage(1)}); err != nil || n <= spec.DefaultMaxToolOutputBytes {
		t.Fatalf("a raw 100-issue page is %d bytes (err %v), within the default tool-output limit; fixture is not realistic enough", n, err)
	}
	runGitHubIssuesList(t, nil, nil)
}

// TestRun_githubIssuesListFitsLoweredToolOutputLimit: a project maxToolOutputBytes
// below the default, with the fail policy (any engine-side overflow would fail the
// step), still yields whole items that pass the resolved limit untouched, and fewer of
// them than at the default.
func TestRun_githubIssuesListFitsLoweredToolOutputLimit(t *testing.T) {
	const lowered = 128 << 10
	if n, err := trace.JSONByteLen(map[string]any{"issues": realisticIssuePage(1)[:30]}); err != nil || n <= lowered {
		t.Fatalf("30 raw issues are %d bytes (err %v), within %d; fixture would not exercise the lowered limit", n, err, lowered)
	}
	def := len(runGitHubIssuesList(t, nil, nil))
	got := runGitHubIssuesList(t, &spec.ExecutionLimits{
		MaxToolOutputBytes:     lowered,
		ToolOutputExceedPolicy: spec.LimitExceedFail,
	}, nil)
	if len(got) >= def {
		t.Fatalf("lowered limit returned %d issues, default returned %d; want fewer at the lower limit", len(got), def)
	}
	if n, err := trace.JSONByteLen(map[string]any{"issues": got, "truncated": true}); err != nil || n > lowered {
		t.Fatalf("result is %d bytes (err %v), over the resolved limit %d", n, err, lowered)
	}
}

// TestRun_githubIssuesListUsesRaisedToolOutputLimit: raising maxToolOutputBytes on the
// GitHub tool (per-tool override, top precedence over the project) returns more
// items than the default, instead of pinning the list to the default budget.
func TestRun_githubIssuesListUsesRaisedToolOutputLimit(t *testing.T) {
	const raised = 1 << 20
	def := len(runGitHubIssuesList(t, nil, nil))
	got := runGitHubIssuesList(t,
		&spec.ExecutionLimits{MaxToolOutputBytes: 64 << 10}, // lower project baseline the tool override must beat
		&spec.ExecutionLimits{MaxToolOutputBytes: raised, ToolOutputExceedPolicy: spec.LimitExceedFail})
	if len(got) <= def {
		t.Fatalf("raised limit returned %d issues, default returned %d; want more at the raised limit", len(got), def)
	}
	if n, err := trace.JSONByteLen(map[string]any{"issues": got, "truncated": true}); err != nil || n > raised || n <= spec.DefaultMaxToolOutputBytes {
		t.Fatalf("result is %d bytes (err %v), want above the default limit and within the raised limit %d", n, err, raised)
	}
}
