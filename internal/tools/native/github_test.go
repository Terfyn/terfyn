package native

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Terfyn/terfyn/internal/spec"
	"github.com/Terfyn/terfyn/internal/tools/toolctx"
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
	if out["total_count"] != int64(1) {
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
	if out["id"] != int64(42) {
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
	if out["number"] != int64(42) || out["html_url"] == "" {
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
	if got["truncated"] != false {
		t.Fatalf("truncated must be false on a complete two-page list: %#v", got)
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

	arr, truncated, err := githubGETArray(context.Background(), "/repos/o/r/issues", "issues.list", githubListMaxLimit)
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

	arr, truncated, err := githubGETArray(context.Background(), "/repos/o/r/issues", "issues.list", githubListMaxLimit)
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
	got, present := parseGitHubLinkNext(`<https://api.github.com/repos/o/r/issues?page=2>; rel="next", <https://api.github.com/repos/o/r/issues?page=4>; rel="last"`)
	if !present || got != "https://api.github.com/repos/o/r/issues?page=2" {
		t.Fatalf("next = %q present=%v", got, present)
	}
	if got, present := parseGitHubLinkNext(`<https://api.github.com/repos/o/r/issues?page=4>; rel="last"`); present || got != "" {
		t.Fatalf("expected no next without rel=next, got %q present=%v", got, present)
	}
	if _, present := parseGitHubLinkNext(""); present {
		t.Fatal("empty header must not report a next link")
	}
	// A comma inside <...> must not split the entry.
	if got, present := parseGitHubLinkNext(`<https://api.github.com/x?labels=a,b&page=2>; rel="next"`); !present || got != "https://api.github.com/x?labels=a,b&page=2" {
		t.Fatalf("comma in target: %q present=%v", got, present)
	}
	// rel may be a list of relation types.
	if _, present := parseGitHubLinkNext(`<https://api.github.com/x?page=2>; rel="next last"`); !present {
		t.Fatal("rel list containing next not detected")
	}
	// Malformed entries that name rel=next are present-but-unusable, not absent.
	for _, bad := range []string{
		`https://api.github.com/x?page=2; rel="next"`,
		`<https://api.github.com/x?page=2; rel="next"`,
		`<>; rel="next"`,
		`rel="next"`,
	} {
		got, present := parseGitHubLinkNext(bad)
		if !present || got != "" {
			t.Fatalf("%q: got %q present=%v, want present with empty target", bad, got, present)
		}
	}
}

func TestGithubFollowNext(t *testing.T) {
	const cur = "https://ghe.example.com/api/v3/repos/o/r/issues?per_page=100"
	t.Setenv("GITHUB_API_URL", "https://ghe.example.com/api/v3/")
	tests := []struct {
		name  string
		link  []string
		state githubNextState
		want  string
	}{
		{"no link", nil, githubNextNone, ""},
		{"only last", []string{`<https://ghe.example.com/api/v3/x?page=9>; rel="last"`}, githubNextNone, ""},
		{"same origin", []string{`<https://ghe.example.com/api/v3/repos/o/r/issues?page=2>; rel="next"`}, githubNextFollow, "https://ghe.example.com/api/v3/repos/o/r/issues?page=2"},
		{"explicit default port", []string{`<https://ghe.example.com:443/api/v3/repos/o/r/issues?page=2>; rel="next"`}, githubNextFollow, "https://ghe.example.com:443/api/v3/repos/o/r/issues?page=2"},
		{"host case", []string{`<https://GHE.Example.com/api/v3/repos/o/r/issues?page=2>; rel="next"`}, githubNextFollow, "https://GHE.Example.com/api/v3/repos/o/r/issues?page=2"},
		{"relative under base", []string{`</api/v3/repos/o/r/issues?page=2>; rel="next"`}, githubNextFollow, "https://ghe.example.com/api/v3/repos/o/r/issues?page=2"},
		{"multiple header lines", []string{`<https://ghe.example.com/api/v3/y>; rel="prev"`, `<https://ghe.example.com/api/v3/z>; rel="next"`}, githubNextFollow, "https://ghe.example.com/api/v3/z"},
		{"scheme downgrade", []string{`<http://ghe.example.com/api/v3/repos/o/r/issues?page=2>; rel="next"`}, githubNextRejected, ""},
		{"other host", []string{`<https://evil.example/api/v3/repos/o/r/issues?page=2>; rel="next"`}, githubNextRejected, ""},
		{"host suffix trick", []string{`<https://ghe.example.com.evil.example/api/v3/x>; rel="next"`}, githubNextRejected, ""},
		{"other port", []string{`<https://ghe.example.com:8443/api/v3/x>; rel="next"`}, githubNextRejected, ""},
		{"userinfo", []string{`<https://user:pw@ghe.example.com/api/v3/x>; rel="next"`}, githubNextRejected, ""},
		{"scheme-relative foreign host", []string{`<//evil.example/api/v3/x>; rel="next"`}, githubNextRejected, ""},
		{"path outside base", []string{`<https://ghe.example.com/other/x>; rel="next"`}, githubNextRejected, ""},
		{"path prefix lookalike", []string{`<https://ghe.example.com/api/v3evil/x>; rel="next"`}, githubNextRejected, ""},
		{"dot segments", []string{`<https://ghe.example.com/api/v3/../../x>; rel="next"`}, githubNextRejected, ""},
		{"non-http scheme", []string{`<ftp://ghe.example.com/api/v3/x>; rel="next"`}, githubNextRejected, ""},
		{"malformed next", []string{`https://ghe.example.com/api/v3/x; rel="next"`}, githubNextRejected, ""},
		{"empty target", []string{`<>; rel="next"`}, githubNextRejected, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, state, reason := githubFollowNext(cur, tc.link)
			if state != tc.state {
				t.Fatalf("state = %v (reason %q), want %v", state, reason, tc.state)
			}
			if got.String() != tc.want {
				t.Fatalf("next = %q, want %q", got.String(), tc.want)
			}
			if state == githubNextRejected && reason == "" {
				t.Fatal("rejected without a reason")
			}
			if strings.Contains(reason, "pw") {
				t.Fatalf("reason leaks credentials: %q", reason)
			}
		})
	}
}

func TestGithubFollowNextDefaultPortNormalization(t *testing.T) {
	t.Setenv("GITHUB_API_URL", "http://ghe.example.com:80")
	if _, st, r := githubFollowNext("http://ghe.example.com:80/repos/o/r/issues", []string{`<http://ghe.example.com/repos/o/r/issues?page=2>; rel="next"`}); st != githubNextFollow {
		t.Fatalf("http :80 vs implicit port: state %v (%s)", st, r)
	}
	if _, st, _ := githubFollowNext("http://ghe.example.com:80/repos/o/r/issues", []string{`<https://ghe.example.com/repos/o/r/issues?page=2>; rel="next"`}); st != githubNextRejected {
		t.Fatalf("http base must not accept https link: state %v", st)
	}
	t.Setenv("GITHUB_API_URL", "")
	if _, st, r := githubFollowNext("https://api.github.com/repos/o/r/issues", []string{`<https://api.github.com/repos/o/r/issues?page=2>; rel="next"`}); st != githubNextFollow {
		t.Fatalf("default base: state %v (%s)", st, r)
	}
}

// recordedReq captures what a test server saw, so tests can prove where the token went.
type recordedReq struct{ path, auth string }

func TestGithubGETArrayRejectedNextFailsAndNeverSendsToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "secret-token")

	type linkFn func(apiURL string, other *httptest.Server) string
	tests := []struct {
		name string
		link linkFn
	}{
		{"different host", func(_ string, other *httptest.Server) string {
			return fmt.Sprintf(`<%s/repos/o/r/issues?page=2>; rel="next"`, other.URL)
		}},
		{"malformed next", func(api string, _ *httptest.Server) string {
			return fmt.Sprintf(`%s/repos/o/r/issues?page=2; rel="next"`, api)
		}},
		{"different port same host", func(api string, _ *httptest.Server) string {
			u, _ := url.Parse(api)
			return fmt.Sprintf(`<http://%s:1/repos/o/r/issues?page=2>; rel="next"`, u.Hostname())
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var otherReqs []recordedReq
			other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				otherReqs = append(otherReqs, recordedReq{r.URL.Path, r.Header.Get("Authorization")})
				mu.Unlock()
				_, _ = w.Write([]byte(`[{"number":99}]`))
			}))
			t.Cleanup(other.Close)
			var api *httptest.Server
			api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Link", tc.link(api.URL, other))
				_, _ = w.Write([]byte(`[{"number":1}]`))
			}))
			t.Cleanup(api.Close)
			t.Setenv("GITHUB_API_URL", api.URL)

			arr, truncated, err := githubGETArray(context.Background(), "/repos/o/r/issues", "issues.list", githubListMaxLimit)
			if err == nil {
				t.Fatalf("want error for unfollowable next link, got %d items truncated=%v", len(arr), truncated)
			}
			if arr != nil {
				t.Fatalf("partial list returned alongside error: %v", arr)
			}
			if !strings.Contains(err.Error(), "next page") || strings.Contains(err.Error(), "secret-token") {
				t.Fatalf("unexpected error text: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(otherReqs) != 0 {
				t.Fatalf("request reached foreign server: %+v", otherReqs)
			}
		})
	}
}

