package serving

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nonlog/ClinePassProxy/internal/admin"
	"github.com/nonlog/ClinePassProxy/internal/cline"
	"github.com/nonlog/ClinePassProxy/internal/config"
	"github.com/nonlog/ClinePassProxy/internal/credentials"
	"github.com/nonlog/ClinePassProxy/internal/models"
	"github.com/nonlog/ClinePassProxy/internal/upstream"
	"github.com/nonlog/ClinePassProxy/internal/version"
)

// management authorizes the management API with a bearer token or HTTP Basic
// Auth. Inference keys never authenticate management calls, and vice versa.
func (s *Server) management(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		settings := s.Config.Get()
		token := bearerToken(r)
		authorized := token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(settings.ManagementToken)) == 1
		if !authorized {
			if user, password, ok := r.BasicAuth(); ok {
				authorized = subtle.ConstantTimeCompare([]byte(user), []byte(settings.ManagementUser)) == 1 &&
					subtle.ConstantTimeCompare([]byte(password), []byte(settings.ManagementToken)) == 1
			}
		}
		if !authorized {
			w.Header().Set("WWW-Authenticate", `Basic realm="ClinePassProxy"`)
			writeError(w, http.StatusUnauthorized, "management authentication required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// configView is the management projection of the settings. It never carries the
// management token or a full gateway key.
type configView struct {
	DataDir              string         `json:"data_dir"`
	BaseURL              string         `json:"base_url"`
	SearchBaseURL        string         `json:"search_base_url"`
	SearchAPIKeySet      bool           `json:"search_api_key_set"`
	GatewayKeys          []string       `json:"gateway_keys"`
	GatewayKeyCount      int            `json:"gateway_key_count"`
	AllowUnauthenticated bool           `json:"allow_unauthenticated"`
	ManagementUser       string         `json:"management_user"`
	Models               []models.Entry `json:"models"`
	TimeoutSeconds       int            `json:"timeout_seconds"`
	MaxResponseBytes     int64          `json:"max_response_bytes"`
	LogRetention         int            `json:"log_retention"`
	RetryFailover        int            `json:"retry_failover"`
	Affinity             string         `json:"affinity"`
	AffinityHeader       string         `json:"affinity_header"`
	RequestLogBodyBytes  int            `json:"request_log_body_bytes"`
}

type configUpdate struct {
	BaseURL              *string         `json:"base_url"`
	SearchBaseURL        *string         `json:"search_base_url"`
	SearchAPIKey         *string         `json:"search_api_key"`
	GatewayKeys          *[]string       `json:"gateway_keys"`
	AllowUnauthenticated *bool           `json:"allow_unauthenticated"`
	ManagementToken      *string         `json:"management_token"`
	ManagementUser       *string         `json:"management_user"`
	Models               *[]models.Entry `json:"models"`
	TimeoutSeconds       *int            `json:"timeout_seconds"`
	MaxResponseBytes     *int64          `json:"max_response_bytes"`
	LogRetention         *int            `json:"log_retention"`
	RetryFailover        *int            `json:"retry_failover"`
	Affinity             *string         `json:"affinity"`
	AffinityHeader       *string         `json:"affinity_header"`
	RequestLogBodyBytes  *int            `json:"request_log_body_bytes"`
}

func viewOf(settings config.Config) configView {
	keys := make([]string, 0, len(settings.GatewayKeys))
	for _, key := range settings.GatewayKeys {
		keys = append(keys, credentials.MaskSecret(key))
	}
	return configView{
		DataDir:              settings.DataDir,
		BaseURL:              settings.BaseURL,
		SearchBaseURL:        settings.SearchBaseURL,
		SearchAPIKeySet:      strings.TrimSpace(settings.SearchAPIKey) != "",
		GatewayKeys:          keys,
		GatewayKeyCount:      len(settings.GatewayKeys),
		AllowUnauthenticated: settings.AllowUnauthenticated,
		ManagementUser:       settings.ManagementUser,
		Models:               settings.Models,
		TimeoutSeconds:       settings.TimeoutSeconds,
		MaxResponseBytes:     settings.MaxResponseBytes,
		LogRetention:         settings.LogRetention,
		RetryFailover:        settings.RetryFailover,
		Affinity:             settings.Affinity,
		AffinityHeader:       settings.AffinityHeader,
		RequestLogBodyBytes:  settings.RequestLogBodyBytes,
	}
}

func (s *Server) handleGetConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, viewOf(s.Config.Get()))
}

