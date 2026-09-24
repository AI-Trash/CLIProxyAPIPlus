package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func newCommandCodeQuotaTestHandler(t *testing.T) (*Handler, *coreauth.Manager, string) {
	t.Helper()
	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{
		ID:       "commandcode-quota-auth",
		Provider: "commandcode",
		Attributes: map[string]string{
			"api_key":  "test-key",
			"base_url": "http://127.0.0.1:1",
		},
	}
	authIndex := auth.EnsureIndex()
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}
	return NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager), manager, authIndex
}

func TestGetCommandCodeQuota_RequiresCredential(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/commandcode-quota", nil)
	h.GetCommandCodeQuota(ctx)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

func TestGetCommandCodeQuota_ReportsUpstreamFailures(t *testing.T) {
	// The credential points at an unroutable base_url: every endpoint fails, so
	// the handler must surface the aggregated errors rather than a 500.
	h, _, authIndex := newCommandCodeQuotaTestHandler(t)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/commandcode-quota?auth_index="+authIndex, nil)
	h.GetCommandCodeQuota(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var payload struct {
		Usage struct {
			TotalRemaining float64 `json:"totalRemaining"`
		} `json:"usage"`
		Errors []string `json:"errors"`
	}
	if errUnmarshal := json.Unmarshal(rec.Body.Bytes(), &payload); errUnmarshal != nil {
		t.Fatalf("failed to decode response: %v body=%s", errUnmarshal, rec.Body.String())
	}
	if len(payload.Errors) == 0 {
		t.Errorf("errors empty, want per-endpoint failures reported")
	}
	if payload.Usage.TotalRemaining != 0 {
		t.Errorf("totalRemaining = %v, want 0", payload.Usage.TotalRemaining)
	}
}

func TestGetCommandCodeQuota_UsesPerCredentialBaseURL(t *testing.T) {
	var hits int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/alpha/whoami":
			_, _ = w.Write([]byte(`{"success":true,"user":{"userName":"tester"}}`))
		case "/alpha/billing/credits":
			_, _ = w.Write([]byte(`{"credits":{"monthlyCredits":4}}`))
		case "/alpha/billing/subscriptions":
			_, _ = w.Write([]byte(`{"success":true,"data":{"planId":"individual-go","status":"active"}}`))
		case "/alpha/usage/summary":
			_, _ = w.Write([]byte(`{"totalCost":1}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{
		ID:         "commandcode-quota-base-url",
		Provider:   "commandcode",
		Attributes: map[string]string{"api_key": "test-key", "base_url": upstream.URL},
	}
	authIndex := auth.EnsureIndex()
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/commandcode-quota?auth_index="+authIndex, nil)
	h.GetCommandCodeQuota(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if hits != 4 {
		t.Errorf("upstream hits = %d, want 4", hits)
	}

	var payload struct {
		Plan struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"plan"`
		Usage struct {
			TotalRemaining float64 `json:"totalRemaining"`
			TotalSpent     float64 `json:"totalSpent"`
		} `json:"usage"`
	}
	if errUnmarshal := json.Unmarshal(rec.Body.Bytes(), &payload); errUnmarshal != nil {
		t.Fatalf("failed to decode response: %v body=%s", errUnmarshal, rec.Body.String())
	}
	if payload.Plan.ID != "individual-go" || payload.Plan.Name != "Go" {
		t.Errorf("plan = %+v", payload.Plan)
	}
	if payload.Usage.TotalRemaining != 4 || payload.Usage.TotalSpent != 1 {
		t.Errorf("usage = %+v", payload.Usage)
	}
}
