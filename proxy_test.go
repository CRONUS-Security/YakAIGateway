package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func testConfig(upstream string) Config {
	return Config{
		Listen:       "127.0.0.1:0",
		Upstream:     upstream,
		APIKey:       "free-user",
		UserAgent:    "",
		UserID:       "11111111-2222-4333-8444-555555555555",
		YakVersion:   "1.4.9-beta6",
		YakBuildTime: "Fri Aug 28 17:23:40 2026 +0800",
		UsageType:    "lightweight",
		MaxBody:      1 << 20,
	}
}

func newTestProxyCfg(t *testing.T, cfg Config) *Proxy {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := &http.Client{Transport: &http.Transport{}}
	totp := NewTOTPManager(cfg.Upstream, client, cfg.UserAgent, cfg.UserID, log)
	return NewProxy(cfg, client, totp, log)
}

func newTestProxy(t *testing.T, upstreamURL string) *Proxy {
	t.Helper()
	return newTestProxyCfg(t, testConfig(upstreamURL))
}

// The gateway must build the upstream request from an allowlist: client junk
// (Cookie, X-Forwarded-For, fake TOTP header, Authorization, User-Agent)
// dropped, overrides passed through, defaults filled in, gateway-owned
// credentials always forced.
func TestChatHeaderFingerprint(t *testing.T) {
	var mu sync.Mutex
	var got http.Header
	var ua string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected upstream path %s", r.URL.Path)
		}
		mu.Lock()
		got = r.Header.Clone()
		ua = r.UserAgent()
		mu.Unlock()
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	gw := httptest.NewServer(newTestProxy(t, upstream.URL).Mux())
	defer gw.Close()

	req, _ := http.NewRequest("POST", gw.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-v3","messages":[]}`))
	req.Header.Set("Authorization", "Bearer my-key") // must be replaced
	req.Header.Set("User-Agent", "some-client/1.0")  // must be replaced
	req.Header.Set("Cookie", "a=b")
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	req.Header.Set("X-Memfit-OTP-Auth", "Z2FyYmFnZQ==")      // must be discarded (non-memfit)
	req.Header.Set("X-User-Id", "99999999-8888-4777-8666-555555555555") // must be discarded
	req.Header.Set("X-Yak-Version", "9.9.9")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}

	mu.Lock()
	defer mu.Unlock()
	checks := []struct{ name, want string }{
		{"Content-Type", "application/json; charset=UTF-8"},
		{"Accept", "application/json"}, // forced to the yaklang client value
		{"Accept-Encoding", "gzip, deflate, br"},
		{"Authorization", "Bearer free-user"},
		{"X-Yak-Version", "1.4.9-beta6"}, // client override is discarded
		{"X-Yak-Build-Time", "Fri Aug 28 17:23:40 2026 +0800"},
		{"X-Yak-AI-Model-Usage-Type", "lightweight"},
		{"X-Memfit-OTP-Auth", ""}, // dropped for non-memfit model
		{"X-User-Id", "11111111-2222-4333-8444-555555555555"},
		{"Cookie", ""}, // client junk dropped
		{"X-Forwarded-For", ""},
	}
	for _, c := range checks {
		if got.Get(c.name) != c.want {
			t.Errorf("upstream %s = %q, want %q", c.name, got.Get(c.name), c.want)
		}
	}
	if ua != "" {
		t.Errorf("upstream saw User-Agent %q, want none", ua)
	}
}

func TestModelsRoute(t *testing.T) {
	var mu sync.Mutex
	var method, auth, ctype string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("unexpected upstream path %s", r.URL.Path)
		}
		mu.Lock()
		method, auth, ctype = r.Method, r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		mu.Unlock()
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer upstream.Close()

	gw := httptest.NewServer(newTestProxy(t, upstream.URL).Mux())
	defer gw.Close()

	resp, err := http.Get(gw.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != `{"object":"list","data":[]}` {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}

	mu.Lock()
	defer mu.Unlock()
	if method != "GET" || auth != "Bearer free-user" || ctype != "" {
		t.Errorf("upstream saw method=%q auth=%q content-type=%q", method, auth, ctype)
	}
}

