package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Cadet94957/Exec-Assist-AI/internal/secrets"
)

// Only local quickstart uses a disk key. Hosted deployments still require
// TOKEN_KEY from their secret manager. Never generate a replacement for an
// existing unreadable or invalid key: that would strand saved credentials.
func ensureLocalTokenKey(dataDir string) error {
	if os.Getenv("TOKEN_KEY") != "" {
		return nil
	}
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return err
	}
	path := filepath.Join(dataDir, "token.key")
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		key := make([]byte, 32)
		if _, err = rand.Read(key); err != nil {
			return err
		}
		raw = []byte(hex.EncodeToString(key))
		f, createErr := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if errors.Is(createErr, os.ErrExist) {
			raw, err = os.ReadFile(path)
		} else if createErr != nil {
			return createErr
		} else {
			_, err = f.Write(raw)
			closeErr := f.Close()
			if err == nil {
				err = closeErr
			}
		}
	}
	if err != nil {
		return fmt.Errorf("read local encryption key: %w", err)
	}
	key := strings.TrimSpace(string(raw))
	if _, err = secrets.NewBox(key); err != nil {
		return fmt.Errorf("local encryption key is invalid; restore token.key from your backup")
	}
	return os.Setenv("TOKEN_KEY", key)
}
