package main

import (
	"crypto/rand"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// clientIDHeader carries the persistent per-client UUID that yaklang sends on
// every aibalance request. The upstream's free-tier risk control buckets
// requests by this identity; without it the gateway falls into the anonymous
// bucket, which is what triggers limit_kind "memfit_client" rejections.
const clientIDHeader = "X-User-Id"

func newUUIDv4() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func defaultClientIDPath() string {
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "yakaigateway", "client-id")
}

// loadOrCreateClientIDAt reads the persisted UUID or generates and stores a
// new one. An existing file is authoritative: unlike yaklang there is no
// database fallback, but the same file survives restarts like yaklang's
// profile KV does.
func loadOrCreateClientIDAt(path string) (string, error) {
	if data, err := os.ReadFile(path); err == nil {
		if id := strings.TrimSpace(string(data)); id != "" {
			return id, nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	id := newUUIDv4()
	if err := os.WriteFile(path, []byte(id+"\n"), 0o600); err != nil {
		return "", err
	}
	return id, nil
}

// loadOrCreateClientID returns the configured identity, falling back to a
// process-lifetime UUID (still better than the anonymous bucket) when the
// config directory is not writable.
func loadOrCreateClientID(log *slog.Logger) string {
	id, err := loadOrCreateClientIDAt(defaultClientIDPath())
	if err != nil {
		log.Warn("client id persistence failed; using a per-process id", "err", err)
		return newUUIDv4()
	}
	return id
}