func TestEmbeddingsRoute(t *testing.T) {
	var mu sync.Mutex
	var otp, ctype, body, seedUA, seedAuth, seedUserID, seedAE string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/memfit-totp-uuid":
			mu.Lock()
			seedUA, seedAuth = r.UserAgent(), r.Header.Get("Authorization")
			seedUserID = r.Header.Get("X-User-Id")
			seedAE = r.Header.Get("Accept-Encoding")
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"uuid":"MEMFIT-AIseed-embed-0001MEMFIT-AI","format":"uuid"}`)
			return
		case "/v1/embeddings":
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			otp, ctype, body = r.Header.Get("X-Memfit-OTP-Auth"), r.Header.Get("Content-Type"), string(b)
			mu.Unlock()
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"data":[{"embedding":[0.1,0.2]}]}`))
			return
		}
		t.Errorf("unexpected upstream path %s", r.URL.Path)
	}))
	defer upstream.Close()

	gw := httptest.NewServer(newTestProxy(t, upstream.URL).Mux())
	defer gw.Close()

	// memfit model: the upstream gates embeddings behind TOTP too, so the
	// gateway must attach its own header.
	resp, err := http.Post(gw.URL+"/v1/embeddings", "application/json",
		strings.NewReader(`{"model":"memfit-light-free","input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("memfit embeddings status %d", resp.StatusCode)
	}
	mu.Lock()
	decoded, err := base64.StdEncoding.DecodeString(otp)
	mu.Unlock()
	if err != nil || len(decoded) != 6 {
		t.Errorf("memfit embeddings TOTP header %q is not base64 of a 6-digit code", otp)
	}
	mu.Lock()
	if seedUA != "" || seedAuth != "" || seedAE != "gzip, deflate, br" || seedUserID != "11111111-2222-4333-8444-555555555555" {
		// Matches yaklang: the TOTP fetch carries the client identity and
		// yakit's Accept-Encoding, but no UA and no Authorization.
		t.Errorf("TOTP seed fetch saw UA=%q auth=%q ae=%q user-id=%q", seedUA, seedAuth, seedAE, seedUserID)
	}
	mu.Unlock()

	// non-memfit model: no TOTP header
	resp, err = http.Post(gw.URL+"/v1/embeddings", "application/json",
		strings.NewReader(`{"model":"text-embedding","input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}

	mu.Lock()
	defer mu.Unlock()
	if otp != "" {
		t.Errorf("non-memfit embeddings must not carry TOTP header, got %q", otp)
	}
	if ctype != "application/json; charset=UTF-8" || body != `{"model":"text-embedding","input":"hi"}` {
		t.Errorf("bad upstream ctype=%q body=%q", ctype, body)
	}
}

// memfit model + 401 TOTP marker → refresh seed, retry exactly once, second
// attempt succeeds. The gateway replaces the client's TOTP header with its own.
func TestMemfitTOTPRetry(t *testing.T) {
	seeds := []string{"seed-aaaa-bbbb-cccc", "seed-dddd-eeee-ffff"}
	var mu sync.Mutex
	seedCalls, chatCalls := 0, 0
	var otpHeaders []string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/memfit-totp-uuid":
			mu.Lock()
			i := seedCalls
			seedCalls++
			mu.Unlock()
			seed := seeds[0]
			if i > 0 {
				seed = seeds[1]
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"uuid":"MEMFIT-AI%sMEMFIT-AI","format":"uuid"}`, seed)
		case "/v1/chat/completions":
			mu.Lock()
			i := chatCalls
			chatCalls++
			otpHeaders = append(otpHeaders, r.Header.Get("X-Memfit-OTP-Auth"))
			mu.Unlock()
			if i == 0 {
				w.WriteHeader(401)
				_, _ = w.Write([]byte(`{"error":{"message":"memfit_totp_auth_failed"}}`))
				return
			}
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
		default:
			t.Errorf("unexpected upstream path %s", r.URL.Path)
		}
	}))
	defer upstream.Close()

	gw := httptest.NewServer(newTestProxy(t, upstream.URL).Mux())
	defer gw.Close()

	req, _ := http.NewRequest("POST", gw.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"memfit-standard-free","messages":[]}`))
	req.Header.Set("X-Memfit-OTP-Auth", "Z2FyYmFnZQ==") // client garbage, must be replaced
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d after retry", resp.StatusCode)
	}

	mu.Lock()
	defer mu.Unlock()
	if chatCalls != 2 {
		t.Fatalf("chat hit %d times, want 2 (one retry)", chatCalls)
	}
	if seedCalls != 2 {
		t.Errorf("seed endpoint hit %d times, want 2 (lazy fetch + refresh)", seedCalls)
	}
	for i, h := range otpHeaders {
		decoded, err := base64.StdEncoding.DecodeString(h)
		if err != nil || len(decoded) != 6 {
			t.Errorf("request %d TOTP header %q is not base64 of a 6-digit code", i, h)
			continue
		}
		for _, c := range decoded {
			if c < '0' || c > '9' {
				t.Errorf("request %d TOTP header decodes to non-digit %q", i, decoded)
			}
		}
	}
	if otpHeaders[0] == otpHeaders[1] {
		t.Errorf("both attempts sent the same TOTP header %q; seed refresh did not take effect", otpHeaders[0])
	}
}

