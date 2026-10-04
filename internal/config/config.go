// Package config loads and persists ClinePassProxy settings.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nonlog/ClinePassProxy/internal/models"
)

// DefaultBaseURL is the Cline Pass/Cline API root.
const DefaultBaseURL = "https://api.cline.bot/api/v1"

// Affinity policies for credential selection.
const (
	AffinitySession = "session"     // hash stable session identity onto a credential
	AffinitySticky  = "sticky"      // keep the first credential while it stays healthy
	AffinityRound   = "round-robin" // deterministic rotation, ignore session identity
)

// Config is the persisted ClinePassProxy configuration.
type Config struct {
	DataDir string `json:"data_dir"`

	BaseURL string `json:"base_url"`

	// SearchBaseURL is the optional CommandCodeProxy API root used by the
	// Codex-compatible /v1/alpha/search adapter. It is intentionally separate
	// from the Cline upstream URL because Cline does not provide this endpoint.
	SearchBaseURL string `json:"search_base_url"`
	// SearchAPIKey authenticates the configured search proxy. When empty, the
	// incoming gateway key is forwarded instead, which is useful when both
	// proxies deliberately share one data-plane key.
	SearchAPIKey string `json:"search_api_key"`

	// GatewayKeys authenticate inference clients (NewAPI channel #53).
	// An empty list closes the inference API unless AllowUnauthenticated is set.
	GatewayKeys []string `json:"gateway_keys"`

	// AllowUnauthenticated re-opens the inference API when GatewayKeys is empty.
	// It is an explicit opt-in because the data plane bills a paid Cline account
	// and must never be world-reachable by accident.
	AllowUnauthenticated bool `json:"allow_unauthenticated"`

	// ManagementToken authenticates the management API and web UI.
	// It is generated on first start when left empty.
	ManagementToken string `json:"management_token"`

	// ManagementUser is the HTTP Basic Auth user for the management UI.
	ManagementUser string `json:"management_user"`

	Models []models.Entry `json:"models"`

	TimeoutSeconds   int   `json:"timeout_seconds"`
	MaxResponseBytes int64 `json:"max_response_bytes"`
	LogRetention     int   `json:"log_retention"`
	RetryFailover    int   `json:"retry_failover"`

	Affinity       string `json:"affinity"`
	AffinityHeader string `json:"affinity_header"`

	// RequestLogBodyBytes bounds how much of each request body is retained for
	// diagnostics. The body is used to derive session identity, never logged raw.
	RequestLogBodyBytes int `json:"request_log_body_bytes"`
}

// Defaults returns the configuration used when no settings file exists.
func Defaults() Config {
	dataDir := "data"
	if env := strings.TrimSpace(os.Getenv("CLINEPASSPROXY_DATA_DIR")); env != "" {
		dataDir = env
	}
	return Config{
		DataDir:             dataDir,
		BaseURL:             DefaultBaseURL,
		SearchBaseURL:       strings.TrimRight(strings.TrimSpace(os.Getenv("CLINEPASSPROXY_SEARCH_BASE_URL")), "/"),
		SearchAPIKey:        strings.TrimSpace(os.Getenv("CLINEPASSPROXY_SEARCH_API_KEY")),
		Models:              []models.Entry{},
		TimeoutSeconds:      300,
		MaxResponseBytes:    64 << 20,
		LogRetention:        2000,
		RetryFailover:       1,
		Affinity:            AffinitySession,
		AffinityHeader:      "",
		ManagementUser:      "admin",
		RequestLogBodyBytes: 1 << 20,
	}
}

// Validate checks structural constraints. It does not touch the network.
func (c *Config) Validate() error {
	u, err := url.Parse(strings.TrimSpace(c.BaseURL))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return errors.New("base_url must be an absolute http(s) URL")
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return errors.New("base_url must use http or https")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("base_url must not carry userinfo, query or fragment")
	}
	if search := strings.TrimSpace(c.SearchBaseURL); search != "" {
		searchURL, err := url.Parse(search)
		if err != nil || searchURL.Scheme == "" || searchURL.Host == "" {
			return errors.New("search_base_url must be an absolute http(s) URL")
		}
		if searchURL.Scheme != "https" && searchURL.Scheme != "http" {
			return errors.New("search_base_url must use http or https")
		}
		if searchURL.User != nil || searchURL.RawQuery != "" || searchURL.Fragment != "" {
			return errors.New("search_base_url must not carry userinfo, query or fragment")
		}
	}
	if c.TimeoutSeconds < 10 || c.TimeoutSeconds > 3600 {
		return errors.New("timeout_seconds must be between 10 and 3600")
	}
	if c.MaxResponseBytes < 64<<10 || c.MaxResponseBytes > 512<<20 {
		return errors.New("max_response_bytes must be between 64 KiB and 512 MiB")
	}
	if c.LogRetention < 50 || c.LogRetention > 100000 {
		return errors.New("log_retention must be between 50 and 100000")
	}
	if c.RetryFailover < 0 || c.RetryFailover > 5 {
		return errors.New("retry_failover must be between 0 and 5")
	}
	if c.RequestLogBodyBytes < 0 || c.RequestLogBodyBytes > 8<<20 {
		return errors.New("request_log_body_bytes must be between 0 and 8 MiB")
	}
	switch c.Affinity {
	case AffinitySession, AffinitySticky, AffinityRound:
	default:
		return fmt.Errorf("affinity must be one of %s, %s, %s", AffinitySession, AffinitySticky, AffinityRound)
	}
	seen := map[string]bool{}
	for i := range c.Models {
		entry := &c.Models[i]
		entry.ID = strings.TrimSpace(entry.ID)
		entry.UpstreamID = strings.TrimSpace(entry.UpstreamID)
		entry.Providers = compactStrings(entry.Providers)
		entry.ProviderPipeline = strings.ToLower(strings.TrimSpace(entry.ProviderPipeline))
		if entry.ProviderPipeline != "" && entry.ProviderPipeline != "planner" && entry.ProviderPipeline != "direct" {
			return fmt.Errorf("model %q has invalid provider_pipeline", entry.ID)
		}
		if entry.ID == "" || seen[entry.ID] {
			return errors.New("model ids must be nonempty and unique")
		}
		seen[entry.ID] = true
		if strings.ContainsAny(entry.ID+entry.UpstreamID, "\r\n\t") {
			return fmt.Errorf("model %q contains control whitespace", entry.ID)
		}
	}
	return nil
}

