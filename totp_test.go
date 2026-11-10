package main

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"strconv"
	"testing"
	"time"
)

// RFC 6238 Appendix B test vectors, SHA1 column (8-digit codes over the ASCII
// seed "12345678901234567890"). The 6-digit code is the same truncated value
// mod 10^6, so the expectation is derived in-test from the published values.
func TestTOTP6RFC6238Vectors(t *testing.T) {
	rfc := map[int64]string{
		59:          "94287082",
		1111111109:  "07081804",
		1111111111:  "14050471",
		1234567890:  "89005924",
		2000000000:  "69279037",
		20000000000: "65353130",
	}
	seed := []byte("12345678901234567890")
	for ts, eight := range rfc {
		n, err := strconv.Atoi(eight)
		if err != nil {
			t.Fatalf("bad vector %q: %v", eight, err)
		}
		want := fmt.Sprintf("%06d", n%1000000)
		if got := totp6(seed, time.Unix(ts, 0)); got != want {
			t.Errorf("TOTP at T=%d: got %s, want %s", ts, got, want)
		}
	}
}

// yaklangGetUTCCode replicates the exact code path of yaklang's twofa package
// (common/twofa/twofa.go ComputeCode + GetToptUTCCodeString): base32-encode
// the seed, base32-decode it back as the HMAC key, 8-byte big-endian counter.
func yaklangGetUTCCode(secret string, now time.Time) string {
	key, err := base32.StdEncoding.DecodeString(base32.StdEncoding.EncodeToString([]byte(secret)))
	if err != nil {
		return ""
	}
	hash := hmac.New(sha1.New, key)
	_ = binary.Write(hash, binary.BigEndian, now.UTC().Unix()/30)
	h := hash.Sum(nil)
	offset := h[19] & 0x0f
	truncated := binary.BigEndian.Uint32(h[offset : offset+4])
	truncated &= 0x7fffffff
	return fmt.Sprintf("%06d", truncated%1000000)
}

// TestYaklangEquivalence pins the claim that skipping the base32 round trip
// produces byte-identical codes to the yaklang implementation.
func TestYaklangEquivalence(t *testing.T) {
	seeds := []string{
		"550e8400-e29b-41d4-a716-446655440000",
		"hello-yak-secret",
		"short",
	}
	for _, seed := range seeds {
		for _, ts := range []int64{0, 1757000000, 1757000029, 1757000030} {
			now := time.Unix(ts, 0)
			got, want := totp6([]byte(seed), now), yaklangGetUTCCode(seed, now)
			if got != want {
				t.Errorf("seed %q @ %d: got %s, yaklang gives %s", seed, ts, got, want)
			}
		}
	}
}

func TestUnwrapSeed(t *testing.T) {
	cases := map[string]string{
		"MEMFIT-AIabcMEMFIT-AI": "abc",
		"abc":                   "abc",
		"":                      "",
		"MEMFIT-AI":             "",
	}
	for in, want := range cases {
		if got := unwrapSeed(in); got != want {
			t.Errorf("unwrapSeed(%q) = %q, want %q", in, got, want)
		}
	}
}
