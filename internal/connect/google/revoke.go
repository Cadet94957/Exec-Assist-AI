package google

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/Cadet94957/Exec-Assist-AI/internal/ledger"
	"github.com/Cadet94957/Exec-Assist-AI/internal/secrets"
)

const revokeURL = "https://oauth2.googleapis.com/revoke"

// RevokeConnection asks Google to invalidate the connected account's refresh
// token. Local deletion is still performed by the caller even if this remote
// cleanup fails, so a disconnected account never remains usable by Exec Assist.
// The caller can surface the remote failure to support if needed.
func RevokeConnection(ctx context.Context, cfg Config, store *ledger.Store, box *secrets.Box) error {
	conn, err := store.GetConnection(Provider)
	if err != nil {
		return err
	}
	tok, err := openToken(box, conn.Token)
	if err != nil {
		return err
	}
	if tok.RefreshToken == "" {
		return fmt.Errorf("google: connection has no refresh token")
	}

	form := url.Values{"token": {tok.RefreshToken}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, revokeURL, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("google: build revoke request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("google: revoke token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("google: revoke token returned %d", resp.StatusCode)
	}
	return nil
}
