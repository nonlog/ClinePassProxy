package cline

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeCline serves the four endpoints Fetch uses, wrapped in Cline's
// {"success":true,"data":...} envelope.
func fakeCline(t *testing.T, handler func(path string, query map[string]string) (int, any)) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"success":false,"error":"unauthorized"}`))
			return
		}
		query := map[string]string{}
		for name, values := range r.URL.Query() {
			if len(values) > 0 {
				query[name] = values[0]
			}
		}
		status, payload := handler(r.URL.Path, query)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		body, err := json.Marshal(map[string]any{"success": status < 400, "data": payload})
		if err != nil {
			t.Errorf("encode fixture: %v", err)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	return server
}

func standardHandler(path string, query map[string]string) (int, any) {
	switch path {
	case "/users/me":
		return http.StatusOK, map[string]any{"id": "user-1234567890abcdef", "createdAt": "2026-01-02T03:04:05Z"}
	case "/users/me/plan/usage-limits":
		return http.StatusOK, map[string]any{"limits": []map[string]any{
			{"type": "five_hour", "percentUsed": 1.0, "resetsAt": time.Now().Add(3 * time.Hour).UTC().Format(time.RFC3339)},
			{"type": "weekly", "percentUsed": 42.5, "resetsAt": time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)},
			{"type": "monthly", "percentUsed": 7.25, "resetsAt": time.Now().Add(300 * time.Hour).UTC().Format(time.RFC3339)},
		}}
	case "/users/me/plan":
		return http.StatusOK, map[string]any{"plan": map[string]any{"displayName": "Cline Pass Pro", "pricePerSeatCents": 1999}}
	case "/users/user-1234567890abcdef/usages/daily":
		to := query["endDate"]
		return http.StatusOK, map[string]any{"items": []map[string]any{
			{"date": to, "costUsd": 1250000, "promptTokens": 800, "completionTokens": 20},
			{"date": to, "costUsd": 250000, "promptTokens": 100, "completionTokens": 5},
			{"date": "2020-01-01", "costUsd": 9999999, "promptTokens": 999999, "completionTokens": 1},
		}}
	case "/users/user-1234567890abcdef/balance":
		return http.StatusOK, map[string]any{"balance": 42500000}
	}
	return http.StatusNotFound, map[string]any{"error": "not found"}
}

func TestFetchBuildsOfficialSnapshot(t *testing.T) {
	server := fakeCline(t, standardHandler)
	client := NewClient(server.URL, "sk_test_key", server.Client())

	snapshot := client.Fetch(context.Background())
	if !snapshot.Available {
		t.Fatalf("snapshot unavailable: %+v", snapshot)
	}
	if snapshot.PlanName != "Cline Pass Pro" || snapshot.PlanPrice != "$19.99 / 月" {
		t.Fatalf("plan = %q %q", snapshot.PlanName, snapshot.PlanPrice)
	}
	if snapshot.Account != "user-123…cdef" {
		t.Fatalf("account id not masked: %q", snapshot.Account)
	}
	if len(snapshot.Limits) != 3 {
		t.Fatalf("limits = %+v", snapshot.Limits)
	}
	if snapshot.Limits[0].Label != "5 小时滚动窗口" || snapshot.Limits[1].Label != "本周额度" || snapshot.Limits[2].Label != "本月额度" {
		t.Fatalf("limit labels = %+v", snapshot.Limits)
	}
	if snapshot.Limits[1].PercentUsed != 42.5 || snapshot.Limits[1].ResetsIn == "" {
		t.Fatalf("weekly window = %+v", snapshot.Limits[1])
	}
	// The 2020 row is outside the 31-day range and must not be counted.
	if snapshot.Tokens.InputTokens != 900 || snapshot.Tokens.OutputTokens != 25 || snapshot.Tokens.TotalTokens != 925 {
		t.Fatalf("tokens = %+v", snapshot.Tokens)
	}
	if snapshot.Tokens.Requests != 2 {
		t.Fatalf("requests = %d", snapshot.Tokens.Requests)
	}
	if got := snapshot.Tokens.CostUSD; got < 1.49 || got > 1.51 {
		t.Fatalf("cost = %v", got)
	}
	if snapshot.Tokens.BalanceUSD != 42.5 {
		t.Fatalf("balance = %v", snapshot.Tokens.BalanceUSD)
	}
	if snapshot.Tokens.FromDate == "" || snapshot.Tokens.ToDate == "" {
		t.Fatalf("range missing: %+v", snapshot.Tokens)
	}
	if len(snapshot.Tokens.Series) != 31 {
		t.Fatalf("series length = %d", len(snapshot.Tokens.Series))
	}
	if snapshot.Tokens.Series[30] != 925 {
		t.Fatalf("series tail = %v", snapshot.Tokens.Series[30])
	}
}

func TestFetchReportsRejectedCredential(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"success":false,"error":"invalid key"}`))
	}))
	defer server.Close()
	client := NewClient(server.URL, "sk_bad", server.Client())

	snapshot := client.Fetch(context.Background())
	if snapshot.Available || !snapshot.Rejected {
		t.Fatalf("rejected credential reported as %+v", snapshot)
	}
	if !strings.Contains(snapshot.Error, "401") {
		t.Fatalf("error lost the status: %q", snapshot.Error)
	}
}

