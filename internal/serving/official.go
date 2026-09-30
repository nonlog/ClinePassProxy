package serving

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nonlog/ClinePassProxy/internal/admin"
	"github.com/nonlog/ClinePassProxy/internal/cline"
	"github.com/nonlog/ClinePassProxy/internal/credentials"
	"github.com/nonlog/ClinePassProxy/internal/version"
)

// officialInterval is how often every credential's Cline account snapshot is
// refreshed. The upstream answers a handful of small GETs per credential, and
// Cline rate limits bursts, so this is deliberately slower than the UI's own
// refresh button.
const officialInterval = 5 * time.Minute

// Traffic windows the dashboard reports for the proxy's own observations.
var trafficWindows = []struct {
	Key      string
	Duration time.Duration
}{
	{"1h", time.Hour},
	{"24h", 24 * time.Hour},
	{"7d", 7 * 24 * time.Hour},
}

// officialCache keeps the last Cline account snapshot per credential.
type officialCache struct {
	mu          sync.RWMutex
	snapshots   map[string]cline.Snapshot
	refreshedAt time.Time
	refreshing  bool
}

func newOfficialCache() *officialCache {
	return &officialCache{snapshots: map[string]cline.Snapshot{}}
}

func (c *officialCache) store(id string, snapshot cline.Snapshot) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.snapshots[id] = snapshot
	c.refreshedAt = time.Now().UTC()
}

// snapshot returns the cached account view for one credential.
func (s *Server) officialSnapshot(id string) *cline.Snapshot {
	s.official.mu.RLock()
	defer s.official.mu.RUnlock()
	snapshot, ok := s.official.snapshots[id]
	if !ok {
		return nil
	}
	return &snapshot
}

// officialSnapshots returns every cached account view in credential order.
func (s *Server) officialSnapshots() []cline.Snapshot {
	s.official.mu.RLock()
	defer s.official.mu.RUnlock()
	out := make([]cline.Snapshot, 0, len(s.official.snapshots))
	for _, record := range s.Creds.All() {
		if snapshot, ok := s.official.snapshots[record.ID]; ok {
			out = append(out, snapshot)
		}
	}
	return out
}

func (s *Server) officialRefreshedAt() time.Time {
	s.official.mu.RLock()
	defer s.official.mu.RUnlock()
	return s.official.refreshedAt
}

// refreshOfficial reads the Cline account for every enabled credential, or for
// one credential when `only` is set. A refresh that fails keeps the previous
// good numbers and records the error next to them, so a transient upstream
// failure does not blank the page.
func (s *Server) refreshOfficial(ctx context.Context, only string) {
	cache := s.official
	cache.mu.Lock()
	if cache.refreshing {
		cache.mu.Unlock()
		return
	}
	cache.refreshing = true
	cache.mu.Unlock()
	defer func() {
		cache.mu.Lock()
		cache.refreshing = false
		cache.mu.Unlock()
	}()

	settings := s.Config.Get()
	for _, record := range s.Creds.Enabled() {
		if only != "" && record.ID != only {
			continue
		}
		snapshot := s.fetchOfficial(ctx, settings.BaseURL, record)
		if !snapshot.Available {
			if previous := s.officialSnapshot(record.ID); previous != nil && previous.Available {
				// Keep the last known official numbers; the error explains why
				// they are not newer.
				snapshot.Limits = previous.Limits
				snapshot.Tokens = previous.Tokens
				snapshot.PlanName = previous.PlanName
				snapshot.PlanPrice = previous.PlanPrice
				snapshot.Account = previous.Account
				snapshot.Available = true
			}
		}
		cache.store(record.ID, snapshot)
	}
}

