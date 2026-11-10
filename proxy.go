package main

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Headers that must never be relayed in either direction (RFC 7230 §6.1),
// plus anything the upstream names in its Connection header.
var hopByHopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// TOTP failure markers from the upstream error body (mirrors yaklang's
// isMemfitTOTPError / ErrMemfitTOTPAuthFailed).
var totpFailureMarkers = [][]byte{
	[]byte("memfit_totp_auth_failed"),
	[]byte("memfit_totp_auth_required"),
	[]byte("Memfit TOTP authentication failed"),
}

const totpFailureSnippetLimit = 64 << 10 // only the first 64KB is inspected

// Proxy forwards /v1/models, /v1/chat/completions and /v1/embeddings to the
// upstream OpenAI-compatible endpoint, owning the memfit TOTP auth header.
type Proxy struct {
	upstream string // base URL, no trailing slash
	client   *http.Client
	totp     *TOTPManager
	cfg      Config
	log      *slog.Logger
}

func NewProxy(cfg Config, hc *http.Client, t *TOTPManager, log *slog.Logger) *Proxy {
	return &Proxy{
		upstream: strings.TrimRight(cfg.Upstream, "/"),
		client:   hc,
		totp:     t,
		cfg:      cfg,
		log:      log,
	}
}

func (p *Proxy) Mux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", p.handleModels)
	mux.HandleFunc("POST /v1/chat/completions", p.handleChat)
	mux.HandleFunc("POST /v1/responses", p.handleResponses)
	mux.HandleFunc("POST /v1/messages", p.handleMessages)
	mux.HandleFunc("POST /v1/messages/count_tokens", p.handleCountTokens)
	mux.HandleFunc("POST /v1/embeddings", p.handleEmbeddings)
	return mux
}

func (p *Proxy) handleModels(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	req, err := p.buildUpstreamRequest(r, "/v1/models", nil, false)
	if err != nil {
		p.badRequest(w, err)
		return
	}
	resp, err := p.client.Do(req)
	if err != nil {
		p.gatewayError(w, err)
		return
	}
	p.log.Info("models", "status", resp.StatusCode, "dur", time.Since(start).Round(time.Millisecond))
	p.relay(w, resp)
}

func (p *Proxy) handleEmbeddings(w http.ResponseWriter, r *http.Request) {
	p.forwardPost(w, r, "/v1/embeddings")
}

func (p *Proxy) handleChat(w http.ResponseWriter, r *http.Request) {
	p.forwardPost(w, r, "/v1/chat/completions")
}

// handleResponses serves the OpenAI Responses API used by Codex; the upstream
// aibalance supports it with the same auth and memfit gates as chat.
func (p *Proxy) handleResponses(w http.ResponseWriter, r *http.Request) {
	p.forwardPost(w, r, "/v1/responses")
}

// handleMessages serves the Anthropic Messages API, so Claude Code can point
// ANTHROPIC_BASE_URL straight at the gateway.
func (p *Proxy) handleMessages(w http.ResponseWriter, r *http.Request) {
	p.forwardPost(w, r, "/v1/messages")
}

func (p *Proxy) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	p.forwardPost(w, r, "/v1/messages/count_tokens")
}

