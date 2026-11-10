package main

import (
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// totp6 computes the RFC 6238 TOTP (HMAC-SHA1, 6 digits, 30s period, T0=0,
// UTC) for the given key at time now.
//
// The key is the raw seed bytes. The upstream aibalance service (and yaklang's
// client, common/twofa) uses the ASCII bytes of the seed UUID string as the
// HMAC key — its base32-encode-then-decode round trip is a no-op, so we skip it.
func totp6(key []byte, now time.Time) string {
	counter := uint64(now.UTC().Unix() / 30)
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)
	h := hmac.New(sha1.New, key)
	h.Write(msg[:])
	sum := h.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	code := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", code%1000000)
}

// unwrapSeed extracts the TOTP seed from the server payload
// {"uuid":"MEMFIT-AI<seed>MEMFIT-AI"} by trimming the marker prefix/suffix.
func unwrapSeed(uuid string) string {
	return strings.TrimSuffix(strings.TrimPrefix(uuid, "MEMFIT-AI"), "MEMFIT-AI")
}

// TOTPManager lazily fetches the aibalance TOTP seed and produces the
// X-Memfit-OTP-Auth header value (base64 of the current 6-digit code).
// Concurrency-safe: a single mutex guards the cached seed; concurrent first
// callers serialize while one of them fetches (~100ms, once per process).
type TOTPManager struct {
	mu      sync.Mutex
	seed    string
	key     []byte // []byte(seed), precomputed
	baseURL   string // upstream base URL, no trailing slash
	userAgent string
	userID    string
	client    *http.Client
	log       *slog.Logger
}

func NewTOTPManager(baseURL string, hc *http.Client, userAgent, userID string, log *slog.Logger) *TOTPManager {
	return &TOTPManager{
		baseURL:   strings.TrimRight(baseURL, "/"),
		userAgent: userAgent,
		userID:    userID,
		client:    hc,
		log:       log,
	}
}

// fetchSeed performs the unauthenticated GET {base}/v1/memfit-totp-uuid and
// unwraps the seed. Caller holds m.mu.
func (m *TOTPManager) fetchSeed(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.baseURL+"/v1/memfit-totp-uuid", nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "gzip, deflate, br")
	req.Header.Set("User-Agent", m.userAgent)
	// yaklang sends the same X-User-Id when fetching the seed as when
	// chatting, so the server can bind the seed to the client identity.
	if m.userID != "" {
		req.Header.Set(clientIDHeader, m.userID)
	}

	resp, err := m.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	body := io.Reader(resp.Body)
	switch strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding"))) {
	case "gzip", "x-gzip":
		if zr, err := gzip.NewReader(resp.Body); err == nil {
			body = zr
		}
	case "deflate":
		if zr, err := zlib.NewReader(resp.Body); err == nil {
			body = zr
		}
	}
	data, err := io.ReadAll(io.LimitReader(body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read body: %w", err)
	}
	var payload struct {
		UUID   string `json:"uuid"`
		Format string `json:"format"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return "", fmt.Errorf("parse body: %w", err)
	}
	seed := unwrapSeed(payload.UUID)
	if seed == "" {
		return "", fmt.Errorf("empty seed in response")
	}
	return seed, nil
}

// AuthHeader returns the X-Memfit-OTP-Auth value, lazily fetching the seed on
// first use. On fetch failure it returns "" and the request is sent without the
// header (mirroring yaklang's behavior; the upstream 401 marker then triggers
// the refresh-and-retry path in the proxy).
func (m *TOTPManager) AuthHeader(ctx context.Context) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.seed == "" {
		seed, err := m.fetchSeed(ctx)
		if err != nil {
			m.log.Warn("TOTP seed fetch failed", "err", err)
			return ""
		}
		m.seed = seed
		m.key = []byte(seed)
		m.log.Debug("TOTP seed fetched", "seed", seed)
	}
	return base64.StdEncoding.EncodeToString([]byte(totp6(m.key, time.Now())))
}

// Refresh force-refetches the seed, swapping the cache atomically. Called only
// from the TOTP-failure retry path. On failure the old seed is kept.
func (m *TOTPManager) Refresh(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	seed, err := m.fetchSeed(ctx)
	if err != nil {
		return err
	}
	m.seed = seed
	m.key = []byte(seed)
	m.log.Debug("TOTP seed refreshed", "seed", seed)
	return nil
}
