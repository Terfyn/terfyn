package native

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// GitHub/Slack result bodies are decoded with jsonnum (docs/SOUNDNESS.md S7), so
// an integer past 2^53 reaches the step output exactly (an int64), not rounded
// through float64. With encoding/json, 9007199254740993 became 9007199254740992.
func TestNativeResults_KeepIntegersPast2p53(t *testing.T) {
	const big = int64(9007199254740993)
	const lit = "9007199254740993"
	var patched string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls/1":
			_, _ = io.WriteString(w, `{"id":`+lit+`}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/1":
			_, _ = io.WriteString(w, `{"id":`+lit+`}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues":
			_, _ = io.WriteString(w, `[{"id":`+lit+`}]`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls":
			_, _ = io.WriteString(w, `[{"id":`+lit+`}]`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/commits/abc/check-runs":
			_, _ = io.WriteString(w, `{"total_count":1,"check_runs":[{"id":`+lit+`}]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues":
			_, _ = io.WriteString(w, `{"number":7,"id":`+lit+`}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/2/comments":
			_, _ = io.WriteString(w, `[{"id":`+lit+`,"body":"old `+AgenticReviewMarker+`"}]`)
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/repos/o/r/issues/comments/"):
			patched = strings.TrimPrefix(r.URL.Path, "/repos/o/r/issues/comments/")
			_, _ = io.WriteString(w, `{"id":`+lit+`}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("GITHUB_TOKEN", "tok")
	t.Setenv("GITHUB_API_URL", srv.URL)

	first := func(v any) map[string]any {
		arr, _ := v.([]any)
		if len(arr) == 0 {
			return nil
		}
		m, _ := arr[0].(map[string]any)
		return m
	}
	obj := func(v any) map[string]any { m, _ := v.(map[string]any); return m }
	cases := []struct {
		op   string
		with map[string]any
		id   func(map[string]any) any
	}{
		{"pull_request.get", map[string]any{"owner": "o", "repo": "r", "number": 1}, func(o map[string]any) any { return obj(o["pull_request"])["id"] }},
		{"issues.get", map[string]any{"owner": "o", "repo": "r", "number": 1}, func(o map[string]any) any { return obj(o["issue"])["id"] }},
		{"issues.list", map[string]any{"owner": "o", "repo": "r"}, func(o map[string]any) any { return first(o["issues"])["id"] }},
		{"pull_request.list", map[string]any{"owner": "o", "repo": "r"}, func(o map[string]any) any { return first(o["pull_requests"])["id"] }},
		{"check_runs.list", map[string]any{"owner": "o", "repo": "r", "ref": "abc"}, func(o map[string]any) any { return first(o["check_runs"])["id"] }},
		{"issues.create", map[string]any{"owner": "o", "repo": "r", "title": "t"}, func(o map[string]any) any { return o["id"] }},
		{"pull_request.post_comment", map[string]any{"owner": "o", "repo": "r", "number": 2, "body": "new"}, func(o map[string]any) any { return o["id"] }},
		{"pull_request.fetch", map[string]any{"pr": `{"id":` + lit + `}`}, func(o map[string]any) any { return obj(o["pull_request"])["id"] }},
	}
	reg := NewRegistry()
	for _, tc := range cases {
		out, _, err := reg.Dispatch(context.Background(), tc.op, tc.with)
		if err != nil {
			t.Fatalf("%s: %v", tc.op, err)
		}
		if got := tc.id(out); got != big {
			t.Errorf("%s: id = %T(%v), want int64(%d); out=%#v", tc.op, got, got, big, out)
		}
	}
	// The upsert path finds the existing comment by id and PATCHes it: a float64
	// decode would have addressed comment ...992, a different comment.
	if patched != lit {
		t.Fatalf("post_comment PATCHed comment %q, want %q", patched, lit)
	}
}

func TestSlackResult_DecodedLosslessly(t *testing.T) {
	newSlackStub(t, `{"ok":true,"ts":"1.2","channel":"C1","n":9007199254740993}`)
	out, err := slackCall(context.Background(), "chat.postMessage", map[string]any{"channel": "C1", "text": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if out["n"] != int64(9007199254740993) {
		t.Fatalf("n = %T(%v)", out["n"], out["n"])
	}
}