// forwardPost handles POST /v1/chat/completions and /v1/embeddings: buffer the
// body, attach the TOTP header when the model is memfit-*, and on a TOTP
// failure refresh the seed and retry exactly once.
func (p *Proxy) forwardPost(w http.ResponseWriter, r *http.Request, path string) {
	start := time.Now()
	body, ok := p.readBody(w, r)
	if !ok {
		return
	}
	if masked := maskClientSignatures(body); !bytes.Equal(masked, body) {
		p.log.Debug("masked agent-client identity signature in request body", "path", path)
		body = masked
	}

	// Peek only the fields the gateway needs to decide auth. A parse error is
	// non-fatal: forward as-is and let upstream return its own 400.
	var info struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	_ = json.Unmarshal(body, &info)
	memfit := isMemfitModel(info.Model)

	for attempt := 0; ; attempt++ {
		req, err := p.buildUpstreamRequest(r, path, body, memfit)
		if err != nil {
			p.badRequest(w, err)
			return
		}
		resp, err := p.client.Do(req)
		if err != nil {
			p.gatewayError(w, err)
			return
		}

		// The retry decision must happen before anything is written to the
		// client. TOTP failures always arrive as an immediate non-2xx before
		// any SSE data, so this window always exists.
		if memfit && resp.StatusCode/100 != 2 && attempt == 0 {
			body, decoded := decodeBody(resp)
			snippet, _ := io.ReadAll(io.LimitReader(body, totpFailureSnippetLimit))
			resp.Body.Close()
			if isTOTPFailure(snippet) {
				p.log.Info("memfit TOTP auth failed; refreshing seed and retrying once",
					"path", path, "status", resp.StatusCode)
				if err := p.totp.Refresh(r.Context()); err != nil {
					p.log.Warn("TOTP seed refresh failed", "err", err)
				}
				continue
			}
			// Non-retryable error: relay the buffered snippet verbatim, but log
			// it — without this the 429 quota path was completely silent.
			p.log.Warn("upstream error relayed",
				"path", path,
				"model", info.Model,
				"status", resp.StatusCode,
				"dur", time.Since(start).Round(time.Millisecond),
				"body", truncateBody(snippet, 300),
			)
			writeResponseHeaders(w, resp, decoded)
			_, _ = w.Write(snippet)
			return
		}

		p.log.Info("post",
			"path", path,
			"model", info.Model,
			"stream", info.Stream,
			"memfit", memfit,
			"status", resp.StatusCode,
			"attempt", attempt,
			"dur", time.Since(start).Round(time.Millisecond),
		)
		p.relay(w, resp)
		return
	}
}

// isMemfitModel matches yaklang: model name starts with "memfit-", case-insensitive.
func isMemfitModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(model), "memfit-")
}

// isTOTPFailure reports whether the upstream error body carries a TOTP
// failure marker.
func isTOTPFailure(snippet []byte) bool {
	for _, marker := range totpFailureMarkers {
		if bytes.Contains(snippet, marker) {
			return true
		}
	}
	return false
}

// The upstream free-tier risk control blacklists requests whose system prompt
// opens with a known agent-client identity (limit_kind memfit_client,
// reason client_blacklisted, e.g. "You are Claude Code, Anthropic's official
// CLI for Claude." or "You are Codex, based on GPT-5..."). Masking exactly
// those phrases is what lets Claude Code and Codex drive the free models.
// The replacement is plain ASCII inside JSON strings, so the body stays
// valid, and everything else stays byte-identical for prompt caching.
var clientSignatureRE = regexp.MustCompile(`(?i)you are claude code|you are codex`)

func maskClientSignatures(body []byte) []byte {
	return clientSignatureRE.ReplaceAll(body, []byte("You are an AI assistant"))
}

// truncateBody shortens an upstream error body for logging.
func truncateBody(b []byte, n int) string {
	s := string(b)
	if len(s) > n {
		s = s[:n] + "...(truncated)"
	}
	return s
}

// buildUpstreamRequest constructs the upstream request from scratch with an
// allowlisted header set — client headers are never cloned wholesale. This
// guarantees no client junk or hop-by-hop leakage reaches the upstream.
func (p *Proxy) buildUpstreamRequest(r *http.Request, path string, body []byte, attachTOTP bool) (*http.Request, error) {
	u := p.upstream + path
	if r.URL.RawQuery != "" {
		u += "?" + r.URL.RawQuery
	}

	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, u, rd)
	if err != nil {
		return nil, err
	}

	// The User-Agent is gateway-owned: client values are discarded. An empty
	// value suppresses net/http's default "Go-http-client/1.1".
	req.Header.Set("User-Agent", p.cfg.UserAgent)

	h := req.Header
	if body != nil {
		h.Set("Content-Type", "application/json; charset=UTF-8")
	}
	// The real aibalance client always sends these exact values; client
	// values are ignored so the fingerprint stays consistent.
	h.Set("Accept", "application/json")
	h.Set("Accept-Encoding", "gzip, deflate, br")

	// The API key is gateway-owned too: every upstream request carries the
	// configured bearer token regardless of what the client sent.
	if p.cfg.APIKey != "" {
		h.Set("Authorization", "Bearer "+p.cfg.APIKey)
	}

	if p.cfg.YakVersion != "" {
		h.Set("X-Yak-Version", p.cfg.YakVersion)
	}
	if p.cfg.YakBuildTime != "" {
		h.Set("X-Yak-Build-Time", p.cfg.YakBuildTime)
	}
	if p.cfg.UsageType != "" {
		h.Set("X-Yak-AI-Model-Usage-Type", p.cfg.UsageType)
	}

	// The client identity is gateway-owned, like yaklang's persistent
	// aibalance trace id: any client value is discarded.
	if p.cfg.UserID != "" {
		h.Set(clientIDHeader, p.cfg.UserID)
	}

	// The TOTP header is gateway-owned: any client value is discarded.
	if attachTOTP {
		if v := p.totp.AuthHeader(r.Context()); v != "" {
			h.Set("X-Memfit-OTP-Auth", v)
		}
	}
	return req, nil
}

