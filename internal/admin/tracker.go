// Package admin records per-request diagnostics and the request/usage history.
package admin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nonlog/ClinePassProxy/internal/config"
)

// Timing milestones recorded per request, in milliseconds from request start.
const (
	StageRequestReceived      = "request_received"
	StageBodyReadDone         = "body_read_done"
	StageParseDone            = "parse_done"
	StageTranslateDone        = "translate_done"
	StageCredentialSelected   = "credential_selected"
	StageUpstreamRequestStart = "upstream_request_start"
	StageUpstreamHeaders      = "upstream_headers_received"
	StageFirstUpstreamEvent   = "first_upstream_event"
	StageFirstProtocolEvent   = "first_protocol_event"
	StageFirstDownstreamWrite = "first_downstream_write"
	// StageFirstTokenWrite marks the write that carried the first visible token.
	// It is the only honest time-to-first-token: the protocol prologue frames
	// (message_start, response.created) are written long before any content.
	StageFirstTokenWrite = "first_token_write"
	StageStreamComplete  = "stream_complete"
	StageRequestComplete = "request_complete"
)

// Record is one completed or failed request.
type Record struct {
	ID        string    `json:"id"`
	StartedAt time.Time `json:"started_at"`

	Endpoint     string `json:"endpoint"`
	SourceFormat string `json:"source_format"`
	Model        string `json:"model"`
	UpstreamMode string `json:"upstream_model"`
	Stream       bool   `json:"stream"`

	AffinityKey    string `json:"affinity_key,omitempty"`
	AffinityReason string `json:"affinity_reason,omitempty"`
	CredentialID   string `json:"credential_id,omitempty"`
	CredentialName string `json:"credential_label,omitempty"`
	Provider       string `json:"provider,omitempty"`

	RequestBytes  int64 `json:"request_bytes"`
	UpstreamBytes int64 `json:"upstream_bytes"`
	ResponseBytes int64 `json:"response_bytes"`

	Timings map[string]int64 `json:"timings_ms"`

	TTFTMS          int64 `json:"ttft_ms"`
	ProviderTTFTMS  int64 `json:"provider_ttft_ms"`
	DurationMS      int64 `json:"duration_ms"`
	DecodeMS        int64 `json:"decode_ms"`
	PromptTokens    int64 `json:"prompt_tokens"`
	CachedTokens    int64 `json:"cached_tokens"`
	CompletionToken int64 `json:"completion_tokens"`
	ReasoningTokens int64 `json:"reasoning_tokens"`

	CacheHitRatio float64 `json:"cache_hit_ratio"`
	DecodeTPS     float64 `json:"decode_tps"`
	EndToEndTPS   float64 `json:"end_to_end_tps"`

	Status         int      `json:"status"`
	Error          string   `json:"error,omitempty"`
	Attempts       []string `json:"attempts,omitempty"`
	FailoverCount  int      `json:"failover_count"`
	UpstreamStatus int      `json:"upstream_status,omitempty"`
}

// Tracker is a mutable record builder; one instance belongs to one request.
type Tracker struct {
	record    Record
	started   time.Time
	completed bool
}

// NewTracker starts tracking a request.
func NewTracker() *Tracker {
	now := time.Now()
	return &Tracker{
		record: Record{
			ID:        config.RandomTokenString(),
			StartedAt: now.UTC(),
			Timings:   map[string]int64{},
		},
		started: now,
	}
}

// Record returns the record under construction.
func (t *Tracker) Record() *Record { return &t.record }

// SetID overrides the generated request ID.
func (t *Tracker) SetID(id string) {
	if strings.TrimSpace(id) != "" {
		t.record.ID = id
	}
}

// Stage records a timing milestone.
func (t *Tracker) Stage(stage string) {
	if t == nil || t.record.Timings == nil {
		return
	}
	if _, exists := t.record.Timings[stage]; exists {
		return
	}
	t.record.Timings[stage] = time.Since(t.started).Milliseconds()
}

// StageDuration returns the elapsed milliseconds between two stages.
func (t *Tracker) StageDuration(from, to string) int64 {
	if t == nil {
		return 0
	}
	start, startOK := t.record.Timings[from]
	end, endOK := t.record.Timings[to]
	if !startOK || !endOK || end < start {
		return 0
	}
	return end - start
}

