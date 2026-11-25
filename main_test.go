package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// fake solver: records what proxio forwarded, answers like the real one.
func fakeSolver(t *testing.T, got *map[string]string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		json.NewDecoder(r.Body).Decode(&in)
		*got = in
		json.NewEncoder(w).Encode(map[string]any{"token": "tok-1", "elapsed": 1.5})
	}))
}

func TestClientRedirectRewrite(t *testing.T) {
	base, _ := url.Parse("https://community.example/libs/clogin.php")
	cases := map[string]string{
		`<meta http-equiv="refresh" content="0;url=/portal/index.php">`:    `/p/https/community.example/portal/index.php`,
		`<script>location.href='/portal/index.php'</script>`:               `/p/https/community.example/portal/index.php`,
		`<script>window.location = "https://community.example/x"</script>`: `/p/https/community.example/x`,
		`<script>top.location.replace('/y')</script>`:                      `/p/https/community.example/y`,
	}
	for in, want := range cases {
		if out := string(rewriteHTML([]byte(in), base)); !strings.Contains(out, want) {
			t.Fatalf("%s -> %s, mau %s", in, out, want)
		}
	}
	if got := rewriteRefresh("0; url=/a", base); got != "0; url=/p/https/community.example/a" {
		t.Fatalf("Refresh header: %s", got)
	}
	// JS bundle redirects get mapped too
	if got := string(rewriteJS([]byte(`location.href="/a"`), base)); got != `location.href="/p/https/community.example/a"` {
		t.Fatalf("rewriteJS: %s", got)
	}
}

func TestTurnstileSolve(t *testing.T) {
	var got map[string]string
	srv := fakeSolver(t, &got)
	defer srv.Close()
	turnstileSolver = srv.URL
	defer func() { turnstileSolver = "" }()

	rec := httptest.NewRecorder()
	turnstileSolve(rec, httptest.NewRequest("POST", "/__turnstile/solve",
		strings.NewReader(`{"sitekey":"0xK","siteurl":"https://a.example/x"}`)))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if got["sitekey"] != "0xK" || got["siteurl"] != "https://a.example/x" {
		t.Fatalf("solver menerima %v", got)
	}
	if !strings.Contains(rec.Body.String(), `"token":"tok-1"`) {
		t.Fatalf("token tidak diteruskan: %s", rec.Body)
	}
}

func TestTurnstilePage(t *testing.T) {
	base, _ := url.Parse("https://a.example/x")
	turnstileSolver = "http://solver"
	defer func() { turnstileSolver = "" }()

	withWidget := []byte(`<html><head></head><body><div class="cf-turnstile" data-sitekey="0xK"></div></body></html>`)
	if out := string(turnstilePage(withWidget, base)); !strings.Contains(out, "turnstile/v0/api.js") {
		t.Fatalf("shim tidak disuntik: %s", out)
	}
	selfLoaded := []byte(`<div class="cf-turnstile" data-sitekey="0xK"></div><script src="https://challenges.cloudflare.com/turnstile/v0/api.js"></script>`)
	if out := string(turnstilePage(selfLoaded, base)); out != string(selfLoaded) {
		t.Fatalf("halaman yang memuat api.js sendiri diubah: %s", out)
	}
	if out := string(turnstilePage([]byte("<p>tanpa widget</p>"), base)); out != "<p>tanpa widget</p>" {
		t.Fatalf("halaman biasa diubah: %s", out)
	}
}