func (s *Server) handlePutConfig(w http.ResponseWriter, r *http.Request) {
	var update configUpdate
	if err := decodeJSON(r, &update); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	current := s.Config.Get()
	if update.BaseURL != nil {
		current.BaseURL = *update.BaseURL
	}
	if update.SearchBaseURL != nil {
		current.SearchBaseURL = *update.SearchBaseURL
	}
	if update.SearchAPIKey != nil {
		current.SearchAPIKey = *update.SearchAPIKey
	}
	if update.GatewayKeys != nil {
		current.GatewayKeys = *update.GatewayKeys
	}
	if update.AllowUnauthenticated != nil {
		current.AllowUnauthenticated = *update.AllowUnauthenticated
	}
	if update.ManagementToken != nil && strings.TrimSpace(*update.ManagementToken) != "" {
		current.ManagementToken = strings.TrimSpace(*update.ManagementToken)
	}
	if update.ManagementUser != nil {
		current.ManagementUser = *update.ManagementUser
	}
	if update.Models != nil {
		current.Models = *update.Models
	}
	if update.TimeoutSeconds != nil {
		current.TimeoutSeconds = *update.TimeoutSeconds
	}
	if update.MaxResponseBytes != nil {
		current.MaxResponseBytes = *update.MaxResponseBytes
	}
	if update.LogRetention != nil {
		current.LogRetention = *update.LogRetention
	}
	if update.RetryFailover != nil {
		current.RetryFailover = *update.RetryFailover
	}
	if update.Affinity != nil {
		current.Affinity = *update.Affinity
	}
	if update.AffinityHeader != nil {
		current.AffinityHeader = *update.AffinityHeader
	}
	if update.RequestLogBodyBytes != nil {
		current.RequestLogBodyBytes = *update.RequestLogBodyBytes
	}
	saved, err := s.Config.Update(current)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.upstream.SetBaseURL(saved.BaseURL)
	writeJSON(w, http.StatusOK, viewOf(saved))
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	enabled := s.Creds.Enabled()
	health := map[string]int{}
	for _, record := range s.Creds.All() {
		state := record.Health
		if state == "" {
			state = credentials.HealthUnknown
		}
		health[state]++
	}
	ready, readyErr := s.readiness()
	settings := s.Config.Get()

	writeJSON(w, http.StatusOK, map[string]any{
		"name":            version.Name,
		"version":         version.Version,
		"commit":          version.ResolvedCommit(),
		"build_time":      version.BuildTime,
		"uptime_sec":      int(time.Since(s.startedAt).Seconds()),
		"active_requests": s.active.Load(),
		"ready":           ready,
		"ready_error":     readyErr,
		"base_url":        settings.BaseURL,
		"affinity":        settings.Affinity,
		"credentials":     s.credentialViews(),
		"enabled_count":   len(enabled),
		"health":          health,
		"usage":           s.History.Usage(),
		"data_dir":        settings.DataDir,
	})
}

// ---------------------------------------------------------------- credentials

// credentialView is a credential's masked management projection plus the
// official Cline account snapshot the proxy last read for it.
type credentialView struct {
	credentials.View
	Official *cline.Snapshot `json:"official,omitempty"`
}

// credentialViews attaches the cached official snapshot to every credential.
func (s *Server) credentialViews() []credentialView {
	views := s.Creds.Views()
	out := make([]credentialView, 0, len(views))
	for _, view := range views {
		out = append(out, credentialView{View: view, Official: s.officialSnapshot(view.ID)})
	}
	return out
}

// credentialViewOf builds one augmented view.
func (s *Server) credentialViewOf(view credentials.View) credentialView {
	return credentialView{View: view, Official: s.officialSnapshot(view.ID)}
}

type credentialPayload struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	APIKey  string `json:"api_key"`
	Enabled *bool  `json:"enabled"`
	// ProxyURL is a pointer so an omitted field keeps the stored proxy while an
	// explicit empty string clears it, which is what the UI promises.
	ProxyURL *string `json:"proxy_url"`
}

func (s *Server) handleListCredentials(w http.ResponseWriter, _ *http.Request) {
	views := s.credentialViews()
	writeJSON(w, http.StatusOK, map[string]any{"credentials": views})
}

