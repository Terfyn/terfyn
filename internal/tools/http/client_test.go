package httptool

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/Terfyn/terfyn/internal/spec"
)

func TestExecute_httptest_GET_success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/ping" {
			t.Errorf("got %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"n":1}`))
	}))
	defer srv.Close()

	out, meta, err := Execute(context.Background(), &spec.ToolHTTP{BaseURL: srv.URL}, nil, "get.ping", nil, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if meta.DurationMs < 0 {
		t.Fatalf("meta %+v", meta)
	}
	if out["ok"] != true || out["n"] != int64(1) {
		t.Fatalf("output %+v", out)
	}
}

func TestExecute_header_envResolution(t *testing.T) {
	t.Setenv("HTTPTOOL_TEST_SECRET", "s3cr3t")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Auth") != "s3cr3t" {
			http.Error(w, "auth", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"auth":"ok"}`))
	}))
	defer srv.Close()

	out, _, err := Execute(context.Background(), &spec.ToolHTTP{
		BaseURL: srv.URL,
		Headers: map[string]string{"X-Auth": "env:HTTPTOOL_TEST_SECRET"},
	}, nil, "get.data", nil, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if out["auth"] != "ok" {
		t.Fatalf("%+v", out)
	}
}

func TestExecute_4xx_notRetried(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "missing", http.StatusNotFound)
	}))
	defer srv.Close()

	_, _, err := Execute(context.Background(), &spec.ToolHTTP{BaseURL: srv.URL}, &spec.ToolRetry{
		MaxAttempts: 3,
		Backoff:     "fixed",
	}, "get.missing", nil, srv.Client())
	if err == nil {
		t.Fatal("expected error")
	}
	if calls.Load() != 1 {
		t.Fatalf("want 1 HTTP request on 404, got %d", calls.Load())
	}
}

func TestExecute_5xx_retried(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"recovered":true}`))
	}))
	defer srv.Close()

	out, _, err := Execute(context.Background(), &spec.ToolHTTP{BaseURL: srv.URL}, &spec.ToolRetry{
		MaxAttempts: 2,
		Backoff:     "fixed",
	}, "get.stable", nil, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("want 2 attempts, got %d", calls.Load())
	}
	if out["recovered"] != true {
		t.Fatalf("%+v", out)
	}
}

func TestParseOperation_postPath(t *testing.T) {
	m, p, err := parseOperation("post.api.v1.items")
	if err != nil {
		t.Fatal(err)
	}
	if m != "POST" || p != "/api/v1/items" {
		t.Fatalf("%s %s", m, p)
	}
}

// TestExecute_GET_encodesWithAsQuery proves GET with-args are sent as query
// parameters rather than silently dropped (#384).
func TestExecute_GET_encodesWithAsQuery(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	_, _, err := Execute(context.Background(), &spec.ToolHTTP{BaseURL: srv.URL}, nil, "get.users",
		map[string]any{"id": "42", "q": "x y", "limit": float64(10), "active": true}, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if gotQuery == "" {
		t.Fatal("GET dropped with-args: no query string sent")
	}
	q, err := url.ParseQuery(gotQuery)
	if err != nil {
		t.Fatal(err)
	}
	if q.Get("id") != "42" || q.Get("q") != "x y" || q.Get("limit") != "10" || q.Get("active") != "true" {
		t.Fatalf("query %q missing/incorrect params: %v", gotQuery, q)
	}
}

// TestExecute_DELETE_encodesWithAsQuery proves DELETE with-args are also sent as
// query parameters (#384).
func TestExecute_DELETE_encodesWithAsQuery(t *testing.T) {
	var gotQuery string
	var gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	_, _, err := Execute(context.Background(), &spec.ToolHTTP{BaseURL: srv.URL}, nil, "delete.session",
		map[string]any{"token": "abc"}, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != "DELETE" {
		t.Fatalf("method %q", gotMethod)
	}
	if q, _ := url.ParseQuery(gotQuery); q.Get("token") != "abc" {
		t.Fatalf("DELETE dropped with-args: query=%q", gotQuery)
	}
}

// TestExecute_GET_rejectsNestedWith proves a nested with-arg is rejected rather
// than silently flattened or dropped on a body-less method (#384).
func TestExecute_GET_rejectsNestedWith(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("server must not be reached; request should fail before send")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	_, _, err := Execute(context.Background(), &spec.ToolHTTP{BaseURL: srv.URL}, nil, "get.users",
		map[string]any{"filter": map[string]any{"role": "admin"}}, srv.Client())
	if err == nil {
		t.Fatal("expected error for nested query value, got nil")
	}
}

func TestDecodeResponseBody_IntegersAbove2p53AreExact(t *testing.T) {
	out, err := decodeResponseBody([]byte(`{"id":9007199254740993,"x":1.5}`), "application/json")
	if err != nil {
		t.Fatal(err)
	}
	if out["id"] != int64(9007199254740993) || out["x"] != 1.5 {
		t.Fatalf("decoded %#v", out)
	}
}