// ObserveUsage records token accounting from an upstream or converted usage block.
func (t *Tracker) ObserveUsage(prompt, cached, completion, reasoning int64) {
	if t == nil {
		return
	}
	if prompt > 0 {
		t.record.PromptTokens = prompt
	}
	if cached > 0 {
		t.record.CachedTokens = cached
	}
	if completion > 0 {
		t.record.CompletionToken = completion
	}
	if reasoning > 0 {
		t.record.ReasoningTokens = reasoning
	}
}

// Finish closes the record and computes derived metrics.
func (t *Tracker) Finish(status int, err error) Record {
	t.Stage(StageRequestComplete)
	record := t.record
	record.DurationMS = time.Since(t.started).Milliseconds()
	record.Status = status
	if err != nil {
		record.Error = err.Error()
	}
	if record.PromptTokens > 0 {
		record.CacheHitRatio = float64(record.CachedTokens) / float64(record.PromptTokens)
	}
	if record.FirstUpstreamEvent() > 0 && record.DurationMS > record.FirstUpstreamEvent() {
		record.DecodeMS = record.DurationMS - record.FirstUpstreamEvent()
		if record.DecodeMS > 0 && record.CompletionToken > 0 {
			record.DecodeTPS = float64(record.CompletionToken) / (float64(record.DecodeMS) / 1000)
		}
	}
	if record.DurationMS > 0 && record.CompletionToken > 0 {
		record.EndToEndTPS = float64(record.CompletionToken) / (float64(record.DurationMS) / 1000)
	}
	if value, ok := record.Timings[StageFirstTokenWrite]; ok {
		record.TTFTMS = value
	}
	if value, ok := record.Timings[StageFirstUpstreamEvent]; ok {
		record.ProviderTTFTMS = value
	}
	t.completed = true
	return record
}

// FirstUpstreamEvent returns the first-upstream-event milestone.
func (r Record) FirstUpstreamEvent() int64 { return r.Timings[StageFirstUpstreamEvent] }

// History retains request records in memory and appends them to a JSONL file.
type History struct {
	mu        sync.RWMutex
	records   []Record
	retention int
	path      string
}

// OpenHistory loads persisted request history from dataDir.
func OpenHistory(dataDir string, retention int) *History {
	history := &History{retention: retention, path: filepath.Join(dataDir, "requests.jsonl")}
	raw, err := os.ReadFile(history.path)
	if err != nil {
		return history
	}
	lines := strings.Split(string(raw), "\n")
	records := make([]Record, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var record Record
		if json.Unmarshal([]byte(line), &record) != nil {
			continue
		}
		records = append(records, record)
	}
	if len(records) > retention {
		records = records[len(records)-retention:]
	}
	history.records = records
	return history
}

// Append stores a record and rewrites the JSONL file within the retention bound.
func (h *History) Append(record Record) {
	h.mu.Lock()
	h.records = append(h.records, record)
	if len(h.records) > h.retention {
		h.records = h.records[len(h.records)-h.retention:]
	}
	snapshot := append([]Record{}, h.records...)
	path := h.path
	h.mu.Unlock()

	var builder strings.Builder
	for _, item := range snapshot {
		encoded, err := json.Marshal(item)
		if err != nil {
			continue
		}
		builder.Write(encoded)
		builder.WriteByte('\n')
	}
	_ = config.WriteFileAtomic(path, []byte(builder.String()), 0o600)
}

// List returns the newest records first, optionally filtered by model.
func (h *History) List(limit int, model string) []Record {
	return h.Query(Filter{Limit: limit, Model: model})
}

// Filter narrows a history query. An empty field matches everything.
type Filter struct {
	Model      string
	Credential string
	Provider   string
	Endpoint   string
	// Status accepts "ok", "error" or an exact HTTP status code.
	Status string
	Since  time.Time
	Limit  int
}

func (f Filter) matches(record Record) bool {
	if f.Model != "" && record.Model != f.Model {
		return false
	}
	if f.Credential != "" && record.CredentialID != f.Credential && record.CredentialName != f.Credential {
		return false
	}
	if f.Provider != "" && record.Provider != f.Provider {
		return false
	}
	if f.Endpoint != "" && record.Endpoint != f.Endpoint {
		return false
	}
	if !f.Since.IsZero() && record.StartedAt.Before(f.Since) {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(f.Status)) {
	case "":
	case "ok", "success", "2xx":
		if record.Status < 200 || record.Status >= 300 {
			return false
		}
	case "error", "failed", "failure":
		if record.Status >= 200 && record.Status < 300 {
			return false
		}
	default:
		if strconv.Itoa(record.Status) != strings.TrimSpace(f.Status) {
			return false
		}
	}
	return true
}