func (s *Server) handleCreateCredential(w http.ResponseWriter, r *http.Request) {
	var payload credentialPayload
	if err := decodeJSON(r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	enabled := true
	if payload.Enabled != nil {
		enabled = *payload.Enabled
	}
	record, err := s.Creds.Upsert(credentials.Record{
		Label:    payload.Label,
		APIKey:   payload.APIKey,
		Enabled:  enabled,
		ProxyURL: stringValue(payload.ProxyURL),
	}, false, false)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, s.credentialViewOf(record.View()))
}

func (s *Server) handleGetCredential(w http.ResponseWriter, r *http.Request) {
	record, ok := s.Creds.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "credential not found")
		return
	}
	writeJSON(w, http.StatusOK, s.credentialViewOf(record.View()))
}

func (s *Server) handleUpdateCredential(w http.ResponseWriter, r *http.Request) {
	var payload credentialPayload
	if err := decodeJSON(r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	id := r.PathValue("id")
	existing, ok := s.Creds.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "credential not found")
		return
	}
	// An omitted `enabled` means "leave it as it is". Defaulting to true here
	// silently re-enabled disabled credentials on every edit.
	enabled := existing.Enabled
	if payload.Enabled != nil {
		enabled = *payload.Enabled
	}
	record, err := s.Creds.Upsert(credentials.Record{
		ID:       id,
		Label:    payload.Label,
		APIKey:   payload.APIKey,
		Enabled:  enabled,
		ProxyURL: stringValue(payload.ProxyURL),
	}, true, payload.ProxyURL == nil)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.credentialViewOf(record.View()))
}

func (s *Server) handleDeleteCredential(w http.ResponseWriter, r *http.Request) {
	if err := s.Creds.Delete(r.PathValue("id")); err != nil {
		writeError(w, http.StatusNotFound, "credential not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": r.PathValue("id")})
}

// handleTestCredential issues a minimal Chat Completions request to validate the
// credential, without recording the result as a proxy request.
func (s *Server) handleTestCredential(w http.ResponseWriter, r *http.Request) {
	record, ok := s.Creds.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "credential not found")
		return
	}
	settings := s.Config.Get()
	s.upstream.SetBaseURL(settings.BaseURL)
	model := strings.TrimSpace(r.URL.Query().Get("model"))
	if model == "" {
		model = defaultTestModel(settings)
	}
	status, body, latency, err := s.testCredential(r.Context(), record, model, time.Duration(settings.TimeoutSeconds)*time.Second)
	if err != nil {
		_, _ = s.Creds.Update(record.ID, func(target *credentials.Record) {
			target.Health = credentials.HealthError
			target.HealthError = credentials.Sanitize(err.Error())
			target.HealthCheckedAt = time.Now().UTC()
		})
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "status": status, "latency_ms": latency, "error": credentials.Sanitize(err.Error())})
		return
	}
	_, _ = s.Creds.Update(record.ID, func(target *credentials.Record) {
		target.Health = credentials.HealthOK
		target.HealthError = ""
		target.HealthCheckedAt = time.Now().UTC()
		target.ConsecutiveErrors = 0
		target.CooldownUntil = time.Time{}
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         status >= 200 && status < 300,
		"status":     status,
		"latency_ms": latency,
		"model":      model,
		"reply":      credentials.Sanitize(truncate(string(body), 240)),
	})
}

// handleRefreshCredential re-reads the credential's Cline account: plan,
// rolling quota windows, the official 31-day totals and the balance.
func (s *Server) handleRefreshCredential(w http.ResponseWriter, r *http.Request) {
	record, ok := s.Creds.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "credential not found")
		return
	}
	if !record.Enabled {
		writeError(w, http.StatusConflict, "credential is disabled; enable it before refreshing its usage")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	s.refreshOfficial(ctx, record.ID)
	writeJSON(w, http.StatusOK, s.credentialViewOf(record.View()))
}

// ---------------------------------------------------------------- models

