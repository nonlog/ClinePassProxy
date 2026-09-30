// Package cline reads Cline's own account API.
//
// These are the endpoints the Cline dashboard uses, so the numbers shown here
// are the account's official ones rather than an estimate derived from the
// proxy's own request history:
//
//	GET /users/me
//	GET /users/me/plan
//	GET /users/me/plan/usage-limits
//	GET /users/{id}/usages/daily?startDate&endDate   (range must be <= 31 days)
//	GET /users/{id}/balance
//
// Every response is wrapped in {"success":true,"data":...}; credits are
// reported in micro-USD (1e-6 USD per unit).
package cline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultBaseURL is Cline's public API root.
const DefaultBaseURL = "https://api.cline.bot/api/v1"

const (
	requestTimeout  = 20 * time.Second
	usageWindowDays = 31
	microUSD        = 1_000_000.0
)

// Window is one rolling limit reported by the usage-limits endpoint.
type Window struct {
	Type        string  `json:"type"`
	Label       string  `json:"label"`
	PercentUsed float64 `json:"percent_used"`
	ResetsAt    string  `json:"resets_at,omitempty"`
	ResetsIn    string  `json:"resets_in,omitempty"`
}

// Tokens carries the official account totals for the last 31 days.
type Tokens struct {
	FromDate     string  `json:"from_date"`
	ToDate       string  `json:"to_date"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	TotalTokens  int64   `json:"total_tokens"`
	CostUSD      float64 `json:"cost_usd"`
	BalanceUSD   float64 `json:"balance_usd"`
	Requests     int64   `json:"requests"`
	// Series is one total-token value per day across the same range, oldest
	// first, for the dashboard sparkline.
	Series []int64 `json:"series,omitempty"`
}

// Snapshot is the official view of one Cline credential.
type Snapshot struct {
	CredentialID string   `json:"credential_id"`
	Label        string   `json:"label,omitempty"`
	Available    bool     `json:"available"`
	Rejected     bool     `json:"rejected,omitempty"`
	Account      string   `json:"account,omitempty"`
	PlanName     string   `json:"plan_name,omitempty"`
	PlanPrice    string   `json:"plan_price,omitempty"`
	Limits       []Window `json:"limits"`
	Tokens       Tokens   `json:"tokens"`
	TokensError  string   `json:"tokens_error,omitempty"`
	FetchedAt    string   `json:"fetched_at,omitempty"`
	Error        string   `json:"error,omitempty"`
}

// statusError carries the upstream HTTP status so a rejected credential can be
// told apart from a transient failure.
type statusError struct{ Status int }

func (e statusError) Error() string { return fmt.Sprintf("Cline returned HTTP %d", e.Status) }

// Client reads one Cline account.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// NewClient builds a client for one credential. httpClient is required so the
// caller controls the transport (and therefore the credential's proxy);
// baseURL falls back to Cline's public API when empty.
func NewClient(baseURL, apiKey string, httpClient *http.Client) *Client {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		base = DefaultBaseURL
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: requestTimeout}
	}
	return &Client{baseURL: base, apiKey: strings.TrimSpace(apiKey), http: httpClient}
}

// Available reports whether the client holds enough to make a call.
func (c *Client) Available() bool {
	return c != nil && c.apiKey != ""
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	if !c.Available() {
		return errors.New("no Cline API key configured for this credential")
	}
	requestCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+c.apiKey)
	request.Header.Set("Accept", "application/json")

	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return statusError{Status: response.StatusCode}
	}
	var envelope struct {
		Success bool            `json:"success"`
		Error   string          `json:"error"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("decode Cline response: %w", err)
	}
	if !envelope.Success {
		if strings.TrimSpace(envelope.Error) == "" {
			return errors.New("Cline reported a failed request")
		}
		return errors.New(envelope.Error)
	}
	if out == nil {
		return nil
	}
	if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return errors.New("Cline returned no data")
	}
	return json.Unmarshal(envelope.Data, out)
}

type account struct {
	ID        string `json:"id"`
	CreatedAt string `json:"createdAt"`
}

func (c *Client) me(ctx context.Context) (account, error) {
	var out account
	err := c.get(ctx, "/users/me", &out)
	return out, err
}

// UsageLimits returns the rolling quota windows.
func (c *Client) UsageLimits(ctx context.Context) ([]Window, error) {
	var payload struct {
		Limits []struct {
			Type        string  `json:"type"`
			PercentUsed float64 `json:"percentUsed"`
			ResetsAt    string  `json:"resetsAt"`
		} `json:"limits"`
	}
	if err := c.get(ctx, "/users/me/plan/usage-limits", &payload); err != nil {
		return nil, err
	}
	out := make([]Window, 0, len(payload.Limits))
	for _, item := range payload.Limits {
		window := Window{
			Type:        item.Type,
			Label:       WindowLabel(item.Type),
			PercentUsed: item.PercentUsed,
			ResetsAt:    item.ResetsAt,
		}
		if resetAt, err := time.Parse(time.RFC3339Nano, item.ResetsAt); err == nil {
			window.ResetsIn = time.Until(resetAt).Round(time.Minute).String()
		}
		out = append(out, window)
	}
	return out, nil
}

type planInfo struct {
	Name     string
	PriceUSD float64
}

func (c *Client) plan(ctx context.Context) (planInfo, error) {
	var payload struct {
		Plan struct {
			DisplayName       string `json:"displayName"`
			PricePerSeatCents int64  `json:"pricePerSeatCents"`
		} `json:"plan"`
	}
	if err := c.get(ctx, "/users/me/plan", &payload); err != nil {
		return planInfo{}, err
	}
	return planInfo{
		Name:     payload.Plan.DisplayName,
		PriceUSD: float64(payload.Plan.PricePerSeatCents) / 100,
	}, nil
}