// fetchOfficial reads one credential, using the credential's own proxy so an
// account behind a proxy is still measurable.
func (s *Server) fetchOfficial(ctx context.Context, baseURL string, record credentials.Record) cline.Snapshot {
	snapshot := cline.Snapshot{
		CredentialID: record.ID,
		Label:        record.Label,
		Limits:       []cline.Window{},
		FetchedAt:    time.Now().UTC().Format(time.RFC3339),
	}
	client, err := s.upstream.HTTPClient(record.ProxyURL)
	if err != nil {
		snapshot.Error = credentials.Sanitize(err.Error())
		return snapshot
	}
	fetched := cline.NewClient(baseURL, record.APIKey, client).Fetch(ctx)
	fetched.CredentialID = record.ID
	fetched.Label = record.Label
	fetched.Error = credentials.Sanitize(fetched.Error)
	fetched.TokensError = credentials.Sanitize(fetched.TokensError)
	if fetched.Limits == nil {
		fetched.Limits = []cline.Window{}
	}
	return fetched
}

// StartOfficialPoller refreshes the account snapshots until ctx is cancelled.
func (s *Server) StartOfficialPoller(ctx context.Context) {
	go func() {
		// One immediate pass so the first page load has real numbers.
		s.refreshOfficial(ctx, "")
		ticker := time.NewTicker(officialInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.refreshOfficial(ctx, "")
			}
		}
	}()
}

// ---------------------------------------------------------------- handlers

// handleOfficial serves the cached Cline account snapshots. `?refresh=1`
// refreshes first, which is what the Usage page's refresh button asks for.
func (s *Server) handleOfficial(w http.ResponseWriter, r *http.Request) {
	if wantsRefresh(r) {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		s.refreshOfficial(ctx, strings.TrimSpace(r.URL.Query().Get("credential")))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"snapshots":        s.officialSnapshots(),
		"refreshed_at":     s.officialRefreshedAt(),
		"interval_seconds": int(officialInterval.Seconds()),
	})
}

// handleDashboard serves everything the Dashboard tab needs in one call.
func (s *Server) handleDashboard(w http.ResponseWriter, _ *http.Request) {
	settings := s.Config.Get()
	ready, readyErr := s.readiness()
	health := map[string]int{}
	for _, record := range s.Creds.All() {
		state := record.Health
		if state == "" {
			state = credentials.HealthUnknown
		}
		health[state]++
	}
	enabledModels := 0
	for _, entry := range settings.Models {
		if !entry.Disabled {
			enabledModels++
		}
	}

	traffic := map[string]admin.UsageSummary{}
	now := time.Now().UTC()
	for _, window := range trafficWindows {
		traffic[window.Key] = s.History.UsageSince(now.Add(-window.Duration))
	}
	traffic["all"] = s.History.UsageSince(time.Time{})

	writeJSON(w, http.StatusOK, map[string]any{
		"service": map[string]any{
			"name":                  version.Name,
			"version":               version.Version,
			"commit":                version.ResolvedCommit(),
			"build_time":            version.BuildTime,
			"uptime_sec":            int(time.Since(s.startedAt).Seconds()),
			"ready":                 ready,
			"ready_error":           readyErr,
			"active_requests":       s.active.Load(),
			"enabled_models":        enabledModels,
			"total_models":          len(settings.Models),
			"credentials":           len(s.Creds.All()),
			"enabled_credentials":   len(s.Creds.Enabled()),
			"health":                health,
			"base_url":              settings.BaseURL,
			"affinity":              settings.Affinity,
			"data_dir":              settings.DataDir,
			"gateway_key_count":     len(settings.GatewayKeys),
			"allow_unauthenticated": settings.AllowUnauthenticated,
		},
		"traffic":     traffic,
		"credentials": s.credentialViews(),
		"requests":    s.History.Query(admin.Filter{Limit: 12}),
		"official": map[string]any{
			"snapshots":        s.officialSnapshots(),
			"refreshed_at":     s.officialRefreshedAt(),
			"interval_seconds": int(officialInterval.Seconds()),
		},
	})
}

func wantsRefresh(r *http.Request) bool {
	raw := strings.TrimSpace(r.URL.Query().Get("refresh"))
	if raw == "" {
		return false
	}
	value, err := strconv.ParseBool(raw)
	return err == nil && value || raw == "1"
}
