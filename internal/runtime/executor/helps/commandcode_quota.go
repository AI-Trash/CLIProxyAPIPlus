package helps

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// DefaultCommandCodeBaseURL is the production Command Code API origin. The
// executor and the management quota endpoint both default to it; per-credential
// overrides come from the base_url attribute/metadata key.
const DefaultCommandCodeBaseURL = "https://api.commandcode.ai"

// commandCodeStudioHost is the Command Code web console origin used to build
// the human-facing usage URL (getStudioUsageUrl in the CLI).
const commandCodeStudioHost = "https://commandcode.ai"

// CommandCodeQuota is the aggregated quota snapshot returned by the management
// quota endpoint. It mirrors the four endpoints combined by the official CLI's
// fetchUsageData() — /alpha/whoami?limits=1, /alpha/billing/credits,
// /alpha/billing/subscriptions and /alpha/usage/summary — plus the values the
// CLI derives in projectUsageView().
type CommandCodeQuota struct {
	// Plan is the subscription plan resolved from the plan id.
	Plan *CommandCodeQuotaPlan `json:"plan,omitempty"`
	// Usage holds the derived credit totals and the consumption percentage.
	Usage *CommandCodeQuotaUsage `json:"usage,omitempty"`
	// Whoami is the /alpha/whoami payload (identity and org spend limits).
	Whoami *CommandCodeQuotaWhoami `json:"whoami,omitempty"`
	// Credits is the /alpha/billing/credits payload (balances and window limits).
	Credits *CommandCodeQuotaCredits `json:"credits,omitempty"`
	// Subscription is the /alpha/billing/subscriptions payload.
	Subscription *CommandCodeQuotaSubscription `json:"subscription,omitempty"`
	// Summary is the /alpha/usage/summary payload (spend for the period).
	Summary *CommandCodeQuotaSummary `json:"summary,omitempty"`
	// Errors collects per-endpoint failures. The CLI treats them as non-fatal
	// (safeUsageFetch) so a partial snapshot is still returned.
	Errors []string `json:"errors,omitempty"`
}

// CommandCodeQuotaPlan is the resolved subscription plan.
type CommandCodeQuotaPlan struct {
	ID             string  `json:"id,omitempty"`
	Name           string  `json:"name,omitempty"`
	MonthlyCredits float64 `json:"monthlyCredits"`
	Status         string  `json:"status,omitempty"`
}

// CommandCodeQuotaUsage holds the derived credit totals shown to the user.
type CommandCodeQuotaUsage struct {
	MonthlyRemaining   float64 `json:"monthlyRemaining"`
	PurchasedRemaining float64 `json:"purchasedRemaining"`
	FreeRemaining      float64 `json:"freeRemaining"`
	TotalRemaining     float64 `json:"totalRemaining"`
	TotalSpent         float64 `json:"totalSpent"`
	TotalPool          float64 `json:"totalPool"`
	UsagePercent       float64 `json:"usagePercent"`
	UsageURL           string  `json:"usageUrl,omitempty"`
	UsageURLDisplay    string  `json:"usageUrlDisplay,omitempty"`
	DaysLeft           *int    `json:"daysLeft,omitempty"`
}

// CommandCodeQuotaWhoami is the /alpha/whoami response. orgLimits is only
// populated when the request asks for limits.
type CommandCodeQuotaWhoami struct {
	Success   bool                            `json:"success"`
	User      *CommandCodeQuotaUser           `json:"user,omitempty"`
	Org       *CommandCodeQuotaOrg            `json:"org,omitempty"`
	OrgLimits []CommandCodeQuotaOrgSpendLimit `json:"orgLimits,omitempty"`
}

// CommandCodeQuotaUser identifies the authenticated account.
type CommandCodeQuotaUser struct {
	ID       string `json:"id,omitempty"`
	Name     string `json:"name,omitempty"`
	Email    string `json:"email,omitempty"`
	UserName string `json:"userName,omitempty"`
}

