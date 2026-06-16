package commandcode

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
	log "github.com/sirupsen/logrus"
)

// CommandCodeTokenStorage stores token information for Command Code authentication.
// This is serialized to a JSON file in the auths/ directory.
type CommandCodeTokenStorage struct {
	// ApiKey is the Command Code API key.
	ApiKey string `json:"api_key"`

	// Email is the email address of the authenticated user.
	Email string `json:"email,omitempty"`

	// BaseURL optionally overrides the default API base URL.
	BaseURL string `json:"base_url,omitempty"`

	// OAuthProvider is the upstream provider (anthropic, codex, github-copilot).
	// When set, the executor sends x-oauth-provider header.
	OAuthProvider string `json:"oauth_provider,omitempty"`

	// OAuthToken is the upstream OAuth access token.
	// When set, the executor sends x-oauth-token header.
	OAuthToken string `json:"oauth_token,omitempty"`

	// OAuthRefresh is the upstream OAuth refresh token (for future auto-refresh).
	OAuthRefresh string `json:"oauth_refresh,omitempty"`

	// OAuthExpires is the Unix timestamp when the upstream OAuth token expires.
	OAuthExpires int64 `json:"oauth_expires,omitempty"`

	// Type indicates the authentication provider type, always "commandcode".
	Type string `json:"type"`
}

// SaveTokenToFile serializes the Command Code token storage to a JSON file.
func (ts *CommandCodeTokenStorage) SaveTokenToFile(authFilePath string) error {
	misc.LogSavingCredentials(authFilePath)
	ts.Type = "commandcode"
	if err := os.MkdirAll(filepath.Dir(authFilePath), 0700); err != nil {
		return fmt.Errorf("failed to create directory: %v", err)
	}

	f, err := os.Create(authFilePath)
	if err != nil {
		return fmt.Errorf("failed to create token file: %w", err)
	}
	defer func() {
		if errClose := f.Close(); errClose != nil {
			log.Errorf("failed to close file: %v", errClose)
		}
	}()

	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err = enc.Encode(ts); err != nil {
		return fmt.Errorf("failed to write token to file: %w", err)
	}
	return nil
}

// CredentialFileName returns the filename for Command Code credentials.
func CredentialFileName(email string) string {
	if email == "" {
		return "commandcode.json"
	}
	return fmt.Sprintf("commandcode-%s.json", sanitizeEmail(email))
}

func sanitizeEmail(email string) string {
	replacer := strings.NewReplacer(
		"@", "_at_",
		":", "_",
		"/", "_",
		"\\", "_",
		" ", "_",
	)
	return replacer.Replace(email)
}
