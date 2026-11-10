package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClientIDPersistsAcrossLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "client-id")

	first, err := loadOrCreateClientIDAt(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 36 || first[8] != '-' || first[13] != '-' || first[18] != '-' || first[23] != '-' {
		t.Fatalf("client id %q is not a canonical UUID", first)
	}
	if first[14] != '4' {
		t.Errorf("client id %q is not UUID v4", first)
	}

	second, err := loadOrCreateClientIDAt(path)
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Errorf("client id changed across loads: %q vs %q", first, second)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != first+"\n" {
		t.Errorf("stored file = %q, want %q", data, first+"\n")
	}
}

func TestClientIDRegeneratesEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client-id")
	if err := os.WriteFile(path, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := loadOrCreateClientIDAt(path)
	if err != nil {
		t.Fatal(err)
	}
	if id == "" {
		t.Fatal("empty client id")
	}
}

func TestNewUUIDv4Format(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id := newUUIDv4()
		if len(id) != 36 || id[14] != '4' || !strings.ContainsRune("89ab", rune(id[19])) {
			t.Fatalf("bad UUID %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate UUID %q", id)
		}
		seen[id] = true
	}
}
