package serving

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nonlog/ClinePassProxy/internal/admin"
	"github.com/nonlog/ClinePassProxy/internal/config"
	"github.com/nonlog/ClinePassProxy/internal/credentials"
)

// fakeClineAccount serves the account endpoints the official snapshot reads.
func fakeClineAccount(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"success":false,"error":"unauthorized"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		var payload any
		switch {
		case r.URL.Path == "/users/me":
			payload = map[string]any{"id": "user-abcdefghijklmnop"}
		case r.URL.Path == "/users/me/plan/usage-limits":
			payload = map[string]any{"limits": []map[string]any{
				{"type": "five_hour", "percentUsed": 1.0, "resetsAt": time.Now().Add(3 * time.Hour).UTC().Format(time.RFC3339)},
				{"type": "weekly", "percentUsed": 42.5, "resetsAt": time.Now().Add(50 * time.Hour).UTC().Format(time.RFC3339)},
				{"type": "monthly", "percentUsed": 7.5, "resetsAt": time.Now().Add(300 * time.Hour).UTC().Format(time.RFC3339)},
			}}
		case r.URL.Path == "/users/me/plan":
			payload = map[string]any{"plan": map[string]any{"displayName": "Cline Pass Pro", "pricePerSeatCents": 1999}}
		case strings.HasSuffix(r.URL.Path, "/usages/daily"):
			payload = map[string]any{"items": []map[string]any{
				{"date": r.URL.Query().Get("endDate"), "costUsd": 1250000, "promptTokens": 819200000, "completionTokens": 8240000},
			}}
		case strings.HasSuffix(r.URL.Path, "/balance"):
			payload = map[string]any{"balance": 42500000}
		default:
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": payload})
	}))
	t.Cleanup(server.Close)
	return server
}

func newOfficialServer(t *testing.T, baseURL string, key string) (*Server, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	settings := config.Defaults()
	settings.DataDir = dir
	settings.BaseURL = baseURL
	settings.GatewayKeys = []string{"gateway-key"}
	settings.ManagementToken = "management-token"
	store, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(settings); err != nil {
		t.Fatal(err)
	}
	creds, err := credentials.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := creds.Upsert(credentials.Record{ID: "www", Label: "www", APIKey: key, Enabled: true}, false, false); err != nil {
		t.Fatal(err)
	}
	server := New(store, creds, admin.OpenHistory(dir, 200))
	return server, httptest.NewServer(server.Handler())
}