func TestFetchSurvivesMalformedAndMissingFields(t *testing.T) {
	server := fakeCline(t, func(path string, _ map[string]string) (int, any) {
		switch path {
		case "/users/me":
			return http.StatusOK, map[string]any{"id": "user-abcdefghijklmnop"}
		case "/users/me/plan/usage-limits":
			// No limits key at all: the window list stays empty instead of failing.
			return http.StatusOK, map[string]any{}
		case "/users/me/plan":
			return http.StatusOK, map[string]any{}
		case "/users/user-abcdefghijklmnop/usages/daily":
			return http.StatusOK, map[string]any{"items": []map[string]any{
				{"date": "not-a-date", "promptTokens": 10},
				{"promptTokens": 99},
			}}
		}
		return http.StatusInternalServerError, map[string]any{}
	})
	client := NewClient(server.URL, "sk_test_key", server.Client())

	snapshot := client.Fetch(context.Background())
	if !snapshot.Available {
		t.Fatalf("available = false: %+v", snapshot)
	}
	if len(snapshot.Limits) != 0 || snapshot.Limits == nil {
		t.Fatalf("limits should be an empty list, got %+v", snapshot.Limits)
	}
	// Undated rows are dropped rather than attributed to today.
	if snapshot.Tokens.TotalTokens != 0 || snapshot.Tokens.Requests != 0 {
		t.Fatalf("malformed rows were counted: %+v", snapshot.Tokens)
	}
	// The balance call is a bonus: its failure must not fail the snapshot.
	if snapshot.PlanName != "" || snapshot.Error != "" {
		t.Fatalf("unexpected fields: %+v", snapshot)
	}
}

func TestFetchWithoutAPIKey(t *testing.T) {
	client := NewClient(DefaultBaseURL, "   ", nil)
	snapshot := client.Fetch(context.Background())
	if snapshot.Available || !strings.Contains(snapshot.Error, "no Cline API key") {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	if snapshot.Limits == nil {
		t.Fatal("limits must serialise as [] rather than null")
	}
}

func TestFetchReportsDailyFailureSeparately(t *testing.T) {
	server := fakeCline(t, func(path string, query map[string]string) (int, any) {
		if strings.HasSuffix(path, "/usages/daily") {
			return http.StatusTooManyRequests, map[string]any{"error": "rate limited"}
		}
		return standardHandler(path, query)
	})
	client := NewClient(server.URL, "sk_test_key", server.Client())

	snapshot := client.Fetch(context.Background())
	if !snapshot.Available {
		t.Fatalf("quota stayed unavailable: %+v", snapshot)
	}
	if snapshot.TokensError == "" {
		t.Fatal("daily failure was not reported")
	}
	if snapshot.Tokens.TotalTokens != 0 {
		t.Fatalf("tokens invented without a daily response: %+v", snapshot.Tokens)
	}
	if snapshot.Error != "" {
		t.Fatalf("quota and totals failures must stay separate: %+v", snapshot)
	}
}

func TestUsageRangeIsThirtyOneDays(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	from, to := UsageRange(now)
	if to != "2026-09-30" || from != "2026-08-31" {
		t.Fatalf("range = %s .. %s", from, to)
	}
	start, _ := time.Parse("2006-01-02", from)
	end, _ := time.Parse("2006-01-02", to)
	if days := int(end.Sub(start).Hours()/24) + 1; days != 31 {
		t.Fatalf("range is %d days, Cline rejects anything above 31", days)
	}
}
