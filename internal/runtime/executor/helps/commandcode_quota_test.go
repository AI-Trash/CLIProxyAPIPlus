package helps

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCommandCodePlanFor_LongestPrefixWins(t *testing.T) {
	// "individual-go" is a strict prefix of "individual-goat" and
	// "individual-pro" of "individual-pro-v1". The CLI sorts plan ids
	// longest-first, so the more specific plan must win.
	tests := []struct {
		planID  string
		name    string
		monthly float64
	}{
		{planID: "individual-goat", name: "GOAT", monthly: 70},
		{planID: "individual-go", name: "Go", monthly: 10},
		{planID: "individual-pro-v1", name: "Pro", monthly: 80},
		{planID: "individual-pro", name: "Pro", monthly: 30},
		{planID: "individual_go", name: "Go", monthly: 10},
		{planID: "INDIVIDUAL-GOAT", name: "GOAT", monthly: 70},
		{planID: "teams-pro", name: "Teams Pro", monthly: 40},
	}
	for _, tt := range tests {
		name, monthly, ok := commandCodePlanFor(tt.planID)
		if !ok {
			t.Errorf("commandCodePlanFor(%q) not resolved", tt.planID)
			continue
		}
		if name != tt.name || monthly != tt.monthly {
			t.Errorf("commandCodePlanFor(%q) = (%q, %v), want (%q, %v)", tt.planID, name, monthly, tt.name, tt.monthly)
		}
	}
	if _, _, ok := commandCodePlanFor("unknown-plan"); ok {
		t.Error("commandCodePlanFor(unknown-plan) resolved, want unresolved")
	}
	if _, _, ok := commandCodePlanFor(""); ok {
		t.Error("commandCodePlanFor(\"\") resolved, want unresolved")
	}
}

func TestCommandCodeBuildQuotaView_DerivedCredits(t *testing.T) {
	quota := &CommandCodeQuota{
		Whoami: &CommandCodeQuotaWhoami{
			User: &CommandCodeQuotaUser{UserName: "tester"},
		},
		Credits: &CommandCodeQuotaCredits{
			Credits: CommandCodeQuotaCreditBalances{MonthlyCredits: 7.5, PurchasedCredits: 2, FreeCredits: 0.5},
		},
		Subscription: &CommandCodeQuotaSubscription{
			Data: &CommandCodeQuotaSubscriptionData{
				PlanID:           "individual-go",
				Status:           "active",
				CurrentPeriodEnd: time.Now().Add(48 * time.Hour).Format(time.RFC3339),
			},
		},
		Summary: &CommandCodeQuotaSummary{TotalCost: 2.5},
	}
	commandCodeBuildQuotaView(quota)

	if quota.Plan == nil || quota.Plan.Name != "Go" || quota.Plan.MonthlyCredits != 10 {
		t.Fatalf("plan = %+v, want Go/10", quota.Plan)
	}
	usage := quota.Usage
	if usage == nil {
		t.Fatal("usage is nil")
	}
	if usage.TotalRemaining != 10 {
		t.Errorf("TotalRemaining = %v, want 10", usage.TotalRemaining)
	}
	// Active subscription: pool is the plan allowance (+ purchased/free), not spend.
	if usage.TotalPool != 12.5 {
		t.Errorf("TotalPool = %v, want 12.5", usage.TotalPool)
	}
	if usage.UsagePercent != 20 {
		t.Errorf("UsagePercent = %v, want 20", usage.UsagePercent)
	}
	if usage.UsageURL != "https://commandcode.ai/tester/settings/usage" {
		t.Errorf("UsageURL = %q", usage.UsageURL)
	}
	if usage.DaysLeft == nil || *usage.DaysLeft != 2 {
		t.Errorf("DaysLeft = %v, want 2", usage.DaysLeft)
	}
}

func TestCommandCodeBuildQuotaView_InactivePlanFallsBackToSpendPool(t *testing.T) {
	quota := &CommandCodeQuota{
		Credits: &CommandCodeQuotaCredits{
			Credits: CommandCodeQuotaCreditBalances{MonthlyCredits: 1},
		},
		Subscription: &CommandCodeQuotaSubscription{
			Data: &CommandCodeQuotaSubscriptionData{PlanID: "individual-go", Status: "canceled"},
		},
		Summary: &CommandCodeQuotaSummary{TotalCost: 4},
	}
	commandCodeBuildQuotaView(quota)

	if quota.Usage.TotalPool != 5 {
		t.Errorf("TotalPool = %v, want 5 (spend + remaining)", quota.Usage.TotalPool)
	}
	if quota.Usage.UsagePercent != 80 {
		t.Errorf("UsagePercent = %v, want 80", quota.Usage.UsagePercent)
	}
}

func TestCommandCodeBuildQuotaView_NoSubscriptionLeavesPlanUnset(t *testing.T) {
	quota := &CommandCodeQuota{}
	commandCodeBuildQuotaView(quota)
	if quota.Plan != nil {
		t.Errorf("plan = %+v, want nil without a subscription", quota.Plan)
	}
	if quota.Usage == nil || quota.Usage.UsagePercent != 0 {
		t.Errorf("usage = %+v, want zeroed snapshot", quota.Usage)
	}
}

func TestCommandCodeDaysLeft(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	if got := commandCodeDaysLeft("", now); got != nil {
		t.Errorf("commandCodeDaysLeft(\"\") = %v, want nil", got)
	}
	if got := commandCodeDaysLeft("not-a-timestamp", now); got != nil {
		t.Errorf("commandCodeDaysLeft(invalid) = %v, want nil", got)
	}
	if got := commandCodeDaysLeft(now.Add(-time.Hour).Format(time.RFC3339), now); got == nil || *got != 0 {
		t.Errorf("commandCodeDaysLeft(past) = %v, want 0", got)
	}
}