func (s *Server) handleListModels(w http.ResponseWriter, _ *http.Request) {
	settings := s.Config.Get()
	observedProviders := s.History.Facets()["providers"]
	views := make([]map[string]any, 0, len(settings.Models))
	for _, entry := range settings.Models {
		modelObservedProviders := s.History.ProvidersForModel(entry.ID)
		views = append(views, map[string]any{
			"id":                 entry.ID,
			"upstream_id":        entry.UpstreamID,
			"providers":          entry.Providers,
			"provider_pipeline":  entry.ProviderPipeline,
			"observed_providers": modelObservedProviders,
			"disabled":           entry.Disabled,
			"suggested":          defaultTestModel(settings) == entry.ID,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"models":             views,
		"seen_models":        s.History.Models(50),
		"observed_providers": observedProviders,
		"passthrough":        len(settings.Models) == 0,
		"default_for_test":   defaultTestModel(settings),
	})
}

type providerProbePayload struct {
	Model         string `json:"model"`
	UpstreamModel string `json:"upstream_model"`
	CredentialID  string `json:"credential_id"`
	Pipeline      string `json:"pipeline"`
}

// handleProbeModelProviders first identifies the model's real Cline routing
// pipeline with an ordinary request. Only after that succeeds do we send the
// pipeline-specific invalid-provider request that returns its catalog. This is
// important for cline-pass/* models: the gateway can discard provider fields,
// in which case showing a guessed catalog would make every saved pin unsafe.
func (s *Server) handleProbeModelProviders(w http.ResponseWriter, r *http.Request) {
	settings := s.Config.Get()
	s.upstream.SetBaseURL(settings.BaseURL)
	var payload providerProbePayload
	if err := decodeJSON(r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	model := strings.TrimSpace(payload.Model)
	if model == "" {
		writeError(w, http.StatusBadRequest, "model is required")
		return
	}
	_, upstreamModel, ok := models.Table{Entries: settings.Models}.ResolveEntry(model)
	if !ok && strings.TrimSpace(payload.UpstreamModel) == "" {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("model is not enabled in ClinePassProxy: %s", model))
		return
	}
	if !ok {
		upstreamModel = model
	}
	if strings.TrimSpace(payload.UpstreamModel) != "" {
		upstreamModel = strings.TrimSpace(payload.UpstreamModel)
	}
	credential, ok := s.probeCredential(payload.CredentialID)
	if !ok {
		if strings.TrimSpace(payload.CredentialID) != "" {
			writeError(w, http.StatusNotFound, "credential not found")
			return
		}
		writeError(w, http.StatusServiceUnavailable, "no enabled Cline credential is configured")
		return
	}

	probeTimeout := time.Duration(settings.TimeoutSeconds) * time.Second
	if probeTimeout <= 0 || probeTimeout > 30*time.Second {
		probeTimeout = 30 * time.Second
	}
	outcome := s.probeModelProviders(ctxOrBackground(r), credential, upstreamModel, probeTimeout)
	providerViews := make([]map[string]string, 0, len(outcome.providers))
	for _, provider := range outcome.providers {
		providerViews = append(providerViews, map[string]string{"name": provider, "status": "available"})
	}
	result := map[string]any{
		"ok":             len(outcome.providers) > 0 && outcome.pipeline != "",
		"pinnable":       len(outcome.providers) > 0 && outcome.pipeline != "",
		"model":          model,
		"upstream_model": upstreamModel,
		"credential":     credential.View(),
		"pipeline":       outcome.pipeline,
		"providers":      providerViews,
		"status":         outcome.status,
		"pinning_status": outcome.pinningStatus,
		"canonical_slug": outcome.canonicalSlug,
	}
	if outcome.actualProvider != "" {
		result["actual_provider"] = outcome.actualProvider
	}
	if outcome.err != nil {
		result["error"] = credentials.Sanitize(outcome.err.Error())
	} else if len(outcome.providers) == 0 {
		result["error"] = "Cline did not return a provider catalog for this model; provider pinning cannot be verified"
	}
	writeJSON(w, http.StatusOK, result)
}

type providerProbeOutcome struct {
	status         int
	providers      []string
	pipeline       string
	actualProvider string
	canonicalSlug  string
	pinningStatus  string
	err            error
}

// probeModelProviders keeps pipeline discovery and catalog discovery in one
// path so the management probe and its tests cannot drift apart.
func (s *Server) probeModelProviders(ctx context.Context, credential credentials.Record, model string, timeout time.Duration) providerProbeOutcome {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	identify := map[string]any{
		"model":    model,
		"messages": []any{map[string]any{"role": "user", "content": "Reply with the single word: ready"}},
		// Cline Pass reasoning models can spend more than 16 tokens before
		// emitting visible text. A too-small budget returns HTTP 500 empty
		// content, preventing pipeline discovery even though the model works.
		"max_tokens": 64,
		"stream":     false,
	}
	status, body, err := s.routingProbeChat(ctx, credential, identify, timeout)
	pipeline, actual := providerPipelineFromResponse(body)
	outcome := providerProbeOutcome{status: status, pipeline: pipeline, actualProvider: actual, canonicalSlug: probeCanonicalSlug(body), pinningStatus: "probe_unavailable", err: err}
	if err != nil {
		return outcome
	}
	if pipeline == "" {
		outcome.pinningStatus = "pipeline_unknown"
		outcome.err = errors.New("Cline response did not identify a provider pipeline; provider pinning cannot be verified")
		return outcome
	}

	probe := map[string]any{
		"model":      model,
		"messages":   []any{map[string]any{"role": "user", "content": "provider probe"}},
		"max_tokens": 16,
		"stream":     false,
	}
	if pipeline == "planner" {
		probe["providerOptions"] = map[string]any{"gateway": map[string]any{"only": []string{"__probe__"}}}
	} else {
		probe["provider"] = map[string]any{"only": []string{"__probe__"}}
	}
	catalogStatus, catalogBody, catalogErr := s.routingProbeChat(ctx, credential, probe, timeout)
	providers, catalogPipeline := providerProbeResults(catalogBody)
	if catalogPipeline != "" && catalogPipeline != pipeline {
		providers = nil
		catalogErr = fmt.Errorf("Cline provider catalog used %s routing, but the model response used %s", catalogPipeline, pipeline)
	}
	outcome.status = catalogStatus
	outcome.providers = providers
	if len(providers) > 0 {
		// The invalid-provider request is expected to return an error status.
		// A parsed, model-specific catalog is still a successful discovery.
		outcome.err = nil
		outcome.pinningStatus = "catalog_available"
		return outcome
	}
	if catalogErr == nil && probeActualProvider(catalogBody) != "" {
		outcome.pinningStatus = "routing_ignored"
		outcome.err = errors.New("Cline ignored the invalid provider restriction and completed with another provider; this model currently does not honor provider pinning")
		return outcome
	}
	outcome.err = catalogErr
	if outcome.err == nil {
		outcome.err = errors.New("Cline did not return a provider catalog for this model; provider pinning cannot be verified")
	}
	return outcome
}

// routingProbeChat uses the same small token budget for the known Cline
// empty-content failure's SSE retry. It never changes the model or falls back
// to a usage-billed catalog model.
func (s *Server) routingProbeChat(ctx context.Context, credential credentials.Record, payload map[string]any, timeout time.Duration) (int, []byte, error) {
	status, body, err := s.chat(ctx, credential, payload, timeout)
	if !isEmptyContentError(status, err) {
		return status, body, err
	}
	streaming := make(map[string]any, len(payload))
	for key, value := range payload {
		streaming[key] = value
	}
	streaming["stream"] = true
	return s.chat(ctx, credential, streaming, timeout)
}

// probeCompletionObject unwraps blocking responses and aggregates only small
// management probe SSE responses. Inference streams remain per-frame.
func probeCompletionObject(body []byte) map[string]any {
	if root, err := models.DecodeObject(unwrapCompletionEnvelope(body)); err == nil {
		return root
	}
	decoder := upstream.NewSSEDecoder(strings.NewReader(string(body)), int64(len(body))+1)
	aggregate := newCompletionAggregate()
	for {
		event, err := decoder.Next()
		if err != nil {
			return nil
		}
		if string(event.Data) == "[DONE]" {
			raw, err := aggregate.result(models.String(aggregate.root["model"]), true)
			if err != nil {
				return nil
			}
			root, _ := models.DecodeObject(raw)
			return root
		}
		if len(event.Data) > 0 && aggregate.observe(unwrapCompletionEnvelope(event.Data)) != nil {
			return nil
		}
	}
}

// providerPipelineFromResponse classifies only response evidence. A provider
// list from an error is not enough to classify a model because the list may
// belong to a different gateway layer.
func providerPipelineFromResponse(body []byte) (string, string) {
	object := probeCompletionObject(body)
	if provider, final := completionProvider(object); final {
		return "planner", provider
	}
	if provider, _ := completionProvider(object); provider != "" {
		return "direct", provider
	}
	return "", ""
}

func probeCanonicalSlug(body []byte) string {
	root := probeCompletionObject(body)
	for _, choice := range models.List(root["choices"]) {
		message := models.Object(models.Object(choice)["message"])
		routing := models.Object(models.Object(models.Object(message["provider_metadata"])["gateway"])["routing"])
		if slug := models.String(routing["canonicalSlug"]); slug != "" {
			return slug
		}
	}
	routing := models.Object(models.Object(models.Object(root["provider_metadata"])["gateway"])["routing"])
	if slug := models.String(routing["canonicalSlug"]); slug != "" {
		return slug
	}
	return models.String(root["model"])
}

func (s *Server) probeCredential(id string) (credentials.Record, bool) {
	if id = strings.TrimSpace(id); id != "" {
		record, ok := s.Creds.Get(id)
		return record, ok && record.Enabled && strings.TrimSpace(record.APIKey) != ""
	}
	enabled := s.Creds.Enabled()
	if len(enabled) == 0 {
		return credentials.Record{}, false
	}
	return enabled[0], true
}

func providerProbeResults(body []byte) ([]string, string) {
	if strings.Contains(string(body), "data:") {
		seen := map[string]bool{}
		providers := make([]string, 0)
		pipeline := ""
		add := func(provider string) {
			provider = strings.TrimSpace(provider)
			if provider != "" && !seen[provider] {
				seen[provider] = true
				providers = append(providers, provider)
			}
		}
		for _, line := range strings.Split(string(body), "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "" || payload == "[DONE]" {
				continue
			}
			root, err := models.DecodeObject([]byte(payload))
			if err != nil {
				continue
			}
			encoded, encodeErr := json.Marshal(root)
			if encodeErr != nil {
				continue
			}
			available, detected := providerProbeResultsJSON(encoded)
			for _, provider := range available {
				add(provider)
			}
			if pipeline == "" && detected != "" {
				pipeline = detected
			}
			// A successful probe can expose the provider that Cline actually
			// chose, but that is not a provider catalog. Only an upstream error
			// listing available providers is safe to save as selectable data.
			if provider, final := completionProvider(root); final && pipeline == "" {
				_ = provider
				pipeline = "planner"
			} else if provider != "" && pipeline == "" {
				pipeline = "direct"
			}
		}
		return providers, pipeline
	}
	return providerProbeResultsJSON(body)
}

