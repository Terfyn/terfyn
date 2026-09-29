package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Terfyn/terfyn/internal/spec"
	"github.com/Terfyn/terfyn/internal/state"
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

// githubListRun is one finished run of runGitHubListWorkflow.
type githubListRun struct {
	st     *sqlite.Store
	runID  string
	runErr error
}

// runGitHubListWorkflow runs steps (native GitHub list ops on the "helper" tool, plus
// anything else) through a real workflow against a repository with many realistic-size
// issues on every list endpoint, with optional project- and tool-level limits. It does
// not judge the outcome; callers inspect the run row, trace, and checkpoint.
func runGitHubListWorkflow(t *testing.T, projectLimits, toolLimits *spec.ExecutionLimits, steps []spec.WorkflowStep, output map[string]any, hitl HitlRunOptions) githubListRun {
	t.Helper()
	var api *httptest.Server
	api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := 1
		if p := r.URL.Query().Get("page"); p != "" {
			_, _ = fmt.Sscan(p, &page)
		}
		w.Header().Set("Link", fmt.Sprintf(`<%s%s?per_page=100&page=%d>; rel="next"`, api.URL, r.URL.Path, page+1))
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
	graph.Workflows["demo"].Spec.Steps = steps
	graph.Workflows["demo"].Spec.Output = &spec.WorkflowOutput{Value: output}

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
	runErr := ex.Run(ctx, RunInput{
		RunID: runID, WorkflowName: "demo", Env: "dev", StartedAt: started, Input: input,
		Hitl: hitl,
	})
	return githubListRun{st: st, runID: runID, runErr: runErr}
}

// githubListCheckpointOutput is a list step's output as stored in the checkpoint.
type githubListCheckpointOutput struct {
	Truncated    bool             `json:"truncated"`
	Issues       []map[string]any `json:"issues"`
	PullRequests []map[string]any `json:"pull_requests"`
}

// latestGitHubListCheckpoint returns the raw latest checkpoint context and the list
// step outputs in it (the step output as downstream steps receive it, after
// enforceToolOutput, is kept raw in the checkpoint; run/step rows are display copies
// with strings redacted).
func latestGitHubListCheckpoint(t *testing.T, r githubListRun) (string, map[string]githubListCheckpointOutput) {
	t.Helper()
	cp, err := r.st.GetLatestCheckpoint(context.Background(), r.runID)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Steps map[string]struct {
			Output githubListCheckpointOutput `json:"Output"`
		} `json:"steps"`
	}
	if err := json.Unmarshal([]byte(cp.ContextJSON), &payload); err != nil {
		t.Fatal(err)
	}
	outs := make(map[string]githubListCheckpointOutput, len(payload.Steps))
	for id, s := range payload.Steps {
		outs[id] = s.Output
	}
	return cp.ContextJSON, outs
}

// assertWholeIssuesInOrder fails unless items are the fixture's first len(items) issues, whole.
func assertWholeIssuesInOrder(t *testing.T, items []map[string]any) {
	t.Helper()
	if len(items) == 0 {
		t.Fatal("no items returned")
	}
	for i, is := range items {
		want := realisticIssue(i + 1)
		if is["number"] != float64(i+1) || is["body"] != want["body"] {
			t.Fatalf("item %d is not whole and in order (number %v, body %d bytes)", i+1, is["number"], len(fmt.Sprint(is["body"])))
		}
	}
}

