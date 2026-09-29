// Package credentials stores the Cline credentials used by the upstream data
// plane. Credential material never leaves this package in an unmasked form.
package credentials

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nonlog/ClinePassProxy/internal/config"
)

// Health values reported for a credential.
const (
	HealthUnknown   = "unknown"
	HealthOK        = "ok"
	HealthError     = "error"
	HealthDisabled  = "disabled"
	HealthExhausted = "exhausted"
)

// Usage is the last known Cline quota snapshot for a credential.
type Usage struct {
	Status    string          `json:"status"`
	Error     string          `json:"error,omitempty"`
	PlanName  string          `json:"plan_name,omitempty"`
	PeriodEnd string          `json:"period_end,omitempty"`
	Limits    []UsageLimit    `json:"limits,omitempty"`
	Raw       json.RawMessage `json:"raw,omitempty"`
	CheckedAt time.Time       `json:"checked_at,omitempty"`
}

// UsageLimit is one quota window reported by Cline.
type UsageLimit struct {
	Type        string   `json:"type"`
	PercentUsed *float64 `json:"percent_used,omitempty"`
	ResetsAt    string   `json:"resets_at,omitempty"`
}

// Record is a stored Cline credential plus its operational state.
type Record struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	APIKey  string `json:"api_key"`
	Enabled bool   `json:"enabled"`

	// ProxyURL applies only to this credential. Supported schemes are
	// http, https, socks5 and socks5h.
	ProxyURL string `json:"proxy_url,omitempty"`

	CreatedAt time.Time `json:"created_at,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`

	Health            string    `json:"health,omitempty"`
	HealthError       string    `json:"health_error,omitempty"`
	HealthCheckedAt   time.Time `json:"health_checked_at,omitempty"`
	LastUsedAt        time.Time `json:"last_used_at,omitempty"`
	ConsecutiveErrors int       `json:"consecutive_errors,omitempty"`
	CooldownUntil     time.Time `json:"cooldown_until,omitempty"`
	Usage             *Usage    `json:"usage,omitempty"`
}

// View is the masked projection returned by the management API.
type View struct {
	ID                string    `json:"id"`
	Label             string    `json:"label"`
	Enabled           bool      `json:"enabled"`
	MaskedKey         string    `json:"masked_key"`
	Proxy             string    `json:"proxy"`
	ProxyScheme       string    `json:"proxy_scheme,omitempty"`
	CreatedAt         time.Time `json:"created_at,omitempty"`
	UpdatedAt         time.Time `json:"updated_at,omitempty"`
	Health            string    `json:"health"`
	HealthError       string    `json:"health_error,omitempty"`
	HealthCheckedAt   time.Time `json:"health_checked_at,omitempty"`
	LastUsedAt        time.Time `json:"last_used_at,omitempty"`
	ConsecutiveErrors int       `json:"consecutive_errors,omitempty"`
	CooldownUntil     time.Time `json:"cooldown_until,omitempty"`
	Usage             *Usage    `json:"usage,omitempty"`
}

// Store owns the credential file and the in-memory copy.
type Store struct {
	path string

	mu      sync.RWMutex
	records map[string]Record
	order   []string
	rotor   uint64
}

type fileFormat struct {
	Version     int      `json:"version"`
	Credentials []Record `json:"credentials"`
}

// Open loads credentials.json from dataDir, creating an empty store when absent.
func Open(dataDir string) (*Store, error) {
	store := &Store{
		path:    filepath.Join(dataDir, "credentials.json"),
		records: map[string]Record{},
	}
	raw, err := os.ReadFile(store.path)
	switch {
	case err == nil:
		var file fileFormat
		if err := json.Unmarshal(raw, &file); err != nil {
			return nil, fmt.Errorf("parse %s: %w", store.path, err)
		}
		for _, record := range file.Credentials {
			if strings.TrimSpace(record.ID) == "" {
				continue
			}
			if record.Health == "" {
				record.Health = HealthUnknown
			}
			store.records[record.ID] = record
			store.order = append(store.order, record.ID)
		}
		sort.Strings(store.order)
	case os.IsNotExist(err):
	default:
		return nil, err
	}
	return store, nil
}

// Path returns the credential file location.
func (s *Store) Path() string { return s.path }