func providerProbeResultsJSON(body []byte) ([]string, string) {
	root, err := models.DecodeObject(body)
	if err != nil {
		return parseProviderListMessage(string(body)), ""
	}
	seen := map[string]bool{}
	var providers []string
	var pipeline string
	var walk func(map[string]any)
	walk = func(object map[string]any) {
		for key, value := range object {
			switch strings.ToLower(key) {
			case "available_providers", "availableproviders":
				for _, provider := range providerList(value) {
					if !seen[provider] {
						seen[provider] = true
						providers = append(providers, provider)
					}
				}
				if pipeline == "" {
					pipeline = "direct"
				}
			case "message", "error":
				if message := models.String(value); message != "" {
					// Cline wraps the provider's JSON error in a prefixed string.
					if start := strings.IndexByte(message, '{'); start >= 0 {
						var embedded map[string]any
						if json.NewDecoder(strings.NewReader(message[start:])).Decode(&embedded) == nil {
							walk(embedded)
							continue
						}
					}
					parsed := parseProviderListMessage(message)
					for _, provider := range parsed {
						if !seen[provider] {
							seen[provider] = true
							providers = append(providers, provider)
						}
					}
					if strings.Contains(strings.ToLower(message), "available providers are:") && pipeline == "" {
						pipeline = "planner"
					}
				}
			}
			if nested := models.Object(value); nested != nil {
				walk(nested)
			}
		}
	}
	walk(root)
	return providers, pipeline
}

