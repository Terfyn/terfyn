package native

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGithubPullRequestGet_happyPath(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-token-xyz")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/widget/pulls/42" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Fatalf("method %s", r.Method)
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Fatalf("missing bearer: %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"number":42,"title":"hi"}`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("GITHUB_API_URL", srv.URL)

	reg := NewRegistry()
	out, _, err := reg.Dispatch(context.Background(), "pull_request.get", map[string]any{
		"owner":  "acme",
		"repo":   "widget",
		"number": float64(42),
	})
	if err != nil {
		t.Fatal(err)
	}
	pr, ok := out["pull_request"].(map[string]any)
	if !ok {
		t.Fatalf("pull_request: %T %#v", out["pull_request"], out["pull_request"])
	}
	if pr["title"] != "hi" {
		t.Fatalf("title %#v", pr["title"])
	}
}

func TestGithubPullRequestDiff_happyPath(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "tok")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/pulls/1" {
			t.Fatalf("path %q", r.URL.Path)
		}
		if got := r.Header.Get("Accept"); got != githubAcceptDiff {
			t.Fatalf("Accept %q want %q", got, githubAcceptDiff)
		}
		_, _ = w.Write([]byte("diff --git a/x b/x\n"))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("GITHUB_API_URL", srv.URL)

	reg := NewRegistry()
	out, _, err := reg.Dispatch(context.Background(), "pull_request.diff", map[string]any{
		"owner": "o", "repo": "r", "pull_number": "1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out["diff"] != "diff --git a/x b/x\n" {
		t.Fatalf("diff %#v", out["diff"])
	}
}