// CommandCodeQuotaOrg identifies the organization when the account belongs to one.
type CommandCodeQuotaOrg struct {
	ID    string `json:"id,omitempty"`
	Login string `json:"login,omitempty"`
	Name  string `json:"name,omitempty"`
}

// CommandCodeQuotaOrgSpendLimit is one org-wide or per-model spend limit.
type CommandCodeQuotaOrgSpendLimit struct {
	Scope         string  `json:"scope,omitempty"`
	Model         string  `json:"model,omitempty"`
	ModelLabel    string  `json:"modelLabel,omitempty"`
	Limit         float64 `json:"limit"`
	Spent         float64 `json:"spent"`
	Exceeded      bool    `json:"exceeded"`
	ResetInterval string  `json:"resetInterval,omitempty"`
	ResetAt       string  `json:"resetAt,omitempty"`
}

// CommandCodeQuotaCredits is the /alpha/billing/credits response.
type CommandCodeQuotaCredits struct {
	Credits        CommandCodeQuotaCreditBalances  `json:"credits"`
	WindowLimits   *CommandCodeQuotaWindowLimits   `json:"windowLimits,omitempty"`
	SandboxAccess  bool                            `json:"sandboxAccess"`
	SandboxMinutes *CommandCodeQuotaSandboxMinutes `json:"sandboxMinutes,omitempty"`
}

// CommandCodeQuotaCreditBalances is the credit bucket breakdown.
type CommandCodeQuotaCreditBalances struct {
	BelowThreshold   bool    `json:"belowThreshold"`
	CreditThreshold  float64 `json:"creditThreshold"`
	MonthlyCredits   float64 `json:"monthlyCredits"`
	PurchasedCredits float64 `json:"purchasedCredits"`
	FreeCredits      float64 `json:"freeCredits"`
	PlanID           string  `json:"planId,omitempty"`
}

// CommandCodeQuotaWindowLimits holds the rolling rate-limit windows.
type CommandCodeQuotaWindowLimits struct {
	Limited  bool                    `json:"limited"`
	Exceeded *bool                   `json:"exceeded,omitempty"`
	FiveHour *CommandCodeQuotaWindow `json:"fiveHour,omitempty"`
	Weekly   *CommandCodeQuotaWindow `json:"weekly,omitempty"`
}

// CommandCodeQuotaWindow is a single rolling usage window.
type CommandCodeQuotaWindow struct {
	Used     float64 `json:"used"`
	Cap      float64 `json:"cap"`
	Exceeded bool    `json:"exceeded"`
	ResetAt  int64   `json:"resetAt,omitempty"`
}

// CommandCodeQuotaSandboxMinutes reports sandbox minute consumption.
type CommandCodeQuotaSandboxMinutes struct {
	LimitMinutes float64 `json:"limitMinutes"`
	UsedMinutes  float64 `json:"usedMinutes"`
	ResetAt      int64   `json:"resetAt,omitempty"`
}

// CommandCodeQuotaSubscription is the /alpha/billing/subscriptions response.
type CommandCodeQuotaSubscription struct {
	Success bool                              `json:"success"`
	Data    *CommandCodeQuotaSubscriptionData `json:"data,omitempty"`
}

// CommandCodeQuotaSubscriptionData is the subscription record.
type CommandCodeQuotaSubscriptionData struct {
	ID                 string `json:"id,omitempty"`
	Status             string `json:"status,omitempty"`
	PlanID             string `json:"planId,omitempty"`
	CreatedAt          string `json:"createdAt,omitempty"`
	CurrentPeriodStart string `json:"currentPeriodStart,omitempty"`
	CurrentPeriodEnd   string `json:"currentPeriodEnd,omitempty"`
	CancelAtPeriodEnd  bool   `json:"cancelAtPeriodEnd,omitempty"`
}