func providerList(value any) []string {
	var raw []string
	switch value := value.(type) {
	case []any:
		for _, item := range value {
			provider := strings.TrimSpace(models.String(item))
			if object := models.Object(item); object != nil {
				provider = strings.TrimSpace(models.String(object["name"]))
				if provider == "" {
					provider = strings.TrimSpace(models.String(object["id"]))
				}
				if provider == "" {
					provider = strings.TrimSpace(models.String(object["provider"]))
				}
			}
			if provider != "" {
				raw = append(raw, provider)
			}
		}
	case map[string]any:
		provider := strings.TrimSpace(models.String(value["name"]))
		if provider == "" {
			provider = strings.TrimSpace(models.String(value["id"]))
		}
		if provider == "" {
			provider = strings.TrimSpace(models.String(value["provider"]))
		}
		if provider != "" {
			raw = append(raw, provider)
		}
	case string:
		if parsed := parseProviderListMessage(value); len(parsed) > 0 {
			raw = parsed
		} else {
			for _, part := range strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == '\n' || r == ';' }) {
				part = strings.Trim(strings.TrimSpace(part), "`\"'[]")
				if part != "" {
					raw = append(raw, part)
				}
			}
		}
	}
	return raw
}

func parseProviderListMessage(message string) []string {
	marker := "available providers are:"
	lower := strings.ToLower(message)
	start := strings.Index(lower, marker)
	if start < 0 {
		return nil
	}
	value := strings.TrimSpace(message[start+len(marker):])
	if end := strings.Index(value, ". "); end >= 0 {
		value = value[:end]
	}
	value = strings.Trim(strings.TrimSpace(value), "[]")
	for _, suffix := range []string{`,"type":`, `,"param":`, `,\"type\":`, `,\"param\":`} {
		if end := strings.Index(value, suffix); end >= 0 {
			value = value[:end]
		}
	}
	value = strings.Trim(value, " .;\n\r\t")
	parts := strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == '\n' || r == ';' })
	providers := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		part = strings.TrimPrefix(part, "- ")
		part = strings.Trim(strings.TrimSpace(part), "`\"'[]")
		if part != "" {
			providers = append(providers, part)
		}
	}
	return providers
}