// runGitHubIssuesList runs the native issues.list op through a real workflow step
// (runToolStep -> enforceToolOutput with the limits resolved for the step) against a
// repository with many realistic-size issues, with optional project- and tool-level
// limits. It requires that the step succeeds, that the engine never had to cut the
// output (no tool_output limit_hit), and that the workflow sees truncated: true with
// whole items in GitHub order; it returns the issues as downstream steps receive them.
func runGitHubIssuesList(t *testing.T, projectLimits, toolLimits *spec.ExecutionLimits) []map[string]any {
	t.Helper()
	r := runGitHubListWorkflow(t, projectLimits, toolLimits, []spec.WorkflowStep{{
		ID:   "list",
		Uses: "tool.helper.issues.list",
		With: map[string]any{"owner": "o", "repo": "r"},
	}}, map[string]any{"truncated": "${steps.list.output.truncated}"}, HitlRunOptions{AutoApprove: true})
	if r.runErr != nil {
		t.Fatal(r.runErr)
	}
	ctx := context.Background()
	st, runID := r.st, r.runID
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
	_, outs := latestGitHubListCheckpoint(t, r)
	out := outs["list"]
	if !out.Truncated {
		t.Fatal("step output truncated = false")
	}
	assertWholeIssuesInOrder(t, out.Issues)
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
// items than the default, instead of pinning the list to the default budget. (At
// 1 MiB the advertised budget is clamped to half the default checkpoint limit; see
// TestRun_githubIssuesListClampedToCheckpointLimit.)
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

// githubListStep is a native GitHub list step on the "helper" tool.
func githubListStep(id, op string) spec.WorkflowStep {
	return spec.WorkflowStep{ID: id, Uses: "tool.helper." + op, With: map[string]any{"owner": "o", "repo": "r"}}
}

// githubListCheckpointSizes reads the run's status and latest checkpoint, and returns
// the checkpoint context size and each list step's JSON-encoded output size.
func githubListCheckpointSizes(t *testing.T, r githubListRun) (status, errText string, cpBytes int, outBytes map[string]int, outs map[string]githubListCheckpointOutput) {
	t.Helper()
	got, err := r.st.GetRun(context.Background(), r.runID)
	if err != nil {
		t.Fatal(err)
	}
	cpJSON, outs := latestGitHubListCheckpoint(t, r)
	outBytes = make(map[string]int, len(outs))
	for id, o := range outs {
		m := map[string]any{"truncated": o.Truncated}
		if o.Issues != nil {
			m["issues"] = o.Issues
		}
		if o.PullRequests != nil {
			m["pull_requests"] = o.PullRequests
		}
		n, err := trace.JSONByteLen(m)
		if err != nil {
			t.Fatal(err)
		}
		outBytes[id] = n
	}
	return got.Status, got.ErrorText, len(cpJSON), outBytes, outs
}

// TestRun_githubIssuesListClampedToCheckpointLimit: a tool maxToolOutputBytes above the
// run's maxCheckpointBytes (2 MiB vs the 1 MiB default) must not make the op return an
// output the run can never checkpoint. Before the clamp the op filled 3/4 of 2 MiB
// (about 1.5 MiB), passed enforceToolOutput, and the run failed at the final checkpoint.
// runToolStep now advertises min(tool limit, checkpoint limit / checkpointOutputShare),
// so the list stays well inside the checkpoint and the run succeeds with whole items.
func TestRun_githubIssuesListClampedToCheckpointLimit(t *testing.T) {
	const toolLimit = 2 << 20
	cpLimit := spec.DefaultMaxCheckpointBytes
	if n, err := trace.JSONByteLen(map[string]any{"issues": append(realisticIssuePage(1), realisticIssuePage(2)...)}); err != nil || n <= cpLimit {
		t.Fatalf("200 raw issues are %d bytes (err %v), within the checkpoint limit; fixture would not exercise the clamp", n, err)
	}
	r := runGitHubListWorkflow(t, nil,
		&spec.ExecutionLimits{MaxToolOutputBytes: toolLimit, ToolOutputExceedPolicy: spec.LimitExceedFail},
		[]spec.WorkflowStep{githubListStep("list", "issues.list")},
		map[string]any{"truncated": "${steps.list.output.truncated}"}, HitlRunOptions{AutoApprove: true})
	if r.runErr != nil {
		t.Fatal(r.runErr)
	}
	status, errText, cpBytes, outBytes, outs := githubListCheckpointSizes(t, r)
	if status != "succeeded" {
		t.Fatalf("status %q err=%q", status, errText)
	}
	out := outs["list"]
	if !out.Truncated {
		t.Fatal("truncated = false, want true for a list clamped by the checkpoint limit")
	}
	assertWholeIssuesInOrder(t, out.Issues)
	if want := cpLimit / checkpointOutputShare * 3 / 4; outBytes["list"] > want {
		t.Fatalf("list output is %d bytes, want <= %d (3/4 of the checkpoint-clamped budget)", outBytes["list"], want)
	}
	if outBytes["list"] > cpLimit*3/4 || cpBytes > cpLimit {
		t.Fatalf("list output %d bytes, checkpoint %d bytes; want output <= 3/4 and checkpoint <= all of %d", outBytes["list"], cpBytes, cpLimit)
	}
	// The raise still helps up to the clamp: more items than at the default tool limit.
	if def := len(runGitHubIssuesList(t, nil, nil)); len(out.Issues) <= def {
		t.Fatalf("clamped list returned %d issues, default returned %d; want more", len(out.Issues), def)
	}
	events, err := r.st.ListTraceEventsByRunID(context.Background(), r.runID)
	if err != nil {
		t.Fatal(err)
	}
	if n := countLimitHitEvents(events, string(spec.LimitKindToolOutput)) + countLimitHitEvents(events, string(spec.LimitKindCheckpoint)); n != 0 {
		t.Fatalf("%d limit_hit events; the clamped list must fit both limits by itself", n)
	}
}

// TestRun_githubListThenApprovalGateSuspends: a suspension checkpoint stores a completed
// step's output twice (steps + the execir memo), which is why the clamp is half the
// checkpoint limit. With a 2 MiB tool limit, a list followed by an approval gate must
// suspend cleanly (a clamp at the full checkpoint limit would put ~1.5 MiB of list
// output in that checkpoint and fail the run instead of interrupting it).
func TestRun_githubListThenApprovalGateSuspends(t *testing.T) {
	r := runGitHubListWorkflow(t, nil,
		&spec.ExecutionLimits{MaxToolOutputBytes: 2 << 20, ToolOutputExceedPolicy: spec.LimitExceedFail},
		[]spec.WorkflowStep{
			githubListStep("list", "issues.list"),
			{ID: "gate", Approval: &spec.WorkflowApprovalValue{Enabled: true}, With: map[string]any{"n": "${steps.list.output.truncated}"}},
		},
		map[string]any{"truncated": "${steps.list.output.truncated}"}, HitlRunOptions{})
	if !errors.Is(r.runErr, ErrInterrupted) {
		t.Fatalf("run err = %v, want ErrInterrupted at the approval gate", r.runErr)
	}
	status, errText, cpBytes, _, outs := githubListCheckpointSizes(t, r)
	if status != state.RunStatusInterrupted {
		t.Fatalf("status %q err=%q, want interrupted", status, errText)
	}
	if cpBytes > spec.DefaultMaxCheckpointBytes {
		t.Fatalf("suspension checkpoint is %d bytes, over %d", cpBytes, spec.DefaultMaxCheckpointBytes)
	}
	if o := outs["list"]; !o.Truncated || len(o.Issues) == 0 {
		t.Fatalf("list output in the suspension checkpoint: truncated=%v, %d issues", o.Truncated, len(o.Issues))
	}
}

// TestRun_githubListsShareCheckpointLimit documents that the clamp is per output: list
// steps in one workflow share the checkpoint, and fitting their sum is the operator's
// responsibility. At a 1 MiB tool limit (default 1 MiB checkpoint) each list is clamped
// to 3/4 of half the checkpoint, so issues.list + pull_request.list fit together and the
// run succeeds (before the clamp each filled 3/4 of 1 MiB and the pair failed the final
// checkpoint). A third list pushes the sum past the checkpoint limit, and the run fails
// at the final checkpoint with an error that names it: accumulation is not clamped.
func TestRun_githubListsShareCheckpointLimit(t *testing.T) {
	toolLimits := &spec.ExecutionLimits{MaxToolOutputBytes: 1 << 20, ToolOutputExceedPolicy: spec.LimitExceedFail}
	output := map[string]any{"truncated": "${steps.issues.output.truncated}"}

	two := runGitHubListWorkflow(t, nil, toolLimits, []spec.WorkflowStep{
		githubListStep("issues", "issues.list"),
		githubListStep("pulls", "pull_request.list"),
	}, output, HitlRunOptions{AutoApprove: true})
	if two.runErr != nil {
		t.Fatal(two.runErr)
	}
	status, errText, cpBytes, _, outs := githubListCheckpointSizes(t, two)
	if status != "succeeded" {
		t.Fatalf("two lists: status %q err=%q, want succeeded", status, errText)
	}
	if cpBytes > spec.DefaultMaxCheckpointBytes || !outs["issues"].Truncated || !outs["pulls"].Truncated {
		t.Fatalf("two lists: checkpoint %d bytes, truncated issues=%v pulls=%v", cpBytes, outs["issues"].Truncated, outs["pulls"].Truncated)
	}
	assertWholeIssuesInOrder(t, outs["issues"].Issues)
	assertWholeIssuesInOrder(t, outs["pulls"].PullRequests)

	three := runGitHubListWorkflow(t, nil, toolLimits, []spec.WorkflowStep{
		githubListStep("issues", "issues.list"),
		githubListStep("pulls", "pull_request.list"),
		githubListStep("more", "issues.list"),
	}, output, HitlRunOptions{AutoApprove: true})
	got, err := three.st.GetRun(context.Background(), three.runID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "failed" || !strings.Contains(got.ErrorText, "checkpoint context exceeds") {
		t.Fatalf("three lists: status %q err=%q (run err %v), want failed on the checkpoint limit", got.Status, got.ErrorText, three.runErr)
	}
}