// Upsert creates or replaces a credential. An empty ID creates a new record.
// keepSecret keeps the stored API key when the incoming record omits it;
// keepProxy keeps the stored proxy URL when the incoming record omits it. Both
// are false for a create, so "leave the field blank" cannot silently restore or
// silently erase stored state.
func (s *Store) Upsert(record Record, keepSecret, keepProxy bool) (Record, error) {
	record.ID = strings.TrimSpace(record.ID)
	record.Label = strings.TrimSpace(record.Label)
	record.APIKey = strings.TrimSpace(record.APIKey)
	record.ProxyURL = strings.TrimSpace(record.ProxyURL)
	if err := validateProxy(record.ProxyURL); err != nil {
		return Record{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	if record.ID == "" {
		id, err := config.RandomToken()
		if err != nil {
			return Record{}, err
		}
		record.ID = "cline-" + id[:12]
	}
	existing, exists := s.records[record.ID]
	if exists {
		if keepSecret && record.APIKey == "" {
			record.APIKey = existing.APIKey
		}
		if record.Label == "" {
			record.Label = existing.Label
		}
		if keepProxy && record.ProxyURL == "" {
			record.ProxyURL = existing.ProxyURL
		}
		record.CreatedAt = existing.CreatedAt
		if record.Usage == nil {
			record.Usage = existing.Usage
		}
	} else {
		record.CreatedAt = now
	}
	if record.APIKey == "" {
		return Record{}, errors.New("api_key is required")
	}
	if record.Label == "" {
		record.Label = record.ID
	}
	record.ID = sanitizeID(record.ID)
	record.UpdatedAt = now
	switch {
	case !record.Enabled:
		record.Health = HealthDisabled
	case record.Health == "":
		record.Health = HealthUnknown
	}
	record.HealthError = sanitizeError(record.HealthError)
	if !exists {
		s.order = append(s.order, record.ID)
		sort.Strings(s.order)
	}
	s.records[record.ID] = record
	if err := s.persistLocked(); err != nil {
		return Record{}, err
	}
	return record, nil
}

// SetEnabled enables or disables a credential.
func (s *Store) SetEnabled(id string, enabled bool) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[id]
	if !ok {
		return Record{}, ErrNotFound
	}
	record.Enabled = enabled
	record.UpdatedAt = time.Now().UTC()
	if !enabled {
		record.Health = HealthDisabled
	} else if record.Health == HealthDisabled {
		record.Health = HealthUnknown
	}
	s.records[id] = record
	if err := s.persistLocked(); err != nil {
		return Record{}, err
	}
	return record, nil
}

// Delete removes a credential.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.records[id]; !ok {
		return ErrNotFound
	}
	delete(s.records, id)
	next := s.order[:0]
	for _, current := range s.order {
		if current != id {
			next = append(next, current)
		}
	}
	s.order = append([]string{}, next...)
	return s.persistLocked()
}

// Get returns a copy of one record.
func (s *Store) Get(id string) (Record, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	record, ok := s.records[id]
	return record, ok
}

// All returns copies of every record in stable order.
func (s *Store) All() []Record {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Record, 0, len(s.order))
	for _, id := range s.order {
		if record, ok := s.records[id]; ok {
			out = append(out, record)
		}
	}
	return out
}

// Enabled returns copies of every enabled record in stable order.
func (s *Store) Enabled() []Record {
	out := make([]Record, 0)
	for _, record := range s.All() {
		if record.Enabled && strings.TrimSpace(record.APIKey) != "" {
			out = append(out, record)
		}
	}
	return out
}

// Views returns masked projections of every record.
func (s *Store) Views() []View {
	records := s.All()
	out := make([]View, 0, len(records))
	for _, record := range records {
		out = append(out, record.View())
	}
	return out
}

// Update applies a mutation to one record and persists the result.
func (s *Store) Update(id string, mutate func(*Record)) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[id]
	if !ok {
		return Record{}, ErrNotFound
	}
	mutate(&record)
	record.HealthError = sanitizeError(record.HealthError)
	if err := validateProxy(record.ProxyURL); err != nil {
		return Record{}, err
	}
	s.records[id] = record
	if err := s.persistLocked(); err != nil {
		return Record{}, err
	}
	return record, nil
}

// NextRotor returns a monotonically increasing counter for round-robin policy.
func (s *Store) NextRotor() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rotor++
	return s.rotor
}

// ErrNotFound reports a missing credential.
var ErrNotFound = errors.New("credential not found")

func (s *Store) persistLocked() error {
	file := fileFormat{Version: 1}
	for _, id := range s.order {
		if record, ok := s.records[id]; ok {
			file.Credentials = append(file.Credentials, record)
		}
	}
	raw, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(s.path, raw, 0o600)
}

// View masks the secret material in a record.
func (r Record) View() View {
	view := View{
		ID:                r.ID,
		Label:             r.Label,
		Enabled:           r.Enabled,
		MaskedKey:         MaskSecret(r.APIKey),
		Proxy:             RedactProxy(r.ProxyURL),
		CreatedAt:         r.CreatedAt,
		UpdatedAt:         r.UpdatedAt,
		Health:            r.Health,
		HealthError:       sanitizeError(r.HealthError),
		HealthCheckedAt:   r.HealthCheckedAt,
		LastUsedAt:        r.LastUsedAt,
		ConsecutiveErrors: r.ConsecutiveErrors,
		CooldownUntil:     r.CooldownUntil,
		Usage:             r.Usage,
	}
	if view.Health == "" {
		view.Health = HealthUnknown
	}
	if parsed, err := url.Parse(strings.TrimSpace(r.ProxyURL)); err == nil && parsed.Scheme != "" {
		view.ProxyScheme = parsed.Scheme
	}
	return view
}

// MaskSecret renders a secret with a short prefix and its length.
func MaskSecret(secret string) string {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return ""
	}
	if len(secret) <= 8 {
		return strings.Repeat("*", len(secret))
	}
	return secret[:4] + "…" + strings.Repeat("*", 6) + " (" + itoa(len(secret)) + ")"
}

// RedactProxy removes credentials embedded in a proxy URL.
func RedactProxy(proxyURL string) string {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return ""
	}
	parsed, err := url.Parse(proxyURL)
	if err != nil {
		return "[invalid proxy url]"
	}
	if parsed.User != nil {
		parsed.User = url.User("[redacted]")
	}
	return parsed.String()
}

func validateProxy(proxyURL string) error {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return nil
	}
	parsed, err := url.Parse(proxyURL)
	if err != nil || parsed.Host == "" {
		return errors.New("invalid credential proxy URL")
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https", "socks5", "socks5h":
		return nil
	default:
		return errors.New("unsupported credential proxy scheme; use http, https, socks5 or socks5h")
	}
}

func sanitizeID(id string) string {
	var builder strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			builder.WriteRune(r)
		default:
			builder.WriteRune('_')
		}
	}
	if builder.Len() == 0 {
		return "cline-credential"
	}
	return builder.String()
}

func itoa(value int) string {
	return fmt.Sprintf("%d", value)
}
