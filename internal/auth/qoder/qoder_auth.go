package qoder

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	log "github.com/sirupsen/logrus"
)

const (
	// QoderOpenAPIBase is the base URL for Qoder OpenAPI
	QoderOpenAPIBase = "https://openapi.qoder.sh"
	// QoderCenterBase is the base URL for Qoder Center API
	QoderCenterBase = "https://center.qoder.sh"
	// QoderLoginURL is the URL for user authentication
	QoderLoginURL = "https://qoder.com/device/selectAccounts"
	// QoderOAuthTokenEndpoint is the URL for polling device code token
	QoderOAuthTokenEndpoint = "https://openapi.qoder.sh/api/v1/deviceToken/poll"
	// QoderRefreshTokenEndpoint is the URL for refreshing access tokens
	QoderRefreshTokenEndpoint = "https://center.qoder.sh/algo/api/v3/user/refresh_token"
	// QoderUserInfoEndpoint is the URL for fetching user information
	QoderUserInfoEndpoint = "https://openapi.qoder.sh/api/v1/userinfo"
	// QoderCLIVersion is the CLI version for COSY authentication
	QoderCLIVersion = "0.9.0"
	// QoderMachineOS is the machine OS identifier for COSY authentication
	QoderMachineOS = "x86_64_linux"
)

// QoderTokenData represents the OAuth credentials from device flow polling
type QoderTokenData struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpireTime   int64  `json:"expire_time"`
	UserID       string `json:"user_id"`
	MachineToken string `json:"machineToken"`
	MachineType  string `json:"machineType"`
}

// DeviceFlowResponse represents the response from the device authorization endpoint
type DeviceFlowResponse struct {
	// VerificationURIComplete is the full URL with PKCE challenge for user authentication
	VerificationURIComplete string `json:"verification_uri_complete"`
	// CodeVerifier is the PKCE code verifier (generated locally, not from server)
	CodeVerifier string `json:"code_verifier"`
	// Nonce is the random nonce for the request
	Nonce string `json:"nonce"`
	// MachineID is the machine identifier
	MachineID string `json:"machine_id"`
}