func probeActualProvider(body []byte) string {
	provider, _ := completionProvider(probeCompletionObject(body))
	return provider
}

func (s *Server) handleTestModel(w http.ResponseWriter, r *http.Request) {
	settings := s.Config.Get()
	s.upstream.SetBaseURL(settings.BaseURL)
	var payload struct {
		Model        string `json:"model"`
		Prompt       string `json:"prompt"`
		CredentialID string `json:"credential_id"`
		Stream       bool   `json:"stream"`
	}
	if err := decodeJSON(r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	entry, upstreamModel, ok := models.Table{Entries: settings.Models}.ResolveEntry(payload.Model)
	if !ok {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("model is not enabled in ClinePassProxy: %s", payload.Model))
		return
	}

	var credential credentials.Record
	if payload.CredentialID != "" {
		record, found := s.Creds.Get(payload.CredentialID)
		if !found {
			writeError(w, http.StatusNotFound, "credential not found")
			return
		}
		credential = record
	} else {
		enabled := s.Creds.Enabled()
		if len(enabled) == 0 {
			writeError(w, http.StatusServiceUnavailable, "no enabled Cline credential is configured")
			return
		}
		credential = enabled[0]
	}

	if payload.Prompt == "" {
		payload.Prompt = "Reply with the single word: ready"
	}
	started := time.Now()
	testPayload := map[string]any{
		"model":      upstreamModel,
		"messages":   []any{map[string]any{"role": "user", "content": payload.Prompt}},
		"max_tokens": 64,
		"stream":     false,
	}
	applyProviderSelection(testPayload, entry.Providers, entry.ProviderPipeline)
	status, body, err := s.routingProbeChat(ctxOrBackground(r), credential, testPayload, time.Duration(settings.TimeoutSeconds)*time.Second)
	latency := time.Since(started).Milliseconds()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "latency_ms": latency, "error": credentials.Sanitize(err.Error())})
		return
	}
	completion := probeCompletionObject(body)
	if completion == nil || len(models.List(completion["choices"])) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "status": status, "latency_ms": latency, "error": "Cline did not return a complete completion"})
		return
	}
	actualProvider := probeActualProvider(body)
	if !providerAllowed(entry.Providers, actualProvider) {
		message := fmt.Sprintf("configured provider selection was not honored: requested [%s], Cline selected %q", strings.Join(entry.Providers, ", "), actualProvider)
		if actualProvider == "" {
			message = "Cline did not report an actual provider; unable to verify the configured selection"
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":              false,
			"status":          status,
			"latency_ms":      latency,
			"model":           payload.Model,
			"upstream_model":  upstreamModel,
			"actual_provider": actualProvider,
			"error":           message,
		})
		return
	}
	choices := models.List(completion["choices"])
	reply := models.String(models.Object(models.Object(choices[0])["message"])["content"])
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                status >= 200 && status < 300,
		"status":            status,
		"latency_ms":        latency,
		"model":             payload.Model,
		"upstream_model":    upstreamModel,
		"provider":          actualProvider,
		"provider_pipeline": entry.ProviderPipeline,
		"credential":        credential.View(),
		"reply":             truncate(reply, 400),
	})
}

// ---------------------------------------------------------------- usage

// usageRanges are the windows the Usage tab can ask for. They describe the
// proxy's own observations; the official Cline numbers live under /api/official.
var usageRanges = map[string]time.Duration{
	"1h":  time.Hour,
	"24h": 24 * time.Hour,
	"7d":  7 * 24 * time.Hour,
	"31d": 31 * 24 * time.Hour,
	"all": 0,
}