// Store owns the on-disk settings file and the in-memory copy.
type Store struct {
	path string

	mu  sync.RWMutex
	cfg Config
}

// Open loads settings from dataDir/config.json, creating defaults when absent.
func Open(dataDir string) (*Store, error) {
	if strings.TrimSpace(dataDir) == "" {
		dataDir = Defaults().DataDir
	}
	absolute, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return nil, err
	}
	store := &Store{path: filepath.Join(absolute, "config.json"), cfg: Defaults()}
	store.cfg.DataDir = absolute

	raw, err := os.ReadFile(store.path)
	switch {
	case err == nil:
		loaded := store.cfg
		if err := json.Unmarshal(raw, &loaded); err != nil {
			return nil, fmt.Errorf("parse %s: %w", store.path, err)
		}
		// The data directory is a runtime concern; never let a stale file
		// relocate the store.
		loaded.DataDir = absolute
		if strings.TrimSpace(loaded.BaseURL) == "" {
			loaded.BaseURL = DefaultBaseURL
		}
		store.cfg = normalize(loaded)
		if err := store.cfg.Validate(); err != nil {
			return nil, fmt.Errorf("invalid settings in %s: %w", store.path, err)
		}
	case os.IsNotExist(err):
		if err := store.save(store.cfg); err != nil {
			return nil, err
		}
	default:
		return nil, err
	}

	if store.cfg.ManagementToken == "" {
		token, err := RandomToken()
		if err != nil {
			return nil, err
		}
		store.cfg.ManagementToken = token
		if err := store.save(store.cfg); err != nil {
			return nil, err
		}
	}
	return store, nil
}

// Path returns the settings file location.
func (s *Store) Path() string { return s.path }

// Get returns a copy of the current configuration.
func (s *Store) Get() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := s.cfg
	out.GatewayKeys = append([]string{}, s.cfg.GatewayKeys...)
	out.Models = append([]models.Entry{}, s.cfg.Models...)
	return out
}

// Update validates and persists a replacement configuration.
func (s *Store) Update(next Config) (Config, error) {
	next = normalize(next)
	if err := next.Validate(); err != nil {
		return Config{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next.DataDir = s.cfg.DataDir
	if next.ManagementToken == "" {
		next.ManagementToken = s.cfg.ManagementToken
	}
	if err := s.save(next); err != nil {
		return Config{}, err
	}
	s.cfg = next
	out := next
	out.GatewayKeys = append([]string{}, next.GatewayKeys...)
	out.Models = append([]models.Entry{}, next.Models...)
	return out, nil
}

func (s *Store) save(cfg Config) error {
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(s.path, raw, 0o600)
}

func normalize(cfg Config) Config {
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	cfg.SearchBaseURL = strings.TrimRight(strings.TrimSpace(cfg.SearchBaseURL), "/")
	cfg.SearchAPIKey = strings.TrimSpace(cfg.SearchAPIKey)
	cfg.ManagementUser = strings.TrimSpace(cfg.ManagementUser)
	if cfg.ManagementUser == "" {
		cfg.ManagementUser = "admin"
	}
	cfg.GatewayKeys = compactStrings(cfg.GatewayKeys)
	cfg.AffinityHeader = strings.TrimSpace(cfg.AffinityHeader)
	if cfg.Affinity == "" {
		cfg.Affinity = AffinitySession
	}
	if cfg.TimeoutSeconds == 0 {
		cfg.TimeoutSeconds = Defaults().TimeoutSeconds
	}
	if cfg.MaxResponseBytes == 0 {
		cfg.MaxResponseBytes = Defaults().MaxResponseBytes
	}
	if cfg.LogRetention == 0 {
		cfg.LogRetention = Defaults().LogRetention
	}
	if cfg.RequestLogBodyBytes == 0 {
		cfg.RequestLogBodyBytes = Defaults().RequestLogBodyBytes
	}
	for i := range cfg.Models {
		entry := &cfg.Models[i]
		entry.ProviderPipeline = strings.ToLower(strings.TrimSpace(entry.ProviderPipeline))
		entry.Providers = compactStrings(entry.Providers)
		// Provider names from request history or pre-pipeline versions are not
		// proof that this model can route to them. Drop those stale selections
		// during load/update so an old config cannot make inference unusable.
		if entry.ProviderPipeline == "" && len(entry.Providers) > 0 {
			entry.Providers = nil
		}
	}
	return cfg
}

func compactStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, value := range in {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

// RandomToken returns a 32-byte hex token.
func RandomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// RandomTokenString returns a short random hex identifier and never fails; a
// failed entropy read degrades to a timestamp-derived value rather than
// aborting the request it identifies.
func RandomTokenString() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

// WriteFileAtomic writes a file through a temporary sibling and a rename, so a
// crash never leaves a partially written settings or credential file.
func WriteFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// Timestamped returns a UTC timestamp suitable for diagnostics.
func Timestamped() string { return time.Now().UTC().Format(time.RFC3339Nano) }