func TestGithubGETArrayHTTPSDowngradeNextIsRejectedWithoutToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "secret-token")
	var mu sync.Mutex
	var plain []recordedReq
	// The downgrade target: a plain-HTTP listener. It shares a hostname (127.0.0.1) with
	// the HTTPS API server but not a scheme or port.
	plainSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		plain = append(plain, recordedReq{r.URL.Path, r.Header.Get("Authorization")})
		mu.Unlock()
		_, _ = w.Write([]byte(`[{"number":2}]`))
	}))
	t.Cleanup(plainSrv.Close)

	var tlsAuth []string
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		tlsAuth = append(tlsAuth, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Link", fmt.Sprintf(`<%s/repos/o/r/issues?page=2>; rel="next"`, plainSrv.URL))
		_, _ = w.Write([]byte(`[{"number":1}]`))
	}))
	t.Cleanup(tlsSrv.Close)
	t.Setenv("GITHUB_API_URL", tlsSrv.URL)

	// Trust the test server's certificate for this client only.
	orig := defaultGitHubHTTPClient
	defaultGitHubHTTPClient = func() *http.Client {
		return newGitHubHTTPClient(tlsSrv.Client().Transport)
	}
	t.Cleanup(func() { defaultGitHubHTTPClient = orig })

	_, _, err := githubGETArray(context.Background(), "/repos/o/r/issues", "issues.list", githubListMaxLimit)
	if err == nil {
		t.Fatal("want error for https -> http next link on the same host")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(plain) != 0 {
		t.Fatalf("downgraded request was sent (token exposure): %+v", plain)
	}
	if len(tlsAuth) != 1 || tlsAuth[0] != "Bearer secret-token" {
		t.Fatalf("API server saw auth headers %v, want exactly one bearer request", tlsAuth)
	}
}