// DeviceFlowPollResponse represents the token response from the polling endpoint.
//
// The upstream JSON keys are inconsistent across endpoints (poll vs refresh)
// and across observed payloads — we accept both snake_case and camelCase
// variants for the same logical field, then merge in selectAccessToken /
// selectMachineToken below. Empty zero-value fields lose to non-empty ones.
type DeviceFlowPollResponse struct {
	Data struct {
		AccessToken      string `json:"access_token"`
		Token            string `json:"token"`
		RefreshToken     string `json:"refresh_token"`
		RefreshTokenAlt  string `json:"refreshToken"`
		ExpireTime       int64  `json:"expire_time"`
		ExpireTimeAlt    int64  `json:"expireTime"`
		ExpiresAt        int64  `json:"expires_at"`
		ExpireTimeStr    string `json:"expireTimeStr"`
		ExpiresAtStr     string `json:"expires_at_str"`
		UserID           string `json:"user_id"`
		UserIDAlt        string `json:"userId"`
		MachineToken     string `json:"machineToken"`
		MachineTokenSnek string `json:"machine_token"`
		MachineType      string `json:"machineType"`
		MachineTypeSnek  string `json:"machine_type"`
	} `json:"data"`
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func firstNonZero(values ...int64) int64 {
	for _, v := range values {
		if v != 0 {
			return v
		}
	}
	return 0
}

// UserInfoResponse represents the response from user info endpoint
type UserInfoResponse struct {
	Data struct {
		Name     string `json:"name"`
		Username string `json:"username"`
		Email    string `json:"email"`
	} `json:"data"`
}

// RefreshTokenResponse represents the response from the refresh-token endpoint.
// Tolerates the same snake_case / camelCase split as DeviceFlowPollResponse.
type RefreshTokenResponse struct {
	Data struct {
		AccessToken     string `json:"access_token"`
		Token           string `json:"token"`
		RefreshToken    string `json:"refresh_token"`
		RefreshTokenAlt string `json:"refreshToken"`
		ExpireTime      int64  `json:"expire_time"`
		ExpireTimeAlt   int64  `json:"expireTime"`
		ExpiresAt       int64  `json:"expires_at"`
		ExpireTimeStr   string `json:"expireTimeStr"`
		ExpiresAtStr    string `json:"expires_at_str"`
	} `json:"data"`
}

// QoderAuth manages authentication and token handling for the Qoder API
type QoderAuth struct {
	httpClient *http.Client
}

// NewQoderAuth creates a new QoderAuth instance with a proxy-configured HTTP client
func NewQoderAuth(cfg *config.Config) *QoderAuth {
	return &QoderAuth{
		httpClient: util.SetProxy(&cfg.SDKConfig, &http.Client{}),
	}
}

// InitiateDeviceFlow starts the OAuth 2.0 device authorization flow.
// Qoder uses a simplified flow: generate PKCE locally and construct the login URL.
func (qa *QoderAuth) InitiateDeviceFlow(ctx context.Context) (*DeviceFlowResponse, error) {
	codeVerifier, codeChallenge, err := generateDevicePKCEPair()
	if err != nil {
		return nil, fmt.Errorf("failed to generate PKCE pair: %w", err)
	}

	nonce := uuid.New().String()
	machineID := generateMachineID()

	verificationURI := fmt.Sprintf(
		"%s?challenge=%s&challenge_method=S256&machine_id=%s&nonce=%s",
		QoderLoginURL,
		codeChallenge,
		machineID,
		nonce,
	)

	return &DeviceFlowResponse{
		VerificationURIComplete: verificationURI,
		CodeVerifier:            codeVerifier,
		Nonce:                   nonce,
		MachineID:               machineID,
	}, nil
}

// PollForToken polls the token endpoint with the device code to obtain an access token.
func (qa *QoderAuth) PollForToken(ctx context.Context, deviceFlow *DeviceFlowResponse) (*QoderTokenData, error) {
	if deviceFlow == nil || deviceFlow.CodeVerifier == "" || deviceFlow.Nonce == "" {
		return nil, fmt.Errorf("device flow is missing code verifier or nonce")
	}

	pollURL := fmt.Sprintf(
		"%s?nonce=%s&verifier=%s&challenge_method=S256",
		QoderOAuthTokenEndpoint,
		url.QueryEscape(deviceFlow.Nonce),
		url.QueryEscape(deviceFlow.CodeVerifier),
	)

	pollInterval := 2 * time.Second
	maxAttempts := 90 // 3 minutes max (180 seconds / 2 seconds per poll)

	for attempt := 0; attempt < maxAttempts; attempt++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		req, err := http.NewRequestWithContext(ctx, "GET", pollURL, nil)
		if err != nil {
			log.Warnf("Polling attempt %d/%d failed: %v", attempt+1, maxAttempts, err)
			time.Sleep(pollInterval)
			continue
		}

		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "Go-http-client/2.0")

		resp, err := qa.httpClient.Do(req)
		if err != nil {
			log.Warnf("Polling attempt %d/%d failed: %v", attempt+1, maxAttempts, err)
			time.Sleep(pollInterval)
			continue
		}

		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			log.Warnf("Polling attempt %d/%d failed: %v", attempt+1, maxAttempts, err)
			time.Sleep(pollInterval)
			continue
		}

		if resp.StatusCode == http.StatusAccepted {
			// Still pending - continue polling
			log.Debugf("Polling attempt %d/%d... (pending)", attempt+1, maxAttempts)
			time.Sleep(pollInterval)
			continue
		}

		if resp.StatusCode == http.StatusNotFound {
			// Token not created yet - user hasn't authenticated, continue polling
			log.Debugf("Polling attempt %d/%d... (token not found, waiting for auth)", attempt+1, maxAttempts)
			time.Sleep(pollInterval)
			continue
		}

		if resp.StatusCode != http.StatusOK {
			// Parse error response
			var errorData map[string]interface{}
			if err = json.Unmarshal(body, &errorData); err == nil {
				if errMsg, ok := errorData["message"].(string); ok {
					return nil, fmt.Errorf("device token poll failed: %s", errMsg)
				}
			}
			return nil, fmt.Errorf("device token poll failed: %d %s. Response: %s", resp.StatusCode, resp.Status, string(body))
		}

		// Success - parse token data
		var response DeviceFlowPollResponse
		if err = json.Unmarshal(body, &response); err != nil {
			return nil, fmt.Errorf("failed to parse token response: %w", err)
		}

		access := firstNonEmpty(response.Data.AccessToken, response.Data.Token)
		refresh := firstNonEmpty(response.Data.RefreshToken, response.Data.RefreshTokenAlt)
		userID := firstNonEmpty(response.Data.UserID, response.Data.UserIDAlt)
		machineToken := firstNonEmpty(response.Data.MachineToken, response.Data.MachineTokenSnek)
		machineType := firstNonEmpty(response.Data.MachineType, response.Data.MachineTypeSnek)
		expire := firstNonZero(response.Data.ExpireTime, response.Data.ExpireTimeAlt, response.Data.ExpiresAt)

		// If expire time still 0, fall back to string-form fields.
		if expire == 0 {
			if str := firstNonEmpty(response.Data.ExpireTimeStr, response.Data.ExpiresAtStr); str != "" {
				expire = parseExpiresAt(str)
			}
		}

		// Defensive: surface a clear error if the upstream returned 200 but
		// the token field is empty. Log raw body at debug level so we can see
		// the real response shape in deployed logs.
		if access == "" {
			log.Debugf("Qoder poll response with empty access token, body: %s", string(body))
			return nil, fmt.Errorf("device token poll returned empty access token; raw response keys may have changed")
		}

		return &QoderTokenData{
			AccessToken:  access,
			RefreshToken: refresh,
			ExpireTime:   expire,
			UserID:       userID,
			MachineToken: machineToken,
			MachineType:  machineType,
		}, nil
	}

	return nil, fmt.Errorf("authentication timeout. Please restart the authentication process")
}