// Non-memfit models never trigger the TOTP retry, even with the marker body.
func TestNoRetryForNonMemfit(t *testing.T) {
	var calls int
	var mu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":{"message":"memfit_totp_auth_failed"}}`))
	}))
	defer upstream.Close()

	gw := httptest.NewServer(newTestProxy(t, upstream.URL).Mux())
	defer gw.Close()

	resp, err := http.Post(gw.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"deepseek-v3","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("chat hit %d times, want 1", calls)
	}
	if resp.StatusCode != 401 || !strings.Contains(string(body), "memfit_totp_auth_failed") {
		t.Errorf("error not relayed verbatim: %d %s", resp.StatusCode, body)
	}
}

// SSE events must be flushed per-event, not buffered until the stream ends:
// the first event must reach the client before the upstream has written the
// third one.
func TestSSEFlushPerEvent(t *testing.T) {
	var mu sync.Mutex
	var writeTimes []time.Time
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		for i := 0; i < 3; i++ {
			fmt.Fprintf(w, "data: event-%d\n\n", i)
			fl.Flush()
			mu.Lock()
			writeTimes = append(writeTimes, time.Now())
			mu.Unlock()
			time.Sleep(250 * time.Millisecond)
		}
	}))
	defer upstream.Close()

	gw := httptest.NewServer(newTestProxy(t, upstream.URL).Mux())
	defer gw.Close()

	req, _ := http.NewRequest("POST", gw.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-v3","stream":true,"messages":[]}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type %q", ct)
	}

	var arrivals []time.Time
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "data: event-") {
			mu.Lock()
			arrivals = append(arrivals, time.Now())
			mu.Unlock()
			if len(arrivals) == 3 {
				break
			}
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(arrivals) != 3 || len(writeTimes) != 3 {
		t.Fatalf("got %d arrivals / %d writes", len(arrivals), len(writeTimes))
	}
	if !arrivals[0].Before(writeTimes[2]) {
		t.Errorf("first event arrived at %v, after third upstream write %v — stream was buffered",
			arrivals[0], writeTimes[2])
	}
}

func TestRoutingAndBodyLimit(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	cfg := testConfig(upstream.URL)
	cfg.MaxBody = 16
	gw := httptest.NewServer(newTestProxyCfg(t, cfg).Mux())
	defer gw.Close()

	// 405: wrong method on a known route
	resp, err := http.Get(gw.URL + "/v1/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /v1/chat/completions = %d, want 405", resp.StatusCode)
	}

	// 404: unknown route
	resp, err = http.Get(gw.URL + "/v1/nonsense")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /v1/nonsense = %d, want 404", resp.StatusCode)
	}

	// 413: oversized body
	resp, err = http.Post(gw.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(strings.Repeat("x", 64)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized POST = %d, want 413", resp.StatusCode)
	}
}