func TestGithubGETArrayNoNextLinkIsComplete(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "tok")
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// rel="last"/"prev" only: no continuation.
		w.Header().Set("Link", `<http://elsewhere.invalid/x?page=1>; rel="prev", <http://elsewhere.invalid/x?page=1>; rel="last"`)
		_, _ = w.Write([]byte(`[{"number":1},{"number":2}]`))
	}))
	t.Cleanup(api.Close)
	t.Setenv("GITHUB_API_URL", api.URL)

	arr, truncated, err := githubGETArray(context.Background(), "/repos/o/r/issues", "issues.list", githubListMaxLimit)
	if err != nil || truncated || len(arr) != 2 {
		t.Fatalf("arr=%d truncated=%v err=%v, want complete 2 items", len(arr), truncated, err)
	}
}

func TestGithubGETArrayFollowsSameOriginNextWithToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "secret-token")
	var mu sync.Mutex
	var auths []string
	var api *httptest.Server
	api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		if r.URL.Query().Get("page") == "2" {
			_, _ = w.Write([]byte(`[{"number":2}]`))
			return
		}
		w.Header().Set("Link", fmt.Sprintf(`<%s/repos/o/r/issues?page=2>; rel="next"`, api.URL))
		_, _ = w.Write([]byte(`[{"number":1}]`))
	}))
	t.Cleanup(api.Close)
	t.Setenv("GITHUB_API_URL", api.URL)

	arr, truncated, err := githubGETArray(context.Background(), "/repos/o/r/issues", "issues.list", githubListMaxLimit)
	if err != nil || truncated || len(arr) != 2 {
		t.Fatalf("arr=%d truncated=%v err=%v, want 2 items complete", len(arr), truncated, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(auths) != 2 || auths[0] != "Bearer secret-token" || auths[1] != "Bearer secret-token" {
		t.Fatalf("auth headers = %v", auths)
	}
}

// fakeGitHubIssue returns an object shaped like a real GitHub REST issue (URL fields,
// a user object, labels, reactions) with a body of bodyLen bytes, so size-sensitive
// tests see payloads close to what GitHub actually sends (a few KB to ~15 KB each).
func fakeGitHubIssue(number, bodyLen int) map[string]any {
	api := "https://api.github.com/repos/o/r/issues/" + fmt.Sprint(number)
	user := map[string]any{
		"login": "someone", "id": 12345, "node_id": "MDQ6VXNlcjEyMzQ1", "type": "User", "site_admin": false,
		"avatar_url": "https://avatars.githubusercontent.com/u/12345?v=4", "gravatar_id": "",
		"url": "https://api.github.com/users/someone", "html_url": "https://github.com/someone",
		"followers_url":       "https://api.github.com/users/someone/followers",
		"following_url":       "https://api.github.com/users/someone/following{/other_user}",
		"gists_url":           "https://api.github.com/users/someone/gists{/gist_id}",
		"starred_url":         "https://api.github.com/users/someone/starred{/owner}{/repo}",
		"subscriptions_url":   "https://api.github.com/users/someone/subscriptions",
		"organizations_url":   "https://api.github.com/users/someone/orgs",
		"repos_url":           "https://api.github.com/users/someone/repos",
		"events_url":          "https://api.github.com/users/someone/events{/privacy}",
		"received_events_url": "https://api.github.com/users/someone/received_events",
	}
	label := map[string]any{
		"id": 1, "node_id": "MDU6TGFiZWwx", "name": "NeedsInvestigation", "color": "ededed", "default": false,
		"url": "https://api.github.com/repos/o/r/labels/NeedsInvestigation", "description": "Someone must examine and confirm this is a valid issue",
	}
	return map[string]any{
		"url": api, "repository_url": "https://api.github.com/repos/o/r",
		"labels_url": api + "/labels{/name}", "comments_url": api + "/comments", "events_url": api + "/events",
		"html_url": "https://github.com/o/r/issues/" + fmt.Sprint(number), "id": 1000000 + number,
		"node_id": "I_kwDOAWBuf86" + fmt.Sprint(number), "number": number,
		"title": fmt.Sprintf("issue %d: something is broken in a moderately long title", number),
		"user":  user, "labels": []any{label, label}, "state": "open", "locked": false,
		"assignee": nil, "assignees": []any{}, "milestone": nil, "comments": 3,
		"created_at": "2026-09-01T12:00:00Z", "updated_at": "2026-09-02T12:00:00Z", "closed_at": nil,
		"author_association": "CONTRIBUTOR", "active_lock_reason": nil,
		"body":         strings.Repeat("lorem ipsum <dolor> & sit amet, ", bodyLen/32+1)[:bodyLen],
		"reactions":    map[string]any{"url": api + "/reactions", "total_count": 0, "+1": 0, "-1": 0, "laugh": 0, "hooray": 0, "confused": 0, "heart": 0, "rocket": 0, "eyes": 0},
		"timeline_url": api + "/timeline", "performed_via_github_app": nil, "state_reason": nil,
	}
}

// fakeIssueBodyLen cycles issue bodies through 3-15 KB.
func fakeIssueBodyLen(number int) int { return 3000 + (number*1237)%12000 }

func TestIssuesListStopsAtByteBudgetWithRealisticItems(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "tok")
	var requests atomic.Int32
	var api *httptest.Server
	api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		page := 1
		if p := r.URL.Query().Get("page"); p != "" {
			_, _ = fmt.Sscan(p, &page)
		}
		items := make([]any, 0, githubListPerPage)
		for i := 0; i < githubListPerPage; i++ {
			n := (page-1)*githubListPerPage + i + 1
			items = append(items, fakeGitHubIssue(n, fakeIssueBodyLen(n)))
		}
		w.Header().Set("Link", fmt.Sprintf(`<%s/repos/o/r/issues?per_page=100&page=%d>; rel="next"`, api.URL, page+1))
		_ = json.NewEncoder(w).Encode(items)
	}))
	t.Cleanup(api.Close)
	t.Setenv("GITHUB_API_URL", api.URL)

	out, _, err := NewRegistry().Dispatch(context.Background(), "issues.list", map[string]any{"owner": "o", "repo": "r"})
	if err != nil {
		t.Fatal(err)
	}
	if out["truncated"] != true {
		t.Fatalf("truncated = %v, want true when the byte budget stops the walk", out["truncated"])
	}
	issues := out["issues"].([]any)
	if len(issues) == 0 || len(issues) >= githubListPerPage {
		t.Fatalf("got %d issues, want a non-empty prefix of the first page", len(issues))
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("made %d requests, want 1 (budget reached within the first page)", got)
	}
	itemsBytes := 0
	for i, it := range issues {
		m := it.(map[string]any)
		n := i + 1
		if m["number"] != int64(n) {
			t.Fatalf("item %d is issue %v, want GitHub order preserved", i, m["number"])
		}
		if len(m["body"].(string)) != fakeIssueBodyLen(n) {
			t.Fatalf("issue %d body cut to %d bytes, want whole items", n, len(m["body"].(string)))
		}
		b, _ := json.Marshal(it)
		itemsBytes += len(b) + 1
	}
	// The budget is filled: the next whole item would not have fit.
	nextB, _ := json.Marshal(fakeGitHubIssue(len(issues)+1, fakeIssueBodyLen(len(issues)+1)))
	if itemsBytes+len(nextB) <= githubListDefaultMaxOutputBytes {
		t.Fatalf("stopped at %d bytes although the next %d-byte item fit in %d", itemsBytes, len(nextB), githubListDefaultMaxOutputBytes)
	}
	total, _ := json.Marshal(out)
	if len(total) > spec.DefaultMaxToolOutputBytes {
		t.Fatalf("result is %d bytes, over the default tool-output limit %d", len(total), spec.DefaultMaxToolOutputBytes)
	}
}

