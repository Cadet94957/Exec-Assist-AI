package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLocalKeyPersistsAndFailsClosed(t *testing.T) {
	t.Setenv("TOKEN_KEY", "")
	dir := t.TempDir()
	if err := ensureLocalTokenKey(dir); err != nil {
		t.Fatal(err)
	}
	first := os.Getenv("TOKEN_KEY")
	if len(first) != 64 {
		t.Fatal("key length")
	}
	os.Setenv("TOKEN_KEY", "")
	if err := ensureLocalTokenKey(dir); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("TOKEN_KEY") != first {
		t.Fatal("key changed after restart")
	}
	os.Setenv("TOKEN_KEY", "")
	if err := os.WriteFile(filepath.Join(dir, "token.key"), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := ensureLocalTokenKey(dir); err == nil {
		t.Fatal("invalid key silently replaced")
	}
}
func TestPersonalAccountStartsWithoutDemo(t *testing.T) {
	t.Setenv("MEMORYWEB_OLLAMA_ENDPOINT", "disabled")
	acct, _, created, err := ensureTenant(t.TempDir(), "personal", "Personal", "America/Los_Angeles", false)
	if err != nil || !created || acct.Trade != "personal" {
		t.Fatalf("personal account: %+v %v", acct, err)
	}
}