// End-to-end: upstream "Connection: close" must not leak into the client
// response. (Go's http client consumes the Connection header from
// resp.Header entirely, so the token-listing path cannot be exercised over
// the wire — it is covered by the copyResponseHeaders unit test below.)
func TestHopByHopStripped(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Connection", "close")
		w.Header().Set("X-Custom", "keep") // end-to-end: must be relayed
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	gw := httptest.NewServer(newTestProxy(t, upstream.URL).Mux())
	defer gw.Close()

	resp, err := http.Get(gw.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("X-Custom"); got != "keep" {
		t.Errorf("X-Custom = %q, want keep", got)
	}
	if resp.Close {
		t.Errorf("upstream Connection: close leaked into the client response")
	}
}

// The gateway advertises "gzip, deflate, br" upstream; gzip responses must be
// decompressed before relaying, with Content-Encoding/Content-Length stripped.
func TestGzipResponseDecoded(t *testing.T) {
	const payload = `{"choices":[{"message":{"content":"hi"}}]}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ae := r.Header.Get("Accept-Encoding"); ae != "gzip, deflate, br" {
			t.Errorf("upstream Accept-Encoding = %q", ae)
		}
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "application/json")
		gz := gzip.NewWriter(w)
		_, _ = gz.Write([]byte(payload))
		_ = gz.Close()
	}))
	defer upstream.Close()

	gw := httptest.NewServer(newTestProxy(t, upstream.URL).Mux())
	defer gw.Close()

	resp, err := http.Get(gw.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != payload {
		t.Errorf("client body = %q, want %q", body, payload)
	}
	if ce := resp.Header.Get("Content-Encoding"); ce != "" {
		t.Errorf("Content-Encoding leaked: %q", ce)
	}
}

func TestMaskClientSignatures(t *testing.T) {
	cases := []struct{ in, want string }{
		{"You are Claude Code, Anthropic's official CLI for Claude.",
			"You are an AI assistant, Anthropic's official CLI for Claude."},
		{"You are Codex, based on GPT-5. You are running as a coding agent.",
			"You are an AI assistant, based on GPT-5. You are running as a coding agent."},
		{"you are claude code", "You are an AI assistant"},
		// The upstream matcher requires single spaces; such variants already pass.
		{"You  are  Claude Code", "You  are  Claude Code"},
		{"You are a helpful assistant.", "You are a helpful assistant."},
	}
	for _, c := range cases {
		if got := string(maskClientSignatures([]byte(c.in))); got != c.want {
			t.Errorf("mask(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// A Claude Code system prompt must lose its identity signature before it
// reaches the upstream, which blacklists it otherwise.
func TestClaudeCodeSystemPromptMasked(t *testing.T) {
	var mu sync.Mutex
	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotBody = b
		mu.Unlock()
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	gw := httptest.NewServer(newTestProxy(t, upstream.URL).Mux())
	defer gw.Close()

	payload, _ := json.Marshal(map[string]any{
		"model": "deepseek-v3",
		"messages": []map[string]string{{
			"role":    "system",
			"content": "You are Claude Code, Anthropic's official CLI for Claude.\n\nFollow the user.",
		}},
	})
	resp, err := http.Post(gw.URL+"/v1/chat/completions", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}

	mu.Lock()
	defer mu.Unlock()
	if strings.Contains(string(gotBody), "You are Claude Code") {
		t.Errorf("identity signature reached upstream: %s", gotBody)
	}
	if !strings.Contains(string(gotBody), "You are an AI assistant") {
		t.Errorf("mask replacement missing from upstream body: %s", gotBody)
	}
}

// The Anthropic Messages routes (Claude Code) and the Responses API (Codex)
// must reach the upstream at the same paths.
func TestAgentRoutes(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	gw := httptest.NewServer(newTestProxy(t, upstream.URL).Mux())
	defer gw.Close()

	want := []string{"/v1/messages", "/v1/messages/count_tokens", "/v1/responses"}
	for _, p := range want {
		resp, err := http.Post(gw.URL+p, "application/json",
			strings.NewReader(`{"model":"deepseek-v3","messages":[],"input":"hi"}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("POST %s = %d, want 200", p, resp.StatusCode)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Errorf("upstream paths = %v, want %v", paths, want)
	}
}

func TestCopyResponseHeaders(t *testing.T) {
	src := http.Header{}
	src.Add("Connection", "close")
	src.Add("Connection", "keep-alive, X-Foo") // second line: token list
	src.Set("X-Foo", "bar")                    // listed in Connection: dropped
	src.Set("Keep-Alive", "timeout=5")         // static hop-by-hop: dropped
	src.Set("Content-Type", "text/event-stream")
	src.Set("X-Multi", "a")
	src.Add("X-Multi", "b") // multi-value end-to-end: both kept

	dst := http.Header{}
	copyResponseHeaders(dst, src)

	for _, dropped := range []string{"X-Foo", "Keep-Alive"} {
		if dst.Get(dropped) != "" {
			t.Errorf("%s was relayed, want dropped", dropped)
		}
	}
	if dst.Values("Connection") != nil {
		t.Errorf("Connection was relayed, want dropped")
	}
	if got := dst.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := dst.Values("X-Multi"); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("X-Multi = %#v, want [a b]", got)
	}
}