// CommandCodeQuotaSummary is the /alpha/usage/summary response.
type CommandCodeQuotaSummary struct {
	TotalCount            int64   `json:"totalCount"`
	TotalCost             float64 `json:"totalCost"`
	AverageCost           float64 `json:"averageCost"`
	SuccessRate           float64 `json:"successRate"`
	CompletedCount        int64   `json:"completedCount"`
	FailedCount           int64   `json:"failedCount"`
	TotalTokensIn         int64   `json:"totalTokensIn"`
	TotalTokensOut        int64   `json:"totalTokensOut"`
	TotalTokens           int64   `json:"totalTokens"`
	TotalCredits          float64 `json:"totalCredits"`
	TotalFreeCredits      float64 `json:"totalFreeCredits"`
	TotalMonthlyCredits   float64 `json:"totalMonthlyCredits"`
	TotalPurchasedCredits float64 `json:"totalPurchasedCredits"`
	PeriodBasis           string  `json:"periodBasis,omitempty"`
}

// commandCodePlanMonthlyCredits mirrors rr (getPlanTotalCredits) in the
// official CLI: the monthly credit allowance included in each plan.
var commandCodePlanMonthlyCredits = map[string]float64{
	"individual-go":       10,
	"individual-goat":     70,
	"individual-pro":      30,
	"individual-pro-v1":   80,
	"individual-provider": 15,
	"individual-max":      150,
	"individual-ultra":    300,
	"teams-pro":           40,
}

// commandCodePlanDisplayNames mirrors or (getPlanDisplayName).
var commandCodePlanDisplayNames = map[string]string{
	"individual-go":       "Go",
	"individual-goat":     "GOAT",
	"individual-pro":      "Pro",
	"individual-pro-v1":   "Pro",
	"individual-provider": "Provider",
	"individual-max":      "Max",
	"individual-ultra":    "Ultra",
	"teams-pro":           "Teams Pro",
}

// commandCodePlanIDs holds the plan ids sorted longest-first, matching sr
// (Object.keys(rr).sort((a, b) => b.length - a.length)) in the official CLI.
var commandCodePlanIDs = func() []string {
	ids := make([]string, 0, len(commandCodePlanMonthlyCredits))
	for id := range commandCodePlanMonthlyCredits {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if len(ids[i]) != len(ids[j]) {
			return len(ids[i]) > len(ids[j])
		}
		return ids[i] < ids[j]
	})
	return ids
}()

// commandCodePlanFor mirrors getPlanInfo: normalize the plan id (lowercase,
// underscores to dashes) and match the longest plan id that prefixes it.
func commandCodePlanFor(planID string) (name string, monthlyCredits float64, ok bool) {
	normalized := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(planID)), "_", "-")
	if normalized == "" {
		return "", 0, false
	}
	for _, id := range commandCodePlanIDs {
		if strings.HasPrefix(normalized, id) {
			return commandCodePlanDisplayNames[id], commandCodePlanMonthlyCredits[id], true
		}
	}
	return "", 0, false
}