func TestFetchCommandCodeQuota_AggregatesEndpointsAndScopesSummary(t *testing.T) {
	var seen []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path+"?"+r.URL.RawQuery)
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("x-command-code-version"); got != CCCLIVersion {
			t.Errorf("x-command-code-version = %q, want %s", got, CCCLIVersion)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/alpha/whoami":
			if r.URL.Query().Get("limits") != "1" {
				t.Errorf("whoami limits = %q, want 1", r.URL.Query().Get("limits"))
			}
			_, _ = w.Write([]byte(`{"success":true,"user":{"userName":"tester"},"org":{"id":"org_1","login":"acme"}}`))
		case "/alpha/billing/credits":
			if r.URL.Query().Get("orgId") != "org_1" {
				t.Errorf("credits orgId = %q, want org_1", r.URL.Query().Get("orgId"))
			}
			_, _ = w.Write([]byte(`{"credits":{"monthlyCredits":5},"windowLimits":{"limited":true,"fiveHour":{"used":1,"cap":3,"resetAt":123}}}`))
		case "/alpha/billing/subscriptions":
			_, _ = w.Write([]byte(`{"success":true,"data":{"planId":"individual-go","status":"active","currentPeriodStart":"2026-09-01T00:00:00.000Z","currentPeriodEnd":"2099-10-01T00:00:00.000Z"}}`))
		case "/alpha/usage/summary":
			if got := r.URL.Query().Get("since"); got != "2026-09-01T00:00:00.000Z" {
				t.Errorf("summary since = %q, want billing period start", got)
			}
			_, _ = w.Write([]byte(`{"totalCost":1.25,"totalTokens":99}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	quota, err := FetchCommandCodeQuota(context.Background(), upstream.Client(), upstream.URL, "test-key")
	if err != nil {
		t.Fatalf("FetchCommandCodeQuota() error = %v", err)
	}
	if len(quota.Errors) != 0 {
		t.Fatalf("errors = %v, want none", quota.Errors)
	}
	if quota.Plan == nil || quota.Plan.ID != "individual-go" || quota.Plan.Name != "Go" {
		t.Errorf("plan = %+v", quota.Plan)
	}
	if quota.Usage.TotalRemaining != 5 || quota.Usage.TotalSpent != 1.25 {
		t.Errorf("usage = %+v", quota.Usage)
	}
	if quota.Usage.UsageURL != "https://commandcode.ai/acme/settings/usage" {
		t.Errorf("UsageURL = %q, want org login based url", quota.Usage.UsageURL)
	}
	if len(seen) != 4 {
		t.Fatalf("endpoint calls = %v, want all four", seen)
	}
}

func TestFetchCommandCodeQuota_CollectsPartialFailures(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/alpha/whoami" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":true}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(strings.Repeat("x", 400)))
	}))
	defer upstream.Close()

	quota, err := FetchCommandCodeQuota(context.Background(), upstream.Client(), upstream.URL, "test-key")
	if err != nil {
		t.Fatalf("FetchCommandCodeQuota() error = %v, want partial snapshot", err)
	}
	if len(quota.Errors) != 3 {
		t.Fatalf("errors = %v, want one per failing endpoint", quota.Errors)
	}
	for _, msg := range quota.Errors {
		if len(msg) > 512 {
			t.Errorf("error message not truncated: %d chars", len(msg))
		}
	}
	if quota.Whoami == nil || !quota.Whoami.Success {
		t.Errorf("whoami = %+v, want the successful payload retained", quota.Whoami)
	}
}

func TestFetchCommandCodeQuota_RequiresAPIKey(t *testing.T) {
	if _, err := FetchCommandCodeQuota(context.Background(), nil, "", ""); err == nil {
		t.Fatal("FetchCommandCodeQuota() with empty key = nil error, want failure")
	}
}

func TestFetchCommandCodeQuota_DecodesSandboxAndWindowLimits(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/alpha/billing/credits" {
			_, _ = w.Write([]byte(`{"credits":{"monthlyCredits":9},"sandboxAccess":true,"sandboxMinutes":{"limitMinutes":600,"usedMinutes":45,"resetAt":777},"windowLimits":{"limited":true,"exceeded":false,"weekly":{"used":2,"cap":6,"resetAt":888}}}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	quota, err := FetchCommandCodeQuota(context.Background(), upstream.Client(), upstream.URL, "test-key")
	if err != nil {
		t.Fatalf("FetchCommandCodeQuota() error = %v", err)
	}
	if quota.Credits == nil || !quota.Credits.SandboxAccess || quota.Credits.SandboxMinutes == nil {
		t.Fatalf("credits = %+v, want sandbox data", quota.Credits)
	}
	if quota.Credits.SandboxMinutes.UsedMinutes != 45 || quota.Credits.SandboxMinutes.LimitMinutes != 600 {
		t.Errorf("sandboxMinutes = %+v", quota.Credits.SandboxMinutes)
	}
	if quota.Credits.WindowLimits == nil || quota.Credits.WindowLimits.Weekly == nil {
		t.Fatalf("windowLimits = %+v", quota.Credits.WindowLimits)
	}
	if quota.Credits.WindowLimits.Weekly.ResetAt != 888 {
		t.Errorf("weekly resetAt = %d, want 888", quota.Credits.WindowLimits.Weekly.ResetAt)
	}
	encoded, errMarshal := json.Marshal(quota)
	if errMarshal != nil {
		t.Fatalf("marshal quota: %v", errMarshal)
	}
	if !strings.Contains(string(encoded), `"sandboxMinutes"`) {
		t.Errorf("serialized quota missing sandboxMinutes: %s", encoded)
	}
}