func TestIssuesListKeepsOversizedFirstItem(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "tok")
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]any{
			fakeGitHubIssue(1, githubListDefaultMaxOutputBytes+1),
			fakeGitHubIssue(2, 100),
		})
	}))
	t.Cleanup(api.Close)
	t.Setenv("GITHUB_API_URL", api.URL)

	arr, truncated, err := githubGETArray(context.Background(), "/repos/o/r/issues", "issues.list", githubListMaxLimit)
	if err != nil {
		t.Fatal(err)
	}
	if len(arr) != 1 || !truncated {
		t.Fatalf("got %d items truncated=%v, want the oversized first item alone and truncated", len(arr), truncated)
	}
}

func TestGithubListOutputBudget(t *testing.T) {
	bg := context.Background()
	tests := []struct {
		name string
		ctx  context.Context
		want int
	}{
		{"no budget on context", bg, githubListDefaultMaxOutputBytes},
		{"default limit", toolctx.WithOutputBudget(bg, spec.DefaultMaxToolOutputBytes), githubListDefaultMaxOutputBytes},
		{"lowered limit", toolctx.WithOutputBudget(bg, 128<<10), 96 << 10},
		{"raised limit", toolctx.WithOutputBudget(bg, 1<<20), 768 << 10},
		{"no limit (0)", toolctx.WithOutputBudget(bg, 0), math.MaxInt},
		{"no limit (negative)", toolctx.WithOutputBudget(bg, -1), math.MaxInt},
		{"huge limit does not overflow", toolctx.WithOutputBudget(bg, math.MaxInt), math.MaxInt - math.MaxInt/4},
	}
	for _, tc := range tests {
		if got := githubListOutputBudget(tc.ctx); got != tc.want {
			t.Errorf("%s: budget = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestIssuesListFollowsContextOutputBudget checks that the byte bound tracks the
// resolved tool-output limit carried on the call context: a lowered limit returns
// fewer whole items, a raised one more, and "no limit" returns up to the page cap's
// worth of what was asked for (limit), always as a prefix of whole items.
func TestIssuesListFollowsContextOutputBudget(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "tok")
	var api *httptest.Server
	api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := 1
		if p := r.URL.Query().Get("page"); p != "" {
			_, _ = fmt.Sscan(p, &page)
		}
		items := make([]any, 0, githubListPerPage)
		for i := 0; i < githubListPerPage; i++ {
			n := (page-1)*githubListPerPage + i + 1
			items = append(items, fakeGitHubIssue(n, fakeIssueBodyLen(n)))
		}
		w.Header().Set("Link", fmt.Sprintf(`<%s/repos/o/r/issues?per_page=100&page=%d>; rel="next"`, api.URL, page+1))
		_ = json.NewEncoder(w).Encode(items)
	}))
	t.Cleanup(api.Close)
	t.Setenv("GITHUB_API_URL", api.URL)

	count := func(ctx context.Context, limit int) (int, int, bool) {
		t.Helper()
		arr, truncated, err := githubGETArray(ctx, "/repos/o/r/issues", "issues.list", limit)
		if err != nil {
			t.Fatal(err)
		}
		size := 0
		for i, it := range arr {
			m := it.(map[string]any)
			if m["number"] != int64(i+1) || len(m["body"].(string)) != fakeIssueBodyLen(i+1) {
				t.Fatalf("item %d is not whole and in order", i)
			}
			b, _ := json.Marshal(it)
			size += len(b) + 1
		}
		return len(arr), size, truncated
	}
	bg := context.Background()
	def, _, defTrunc := count(bg, githubListMaxLimit)
	low, lowSize, lowTrunc := count(toolctx.WithOutputBudget(bg, 128<<10), githubListMaxLimit)
	high, highSize, highTrunc := count(toolctx.WithOutputBudget(bg, 1<<20), githubListMaxLimit)
	if !defTrunc || !lowTrunc || !highTrunc {
		t.Fatalf("truncated default=%v low=%v high=%v, want all true", defTrunc, lowTrunc, highTrunc)
	}
	if !(low < def && def < high) {
		t.Fatalf("item counts low=%d default=%d high=%d, want low < default < high", low, def, high)
	}
	if lowSize > 96<<10 || highSize > 768<<10 {
		t.Fatalf("items exceed 3/4 of the limit: low %d bytes, high %d bytes", lowSize, highSize)
	}
	unl, _, unlTrunc := count(toolctx.WithOutputBudget(bg, 0), 250)
	if unl != 250 || !unlTrunc {
		t.Fatalf("no output limit: got %d items truncated=%v, want the 250 asked for and truncated", unl, unlTrunc)
	}
}