// FetchCommandCodeQuota aggregates the Command Code quota endpoints into one
// snapshot.
//
// It mirrors fetchUsageData() in the official CLI: /alpha/whoami?limits=1
// establishes the identity and org id, then /alpha/billing/credits,
// /alpha/billing/subscriptions and /alpha/usage/summary supply the balances,
// rolling window limits, plan and spend figures. Individual endpoint failures
// are collected in Errors instead of failing the whole call, matching the
// CLI's safeUsageFetch behaviour. An empty apiKey is the only hard error.
func FetchCommandCodeQuota(ctx context.Context, client *http.Client, baseURL, apiKey string) (*CommandCodeQuota, error) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, fmt.Errorf("commandcode: api key is empty")
	}
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = DefaultCommandCodeBaseURL
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}

	quota := &CommandCodeQuota{}
	record := func(err error) {
		if err != nil {
			quota.Errors = append(quota.Errors, err.Error())
		}
	}

	// 1. Identity (and org id used to scope the remaining requests).
	whoami, errWhoami := commandCodeGetJSON[CommandCodeQuotaWhoami](ctx, client, baseURL, apiKey,
		"/alpha/whoami", url.Values{"limits": {"1"}})
	if errWhoami != nil {
		record(errWhoami)
	} else {
		quota.Whoami = whoami
	}

	orgID := ""
	if quota.Whoami != nil && quota.Whoami.Org != nil {
		orgID = strings.TrimSpace(quota.Whoami.Org.ID)
	}
	scoped := url.Values{}
	if orgID != "" {
		scoped.Set("orgId", orgID)
	}

	// 2. Credit balances and rolling window limits.
	credits, errCredits := commandCodeGetJSON[CommandCodeQuotaCredits](ctx, client, baseURL, apiKey,
		"/alpha/billing/credits", scoped)
	if errCredits != nil {
		record(errCredits)
	} else {
		quota.Credits = credits
	}

	// 3. Subscription (carries the billing period used to scope the summary).
	subscription, errSubscription := commandCodeGetJSON[CommandCodeQuotaSubscription](ctx, client, baseURL, apiKey,
		"/alpha/billing/subscriptions", scoped)
	if errSubscription != nil {
		record(errSubscription)
	} else {
		quota.Subscription = subscription
	}

	// 4. Spend summary for the current billing period.
	summaryParams := url.Values{}
	if orgID != "" {
		summaryParams.Set("orgId", orgID)
	}
	if quota.Subscription != nil && quota.Subscription.Data != nil {
		if since := strings.TrimSpace(quota.Subscription.Data.CurrentPeriodStart); since != "" {
			summaryParams.Set("since", since)
		}
	}
	summary, errSummary := commandCodeGetJSON[CommandCodeQuotaSummary](ctx, client, baseURL, apiKey,
		"/alpha/usage/summary", summaryParams)
	if errSummary != nil {
		record(errSummary)
	} else {
		quota.Summary = summary
	}

	commandCodeBuildQuotaView(quota)
	return quota, nil
}

// commandCodeBuildQuotaView fills Plan and Usage from the fetched payloads,
// mirroring projectUsageView() in the official CLI.
func commandCodeBuildQuotaView(quota *CommandCodeQuota) {
	if quota == nil {
		return
	}
	var subscription *CommandCodeQuotaSubscriptionData
	if quota.Subscription != nil {
		subscription = quota.Subscription.Data
	}

	// projectUsageView keeps plan null unless a subscription plan id resolves.
	var plan *CommandCodeQuotaPlan
	if subscription != nil {
		if planID := strings.TrimSpace(subscription.PlanID); planID != "" {
			plan = &CommandCodeQuotaPlan{ID: planID, Status: strings.TrimSpace(subscription.Status)}
			if name, monthly, ok := commandCodePlanFor(planID); ok {
				plan.Name = name
				plan.MonthlyCredits = monthly
			}
		}
	}
	quota.Plan = plan

	usage := &CommandCodeQuotaUsage{}
	if quota.Credits != nil {
		balances := quota.Credits.Credits
		usage.MonthlyRemaining = math.Max(0, balances.MonthlyCredits)
		usage.PurchasedRemaining = math.Max(0, balances.PurchasedCredits)
		usage.FreeRemaining = math.Max(0, balances.FreeCredits)
	}
	usage.TotalRemaining = usage.MonthlyRemaining + usage.PurchasedRemaining + usage.FreeRemaining
	if quota.Summary != nil {
		usage.TotalSpent = math.Max(0, quota.Summary.TotalCost)
	}

	// The CLI compares consumption against the plan allowance while the
	// subscription is active, and against spend plus remaining credits
	// otherwise (projectUsageView totalPool).
	planAllowance := 0.0
	if plan != nil && plan.MonthlyCredits > 0 && strings.EqualFold(plan.Status, "active") {
		planAllowance = plan.MonthlyCredits
	}
	if planAllowance > 0 {
		usage.TotalPool = math.Max(planAllowance, usage.MonthlyRemaining) + usage.PurchasedRemaining + usage.FreeRemaining
	} else {
		usage.TotalPool = usage.TotalSpent + usage.TotalRemaining
	}
	if usage.TotalRemaining > 0 || usage.TotalSpent > 0 {
		usage.UsagePercent = math.Round(commandCodeUsagePercent(usage.TotalPool-usage.TotalRemaining, usage.TotalPool)*100) / 100
	}

	if account := commandCodeUsageAccount(quota.Whoami); account != "" {
		usage.UsageURL = fmt.Sprintf("%s/%s/settings/usage", commandCodeStudioHost, account)
		usage.UsageURLDisplay = account + "/settings/usage"
	}
	if subscription != nil {
		usage.DaysLeft = commandCodeDaysLeft(subscription.CurrentPeriodEnd, time.Now())
	}
	quota.Usage = usage
}