func TestOfficialSnapshotIsServedThroughTheManagementAPI(t *testing.T) {
	account := fakeClineAccount(t)
	server, api := newOfficialServer(t, account.URL, "sk_live_key")
	defer api.Close()

	server.refreshOfficial(context.Background(), "")

	request, _ := http.NewRequest(http.MethodGet, api.URL+"/api/official", nil)
	request.Header.Set("Authorization", "Bearer management-token")
	recorder := httptest.NewRecorder()
	api.Config.Handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Snapshots []struct {
			CredentialID string `json:"credential_id"`
			Available    bool   `json:"available"`
			PlanName     string `json:"plan_name"`
			Account      string `json:"account"`
			Limits       []struct {
				Label       string  `json:"label"`
				PercentUsed float64 `json:"percent_used"`
				ResetsIn    string  `json:"resets_in"`
			} `json:"limits"`
			Tokens struct {
				TotalTokens  int64   `json:"total_tokens"`
				InputTokens  int64   `json:"input_tokens"`
				OutputTokens int64   `json:"output_tokens"`
				BalanceUSD   float64 `json:"balance_usd"`
				CostUSD      float64 `json:"cost_usd"`
			} `json:"tokens"`
		} `json:"snapshots"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Snapshots) != 1 {
		t.Fatalf("snapshots = %+v", payload.Snapshots)
	}
	snapshot := payload.Snapshots[0]
	if !snapshot.Available || snapshot.CredentialID != "www" || snapshot.PlanName != "Cline Pass Pro" {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	if len(snapshot.Limits) != 3 || snapshot.Limits[0].Label != "5 小时滚动窗口" || snapshot.Limits[0].ResetsIn == "" {
		t.Fatalf("limits = %+v", snapshot.Limits)
	}
	if snapshot.Tokens.TotalTokens != 827440000 || snapshot.Tokens.BalanceUSD != 42.5 {
		t.Fatalf("tokens = %+v", snapshot.Tokens)
	}
	if snapshot.Tokens.CostUSD < 1.24 || snapshot.Tokens.CostUSD > 1.26 {
		t.Fatalf("cost = %v", snapshot.Tokens.CostUSD)
	}
	// The key never leaves the process, and the account id is masked.
	if strings.Contains(recorder.Body.String(), "sk_live_key") {
		t.Fatalf("management response leaked the credential: %s", recorder.Body.String())
	}
	if !strings.Contains(snapshot.Account, "…") {
		t.Fatalf("account id was not masked: %q", snapshot.Account)
	}
}

func TestOfficialRefreshKeepsLastNumbersOnFailure(t *testing.T) {
	failing := false
	account := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failing {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"success":false,"error":"upstream exploded"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		var payload any
		switch {
		case r.URL.Path == "/users/me":
			payload = map[string]any{"id": "user-abcdefghijklmnop"}
		case r.URL.Path == "/users/me/plan/usage-limits":
			payload = map[string]any{"limits": []map[string]any{{"type": "weekly", "percentUsed": 42.5}}}
		case strings.HasSuffix(r.URL.Path, "/usages/daily"):
			payload = map[string]any{"items": []map[string]any{{"date": r.URL.Query().Get("endDate"), "promptTokens": 100, "completionTokens": 10}}}
		default:
			payload = map[string]any{}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": payload})
	}))
	defer account.Close()

	server, _ := newOfficialServer(t, account.URL, "sk_live_key")
	server.refreshOfficial(context.Background(), "")
	before := server.officialSnapshot("www")
	if before == nil || !before.Available || before.Tokens.TotalTokens != 110 {
		t.Fatalf("first refresh = %+v", before)
	}

	failing = true
	server.refreshOfficial(context.Background(), "")
	after := server.officialSnapshot("www")
	if after == nil || !after.Available {
		t.Fatalf("a failed refresh blanked the account: %+v", after)
	}
	if after.Tokens.TotalTokens != 110 || len(after.Limits) != 1 {
		t.Fatalf("last good numbers were dropped: %+v", after)
	}
	if after.Error == "" {
		t.Fatal("the failure was not reported next to the stale numbers")
	}
}

func TestDashboardAndUsageReportWindowsAndActiveRequests(t *testing.T) {
	account := fakeClineAccount(t)
	server, api := newOfficialServer(t, account.URL, "sk_live_key")
	defer api.Close()
	server.refreshOfficial(context.Background(), "")

	now := time.Now().UTC()
	server.History.Append(admin.Record{ID: "recent", StartedAt: now.Add(-10 * time.Minute), Status: 200, Model: "m", PromptTokens: 100, CachedTokens: 90, CompletionToken: 5})
	server.History.Append(admin.Record{ID: "old", StartedAt: now.Add(-72 * time.Hour), Status: 502, Model: "m", PromptTokens: 10, CompletionToken: 1})

	call := func(path string) map[string]any {
		t.Helper()
		request, _ := http.NewRequest(http.MethodGet, api.URL+path, nil)
		request.Header.Set("Authorization", "Bearer management-token")
		recorder := httptest.NewRecorder()
		api.Config.Handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s -> %d %s", path, recorder.Code, recorder.Body.String())
		}
		var payload map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		return payload
	}

	dashboard := call("/api/dashboard")
	traffic, _ := dashboard["traffic"].(map[string]any)
	oneHour, _ := traffic["1h"].(map[string]any)
	all, _ := traffic["all"].(map[string]any)
	if oneHour["requests"] != float64(1) || all["requests"] != float64(2) {
		t.Fatalf("traffic windows = %v / %v", oneHour, all)
	}
	service, _ := dashboard["service"].(map[string]any)
	if service["active_requests"] != float64(0) {
		t.Fatalf("active requests = %v", service["active_requests"])
	}
	credentialsList, _ := dashboard["credentials"].([]any)
	if len(credentialsList) != 1 {
		t.Fatalf("credentials = %v", dashboard["credentials"])
	}
	first, _ := credentialsList[0].(map[string]any)
	if _, present := first["official"]; !present {
		t.Fatalf("credential view has no official snapshot: %v", first)
	}

	usage := call("/api/usage?range=24h")
	summary, _ := usage["summary"].(map[string]any)
	if summary["requests"] != float64(1) {
		t.Fatalf("24h summary = %v", summary)
	}
	windows, _ := usage["windows"].(map[string]any)
	for _, key := range []string{"1h", "24h", "7d", "31d", "all"} {
		if _, present := windows[key]; !present {
			t.Fatalf("usage window %s missing: %v", key, windows)
		}
	}
}

func TestRequestFiltersAndFacets(t *testing.T) {
	server, api := newOfficialServer(t, "http://127.0.0.1:1", "sk_live_key")
	defer api.Close()

	now := time.Now().UTC()
	server.History.Append(admin.Record{ID: "1", StartedAt: now.Add(-time.Minute), Status: 200, Model: "cline-pass/qwen3.7-max", CredentialName: "www", Provider: "clinepass", Endpoint: "/v1/messages"})
	server.History.Append(admin.Record{ID: "2", StartedAt: now.Add(-2 * time.Minute), Status: 502, Model: "cline-pass/kimi-k3", CredentialName: "alt", Provider: "clinepass", Endpoint: "/v1/responses"})

	cases := []struct {
		query string
		want  int
	}{
		{"", 2},
		{"model=cline-pass/qwen3.7-max", 1},
		{"credential=alt", 1},
		{"provider=clinepass", 2},
		{"endpoint=/v1/responses", 1},
		{"status=ok", 1},
		{"status=error", 1},
		{"status=502", 1},
		{"range=1h", 2},
	}
	for _, testCase := range cases {
		request, _ := http.NewRequest(http.MethodGet, api.URL+"/api/requests?"+testCase.query, nil)
		request.Header.Set("Authorization", "Bearer management-token")
		recorder := httptest.NewRecorder()
		api.Config.Handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s -> %d", testCase.query, recorder.Code)
		}
		var payload struct {
			Requests []admin.Record `json:"requests"`
			Facets   map[string][]string
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		if len(payload.Requests) != testCase.want {
			t.Fatalf("%q returned %d records, want %d", testCase.query, len(payload.Requests), testCase.want)
		}
		if testCase.query == "" && len(payload.Facets["models"]) != 2 {
			t.Fatalf("facets = %v", payload.Facets)
		}
	}
}

// Two credentials must produce two independent official snapshots: the UI
// renders one quota block per account and must never mix them together.
func TestOfficialSnapshotsCoverEveryCredential(t *testing.T) {
	account := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		percent := 1.0
		if key == "sk_alt_key" {
			percent = 55.0
		}
		w.Header().Set("Content-Type", "application/json")
		var payload any
		switch {
		case r.URL.Path == "/users/me":
			payload = map[string]any{"id": "user-" + key}
		case r.URL.Path == "/users/me/plan/usage-limits":
			payload = map[string]any{"limits": []map[string]any{{"type": "weekly", "percentUsed": percent}}}
		case strings.HasSuffix(r.URL.Path, "/usages/daily"):
			payload = map[string]any{"items": []map[string]any{{"date": r.URL.Query().Get("endDate"), "promptTokens": 10, "completionTokens": 1}}}
		case strings.HasSuffix(r.URL.Path, "/balance"):
			payload = map[string]any{"balance": 1000000}
		default:
			payload = map[string]any{}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": payload})
	}))
	defer account.Close()

	server, api := newOfficialServer(t, account.URL, "sk_live_key")
	defer api.Close()
	if _, err := server.Creds.Upsert(credentials.Record{ID: "alt", Label: "alt", APIKey: "sk_alt_key", Enabled: true}, false, false); err != nil {
		t.Fatal(err)
	}
	server.refreshOfficial(context.Background(), "")

	request, _ := http.NewRequest(http.MethodGet, api.URL+"/api/official", nil)
	request.Header.Set("Authorization", "Bearer management-token")
	recorder := httptest.NewRecorder()
	api.Config.Handler.ServeHTTP(recorder, request)
	body := recorder.Body.String()
	if strings.Contains(body, "sk_live_key") || strings.Contains(body, "sk_alt_key") {
		t.Fatalf("management response leaked a credential: %s", body)
	}
	var payload struct {
		Snapshots []struct {
			CredentialID string `json:"credential_id"`
			Available    bool   `json:"available"`
			Limits       []struct {
				PercentUsed float64 `json:"percent_used"`
			} `json:"limits"`
		} `json:"snapshots"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Snapshots) != 2 {
		t.Fatalf("snapshots = %+v", payload.Snapshots)
	}
	percents := map[string]float64{}
	for _, snapshot := range payload.Snapshots {
		if !snapshot.Available || len(snapshot.Limits) != 1 {
			t.Fatalf("snapshot = %+v", snapshot)
		}
		percents[snapshot.CredentialID] = snapshot.Limits[0].PercentUsed
	}
	if percents["www"] != 1 || percents["alt"] != 55 {
		t.Fatalf("per-credential quotas were mixed: %v", percents)
	}
}

// An upstream error message that echoes the credential must be redacted before
// it reaches the management API.
func TestOfficialRefreshRedactsKeyInUpstreamError(t *testing.T) {
	account := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": false,
			"error":   "rejected key " + strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "),
		})
	}))
	defer account.Close()

	server, _ := newOfficialServer(t, account.URL, "sk_live_secret_value")
	server.refreshOfficial(context.Background(), "")
	snapshot := server.officialSnapshot("www")
	if snapshot == nil || snapshot.Available {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	if snapshot.Error == "" {
		t.Fatal("the upstream failure was not reported")
	}
	if strings.Contains(snapshot.Error, "sk_live_secret_value") {
		t.Fatalf("error leaked the credential: %q", snapshot.Error)
	}
}