// decodeBody wraps the response body with a decompressor when the upstream
// honoured our Accept-Encoding. The bool reports whether decoding happened, in
// which case Content-Encoding/Content-Length must not be relayed. "br" is
// passed through untouched: Ubuntu's nginx has no brotli module, so it never
// appears in practice, and stdlib cannot decode it.
func decodeBody(resp *http.Response) (io.Reader, bool) {
	switch strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding"))) {
	case "gzip", "x-gzip":
		if zr, err := gzip.NewReader(resp.Body); err == nil {
			return zr, true
		}
	case "deflate":
		if zr, err := zlib.NewReader(resp.Body); err == nil {
			return zr, true
		}
	}
	return resp.Body, false
}

// readBody buffers the request body (needed anyway for the model peek and for
// replay on retry), enforcing the configured size limit with a 413.
func (p *Proxy) readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, p.cfg.MaxBody+1))
	if err != nil {
		http.Error(w, "failed to read request body: "+err.Error(), http.StatusBadRequest)
		return nil, false
	}
	if int64(len(body)) > p.cfg.MaxBody {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return nil, false
	}
	return body, true
}

func writeResponseHeaders(w http.ResponseWriter, resp *http.Response, decoded bool) {
	copyResponseHeaders(w.Header(), resp.Header)
	if decoded {
		w.Header().Del("Content-Encoding")
		w.Header().Del("Content-Length")
	}
	// Hint for anything in front of us (nginx etc.) not to buffer SSE.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(resp.StatusCode)
}

// relay streams the upstream response to the client, flushing after every
// write so SSE events are delivered as they arrive.
func (p *Proxy) relay(w http.ResponseWriter, resp *http.Response) {
	defer resp.Body.Close()
	body, decoded := decodeBody(resp)
	writeResponseHeaders(w, resp, decoded)
	_, _ = io.Copy(flushWriter{w}, body) // io.Copy's pooled 32KB buffer: no per-request alloc
}

func copyResponseHeaders(dst, src http.Header) {
	connListed := connectionListedHeaders(src)
	for k, vs := range src {
		if isHopByHop(k, connListed) {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// connectionListedHeaders returns all headers named in any Connection header
// line (there may be several; Header.Get would only see the first).
func connectionListedHeaders(src http.Header) []string {
	var out []string
	for _, v := range src.Values("Connection") {
		out = append(out, strings.Split(v, ",")...)
	}
	return out
}

func isHopByHop(name string, connListed []string) bool {
	for _, h := range hopByHopHeaders {
		if strings.EqualFold(name, h) {
			return true
		}
	}
	for _, h := range connListed {
		if strings.EqualFold(name, strings.TrimSpace(h)) {
			return true
		}
	}
	return false
}

// flushWriter flushes after every write so each upstream read becomes one
// client write+flush (HTTP/1.1 and HTTP/2 alike).
type flushWriter struct{ w http.ResponseWriter }

func (f flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if fl, ok := f.w.(http.Flusher); ok {
		fl.Flush()
	}
	return n, err
}

func (p *Proxy) badRequest(w http.ResponseWriter, err error) {
	http.Error(w, err.Error(), http.StatusBadRequest)
}

func (p *Proxy) gatewayError(w http.ResponseWriter, err error) {
	p.log.Warn("upstream request failed", "err", err)
	http.Error(w, "upstream request failed: "+err.Error(), http.StatusBadGateway)
}