func TestIssuesListLimit(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "tok")
	tests := []struct {
		name          string
		limit         any
		total         int // items the repository has
		wantPerPage   string
		wantItems     int
		wantTruncated bool
		wantRequests  int32
	}{
		{"small limit, more remain", float64(3), 250, "3", 3, true, 1},
		{"limit across pages", 150, 250, "100", 150, true, 2},
		{"limit equals total", "5", 5, "5", 5, false, 1},
		{"limit above total", 40, 7, "40", 7, false, 1},
		{"limit at page boundary, more remain", 100, 250, "100", 100, true, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			var api *httptest.Server
			api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if got := r.URL.Query().Get("per_page"); got != tc.wantPerPage {
					t.Errorf("per_page = %q, want %q", got, tc.wantPerPage)
				}
				var per, page int
				_, _ = fmt.Sscan(r.URL.Query().Get("per_page"), &per)
				page = 1
				if p := r.URL.Query().Get("page"); p != "" {
					_, _ = fmt.Sscan(p, &page)
				}
				start := (page - 1) * per
				end := min(start+per, tc.total)
				items := []any{}
				for n := start + 1; n <= end; n++ {
					items = append(items, map[string]any{"number": n})
				}
				if end < tc.total {
					w.Header().Set("Link", fmt.Sprintf(`<%s/repos/o/r/issues?per_page=%d&page=%d>; rel="next"`, api.URL, per, page+1))
				}
				_ = json.NewEncoder(w).Encode(items)
			}))
			t.Cleanup(api.Close)
			t.Setenv("GITHUB_API_URL", api.URL)

			out, _, err := NewRegistry().Dispatch(context.Background(), "issues.list", map[string]any{"owner": "o", "repo": "r", "limit": tc.limit})
			if err != nil {
				t.Fatal(err)
			}
			if n := len(out["issues"].([]any)); n != tc.wantItems {
				t.Fatalf("got %d issues, want %d", n, tc.wantItems)
			}
			if out["truncated"] != tc.wantTruncated {
				t.Fatalf("truncated = %v, want %v", out["truncated"], tc.wantTruncated)
			}
			if got := requests.Load(); got != tc.wantRequests {
				t.Fatalf("made %d requests, want %d", got, tc.wantRequests)
			}
		})
	}
}