func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSpace(r.URL.Query().Get("range"))
	if key == "" {
		key = "all"
	}
	duration, ok := usageRanges[key]
	if !ok {
		writeError(w, http.StatusBadRequest, "range must be one of 1h, 24h, 7d, 31d, all")
		return
	}
	var since time.Time
	if duration > 0 {
		since = time.Now().UTC().Add(-duration)
	}
	windows := make(map[string]admin.UsageSummary, len(usageRanges))
	for name, span := range usageRanges {
		var from time.Time
		if span > 0 {
			from = time.Now().UTC().Add(-span)
		}
		windows[name] = s.History.UsageSince(from)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"range":       key,
		"summary":     s.History.UsageSince(since),
		"windows":     windows,
		"credentials": s.credentialViews(),
		"official": map[string]any{
			"snapshots":        s.officialSnapshots(),
			"refreshed_at":     s.officialRefreshedAt(),
			"interval_seconds": int(officialInterval.Seconds()),
		},
		"retention": s.Config.Get().LogRetention,
	})
}

func (s *Server) handleListRequests(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	limit := 100
	if raw := query.Get("limit"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 && parsed <= 1000 {
			limit = parsed
		}
	}
	filter := admin.Filter{
		Limit:      limit,
		Model:      strings.TrimSpace(query.Get("model")),
		Credential: strings.TrimSpace(query.Get("credential")),
		Provider:   strings.TrimSpace(query.Get("provider")),
		Endpoint:   strings.TrimSpace(query.Get("endpoint")),
		Status:     strings.TrimSpace(query.Get("status")),
	}
	if raw := strings.TrimSpace(query.Get("range")); raw != "" {
		if duration, ok := usageRanges[raw]; ok && duration > 0 {
			filter.Since = time.Now().UTC().Add(-duration)
		}
	}
	records := s.History.Query(filter)
	if records == nil {
		records = []admin.Record{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"requests": records,
		"facets":   s.History.Facets(),
	})
}

func (s *Server) handleGetRequest(w http.ResponseWriter, r *http.Request) {
	record, ok := s.History.Find(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "request not found")
		return
	}
	writeJSON(w, http.StatusOK, record)
}

// ---------------------------------------------------------------- helpers

func decodeJSON(r *http.Request, target any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		return err
	}
	if len(body) == 0 {
		return errors.New("request body is required")
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	if err := decoder.Decode(target); err != nil {
		return errors.New("request body must be valid JSON")
	}
	return nil
}

func defaultTestModel(settings config.Config) string {
	for _, entry := range settings.Models {
		if !entry.Disabled {
			return entry.ID
		}
	}
	return ""
}

func truncate(value string, max int) string {
	value = strings.TrimSpace(value)
	if max <= 0 || len(value) <= max {
		return value
	}
	return value[:max] + "…"
}

// stringValue dereferences an optional JSON string, treating an omitted field
// as the empty value.
func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// testCredential issues a minimal completion request against Cline.
func (s *Server) testCredential(ctx context.Context, record credentials.Record, model string, timeout time.Duration) (int, []byte, int64, error) {
	if model == "" {
		return 0, nil, 0, errors.New("no model is configured for a credential test")
	}
	started := time.Now()
	status, body, err := s.chat(ctx, record, map[string]any{
		"model":      model,
		"messages":   []any{map[string]any{"role": "user", "content": "Reply with the single word: ready"}},
		"max_tokens": 16,
		"stream":     false,
	}, timeout)
	return status, body, time.Since(started).Milliseconds(), err
}

// chat issues one blocking Chat Completions request.
func (s *Server) chat(ctx context.Context, record credentials.Record, payload map[string]any, timeout time.Duration) (int, []byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, err
	}
	stream, err := s.upstream.ChatCompletions(ctx, record, upstream.ChatRequest{
		Body:    body,
		Stream:  models.Bool(payload["stream"]),
		Timeout: timeout,
	})
	if err != nil {
		return 0, nil, err
	}
	defer stream.Close()
	raw, err := readAll(stream, s.limitBytes())
	if err != nil {
		return stream.StatusCode, raw, err
	}
	if stream.StatusCode < 200 || stream.StatusCode >= 300 {
		return stream.StatusCode, raw, errors.New(upstreamMessage(raw, stream.StatusCode))
	}
	return stream.StatusCode, raw, nil
}

func ctxOrBackground(r *http.Request) context.Context {
	if r == nil || r.Context() == nil {
		return context.Background()
	}
	return r.Context()
}
