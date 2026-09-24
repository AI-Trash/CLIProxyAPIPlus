package management

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// GetCommandCodeQuota returns the credit, plan and rate-limit snapshot for a
// Command Code credential.
//
// Endpoint:
//
//	GET /v0/management/commandcode-quota
//
// Query Parameters (optional):
//   - auth_index: The credential "auth_index" from GET /v0/management/auth-files.
//     If omitted, the first available Command Code credential is used.
//
// The snapshot combines /alpha/whoami?limits=1, /alpha/billing/credits,
// /alpha/billing/subscriptions and /alpha/usage/summary — the same endpoints
// the official CLI reads for its /usage view — plus derived credit totals and
// the consumption percentage. Per-endpoint failures are reported in "errors"
// while the remaining sections are still returned.
//
// Example:
//
//	curl -sS -X GET "http://127.0.0.1:8317/v0/management/commandcode-quota?auth_index=<AUTH_INDEX>" \
//	  -H "Authorization: Bearer <MANAGEMENT_KEY>"
func (h *Handler) GetCommandCodeQuota(c *gin.Context) {
	authIndex := strings.TrimSpace(c.Query("auth_index"))
	if authIndex == "" {
		authIndex = strings.TrimSpace(c.Query("authIndex"))
	}
	if authIndex == "" {
		authIndex = strings.TrimSpace(c.Query("AuthIndex"))
	}

	if h == nil || h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}

	auth := h.findCommandCodeAuth(authIndex)
	if auth == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no command code credential found"})
		return
	}

	apiKey := commandCodeQuotaAPIKey(auth)
	if apiKey == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "command code api key not found"})
		return
	}

	client := &http.Client{
		Timeout:   defaultAPICallTimeout,
		Transport: h.apiCallTransport(auth, ""),
	}

	quota, err := helps.FetchCommandCodeQuota(c.Request.Context(), client, commandCodeQuotaBaseURL(auth), apiKey)
	if err != nil {
		log.WithError(err).Debug("command code quota request failed")
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, quota)
}

// findCommandCodeAuth locates a Command Code credential by auth_index or returns
// the first available one.
func (h *Handler) findCommandCodeAuth(authIndex string) *coreauth.Auth {
	if h == nil || h.authManager == nil {
		return nil
	}
	authIndex = strings.TrimSpace(authIndex)

	var first *coreauth.Auth
	for _, auth := range h.authManager.List() {
		if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "commandcode") {
			continue
		}
		if first == nil {
			first = auth
		}
		if authIndex == "" {
			continue
		}
		auth.EnsureIndex()
		if auth.Index == authIndex {
			return auth
		}
	}
	return first
}

// commandCodeQuotaAPIKey resolves the API key for a Command Code credential.
func commandCodeQuotaAPIKey(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		if key := strings.TrimSpace(auth.Attributes["api_key"]); key != "" {
			return key
		}
	}
	if auth.Metadata != nil {
		if key, ok := auth.Metadata["api_key"].(string); ok {
			return strings.TrimSpace(key)
		}
	}
	return ""
}

// commandCodeQuotaBaseURL resolves the API origin for a Command Code credential,
// preferring the per-credential override over the production default.
func commandCodeQuotaBaseURL(auth *coreauth.Auth) string {
	if auth != nil {
		if auth.Attributes != nil {
			if base := strings.TrimSpace(auth.Attributes["base_url"]); base != "" {
				return base
			}
		}
		if auth.Metadata != nil {
			if base, ok := auth.Metadata["base_url"].(string); ok {
				if trimmed := strings.TrimSpace(base); trimmed != "" {
					return trimmed
				}
			}
		}
	}
	return helps.DefaultCommandCodeBaseURL
}