func TestListLimitValidation(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "tok")
	t.Setenv("GITHUB_API_URL", "http://127.0.0.1:1") // never reached
	for _, op := range []string{"issues.list", "pull_request.list"} {
		for _, bad := range []any{0, -1, githubListMaxLimit + 1, 2.5, "many", true} {
			_, _, err := NewRegistry().Dispatch(context.Background(), op, map[string]any{"owner": "o", "repo": "r", "limit": bad})
			if err == nil || !strings.Contains(err.Error(), "limit") {
				t.Fatalf("%s limit=%v: err = %v, want a limit validation error", op, bad, err)
			}
		}
	}
}

func TestListSchemasDeclareLimit(t *testing.T) {
	for _, op := range []string{"issues.list", "pull_request.list"} {
		raw, ok := OperationInputSchema(op)
		if !ok {
			t.Fatalf("%s: no input schema", op)
		}
		var sc struct {
			Properties map[string]struct {
				Type    string `json:"type"`
				Minimum *int   `json:"minimum"`
				Maximum *int   `json:"maximum"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(raw, &sc); err != nil {
			t.Fatal(err)
		}
		l, ok := sc.Properties["limit"]
		if !ok || l.Type != "integer" || l.Minimum == nil || *l.Minimum != 1 || l.Maximum == nil || *l.Maximum != githubListMaxLimit {
			t.Fatalf("%s: limit schema = %+v, want integer in [1, %d]", op, l, githubListMaxLimit)
		}
		args, _ := TopLevelArgsForOperation(op)
		if !containsString(args, "limit") {
			t.Fatalf("%s: catalog args %v missing limit", op, args)
		}
	}
}

func containsString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func TestFindAgenticReviewCommentFollowsLinkCursor(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "tok")
	var posted, patched bool
	var api *httptest.Server
	api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/9/comments":
			if r.URL.Query().Get("after") == "cur1" {
				_, _ = w.Write([]byte(`[{"id":77,"body":"old ` + AgenticReviewMarker + `"}]`))
				return
			}
			// A short first page with a cursor next link: the old page=N / len<per_page
			// loop stopped here and created a duplicate.
			w.Header().Set("Link", fmt.Sprintf(`<%s/repos/o/r/issues/9/comments?per_page=100&after=cur1>; rel="next"`, api.URL))
			_, _ = w.Write([]byte(`[{"id":1,"body":"unrelated"}]`))
		case r.Method == http.MethodPatch && r.URL.Path == "/repos/o/r/issues/comments/77":
			patched = true
			_, _ = w.Write([]byte(`{"id":77}`))
		case r.Method == http.MethodPost:
			posted = true
			_, _ = w.Write([]byte(`{"id":78}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(api.Close)
	t.Setenv("GITHUB_API_URL", api.URL)

	out, _, err := NewRegistry().Dispatch(context.Background(), "pull_request.post_comment", map[string]any{
		"owner": "o", "repo": "r", "number": "9", "body": "new review",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !patched || posted || out["updated"] != true {
		t.Fatalf("patched=%v posted=%v updated=%v, want the page-2 marker comment updated", patched, posted, out["updated"])
	}
}

func TestFindAgenticReviewCommentCapIsErrorNotDuplicate(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "tok")
	var gets atomic.Int32
	var posted atomic.Bool
	var api *httptest.Server
	api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posted.Store(true)
			_, _ = w.Write([]byte(`{"id":1}`))
			return
		}
		n := gets.Add(1)
		comments := make([]any, githubListPerPage)
		for i := range comments {
			comments[i] = map[string]any{"id": int(n)*1000 + i, "body": "chatter"}
		}
		w.Header().Set("Link", fmt.Sprintf(`<%s/repos/o/r/issues/4/comments?per_page=100&page=%d>; rel="next"`, api.URL, n+1))
		_ = json.NewEncoder(w).Encode(comments)
	}))
	t.Cleanup(api.Close)
	t.Setenv("GITHUB_API_URL", api.URL)

	_, _, err := NewRegistry().Dispatch(context.Background(), "pull_request.post_comment", map[string]any{
		"owner": "o", "repo": "r", "number": "4", "body": "review",
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("err = %v, want a refusal when the marker search hits the page cap", err)
	}
	if posted.Load() {
		t.Fatal("posted a new comment although earlier comments were never searched")
	}
	if got := gets.Load(); got != githubListMaxPages {
		t.Fatalf("fetched %d comment pages, want %d", got, githubListMaxPages)
	}
}

func TestFindAgenticReviewCommentEndOfListCreates(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "tok")
	var posted bool
	var api *httptest.Server
	api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posted = true
			_, _ = w.Write([]byte(`{"id":5}`))
			return
		}
		if r.URL.Query().Get("page") == "2" {
			_, _ = w.Write([]byte(`[{"id":2,"body":"b"}]`))
			return
		}
		w.Header().Set("Link", fmt.Sprintf(`<%s/repos/o/r/issues/4/comments?page=2>; rel="next"`, api.URL))
		_, _ = w.Write([]byte(`[{"id":1,"body":"a"}]`))
	}))
	t.Cleanup(api.Close)
	t.Setenv("GITHUB_API_URL", api.URL)

	out, _, err := NewRegistry().Dispatch(context.Background(), "pull_request.post_comment", map[string]any{
		"owner": "o", "repo": "r", "number": "4", "body": "review",
	})
	if err != nil || !posted || out["created"] != true {
		t.Fatalf("err=%v posted=%v created=%v, want a new comment once the list truly ends", err, posted, out["created"])
	}
}