// Query returns the matching records, newest first.
func (h *History) Query(filter Filter) []Record {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]Record, 0, len(h.records))
	for i := len(h.records) - 1; i >= 0; i-- {
		record := h.records[i]
		if !filter.matches(record) {
			continue
		}
		out = append(out, record)
		if filter.Limit > 0 && len(out) >= filter.Limit {
			break
		}
	}
	return out
}

// Facets lists the distinct filter values seen in the retained records so the
// request viewer can offer real choices instead of free text.
func (h *History) Facets() map[string][]string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	sets := map[string]map[string]bool{"models": {}, "credentials": {}, "providers": {}, "endpoints": {}}
	for _, record := range h.records {
		if record.Model != "" {
			sets["models"][record.Model] = true
		}
		if record.CredentialName != "" {
			sets["credentials"][record.CredentialName] = true
		}
		if record.Provider != "" {
			sets["providers"][record.Provider] = true
		}
		if record.Endpoint != "" {
			sets["endpoints"][record.Endpoint] = true
		}
	}
	out := make(map[string][]string, len(sets))
	for name, set := range sets {
		values := make([]string, 0, len(set))
		for value := range set {
			values = append(values, value)
		}
		sort.Strings(values)
		out[name] = values
	}
	return out
}

// Find returns one record by ID.
func (h *History) Find(id string) (Record, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for i := len(h.records) - 1; i >= 0; i-- {
		if h.records[i].ID == id {
			return h.records[i], true
		}
	}
	return Record{}, false
}

// UsageSummary aggregates token and throughput statistics.
type UsageSummary struct {
	Requests         int     `json:"requests"`
	Successes        int     `json:"successes"`
	Errors           int     `json:"errors"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CachedTokens     int64   `json:"cached_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	ReasoningTokens  int64   `json:"reasoning_tokens"`
	CacheHitRatio    float64 `json:"cache_hit_ratio"`
	AverageTTFTMS    float64 `json:"average_ttft_ms"`
	AverageDecodeTPS float64 `json:"average_decode_tps"`
	AverageE2ETPS    float64 `json:"average_end_to_end_tps"`
	AverageDuration  float64 `json:"average_duration_ms"`
}

// Usage aggregates every retained record.
func (h *History) Usage() UsageSummary {
	return h.UsageSince(time.Time{})
}

// UsageSince aggregates the records started at or after `since`. A zero time
// aggregates everything that is still retained.
func (h *History) UsageSince(since time.Time) UsageSummary {
	h.mu.RLock()
	defer h.mu.RUnlock()
	summary := UsageSummary{}
	var ttftCount, decodeCount, e2eCount, durationCount int
	for _, record := range h.records {
		if !since.IsZero() && record.StartedAt.Before(since) {
			continue
		}
		summary.Requests++
		if record.Status >= 200 && record.Status < 300 {
			summary.Successes++
		} else {
			summary.Errors++
		}
		summary.PromptTokens += record.PromptTokens
		summary.CachedTokens += record.CachedTokens
		summary.CompletionTokens += record.CompletionToken
		summary.ReasoningTokens += record.ReasoningTokens
		if record.TTFTMS > 0 {
			summary.AverageTTFTMS += float64(record.TTFTMS)
			ttftCount++
		}
		if record.DecodeTPS > 0 {
			summary.AverageDecodeTPS += record.DecodeTPS
			decodeCount++
		}
		if record.EndToEndTPS > 0 {
			summary.AverageE2ETPS += record.EndToEndTPS
			e2eCount++
		}
		if record.DurationMS > 0 {
			summary.AverageDuration += float64(record.DurationMS)
			durationCount++
		}
	}
	if summary.PromptTokens > 0 {
		summary.CacheHitRatio = float64(summary.CachedTokens) / float64(summary.PromptTokens)
	}
	if ttftCount > 0 {
		summary.AverageTTFTMS /= float64(ttftCount)
	}
	if decodeCount > 0 {
		summary.AverageDecodeTPS /= float64(decodeCount)
	}
	if e2eCount > 0 {
		summary.AverageE2ETPS /= float64(e2eCount)
	}
	if durationCount > 0 {
		summary.AverageDuration /= float64(durationCount)
	}
	return summary
}

// Models returns the distinct models seen, most recent first.
func (h *History) Models(limit int) []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	seen := map[string]bool{}
	out := make([]string, 0)
	for i := len(h.records) - 1; i >= 0; i-- {
		model := h.records[i].Model
		if model == "" || seen[model] {
			continue
		}
		seen[model] = true
		out = append(out, model)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	sort.Strings(out)
	return out
}