func TestGithubCheckRunsList_happyPath(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "tok")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := "/repos/acme/r/commits/deadbeef/check-runs"
		if r.URL.Path != want {
			t.Fatalf("path %q want %q", r.URL.Path, want)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"total_count":1,"check_runs":[{"name":"ci"}]}`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("GITHUB_API_URL", srv.URL)

	reg := NewRegistry()
	out, _, err := reg.Dispatch(context.Background(), "check_runs.list", map[string]any{
		"owner": "acme", "repo": "r", "head_sha": "deadbeef",
	})
	if err != nil {
		t.Fatal(err)
	}
	if int(out["total_count"].(float64)) != 1 {
		t.Fatalf("total_count %#v", out["total_count"])
	}
}

func TestGithubPullRequestGet_missingToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	t.Setenv("GITHUB_API_URL", srv.URL)

	reg := NewRegistry()
	_, _, err := reg.Dispatch(context.Background(), "pull_request.get", map[string]any{
		"owner": "a", "repo": "b", "number": 1,
	})
	if err == nil || !strings.Contains(err.Error(), "GITHUB_TOKEN") {
		t.Fatalf("err=%v", err)
	}
}

func TestGithubPostComment_liveRequiresToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	reg := NewRegistry()
	_, _, err := reg.Dispatch(context.Background(), "pull_request.post_comment", map[string]any{
		"owner": "o", "repo": "r", "number": "1", "body": "x",
	})
	if err == nil || !strings.Contains(err.Error(), "GITHUB_TOKEN") {
		t.Fatalf("err=%v", err)
	}
}

func TestGithubPostComment_simulatedWhenRepoContextMissing(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "should-be-ignored")
	reg := NewRegistry()
	out, _, err := reg.Dispatch(context.Background(), "pull_request.post_comment", map[string]any{
		"body": "hello world comment",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out["simulated"] != true {
		t.Fatalf("want simulated, got %#v", out)
	}
}

func TestGithubPostComment_liveCreatesIssueComment(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "tok")
	var postBodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/3/comments":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/3/comments":
			b, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatal(err)
			}
			postBodies = append(postBodies, string(b))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":42,"html_url":"https://github.com/o/r/issues/3#issuecomment-42"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("GITHUB_API_URL", srv.URL)

	reg := NewRegistry()
	out, _, err := reg.Dispatch(context.Background(), "pull_request.post_comment", map[string]any{
		"owner": "o", "repo": "r", "number": "3", "body": "hi",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out["simulated"] != false {
		t.Fatalf("simulated %#v", out["simulated"])
	}
	if out["id"].(float64) != 42 {
		t.Fatalf("id %#v", out["id"])
	}
	if out["created"] != true {
		t.Fatalf("created %#v", out["created"])
	}
	if len(postBodies) != 1 || !strings.Contains(postBodies[0], "agentic-review") {
		t.Fatalf("POST body should include marker: %v", postBodies)
	}
}

func TestGithubPostComment_appendSkipsListAndMarker(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "tok")
	var gotGET bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			gotGET = true
		}
		if r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/1/comments" {
			b, _ := io.ReadAll(r.Body)
			if strings.Contains(string(b), AgenticReviewMarker) {
				t.Fatal("append strategy should not inject marker")
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":1}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("GITHUB_API_URL", srv.URL)

	reg := NewRegistry()
	_, _, err := reg.Dispatch(context.Background(), "pull_request.post_comment", map[string]any{
		"owner": "o", "repo": "r", "number": "1", "body": "plain",
		"comment_strategy": "append",
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotGET {
		t.Fatal("append should not list comments")
	}
}

func TestGithubPostComment_replaceUpdatesExisting(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "tok")
	var methods []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/5/comments":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"id":99,"body":"old\n\n` + AgenticReviewMarker + `"}]`))
		case r.Method == http.MethodPatch && r.URL.Path == "/repos/o/r/issues/comments/99":
			b, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(b), "updated review") {
				t.Fatalf("patch body %s", string(b))
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":99,"html_url":"https://example/99"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("GITHUB_API_URL", srv.URL)

	reg := NewRegistry()
	out, _, err := reg.Dispatch(context.Background(), "pull_request.post_comment", map[string]any{
		"owner": "o", "repo": "r", "number": "5", "body": "updated review",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out["updated"] != true || out["created"] != false {
		t.Fatalf("updated/created %#v %#v", out["updated"], out["created"])
	}
	if !strings.Contains(strings.Join(methods, ";"), "PATCH") {
		t.Fatalf("methods %v", methods)
	}
}

func TestGithubPostComment_replaceCreatesWhenNoMarker(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "tok")
	posted := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments") {
			_, _ = w.Write([]byte(`[{"id":1,"body":"unrelated"}]`))
			return
		}
		if r.Method == http.MethodPost {
			posted = true
			_, _ = w.Write([]byte(`{"id":2}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("GITHUB_API_URL", srv.URL)

	reg := NewRegistry()
	out, _, err := reg.Dispatch(context.Background(), "pull_request.post_comment", map[string]any{
		"owner": "o", "repo": "r", "number": "2", "body": "first",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !posted {
		t.Fatal("expected POST")
	}
	if out["created"] != true {
		t.Fatalf("created %#v", out["created"])
	}
}

func TestGithubPostComment_commentIDPatchesDirectly(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "tok")
	patched := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch && r.URL.Path == "/repos/a/b/issues/comments/777" {
			patched = true
			_, _ = w.Write([]byte(`{"id":777}`))
			return
		}
		if r.Method == http.MethodGet {
			t.Fatal("comment_id should skip list")
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("GITHUB_API_URL", srv.URL)

	reg := NewRegistry()
	_, _, err := reg.Dispatch(context.Background(), "pull_request.post_comment", map[string]any{
		"owner": "a", "repo": "b", "number": "1", "body": "x", "comment_id": "777",
	})
	if err != nil || !patched {
		t.Fatalf("err=%v patched=%v", err, patched)
	}
}

func TestGithubPostComment_invalidStrategy(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "tok")
	reg := NewRegistry()
	_, _, err := reg.Dispatch(context.Background(), "pull_request.post_comment", map[string]any{
		"owner": "o", "repo": "r", "number": "1", "body": "x",
		"comment_strategy": "merge",
	})
	if err == nil || !strings.Contains(err.Error(), "comment_strategy") {
		t.Fatalf("err=%v", err)
	}
}

func TestGithubPostComment_upsertAlias(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "tok")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`[{"id":5,"body":"` + AgenticReviewMarker + `"}]`))
			return
		}
		if r.Method == http.MethodPatch {
			_, _ = w.Write([]byte(`{"id":5}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("GITHUB_API_URL", srv.URL)

	reg := NewRegistry()
	out, _, err := reg.Dispatch(context.Background(), "pull_request.post_comment", map[string]any{
		"owner": "o", "repo": "r", "number": "1", "body": "via upsert", "upsert": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out["updated"] != true {
		t.Fatalf("updated %#v", out["updated"])
	}
}

func TestEnsureAgenticReviewMarker(t *testing.T) {
	got := ensureAgenticReviewMarker("hello")
	if !strings.Contains(got, AgenticReviewMarker) {
		t.Fatalf("got %q", got)
	}
	if strings.Count(got, AgenticReviewMarker) != 1 {
		t.Fatalf("duplicate marker: %q", got)
	}
	if ensureAgenticReviewMarker(got) != got {
		t.Fatal("idempotent")
	}
}

func TestGithubCommentStrategy_defaultsReplace(t *testing.T) {
	s, err := githubCommentStrategy(map[string]any{})
	if err != nil || s != commentStrategyReplace {
		t.Fatalf("s=%q err=%v", s, err)
	}
}

func TestGithubPullRequestGet_httpError(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "tok")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("GITHUB_API_URL", srv.URL)

	reg := NewRegistry()
	_, _, err := reg.Dispatch(context.Background(), "pull_request.get", map[string]any{
		"owner": "a", "repo": "b", "number": 1,
	})
	if err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("err=%v", err)
	}
}

func TestGithubIssuesGet_happyPath(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-token-xyz")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// issues.get must hit the /issues/ endpoint, not /pulls/ (which 404s on a
		// plain issue number).
		if r.URL.Path != "/repos/acme/widget/issues/123" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Fatalf("method %s", r.Method)
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Fatalf("missing bearer: %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"number":123,"title":"Fix the CSRF bug","body":"details"}`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("GITHUB_API_URL", srv.URL)

	reg := NewRegistry()
	out, _, err := reg.Dispatch(context.Background(), "issues.get", map[string]any{
		"owner":  "acme",
		"repo":   "widget",
		"number": float64(123),
	})
	if err != nil {
		t.Fatal(err)
	}
	issue, ok := out["issue"].(map[string]any)
	if !ok {
		t.Fatalf("issue: %T %#v", out["issue"], out["issue"])
	}
	if issue["title"] != "Fix the CSRF bug" || issue["body"] != "details" {
		t.Fatalf("issue %#v", issue)
	}
}

func TestGithubIssuesGet_requiresOwner(t *testing.T) {
	reg := NewRegistry()
	if _, _, err := reg.Dispatch(context.Background(), "issues.get", map[string]any{
		"repo": "widget", "number": float64(1),
	}); err == nil {
		t.Fatal("issues.get without owner should error")
	}
}

func TestGithubPullRequestCreate(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "tok")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/repos/acme/widget/pulls" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		body := string(b)
		for _, want := range []string{`"head":"terfyn/fix-7"`, `"base":"main"`, `"title":"Fix it"`, `"maintainer_can_modify":true`} {
			if !strings.Contains(body, want) {
				t.Fatalf("request body %q missing %q", body, want)
			}
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"number":42,"html_url":"https://github.com/acme/widget/pull/42","state":"open"}`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("GITHUB_API_URL", srv.URL)

	out, _, err := NewRegistry().Dispatch(context.Background(), "pull_request.create", map[string]any{
		"owner": "acme", "repo": "widget", "head": "terfyn/fix-7", "base": "main", "title": "Fix it",
		"maintainer_can_modify": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out["number"] != float64(42) || out["html_url"] == "" {
		t.Fatalf("out %#v", out)
	}
}

func TestGithubPullRequestCreate_requiresTitleOrIssue(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "tok")
	_, _, err := NewRegistry().Dispatch(context.Background(), "pull_request.create", map[string]any{
		"owner": "a", "repo": "b", "head": "h", "base": "main",
	})
	if err == nil || !strings.Contains(err.Error(), "title or issue") {
		t.Fatalf("err = %v, want title-or-issue", err)
	}
}

func TestGithubIssuesList(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "tok")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/issues" || r.URL.Query().Get("state") != "open" {
			t.Fatalf("path %q query %q", r.URL.Path, r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"number":1},{"number":2}]`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("GITHUB_API_URL", srv.URL)

	out, _, err := NewRegistry().Dispatch(context.Background(), "issues.list", map[string]any{
		"owner": "o", "repo": "r", "state": "open",
	})
	if err != nil {
		t.Fatal(err)
	}
	if arr, ok := out["issues"].([]any); !ok || len(arr) != 2 {
		t.Fatalf("issues %#v", out["issues"])
	}
}

func TestGithubPullRequestUpdate(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "tok")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || r.URL.Path != "/repos/o/r/pulls/7" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(b), `"state":"closed"`) {
			t.Fatalf("body %q missing state", b)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"number":7,"state":"closed"}`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("GITHUB_API_URL", srv.URL)

	out, _, err := NewRegistry().Dispatch(context.Background(), "pull_request.update", map[string]any{
		"owner": "o", "repo": "r", "number": float64(7), "state": "closed",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out["state"] != "closed" {
		t.Fatalf("out %#v", out)
	}
}

func TestGithubPullRequestUpdate_requiresAField(t *testing.T) {
	_, _, err := NewRegistry().Dispatch(context.Background(), "pull_request.update", map[string]any{
		"owner": "o", "repo": "r", "number": float64(7),
	})
	if err == nil || !strings.Contains(err.Error(), "requires one of") {
		t.Fatalf("err = %v, want 'requires one of'", err)
	}
}

func TestGithubIssuesUpdate(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "tok")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || r.URL.Path != "/repos/o/r/issues/5" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(b), `"labels":["bug"]`) {
			t.Fatalf("body %q missing labels", b)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"number":5,"state":"open"}`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("GITHUB_API_URL", srv.URL)

	if _, _, err := NewRegistry().Dispatch(context.Background(), "issues.update", map[string]any{
		"owner": "o", "repo": "r", "number": float64(5), "labels": []any{"bug"},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestGithubPullRequestList(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "tok")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/repos/o/r/pulls" || r.URL.Query().Get("base") != "main" {
			t.Fatalf("%s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"number":1}]`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("GITHUB_API_URL", srv.URL)

	out, _, err := NewRegistry().Dispatch(context.Background(), "pull_request.list", map[string]any{
		"owner": "o", "repo": "r", "base": "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	if arr, ok := out["pull_requests"].([]any); !ok || len(arr) != 1 {
		t.Fatalf("pull_requests %#v", out["pull_requests"])
	}
}

func TestIssuesListFollowsPagination(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			_, _ = w.Write([]byte(`[{"number":2}]`))
			return
		}
		w.Header().Set("Link", fmt.Sprintf(`<%s/repos/o/r/issues?page=2>; rel="next"`, srv.URL))
		_, _ = w.Write([]byte(`[{"number":1}]`))
	}))
	defer srv.Close()
	t.Setenv("GITHUB_TOKEN", "tok")
	t.Setenv("GITHUB_API_URL", srv.URL)

	got, err := githubIssuesList(context.Background(), map[string]any{"owner": "o", "repo": "r"})
	if err != nil {
		t.Fatal(err)
	}
	if issues := got["issues"].([]any); len(issues) != 2 {
		t.Fatalf("issues.list returned %d item(s), want both pages", len(issues))
	}
	if _, ok := got["truncated"]; ok {
		t.Fatalf("truncated set on a complete two-page list: %#v", got)
	}
}

func TestPullRequestListFollowsPagination(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/pulls" {
			t.Fatalf("path %q", r.URL.Path)
		}
		if r.URL.Query().Get("page") == "2" {
			_, _ = w.Write([]byte(`[{"number":2}]`))
			return
		}
		w.Header().Set("Link", fmt.Sprintf(`<%s/repos/o/r/pulls?page=2>; rel="next"`, srv.URL))
		_, _ = w.Write([]byte(`[{"number":1}]`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("GITHUB_TOKEN", "tok")
	t.Setenv("GITHUB_API_URL", srv.URL)

	got, err := githubPullRequestList(context.Background(), map[string]any{"owner": "o", "repo": "r"})
	if err != nil {
		t.Fatal(err)
	}
	if prs := got["pull_requests"].([]any); len(prs) != 2 {
		t.Fatalf("pull_request.list returned %d item(s), want both pages", len(prs))
	}
}

func TestGithubGETArrayEmptyTerminalPage(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		w.Header().Set("Link", fmt.Sprintf(`<%s/repos/o/r/issues?page=2>; rel="next"`, srv.URL))
		_, _ = w.Write([]byte(`[{"number":1}]`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("GITHUB_TOKEN", "tok")
	t.Setenv("GITHUB_API_URL", srv.URL)

	arr, truncated, err := githubGETArray(context.Background(), "/repos/o/r/issues", "issues.list")
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Fatal("truncated on empty terminal page")
	}
	if len(arr) != 1 {
		t.Fatalf("got %d items, want first page only", len(arr))
	}
}

func TestGithubGETArrayStopsAtMaxPages(t *testing.T) {
	var srv *httptest.Server
	pages := 0
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		next := pages + 1
		w.Header().Set("Link", fmt.Sprintf(`<%s/repos/o/r/issues?page=%d>; rel="next"`, srv.URL, next))
		_, _ = w.Write([]byte(fmt.Sprintf(`[{"number":%d}]`, pages)))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("GITHUB_TOKEN", "tok")
	t.Setenv("GITHUB_API_URL", srv.URL)

	arr, truncated, err := githubGETArray(context.Background(), "/repos/o/r/issues", "issues.list")
	if err != nil {
		t.Fatal(err)
	}
	if !truncated {
		t.Fatal("want truncated when rel=next remains after max pages")
	}
	if len(arr) != githubListMaxPages {
		t.Fatalf("got %d items, want %d pages", len(arr), githubListMaxPages)
	}
	if pages != githubListMaxPages {
		t.Fatalf("fetched %d pages, want %d", pages, githubListMaxPages)
	}
}

func TestParseGitHubLinkNext(t *testing.T) {
	got := parseGitHubLinkNext(`<https://api.github.com/repos/o/r/issues?page=2>; rel="next", <https://api.github.com/repos/o/r/issues?page=4>; rel="last"`)
	if got != "https://api.github.com/repos/o/r/issues?page=2" {
		t.Fatalf("next = %q", got)
	}
	if parseGitHubLinkNext(`<https://api.github.com/repos/o/r/issues?page=4>; rel="last"`) != "" {
		t.Fatal("expected empty without rel=next")
	}
}

func TestGithubAllowFollowRejectsForeignHost(t *testing.T) {
	t.Setenv("GITHUB_API_URL", "https://api.github.com")
	if _, ok := githubAllowFollow("https://evil.example/repos/o/r/issues?page=2"); ok {
		t.Fatal("followed foreign host")
	}
	if got, ok := githubAllowFollow("/repos/o/r/issues?page=2"); !ok || got != "/repos/o/r/issues?page=2" {
		t.Fatalf("path follow = %q ok=%v", got, ok)
	}
}