func TestGithubRequestIsPathOnly(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "secret-token")
	var hits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(other.Close)
	t.Setenv("GITHUB_API_URL", "http://127.0.0.1:1")
	u, _ := url.Parse(other.URL)
	for _, p := range []string{other.URL + "/repos/o/r", "@" + u.Host + "/repos/o/r", "repos/o/r"} {
		if _, err := githubGET(context.Background(), p, githubAcceptJSON, maxGitHubJSONBody); err == nil || !strings.Contains(err.Error(), "API-relative") {
			t.Fatalf("githubGET(%q): err = %v, want path-only refusal", p, err)
		}
		if _, err := githubPOSTJSON(context.Background(), p, map[string]any{}, maxGitHubJSONBody); err == nil || !strings.Contains(err.Error(), "API-relative") {
			t.Fatalf("githubPOSTJSON(%q): err = %v, want path-only refusal", p, err)
		}
	}
	if _, _, err := githubGETURL(context.Background(), githubVettedURL{}, githubAcceptJSON, maxGitHubJSONBody); err == nil {
		t.Fatal("githubGETURL accepted an unvetted (zero) URL")
	}
	if hits.Load() != 0 {
		t.Fatal("a request reached a server other than GITHUB_API_URL")
	}
}

func TestGithubRedirectPolicy(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "secret-token")
	var mu sync.Mutex
	var plainAuth []string
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		plainAuth = append(plainAuth, r.Header.Get("Authorization"))
		mu.Unlock()
		_, _ = w.Write([]byte(`{"number":1}`))
	}))
	t.Cleanup(plain.Close)
	var sameOriginAuth string
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/issues/1": // same host, downgraded scheme and different port
			http.Redirect(w, r, plain.URL+"/repos/o/r/issues/1", http.StatusFound)
		case "/repos/o/r/issues/2": // renamed repo: same-origin redirect
			http.Redirect(w, r, "/repositories/42/issues/2", http.StatusMovedPermanently)
		case "/repositories/42/issues/2":
			mu.Lock()
			sameOriginAuth = r.Header.Get("Authorization")
			mu.Unlock()
			_, _ = w.Write([]byte(`{"number":2}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(tlsSrv.Close)
	t.Setenv("GITHUB_API_URL", tlsSrv.URL)
	orig := defaultGitHubHTTPClient
	defaultGitHubHTTPClient = func() *http.Client { return newGitHubHTTPClient(tlsSrv.Client().Transport) }
	t.Cleanup(func() { defaultGitHubHTTPClient = orig })

	_, err := githubGET(context.Background(), "/repos/o/r/issues/1", githubAcceptJSON, maxGitHubJSONBody)
	if err == nil || !strings.Contains(err.Error(), "different origin") || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("err = %v, want a refused cross-origin redirect", err)
	}
	if _, err := githubGET(context.Background(), "/repos/o/r/issues/2", githubAcceptJSON, maxGitHubJSONBody); err != nil {
		t.Fatalf("same-origin redirect: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(plainAuth) != 0 {
		t.Fatalf("downgraded redirect was followed (auth %v)", plainAuth)
	}
	if sameOriginAuth != "Bearer secret-token" {
		t.Fatalf("same-origin redirect auth = %q", sameOriginAuth)
	}
	if defaultGitHubHTTPClient == nil || orig().CheckRedirect == nil {
		t.Fatal("production GitHub client has no redirect policy")
	}
}
