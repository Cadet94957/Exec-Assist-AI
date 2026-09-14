package secrets_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/Cadet94957/Exec-Assist-AI/internal/secrets"
)

const key = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
const otherKey = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"

func TestSealOpenRoundTrip(t *testing.T) {
	box, err := secrets.NewBox(key)
	if err != nil {
		t.Fatalf("new box: %v", err)
	}

	plain := []byte(`{"refresh_token":"1//very-secret"}`)
	sealed, err := box.Seal(plain)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if strings.Contains(string(sealed), "very-secret") {
		t.Fatal("the sealed value still contains the plaintext")
	}

	opened, err := box.Open(sealed)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if string(opened) != string(plain) {
		t.Errorf("round trip = %q, want %q", opened, plain)
	}
}

// Two seals of the same plaintext must differ, or an observer can tell which
// tenants share a credential.
func TestSealIsNondeterministic(t *testing.T) {
	box, _ := secrets.NewBox(key)
	a, _ := box.Seal([]byte("same"))
	b, _ := box.Seal([]byte("same"))
	if string(a) == string(b) {
		t.Error("two seals produced identical ciphertext — the nonce is not random")
	}
}

func TestOpenRejectsTampering(t *testing.T) {
	box, _ := secrets.NewBox(key)
	sealed, _ := box.Seal([]byte("refresh-token"))

	tampered := make([]byte, len(sealed))
	copy(tampered, sealed)
	tampered[len(tampered)-1] ^= 0xFF

	if _, err := box.Open(tampered); err == nil {
		t.Error("opening tampered data succeeded — GCM authentication is not working")
	}
}

func TestOpenRejectsWrongKey(t *testing.T) {
	a, _ := secrets.NewBox(key)
	b, _ := secrets.NewBox(otherKey)

	sealed, _ := a.Seal([]byte("refresh-token"))
	if _, err := b.Open(sealed); err == nil {
		t.Error("a different key opened the value")
	}
}

func TestOpenRejectsShortInput(t *testing.T) {
	box, _ := secrets.NewBox(key)
	if _, err := box.Open([]byte{1, 2, 3}); err == nil {
		t.Error("expected an error for a truncated value")
	}
}

// An unset key must be a refusal, never a silent fallback to plaintext.
func TestNewBoxValidatesKey(t *testing.T) {
	if _, err := secrets.NewBox(""); !errors.Is(err, secrets.ErrNoKey) {
		t.Errorf("empty key: got %v, want ErrNoKey", err)
	}
	if _, err := secrets.NewBox("nothex!!"); err == nil {
		t.Error("expected an error for a non-hex key")
	}
	if _, err := secrets.NewBox("abcd"); err == nil {
		t.Error("expected an error for a key that is too short")
	}
}
