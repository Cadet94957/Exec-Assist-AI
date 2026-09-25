package receptionist

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"testing"
	"time"
)

func TestVerifyRejectsForgeryAndExpiredReplay(t *testing.T) {
	now := time.Now()
	key := []byte("01234567890123456789012345678901")
	body := []byte(`{"id":"one"}`)
	ts := strconv.FormatInt(now.Unix(), 10)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	sig := hex.EncodeToString(mac.Sum(nil))
	if !Verify(key, ts, sig, body, now) {
		t.Fatal("valid signature rejected")
	}
	if Verify(key, ts, sig, []byte(`{"id":"two"}`), now) || Verify(key, ts, sig, body, now.Add(6*time.Minute)) || Verify(key, ts, sig, body, now.Add(-6*time.Minute)) || Verify(key, "invalid", sig, body, now) {
		t.Fatal("forgery or expired replay accepted")
	}
}
