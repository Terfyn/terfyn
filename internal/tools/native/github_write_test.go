package native

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// githubStub starts a stub GitHub API that records the last request and replies with respBody.
type githubStub struct {
	method  string
	path    string
	body    map[string]any
	rawBody string
}

func newGitHubStub(t *testing.T, status int, respBody string) *githubStub {
	t.Helper()
	s := &githubStub{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.method = r.Method
		s.path = r.URL.Path
		if b, _ := io.ReadAll(r.Body); len(b) > 0 {
			s.rawBody = string(b)
			_ = json.Unmarshal(b, &s.body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, respBody)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("GITHUB_TOKEN", "tok")
	t.Setenv("GITHUB_API_URL", srv.URL)
	return s
}

func TestGithubIssuesCreate_happyPath(t *testing.T) {
	stub := newGitHubStub(t, 201, `{"number":42,"id":9001,"html_url":"https://gh/x/42","state":"open","extra":"dropped"}`)
	out, err := githubIssuesCreate(context.Background(), map[string]any{
		"owner": "acme", "repo": "api", "title": "Bug", "body": "details",
	})
	if err != nil {
		t.Fatal(err)
	}
	if stub.method != http.MethodPost || stub.path != "/repos/acme/api/issues" {
		t.Fatalf("request %s %s", stub.method, stub.path)
	}
	if stub.body["title"] != "Bug" || stub.body["body"] != "details" {
		t.Fatalf("payload %#v", stub.body)
	}
	// Result is the curated subset — number/id/html_url/state, not the whole object.
	if out["number"] != float64(42) || out["state"] != "open" {
		t.Fatalf("out %#v", out)
	}
	if _, leaked := out["extra"]; leaked {
		t.Fatalf("unexpected field leaked into result: %#v", out)
	}
}

func TestGithubIssuesCreate_missingTitle(t *testing.T) {
	newGitHubStub(t, 201, `{}`)
	if _, err := githubIssuesCreate(context.Background(), map[string]any{"owner": "a", "repo": "b"}); err == nil {
		t.Fatal("expected an error for a missing title")
	}
}

func TestGithubIssuesComment_happyPath(t *testing.T) {
	stub := newGitHubStub(t, 201, `{"id":7,"html_url":"https://gh/c/7"}`)
	out, err := githubIssuesComment(context.Background(), map[string]any{
		"owner": "acme", "repo": "api", "number": float64(42), "body": "hi",
	})
	if err != nil {
		t.Fatal(err)
	}
	if stub.path != "/repos/acme/api/issues/42/comments" || stub.body["body"] != "hi" {
		t.Fatalf("request %s payload %#v", stub.path, stub.body)
	}
	if out["id"] != float64(7) {
		t.Fatalf("out %#v", out)
	}
}

func TestGithubCreateReview_eventValidationAndBody(t *testing.T) {
	// APPROVE needs no body.
	stub := newGitHubStub(t, 200, `{"id":5,"state":"APPROVED"}`)
	if _, err := githubPullRequestCreateReview(context.Background(), map[string]any{
		"owner": "a", "repo": "b", "number": float64(3), "event": "approve",
	}); err != nil {
		t.Fatal(err)
	}
	if stub.body["event"] != "APPROVE" {
		t.Fatalf("event should be upper-cased: %#v", stub.body)
	}
	if _, hasBody := stub.body["body"]; hasBody {
		t.Fatalf("no body should be sent for APPROVE: %#v", stub.body)
	}

	// REQUEST_CHANGES requires a body.
	newGitHubStub(t, 200, `{"id":6,"state":"CHANGES_REQUESTED"}`)
	if _, err := githubPullRequestCreateReview(context.Background(), map[string]any{
		"owner": "a", "repo": "b", "number": float64(3), "event": "REQUEST_CHANGES",
	}); err == nil {
		t.Fatal("REQUEST_CHANGES without a body should error")
	}

	// An unknown event is rejected.
	newGitHubStub(t, 200, `{}`)
	if _, err := githubPullRequestCreateReview(context.Background(), map[string]any{
		"owner": "a", "repo": "b", "number": float64(3), "event": "MERGE",
	}); err == nil {
		t.Fatal("an unknown event should error")
	}
}

func TestGithubCommitStatusCreate_happyPathAndState(t *testing.T) {
	stub := newGitHubStub(t, 201, `{"id":11,"state":"success","context":"ci"}`)
	out, err := githubCommitStatusCreate(context.Background(), map[string]any{
		"owner": "a", "repo": "b", "sha": "abc123", "state": "SUCCESS",
		"context": "ci", "description": "all green", "target_url": "https://ci/run/1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if stub.path != "/repos/a/b/statuses/abc123" {
		t.Fatalf("path %s", stub.path)
	}
	if stub.body["state"] != "success" || stub.body["context"] != "ci" || stub.body["target_url"] != "https://ci/run/1" {
		t.Fatalf("payload %#v", stub.body)
	}
	if out["state"] != "success" {
		t.Fatalf("out %#v", out)
	}

	// An invalid state is rejected before any request.
	newGitHubStub(t, 201, `{}`)
	if _, err := githubCommitStatusCreate(context.Background(), map[string]any{
		"owner": "a", "repo": "b", "sha": "abc", "state": "green",
	}); err == nil {
		t.Fatal("an invalid state should error")
	}
}

func TestGithubWrite_non2xxIsError(t *testing.T) {
	newGitHubStub(t, 422, `{"message":"Validation Failed"}`)
	_, err := githubIssuesCreate(context.Background(), map[string]any{"owner": "a", "repo": "b", "title": "x"})
	if err == nil || !strings.Contains(err.Error(), "422") {
		t.Fatalf("expected a 422 error, got %v", err)
	}
}

func TestGithubWrite_requiresToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	if _, err := githubIssuesCreate(context.Background(), map[string]any{"owner": "a", "repo": "b", "title": "x"}); err == nil || !strings.Contains(err.Error(), "GITHUB_TOKEN") {
		t.Fatalf("expected a GITHUB_TOKEN error, got %v", err)
	}
}

func TestMutablePatchCanClearBody(t *testing.T) {
	got, err := githubMutablePatch(map[string]any{"body": ""}, "body")
	if err != nil {
		t.Fatal(err)
	}
	if body, ok := got["body"]; !ok || body != "" {
		t.Fatalf("githubMutablePatch omitted explicit empty body: %#v", got)
	}
}

func TestGithubIssuesUpdate_bodyHandling(t *testing.T) {
	t.Run("clear_body_only", func(t *testing.T) {
		stub := newGitHubStub(t, 200, `{"number":42,"id":9001,"html_url":"https://gh/x/42","state":"open"}`)
		out, err := githubIssuesUpdate(context.Background(), map[string]any{
			"owner": "acme", "repo": "api", "number": float64(42), "body": "",
		})
		if err != nil {
			t.Fatal(err)
		}
		if stub.method != http.MethodPatch || stub.path != "/repos/acme/api/issues/42" {
			t.Fatalf("request %s %s", stub.method, stub.path)
		}
		if body, ok := stub.body["body"]; !ok || body != "" {
			t.Fatalf("expected empty body in payload, got %#v", stub.body)
		}
		if !strings.Contains(stub.rawBody, `"body":""`) {
			t.Fatalf("expected exact `\"body\":\"\"` in raw json, got %s", stub.rawBody)
		}
		if out["number"] != float64(42) {
			t.Fatalf("out %#v", out)
		}
	})

	t.Run("body_and_title", func(t *testing.T) {
		stub := newGitHubStub(t, 200, `{"number":42,"id":9001,"html_url":"https://gh/x/42","state":"open"}`)
		_, err := githubIssuesUpdate(context.Background(), map[string]any{
			"owner": "acme", "repo": "api", "number": float64(42), "title": "Updated Title", "body": "",
		})
		if err != nil {
			t.Fatal(err)
		}
		if stub.method != http.MethodPatch || stub.path != "/repos/acme/api/issues/42" {
			t.Fatalf("request %s %s", stub.method, stub.path)
		}
		if stub.body["title"] != "Updated Title" {
			t.Fatalf("expected updated title, got %#v", stub.body)
		}
		if body, ok := stub.body["body"]; !ok || body != "" {
			t.Fatalf("expected empty body in payload, got %#v", stub.body)
		}
		if !strings.Contains(stub.rawBody, `"body":""`) {
			t.Fatalf("expected exact `\"body\":\"\"` in raw json, got %s", stub.rawBody)
		}
	})

	t.Run("omitted_body", func(t *testing.T) {
		stub := newGitHubStub(t, 200, `{"number":42,"id":9001,"html_url":"https://gh/x/42","state":"open"}`)
		_, err := githubIssuesUpdate(context.Background(), map[string]any{
			"owner": "acme", "repo": "api", "number": float64(42), "title": "Updated Title",
		})
		if err != nil {
			t.Fatal(err)
		}
		if stub.method != http.MethodPatch || stub.path != "/repos/acme/api/issues/42" {
			t.Fatalf("request %s %s", stub.method, stub.path)
		}
		if _, ok := stub.body["body"]; ok {
			t.Fatalf("body should be omitted, got %#v", stub.body)
		}
		if strings.Contains(stub.rawBody, `"body"`) {
			t.Fatalf("expected body omitted in raw json, got %s", stub.rawBody)
		}
	})

	t.Run("whitespace_only_body", func(t *testing.T) {
		stub := newGitHubStub(t, 200, `{"number":42,"id":9001,"html_url":"https://gh/x/42","state":"open"}`)
		_, err := githubIssuesUpdate(context.Background(), map[string]any{
			"owner": "acme", "repo": "api", "number": float64(42), "body": "   \n  ",
		})
		if err != nil {
			t.Fatal(err)
		}
		if stub.method != http.MethodPatch || stub.path != "/repos/acme/api/issues/42" {
			t.Fatalf("request %s %s", stub.method, stub.path)
		}
		if stub.body["body"] != "   \n  " {
			t.Fatalf("expected whitespace-only body preserved, got %#v", stub.body)
		}
	})

	t.Run("preserve_nonempty_body", func(t *testing.T) {
		stub := newGitHubStub(t, 200, `{"number":42,"id":9001,"html_url":"https://gh/x/42","state":"open"}`)
		_, err := githubIssuesUpdate(context.Background(), map[string]any{
			"owner": "acme", "repo": "api", "number": float64(42), "body": "  preserved body  ",
		})
		if err != nil {
			t.Fatal(err)
		}
		if stub.method != http.MethodPatch || stub.path != "/repos/acme/api/issues/42" {
			t.Fatalf("request %s %s", stub.method, stub.path)
		}
		if stub.body["body"] != "  preserved body  " {
			t.Fatalf("expected body preserved verbatim, got %#v", stub.body)
		}
	})

	t.Run("reject_empty_title", func(t *testing.T) {
		newGitHubStub(t, 200, `{}`)
		_, err := githubIssuesUpdate(context.Background(), map[string]any{
			"owner": "acme", "repo": "api", "number": float64(42), "title": "",
		})
		if err == nil || !strings.Contains(err.Error(), "requires one of") {
			t.Fatalf("expected requires one of error, got %v", err)
		}
	})

	t.Run("empty_title_with_valid_body", func(t *testing.T) {
		stub := newGitHubStub(t, 200, `{"number":42,"id":9001,"html_url":"https://gh/x/42","state":"open"}`)
		_, err := githubIssuesUpdate(context.Background(), map[string]any{
			"owner": "acme", "repo": "api", "number": float64(42), "title": "", "body": "cleared or set",
		})
		if err != nil {
			t.Fatal(err)
		}
		if stub.method != http.MethodPatch || stub.path != "/repos/acme/api/issues/42" {
			t.Fatalf("request %s %s", stub.method, stub.path)
		}
		if _, ok := stub.body["title"]; ok {
			t.Fatalf("empty title must not be sent in payload: %#v", stub.body)
		}
		if stub.body["body"] != "cleared or set" {
			t.Fatalf("expected body in payload, got %#v", stub.body)
		}
	})
}

func TestGithubPullRequestUpdate_bodyHandling(t *testing.T) {
	t.Run("clear_body_only", func(t *testing.T) {
		stub := newGitHubStub(t, 200, `{"number":101,"id":9002,"html_url":"https://gh/x/101","state":"open"}`)
		out, err := githubPullRequestUpdate(context.Background(), map[string]any{
			"owner": "acme", "repo": "api", "number": float64(101), "body": "",
		})
		if err != nil {
			t.Fatal(err)
		}
		if stub.method != http.MethodPatch || stub.path != "/repos/acme/api/pulls/101" {
			t.Fatalf("request %s %s", stub.method, stub.path)
		}
		if body, ok := stub.body["body"]; !ok || body != "" {
			t.Fatalf("expected empty body in payload, got %#v", stub.body)
		}
		if !strings.Contains(stub.rawBody, `"body":""`) {
			t.Fatalf("expected exact `\"body\":\"\"` in raw json, got %s", stub.rawBody)
		}
		if out["number"] != float64(101) {
			t.Fatalf("out %#v", out)
		}
	})

	t.Run("body_and_title", func(t *testing.T) {
		stub := newGitHubStub(t, 200, `{"number":101,"id":9002,"html_url":"https://gh/x/101","state":"open"}`)
		_, err := githubPullRequestUpdate(context.Background(), map[string]any{
			"owner": "acme", "repo": "api", "number": float64(101), "title": "New PR Title", "body": "",
		})
		if err != nil {
			t.Fatal(err)
		}
		if stub.method != http.MethodPatch || stub.path != "/repos/acme/api/pulls/101" {
			t.Fatalf("request %s %s", stub.method, stub.path)
		}
		if stub.body["title"] != "New PR Title" {
			t.Fatalf("expected updated title, got %#v", stub.body)
		}
		if body, ok := stub.body["body"]; !ok || body != "" {
			t.Fatalf("expected empty body in payload, got %#v", stub.body)
		}
		if !strings.Contains(stub.rawBody, `"body":""`) {
			t.Fatalf("expected exact `\"body\":\"\"` in raw json, got %s", stub.rawBody)
		}
	})

	t.Run("omitted_body", func(t *testing.T) {
		stub := newGitHubStub(t, 200, `{"number":101,"id":9002,"html_url":"https://gh/x/101","state":"closed"}`)
		_, err := githubPullRequestUpdate(context.Background(), map[string]any{
			"owner": "acme", "repo": "api", "number": float64(101), "state": "closed",
		})
		if err != nil {
			t.Fatal(err)
		}
		if stub.method != http.MethodPatch || stub.path != "/repos/acme/api/pulls/101" {
			t.Fatalf("request %s %s", stub.method, stub.path)
		}
		if _, ok := stub.body["body"]; ok {
			t.Fatalf("body should be omitted, got %#v", stub.body)
		}
		if strings.Contains(stub.rawBody, `"body"`) {
			t.Fatalf("expected body omitted in raw json, got %s", stub.rawBody)
		}
	})

	t.Run("whitespace_only_body", func(t *testing.T) {
		stub := newGitHubStub(t, 200, `{"number":101,"id":9002,"html_url":"https://gh/x/101","state":"open"}`)
		_, err := githubPullRequestUpdate(context.Background(), map[string]any{
			"owner": "acme", "repo": "api", "number": float64(101), "body": "   \t  ",
		})
		if err != nil {
			t.Fatal(err)
		}
		if stub.method != http.MethodPatch || stub.path != "/repos/acme/api/pulls/101" {
			t.Fatalf("request %s %s", stub.method, stub.path)
		}
		if stub.body["body"] != "   \t  " {
			t.Fatalf("expected whitespace-only body preserved, got %#v", stub.body)
		}
	})

	t.Run("preserve_nonempty_body", func(t *testing.T) {
		stub := newGitHubStub(t, 200, `{"number":101,"id":9002,"html_url":"https://gh/x/101","state":"open"}`)
		_, err := githubPullRequestUpdate(context.Background(), map[string]any{
			"owner": "acme", "repo": "api", "number": float64(101), "body": "  kept body  ",
		})
		if err != nil {
			t.Fatal(err)
		}
		if stub.method != http.MethodPatch || stub.path != "/repos/acme/api/pulls/101" {
			t.Fatalf("request %s %s", stub.method, stub.path)
		}
		if stub.body["body"] != "  kept body  " {
			t.Fatalf("expected body preserved verbatim, got %#v", stub.body)
		}
	})

	t.Run("reject_empty_title", func(t *testing.T) {
		newGitHubStub(t, 200, `{}`)
		_, err := githubPullRequestUpdate(context.Background(), map[string]any{
			"owner": "acme", "repo": "api", "number": float64(101), "title": "",
		})
		if err == nil || !strings.Contains(err.Error(), "requires one of") {
			t.Fatalf("expected requires one of error, got %v", err)
		}
	})

	t.Run("empty_title_with_valid_body", func(t *testing.T) {
		stub := newGitHubStub(t, 200, `{"number":101,"id":9002,"html_url":"https://gh/x/101","state":"open"}`)
		_, err := githubPullRequestUpdate(context.Background(), map[string]any{
			"owner": "acme", "repo": "api", "number": float64(101), "title": "", "body": "cleared or set",
		})
		if err != nil {
			t.Fatal(err)
		}
		if stub.method != http.MethodPatch || stub.path != "/repos/acme/api/pulls/101" {
			t.Fatalf("request %s %s", stub.method, stub.path)
		}
		if _, ok := stub.body["title"]; ok {
			t.Fatalf("empty title must not be sent in payload: %#v", stub.body)
		}
		if stub.body["body"] != "cleared or set" {
			t.Fatalf("expected body in payload, got %#v", stub.body)
		}
	})
}

func TestGithubCreateOperations_bodyBehaviorUnchanged(t *testing.T) {
	t.Run("issues_create_omits_empty_body", func(t *testing.T) {
		stub := newGitHubStub(t, 201, `{"number":1,"id":10,"html_url":"https://gh/1","state":"open"}`)
		_, err := githubIssuesCreate(context.Background(), map[string]any{
			"owner": "acme", "repo": "api", "title": "Valid Title", "body": "",
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := stub.body["body"]; ok {
			t.Fatalf("expected body omitted in create, got %#v", stub.body)
		}
	})

	t.Run("issues_create_omits_whitespace_body", func(t *testing.T) {
		stub := newGitHubStub(t, 201, `{"number":1,"id":10,"html_url":"https://gh/1","state":"open"}`)
		_, err := githubIssuesCreate(context.Background(), map[string]any{
			"owner": "acme", "repo": "api", "title": "Valid Title", "body": "   ",
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := stub.body["body"]; ok {
			t.Fatalf("expected whitespace body omitted in create, got %#v", stub.body)
		}
	})

	t.Run("issues_create_preserves_nonempty_body", func(t *testing.T) {
		stub := newGitHubStub(t, 201, `{"number":1,"id":10,"html_url":"https://gh/1","state":"open"}`)
		_, err := githubIssuesCreate(context.Background(), map[string]any{
			"owner": "acme", "repo": "api", "title": "Valid Title", "body": "my issue details",
		})
		if err != nil {
			t.Fatal(err)
		}
		if stub.body["body"] != "my issue details" {
			t.Fatalf("expected body set in create, got %#v", stub.body)
		}
	})

	t.Run("issues_create_rejects_empty_title", func(t *testing.T) {
		newGitHubStub(t, 201, `{}`)
		_, err := githubIssuesCreate(context.Background(), map[string]any{
			"owner": "acme", "repo": "api", "title": "",
		})
		if err == nil {
			t.Fatal("expected error for empty title in issues.create")
		}
	})

	t.Run("pull_request_create_omits_empty_body", func(t *testing.T) {
		stub := newGitHubStub(t, 201, `{"number":2,"id":20,"html_url":"https://gh/2","state":"open"}`)
		_, err := githubPullRequestCreate(context.Background(), map[string]any{
			"owner": "acme", "repo": "api", "head": "feature", "base": "main", "title": "PR Title", "body": "",
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := stub.body["body"]; ok {
			t.Fatalf("expected body omitted in PR create, got %#v", stub.body)
		}
	})

	t.Run("pull_request_create_omits_whitespace_body", func(t *testing.T) {
		stub := newGitHubStub(t, 201, `{"number":2,"id":20,"html_url":"https://gh/2","state":"open"}`)
		_, err := githubPullRequestCreate(context.Background(), map[string]any{
			"owner": "acme", "repo": "api", "head": "feature", "base": "main", "title": "PR Title", "body": "   \n\t",
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := stub.body["body"]; ok {
			t.Fatalf("expected whitespace body omitted in PR create, got %#v", stub.body)
		}
	})

	t.Run("pull_request_create_rejects_empty_title_without_issue", func(t *testing.T) {
		newGitHubStub(t, 201, `{}`)
		_, err := githubPullRequestCreate(context.Background(), map[string]any{
			"owner": "acme", "repo": "api", "head": "feature", "base": "main", "title": "",
		})
		if err == nil {
			t.Fatal("expected error for empty title in pull_request.create without issue")
		}
	})
}

func TestGithubUpdateRejectsMalformedBody(t *testing.T) {
	updates := []struct {
		name   string
		number float64
		run    func(context.Context, map[string]any) (map[string]any, error)
	}{
		{"issues", 42, githubIssuesUpdate},
		{"pull_request", 101, githubPullRequestUpdate},
	}

	for _, update := range updates {
		t.Run(update.name, func(t *testing.T) {
			values := []struct {
				name  string
				value any
			}{
				{"object", map[string]any{"nested": "object"}},
				{"array", []any{1, 2, 3}},
			}

			for _, value := range values {
				for _, withTitle := range []bool{false, true} {
					name := value.name + "/body_only"
					if withTitle {
						name = value.name + "/with_title"
					}

					t.Run(name, func(t *testing.T) {
						stub := newGitHubStub(t, 200, `{}`)
						input := map[string]any{
							"owner": "acme", "repo": "api",
							"number": update.number, "body": value.value,
						}
						if withTitle {
							input["title"] = "Valid title"
						}

						_, err := update.run(context.Background(), input)
						if err == nil || !strings.Contains(err.Error(), `field "body"`) {
							t.Fatalf("expected explicit body validation error, got %v", err)
						}
						if stub.method != "" {
							t.Fatalf("malformed body triggered HTTP request: %s %s", stub.method, stub.path)
						}
					})
				}
			}
		})
	}
}
