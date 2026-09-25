// Package receptionist authenticates the v1 webhook adapter. The signature
// covers timestamp + "." + raw request body, using a per-connection secret.
package receptionist

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"time"
)

func Verify(secret []byte, timestamp, signature string, body []byte, now time.Time) bool {
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || len(secret) < 32 {
		return false
	}
	age := now.Sub(time.Unix(seconds, 0))
	if age < -5*time.Minute || age > 5*time.Minute {
		return false
	}
	got, err := hex.DecodeString(signature)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(timestamp + "."))
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}
