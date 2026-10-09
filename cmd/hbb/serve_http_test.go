package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func fakeScore(_ context.Context, r *serveRequest) serveResponse {
	return serveResponse{Path: r.Path, Lang: "c", Lines: 1,
		Windows: []serveWindow{{From: 1, To: 1, P: map[string]float64{"cwe_79": 0.5}}}}
}

func do(t *testing.T, h http.Handler, method, path, body, auth string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	b, _ := io.ReadAll(rec.Result().Body)
	return rec.Code, string(b)
}

func TestHTTPScoreAndInfo(t *testing.T) {
	h := (&httpListen{maxBody: 1 << 10}).handler(map[string]any{"ready": true, "model": "m"}, fakeScore)
	code, body := do(t, h, "GET", "/v1/info", "", "")
	if code != 200 || !strings.Contains(body, `"model":"m"`) || !strings.Contains(body, `"protocol":1`) ||
		!strings.Contains(body, `"max_body":1024`) {
		t.Fatalf("info: %d %s", code, body)
	}
	code, body = do(t, h, "POST", "/v1/score", `{"id":7,"path":"a.c","text":"int x;"}`, "")
	if code != 200 || !strings.Contains(body, `"id":7`) || !strings.Contains(body, `"cwe_79":0.5`) {
		t.Fatalf("score: %d %s", code, body)
	}
}

func TestHTTPRefusesFileAndBigBodies(t *testing.T) {
	h := (&httpListen{maxBody: 64}).handler(map[string]any{}, fakeScore)
	if code, _ := do(t, h, "POST", "/v1/score", `{"path":"a.c","file":"/etc/passwd"}`, ""); code != 400 {
		t.Fatalf("file: %d", code)
	}
	if code, _ := do(t, h, "POST", "/v1/score", `{"path":"a.c","text":"`+strings.Repeat("x", 200)+`"}`, ""); code != 413 {
		t.Fatalf("big: %d", code)
	}
	if code, _ := do(t, h, "POST", "/v1/score", `nope`, ""); code != 400 {
		t.Fatalf("bad json: %d", code)
	}
}

func TestHTTPToken(t *testing.T) {
	h := (&httpListen{maxBody: 1 << 10, token: "s3cret"}).handler(map[string]any{}, fakeScore)
	if code, _ := do(t, h, "GET", "/v1/info", "", ""); code != 401 {
		t.Fatalf("no token: %d", code)
	}
	if code, _ := do(t, h, "GET", "/v1/info", "", "Bearer nope"); code != 401 {
		t.Fatalf("wrong token: %d", code)
	}
	if code, _ := do(t, h, "GET", "/v1/info", "", "Bearer s3cret"); code != 200 {
		t.Fatalf("right token: %d", code)
	}
	if code, _ := do(t, h, "GET", "/healthz", "", ""); code != 200 {
		t.Fatalf("healthz: %d", code)
	}
}

func TestListenNeedsTokenOffLoopback(t *testing.T) {
	t.Setenv("HBB_SERVE_TOKEN", "")
	for addr, ok := range map[string]bool{"127.0.0.1:1": true, "[::1]:1": true, "localhost:1": true, ":8140": false, "0.0.0.0:1": false} {
		h := &httpListen{addr: addr, maxBody: 1}
		if err := h.check(); (err == nil) != ok {
			t.Errorf("%s: err=%v, want ok=%v", addr, err, ok)
		}
	}
	t.Setenv("HBB_SERVE_TOKEN", "x")
	if err := (&httpListen{addr: ":8140", maxBody: 1}).check(); err != nil {
		t.Errorf("with token: %v", err)
	}
}