type dailyUsageItem struct {
	Date             string `json:"date"`
	CostUnits        int64  `json:"costUsd"`
	PromptTokens     int64  `json:"promptTokens"`
	CompletionTokens int64  `json:"completionTokens"`
}

func (c *Client) dailyUsage(ctx context.Context, userID, from, to string) ([]dailyUsageItem, error) {
	var payload struct {
		Items []dailyUsageItem `json:"items"`
	}
	path := fmt.Sprintf("/users/%s/usages/daily?startDate=%s&endDate=%s", userID, from, to)
	if err := c.get(ctx, path, &payload); err != nil {
		return nil, err
	}
	return payload.Items, nil
}

func (c *Client) balance(ctx context.Context, userID string) (float64, error) {
	var payload struct {
		BalanceUnits int64 `json:"balance"`
	}
	if err := c.get(ctx, "/users/"+userID+"/balance", &payload); err != nil {
		return 0, err
	}
	return float64(payload.BalanceUnits) / microUSD, nil
}

// Fetch reads one credential's official snapshot. A failure is reported inside
// the snapshot instead of as an error, so one bad credential cannot blank out
// the page for the others.
func (c *Client) Fetch(ctx context.Context) Snapshot {
	snapshot := Snapshot{Limits: []Window{}, FetchedAt: time.Now().UTC().Format(time.RFC3339)}
	if !c.Available() {
		snapshot.Error = "no Cline API key configured for this credential"
		return snapshot
	}

	me, err := c.me(ctx)
	if err != nil {
		snapshot.Rejected = isRejected(err)
		snapshot.Error = err.Error()
		return snapshot
	}
	snapshot.Account = MaskAccountID(me.ID)

	limits, err := c.UsageLimits(ctx)
	if err != nil {
		snapshot.Rejected = snapshot.Rejected || isRejected(err)
		snapshot.Error = err.Error()
		return snapshot
	}
	snapshot.Limits = limits
	snapshot.Available = true

	if plan, err := c.plan(ctx); err == nil {
		snapshot.PlanName = plan.Name
		if plan.PriceUSD > 0 {
			snapshot.PlanPrice = fmt.Sprintf("$%.2f / 月", plan.PriceUSD)
		}
	}

	from, to := UsageRange(time.Now())
	items, err := c.dailyUsage(ctx, me.ID, from, to)
	if err != nil {
		snapshot.TokensError = err.Error()
		return snapshot
	}
	snapshot.Tokens = SumDailyUsage(items, from, to)
	if balance, err := c.balance(ctx, me.ID); err == nil {
		snapshot.Tokens.BalanceUSD = balance
	}
	return snapshot
}

// UsageRange returns the date range the daily endpoint accepts: it rejects
// ranges above 31 days, so the window is today-30 .. today (UTC).
func UsageRange(now time.Time) (string, string) {
	utc := now.UTC()
	return utc.AddDate(0, 0, -(usageWindowDays - 1)).Format("2006-01-02"), utc.Format("2006-01-02")
}

// SumDailyUsage folds the daily rows into the account totals. Rows outside the
// range are ignored, and the per-day series is filled positionally so a gap
// stays a zero instead of shifting the later days.
func SumDailyUsage(items []dailyUsageItem, from, to string) Tokens {
	totals := Tokens{FromDate: from, ToDate: to}
	start, err := time.Parse("2006-01-02", from)
	if err != nil {
		start = time.Time{}
	}
	end, err := time.Parse("2006-01-02", to)
	if err != nil {
		end = time.Time{}
	}
	if !start.IsZero() && !end.IsZero() {
		days := int(end.Sub(start).Hours()/24) + 1
		if days > 0 {
			totals.Series = make([]int64, days)
		}
	}
	for _, item := range items {
		day, err := time.Parse("2006-01-02", strings.TrimSpace(item.Date))
		if err != nil {
			continue
		}
		if !start.IsZero() && day.Before(start) {
			continue
		}
		if !end.IsZero() && day.After(end) {
			continue
		}
		totals.InputTokens += item.PromptTokens
		totals.OutputTokens += item.CompletionTokens
		totals.CostUSD += float64(item.CostUnits) / microUSD
		totals.Requests++
		if totals.Series != nil {
			if index := int(day.Sub(start).Hours() / 24); index >= 0 && index < len(totals.Series) {
				totals.Series[index] += item.PromptTokens + item.CompletionTokens
			}
		}
	}
	totals.TotalTokens = totals.InputTokens + totals.OutputTokens
	return totals
}

// isRejected reports a credential the upstream refused.
func isRejected(err error) bool {
	var status statusError
	if errors.As(err, &status) {
		return status.Status == http.StatusUnauthorized || status.Status == http.StatusForbidden
	}
	return false
}

// Rejected reports the same classification for an error returned from Fetch.
func Rejected(err error) bool { return isRejected(err) }

// WindowLabel renders one limit type in the words the dashboard uses.
func WindowLabel(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "five_hour", "five_hours", "5_hour", "5h":
		return "5 小时滚动窗口"
	case "weekly", "week":
		return "本周额度"
	case "monthly", "month":
		return "本月额度"
	default:
		return kind
	}
}

// MaskAccountID shortens an account id for display.
func MaskAccountID(id string) string {
	trimmed := strings.TrimSpace(id)
	if len(trimmed) <= 12 {
		return trimmed
	}
	return trimmed[:8] + "…" + trimmed[len(trimmed)-4:]
}