// RefreshTokens exchanges a refresh token for a new access token
func (qa *QoderAuth) RefreshTokens(ctx context.Context, accessToken, refreshToken string) (*QoderTokenData, error) {
	reqBody := map[string]string{
		"refreshToken": refreshToken,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal refresh request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", QoderRefreshTokenEndpoint, strings.NewReader(string(bodyBytes)))
	if err != nil {
		return nil, fmt.Errorf("failed to create refresh request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")

	resp, err := qa.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("refresh request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read refresh response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var errorData map[string]interface{}
		if err = json.Unmarshal(body, &errorData); err == nil {
			if errMsg, ok := errorData["message"].(string); ok {
				return nil, fmt.Errorf("token refresh failed: %s", errMsg)
			}
		}
		return nil, fmt.Errorf("token refresh failed: %d %s. Response: %s", resp.StatusCode, resp.Status, string(body))
	}

	var response RefreshTokenResponse
	if err = json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("failed to parse refresh response: %w", err)
	}

	access := firstNonEmpty(response.Data.AccessToken, response.Data.Token)
	refresh := firstNonEmpty(response.Data.RefreshToken, response.Data.RefreshTokenAlt)
	expire := firstNonZero(response.Data.ExpireTime, response.Data.ExpireTimeAlt, response.Data.ExpiresAt)
	if expire == 0 {
		if str := firstNonEmpty(response.Data.ExpireTimeStr, response.Data.ExpiresAtStr); str != "" {
			expire = parseExpiresAt(str)
		}
	}

	if access == "" {
		log.Debugf("Qoder refresh response with empty access token, body: %s", string(body))
		return nil, fmt.Errorf("token refresh returned empty access token; raw response keys may have changed")
	}

	return &QoderTokenData{
		AccessToken:  access,
		RefreshToken: refresh,
		ExpireTime:   expire,
	}, nil
}

// FetchUserInfo fetches user information from the API
func (qa *QoderAuth) FetchUserInfo(ctx context.Context, accessToken string) (name, email string, err error) {
	req, err := http.NewRequestWithContext(ctx, "GET", QoderUserInfoEndpoint, nil)
	if err != nil {
		return "", "", fmt.Errorf("failed to create user info request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Go-http-client/2.0")

	resp, err := qa.httpClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("user info request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", fmt.Errorf("failed to read user info response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("user info request failed: %d %s", resp.StatusCode, resp.Status)
	}

	var response UserInfoResponse
	if err = json.Unmarshal(body, &response); err != nil {
		return "", "", fmt.Errorf("failed to parse user info response: %w", err)
	}

	name = response.Data.Name
	if name == "" {
		name = response.Data.Username
	}
	email = response.Data.Email

	return name, email, nil
}

// SaveUserInfo stores the user info alongside auth metadata for later use.
// This mirrors the behavior in qoder-direct.py where user_id is persisted
// and userinfo fields are updated if available.
func (qa *QoderAuth) SaveUserInfo(ctx context.Context, accessToken, userID, name, email string) (string, string) {
	if strings.TrimSpace(accessToken) == "" {
		return name, email
	}

	if strings.TrimSpace(name) == "" || strings.TrimSpace(email) == "" {
		if fetchedName, fetchedEmail, err := qa.FetchUserInfo(ctx, accessToken); err == nil {
			if strings.TrimSpace(name) == "" {
				name = fetchedName
			}
			if strings.TrimSpace(email) == "" {
				email = fetchedEmail
			}
		}
	}

	return name, email
}

// CreateTokenStorage creates a QoderTokenStorage object from a QoderTokenData object
func (qa *QoderAuth) CreateTokenStorage(tokenData *QoderTokenData, machineID string) *QoderTokenStorage {
	storage := &QoderTokenStorage{
		Token:        tokenData.AccessToken,
		RefreshToken: tokenData.RefreshToken,
		UserID:       tokenData.UserID,
		ExpireTime:   tokenData.ExpireTime,
		LastRefresh:  time.Now().Format(time.RFC3339),
		MachineID:    machineID,
		MachineToken: tokenData.MachineToken,
		MachineType:  tokenData.MachineType,
	}

	return storage
}

// UpdateTokenStorage updates an existing token storage with new token data
func (qa *QoderAuth) UpdateTokenStorage(storage *QoderTokenStorage, tokenData *QoderTokenData) {
	storage.Token = tokenData.AccessToken
	storage.RefreshToken = tokenData.RefreshToken
	storage.ExpireTime = tokenData.ExpireTime
	storage.LastRefresh = time.Now().Format(time.RFC3339)
}

// RefreshTokensWithRetry attempts to refresh tokens with a specified number of retries upon failure
func (qa *QoderAuth) RefreshTokensWithRetry(ctx context.Context, accessToken, refreshToken string, maxRetries int) (*QoderTokenData, error) {
	var lastErr error

	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			// Wait before retry
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}

		tokenData, err := qa.RefreshTokens(ctx, accessToken, refreshToken)
		if err == nil {
			return tokenData, nil
		}

		lastErr = err
		log.Warnf("Token refresh attempt %d/%d failed: %v", attempt+1, maxRetries, err)
	}

	return nil, fmt.Errorf("token refresh failed after %d attempts: %w", maxRetries, lastErr)
}