// commandCodeUsagePercent mirrors getUsagePercent: 0 when the total is not
// positive, clamped to 100.
func commandCodeUsagePercent(used, total float64) float64 {
	if total <= 0 {
		return 0
	}
	return math.Min(used/total*100, 100)
}

// commandCodeDaysLeft mirrors getDaysRemainingFromNow: whole days until the
// period end, floored at zero; nil when the timestamp is absent or invalid.
func commandCodeDaysLeft(periodEnd string, now time.Time) *int {
	trimmed := strings.TrimSpace(periodEnd)
	if trimmed == "" {
		return nil
	}
	end, err := time.Parse(time.RFC3339, trimmed)
	if err != nil {
		return nil
	}
	days := int(math.Ceil(end.Sub(now).Hours() / 24))
	if days < 0 {
		days = 0
	}
	return &days
}

// commandCodeUsageAccount mirrors getStudioUsageUrl: prefer the org login, then
// the user name.
func commandCodeUsageAccount(whoami *CommandCodeQuotaWhoami) string {
	if whoami == nil {
		return ""
	}
	if whoami.Org != nil {
		if login := strings.TrimSpace(whoami.Org.Login); login != "" {
			return login
		}
	}
	if whoami.User != nil {
		return strings.TrimSpace(whoami.User.UserName)
	}
	return ""
}

// commandCodeGetJSON performs one authenticated GET against the Command Code
// API and decodes the JSON response. The header set mirrors
// buildCommandApiHeaders() in the official CLI for API-client requests
// (x-cli-environment, x-command-code-version, User-Agent: cli).
func commandCodeGetJSON[T any](ctx context.Context, client *http.Client, baseURL, apiKey, endpoint string, params url.Values) (*T, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	target := baseURL + endpoint
	if len(params) > 0 {
		target += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("commandcode: build request for %s: %w", endpoint, err)
	}
	setLower := func(key, value string) {
		req.Header.Del(key)
		req.Header[key] = []string{value}
	}
	setLower("content-type", "application/json")
	setLower("authorization", "Bearer "+apiKey)
	setLower("x-cli-environment", "production")
	setLower("x-command-code-version", CCCLIVersion)
	setLower("User-Agent", "cli")

	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("commandcode: %s request failed: %w", endpoint, errDo)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	body, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		return nil, fmt.Errorf("commandcode: read %s response: %w", endpoint, errRead)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("commandcode: %s returned status %d: %s", endpoint, resp.StatusCode, truncateQuotaBody(body))
	}

	var out T
	if errUnmarshal := json.Unmarshal(body, &out); errUnmarshal != nil {
		return nil, fmt.Errorf("commandcode: decode %s response: %w", endpoint, errUnmarshal)
	}
	return &out, nil
}

// truncateQuotaBody bounds an error payload so a large upstream body cannot
// flood the management response.
func truncateQuotaBody(body []byte) string {
	const maxLen = 256
	trimmed := strings.TrimSpace(string(body))
	if len(trimmed) <= maxLen {
		return trimmed
	}
	return trimmed[:maxLen] + "..."
}
