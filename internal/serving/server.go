// Package serving exposes the inference API, the management API and the
// embedded web UI.
package serving

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nonlog/ClinePassProxy/internal/admin"
	"github.com/nonlog/ClinePassProxy/internal/affinity"
	"github.com/nonlog/ClinePassProxy/internal/config"
	"github.com/nonlog/ClinePassProxy/internal/credentials"
	"github.com/nonlog/ClinePassProxy/internal/models"
	"github.com/nonlog/ClinePassProxy/internal/upstream"
	"github.com/nonlog/ClinePassProxy/internal/version"
)

// Server is the ClinePassProxy HTTP service.
type Server struct {
	Config  *config.Store
	Creds   *credentials.Store
	History *admin.History

	upstream *upstream.Client
	selector *affinity.Selector

	startedAt time.Time
	official  *officialCache
	// active counts the inference requests being served right now.
	active atomic.Int64
}

// New builds a server around the given stores.
func New(settings *config.Store, creds *credentials.Store, history *admin.History) *Server {
	server := &Server{
		Config:    settings,
		Creds:     creds,
		History:   history,
		upstream:  upstream.New(settings.Get().BaseURL),
		selector:  affinity.NewSelector(),
		startedAt: time.Now().UTC(),
		official:  newOfficialCache(),
	}
	return server
}

// readiness reports whether the proxy can serve inference traffic. It always
// derives from the live credential pool, so enabling or disabling a credential
// through the management API changes /ready immediately.
func (s *Server) readiness() (bool, string) {
	settings := s.Config.Get()
	if len(s.Creds.Enabled()) == 0 {
		return false, "no enabled Cline credential is configured"
	}
	if len(settings.GatewayKeys) == 0 && !settings.AllowUnauthenticated {
		return false, "no gateway API key is configured, so the inference API is closed"
	}
	return true, ""
}

// Handler builds the HTTP routing table.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /ready", s.handleReady)

	// Inference data plane.
	mux.Handle("POST /v1/messages", s.inference(s.handleMessages))
	mux.Handle("POST /v1/responses", s.inference(s.handleResponses))
	mux.Handle("POST /v1/chat/completions", s.inference(s.handleChatCompletions))

	// Management API.
	mux.HandleFunc("GET /api/version", s.handleVersion)
	mux.Handle("GET /api/dashboard", s.management(http.HandlerFunc(s.handleDashboard)))
	mux.Handle("GET /api/official", s.management(http.HandlerFunc(s.handleOfficial)))
	mux.Handle("GET /api/status", s.management(http.HandlerFunc(s.handleStatus)))
	mux.Handle("GET /api/config", s.management(http.HandlerFunc(s.handleGetConfig)))
	mux.Handle("PUT /api/config", s.management(http.HandlerFunc(s.handlePutConfig)))
	mux.Handle("GET /api/credentials", s.management(http.HandlerFunc(s.handleListCredentials)))
	mux.Handle("POST /api/credentials", s.management(http.HandlerFunc(s.handleCreateCredential)))
	mux.Handle("GET /api/credentials/{id}", s.management(http.HandlerFunc(s.handleGetCredential)))
	mux.Handle("PUT /api/credentials/{id}", s.management(http.HandlerFunc(s.handleUpdateCredential)))
	mux.Handle("DELETE /api/credentials/{id}", s.management(http.HandlerFunc(s.handleDeleteCredential)))
	mux.Handle("POST /api/credentials/{id}/test", s.management(http.HandlerFunc(s.handleTestCredential)))
	mux.Handle("POST /api/credentials/{id}/refresh", s.management(http.HandlerFunc(s.handleRefreshCredential)))
	mux.Handle("GET /api/models", s.management(http.HandlerFunc(s.handleListModels)))
	mux.Handle("POST /api/models/test", s.management(http.HandlerFunc(s.handleTestModel)))
	mux.Handle("GET /api/usage", s.management(http.HandlerFunc(s.handleUsage)))
	mux.Handle("GET /api/requests", s.management(http.HandlerFunc(s.handleListRequests)))
	mux.Handle("GET /api/requests/{id}", s.management(http.HandlerFunc(s.handleGetRequest)))

	// Embedded UI.
	mux.Handle("/", s.ui())
	return mux
}

// ---------------------------------------------------------------- health

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":     "ok",
		"name":       version.Name,
		"version":    version.Version,
		"commit":     version.ResolvedCommit(),
		"uptime_sec": int(time.Since(s.startedAt).Seconds()),
	})
}

func (s *Server) handleReady(w http.ResponseWriter, _ *http.Request) {
	ready, reason := s.readiness()
	status := http.StatusOK
	if !ready {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]any{
		"ready":       ready,
		"credentials": len(s.Creds.Enabled()),
		"reason":      reason,
	})
}

func (s *Server) handleVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"name":       version.Name,
		"version":    version.Version,
		"commit":     version.ResolvedCommit(),
		"build_time": version.BuildTime,
	})
}

// ---------------------------------------------------------------- inference

// inference wraps a data-plane handler with gateway authentication.
func (s *Server) inference(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		settings := s.Config.Get()
		keys := settings.GatewayKeys
		if len(keys) == 0 {
			if !settings.AllowUnauthenticated {
				writeError(w, http.StatusServiceUnavailable,
					"the inference API is closed: add a gateway API key in the management UI, or set allow_unauthenticated")
				return
			}
			next(w, r)
			return
		}
		if !matchesAnyKey(bearerToken(r), keys) {
			writeError(w, http.StatusUnauthorized, "invalid gateway API key")
			return
		}
		next(w, r)
	})
}

func bearerToken(r *http.Request) string {
	header := r.Header.Get("Authorization")
	if len(header) > 7 && strings.EqualFold(header[:7], "bearer ") {
		return strings.TrimSpace(header[7:])
	}
	if header != "" {
		return strings.TrimSpace(header)
	}
	return strings.TrimSpace(r.Header.Get("X-Api-Key"))
}

func matchesAnyKey(candidate string, keys []string) bool {
	candidate = strings.TrimSpace(candidate)
	if candidate == "" {
		return false
	}
	match := false
	for _, key := range keys {
		if subtle.ConstantTimeCompare([]byte(candidate), []byte(key)) == 1 {
			match = true
		}
	}
	return match
}

// requestContext holds the parsed request shared by every protocol handler.
type requestContext struct {
	raw       []byte
	body      map[string]any
	tracker   *admin.Tracker
	trail     *trail
	model     string
	upstream  string
	sessionID string
	sessionBy string
	// streamed records that SSE headers and at least the protocol prologue have
	// already been written, so a later failure must not append a JSON error body
	// to the open event stream.
	streamed  bool
	transport []*upstream.Trace
}

type trail struct {
	mu       sync.Mutex
	attempts []string
}

func (t *trail) add(message string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.attempts = append(t.attempts, message)
}

func (t *trail) all() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string{}, t.attempts...)
}

// readRequest parses the body and resolves the model.
func (s *Server) readRequest(w http.ResponseWriter, r *http.Request, sourceFormat string) (*requestContext, bool) {
	tracker := admin.NewTracker()
	tracker.Stage(admin.StageRequestReceived)

	limit := s.limitBytes()
	raw, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		s.fail(w, tracker, http.StatusBadRequest, fmt.Errorf("read request body: %w", err))
		return nil, false
	}
	if int64(len(raw)) > limit {
		s.fail(w, tracker, http.StatusRequestEntityTooLarge, errors.New("request body exceeds the configured limit"))
		return nil, false
	}
	tracker.Stage(admin.StageBodyReadDone)

	body, err := models.DecodeObject(raw)
	if err != nil {
		s.fail(w, tracker, http.StatusBadRequest, errors.New("request body must be a JSON object"))
		return nil, false
	}
	tracker.Stage(admin.StageParseDone)

	record := tracker.Record()
	record.Endpoint = r.URL.Path
	record.SourceFormat = sourceFormat
	record.Stream = models.Bool(body["stream"])
	record.RequestBytes = int64(len(raw))
	record.Model = models.String(body["model"])
	if requestID := strings.TrimSpace(r.Header.Get("X-Request-Id")); requestID != "" {
		tracker.SetID(requestID)
		record = tracker.Record()
	}

	settings := s.Config.Get()
	tracker.Stage("affinity_start")
	upstreamModel, ok := models.Table{Entries: settings.Models}.Resolve(record.Model)
	if !ok {
		s.fail(w, tracker, http.StatusBadRequest, fmt.Errorf("model is not enabled in ClinePassProxy: %s", record.Model))
		return nil, false
	}
	sessionID, sessionBy := SessionIdentity(r.Header, body, settings.AffinityHeader)
	tracker.Stage("affinity_done")
	record.AffinityKey = sessionID
	record.UpstreamMode = upstreamModel

	return &requestContext{
		raw:       raw,
		body:      body,
		tracker:   tracker,
		trail:     &trail{},
		model:     record.Model,
		upstream:  upstreamModel,
		sessionID: sessionID,
		sessionBy: sessionBy,
	}, true
}

func (s *Server) limitBytes() int64 {
	settings := s.Config.Get()
	if settings.MaxResponseBytes <= 0 {
		return 64 << 20
	}
	return settings.MaxResponseBytes
}

// fail records a failed request and writes an OpenAI-shaped error.
func (s *Server) fail(w http.ResponseWriter, tracker *admin.Tracker, status int, err error) {
	err = errors.New(credentials.Sanitize(err.Error()))
	record := tracker.Finish(status, err)
	s.History.Append(record)
	writeError(w, status, err.Error())
}

func (s *Server) failWithTrail(w http.ResponseWriter, request *requestContext, status int, err error) {
	record := request.tracker.Record()
	s.snapshotTransport(request)
	record.Attempts = request.trail.all()
	record.FailoverCount = len(record.Attempts)
	final := request.tracker.Finish(status, errors.New(credentials.Sanitize(err.Error())))
	s.History.Append(final)
	// Once SSE frames are on the wire the status code is already sent; a JSON
	// error body would land inside the event stream.
	if !request.streamed {
		writeError(w, status, final.Error)
	}
}

// dispatch runs one inference request, including credential failover.
func (s *Server) dispatch(w http.ResponseWriter, r *http.Request, request *requestContext) {
	s.active.Add(1)
	defer s.active.Add(-1)

	settings := s.Config.Get()
	s.upstream.SetBaseURL(settings.BaseURL)

	timeout := time.Duration(settings.TimeoutSeconds) * time.Second
	ignore := map[string]bool{}
	var lastErr error
	var lastStatus int

	for attempt := 0; attempt <= settings.RetryFailover; attempt++ {
		enabled := s.Creds.Enabled()
		if len(enabled) == 0 {
			s.failWithTrail(w, request, http.StatusServiceUnavailable, errors.New("no enabled Cline credential is configured"))
			return
		}
		index, decision := s.selector.Select(settings.Affinity, request.sessionID, enabled, s.Creds.NextRotor(), ignore)
		if index < 0 {
			s.failWithTrail(w, request, http.StatusServiceUnavailable, errors.New("no Cline credential is available for this request"))
			return
		}
		credential := enabled[index]
		record := request.tracker.Record()
		record.CredentialID = credential.ID
		record.CredentialName = credential.Label
		record.AffinityReason = decision.Reason
		record.AffinityKey = decision.AffinityKey
		request.tracker.Stage(admin.StageCredentialSelected)
		request.tracker.Stage("translate_start")

		status, err, retryable := s.serve(w, r, request, credential, timeout)
		if err == nil {
			s.recordCredentialSuccess(credential.ID)
			return
		}
		// A client that cancelled the turn (pi stopped the request) is not a
		// credential failure: the same key would serve the retry just as well,
		// and failing the credential over would poison a healthy key.
		if r.Context().Err() != nil {
			request.trail.add(fmt.Sprintf("%s -> client cancelled: %s", credential.Label, credentials.Sanitize(err.Error())))
			s.finishRequest(request, statusClientClosed, err)
			return
		}
		lastErr, lastStatus = err, status
		request.trail.add(fmt.Sprintf("%s -> HTTP %d: %s", credential.Label, status, credentials.Sanitize(err.Error())))
		s.recordCredentialFailure(credential.ID, status, err)
		if !retryable || attempt == settings.RetryFailover {
			break
		}
		// Failover only helps when another credential remains untried. With a
		// single credential a retry would replay the same warm-cache session on
		// the same key, so report the upstream error instead.
		ignore[credential.ID] = true
		if !hasUntriedCredential(enabled, ignore) {
			delete(ignore, credential.ID)
			break
		}
	}

	if lastStatus == 0 {
		lastStatus = http.StatusBadGateway
	}
	s.failWithTrail(w, request, lastStatus, lastErr)
}

// hasUntriedCredential reports whether any enabled credential has not been
// attempted for this request yet.
func hasUntriedCredential(enabled []credentials.Record, ignore map[string]bool) bool {
	for _, record := range enabled {
		if !ignore[record.ID] {
			return true
		}
	}
	return false
}

// retryableUpstreamStatus reports whether another credential may succeed.
func retryableUpstreamStatus(status int) bool {
	switch {
	case status == http.StatusUnauthorized, status == http.StatusForbidden,
		status == http.StatusTooManyRequests, status == http.StatusPaymentRequired,
		status >= 500:
		return true
	default:
		return false
	}
}

func (s *Server) recordCredentialSuccess(id string) {
	_, _ = s.Creds.Update(id, func(record *credentials.Record) {
		record.Health = credentials.HealthOK
		record.HealthError = ""
		record.HealthCheckedAt = time.Now().UTC()
		record.LastUsedAt = time.Now().UTC()
		record.ConsecutiveErrors = 0
		record.CooldownUntil = time.Time{}
	})
}

func (s *Server) recordCredentialFailure(id string, status int, err error) {
	_, _ = s.Creds.Update(id, func(record *credentials.Record) {
		record.Health = credentials.HealthError
		record.HealthError = credentials.Sanitize(err.Error())
		record.HealthCheckedAt = time.Now().UTC()
		record.ConsecutiveErrors++
		switch status {
		case http.StatusUnauthorized, http.StatusForbidden:
			record.Health = "unauthorized"
		case http.StatusTooManyRequests, http.StatusPaymentRequired:
			record.Health = credentials.HealthExhausted
			record.CooldownUntil = time.Now().Add(5 * time.Minute)
		default:
			if record.ConsecutiveErrors >= 3 {
				record.CooldownUntil = time.Now().Add(time.Minute)
			}
		}
	})
}

// openUpstream opens the Cline stream for a prepared Chat Completions payload.
func (s *Server) openUpstream(r *http.Request, request *requestContext, credential credentials.Record, payload map[string]any, stream bool, timeout time.Duration) (*upstream.Stream, error) {
	if stream {
		options := models.Object(payload["stream_options"])
		if options == nil {
			options = map[string]any{}
		}
		options["include_usage"] = true
		payload["stream_options"] = options
	} else {
		delete(payload, "stream_options")
	}
	payload["stream"] = stream

	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	record := request.tracker.Record()
	record.UpstreamBytes = int64(len(encoded))
	request.tracker.Stage(admin.StageUpstreamRequestStart)
	trace := upstream.NewTrace(request.tracker.Started())
	request.transport = append(request.transport, trace)

	// The upstream deadline is independent of the client connection, but a
	// client disconnect must still cancel the upstream request immediately.
	// ChatCompletions derives its own timeout context from this one, so the
	// cancellation survives for the whole life of the response body: a client
	// that goes away (pi cancels a turn) stops the Cline request at once
	// instead of letting it run to the upstream deadline.
	return s.upstream.ChatCompletions(r.Context(), credential, upstream.ChatRequest{
		Body:    encoded,
		Stream:  stream,
		Timeout: timeout,
		Trace:   trace,
	})
}

// consumeUpstream streams the upstream SSE through a per-frame converter. It
// reports whether the upstream closed the stream with an explicit [DONE] frame.
func (s *Server) consumeUpstream(r *http.Request, request *requestContext, stream *upstream.Stream, emit func(raw []byte) ([][]byte, error), write func(frames [][]byte) error, done func() ([][]byte, error)) (bool, error) {
	decoder := upstream.NewSSEDecoder(stream.Body, s.limitBytes())
	record := request.tracker.Record()
	sawDone := false
	for {
		event, err := decoder.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return sawDone, err
		}
		payload := strings.TrimSpace(string(event.Data))
		if payload == "" {
			continue
		}
		if record.Timings[admin.StageFirstUpstreamEvent] == 0 {
			request.tracker.Stage(admin.StageFirstUpstreamEvent)
		}
		if payload == "[DONE]" {
			sawDone = true
			break
		}
		chunk := unwrapCompletionEnvelope([]byte(payload))
		s.observeChunk(request, chunk)
		frames, err := emit(chunk)
		if err != nil {
			return sawDone, err
		}
		if len(frames) == 0 {
			continue
		}
		if record.Timings[admin.StageFirstProtocolEvent] == 0 {
			request.tracker.Stage(admin.StageFirstProtocolEvent)
		}
		if err := write(frames); err != nil {
			return sawDone, err
		}
	}
	if done != nil {
		frames, err := done()
		if err != nil {
			return sawDone, err
		}
		if len(frames) > 0 {
			if err := write(frames); err != nil {
				return sawDone, err
			}
		}
	}
	request.tracker.Stage(admin.StageStreamComplete)
	return sawDone, nil
}

// observeChunk records usage and provider metadata from an upstream chunk.
func (s *Server) observeChunk(request *requestContext, raw []byte) {
	raw = unwrapCompletionEnvelope(raw)
	root, err := models.DecodeObject(raw)
	if err != nil {
		return
	}
	record := request.tracker.Record()
	if record.Stream {
		kinds := completionKinds(root)
		for _, kind := range kinds {
			request.tracker.Stage("first_" + kind + "_event")
			request.tracker.LastStage("last_" + kind + "_event")
		}
	}
	if provider := models.String(root["provider"]); provider != "" && record.Provider == "" {
		record.Provider = provider
	}
	usage := models.Object(root["usage"])
	if usage == nil {
		return
	}
	prompt := models.Number(usage["prompt_tokens"])
	if prompt == 0 {
		prompt = models.Number(usage["input_tokens"])
	}
	details := models.Object(usage["prompt_tokens_details"])
	if details == nil {
		details = models.Object(usage["input_tokens_details"])
	}
	cached := models.Number(details["cached_tokens"])
	completion := models.Number(usage["completion_tokens"])
	if completion == 0 {
		completion = models.Number(usage["output_tokens"])
	}
	reasoning := models.Number(models.Object(usage["completion_tokens_details"])["reasoning_tokens"])
	if reasoning == 0 {
		reasoning = models.Number(models.Object(usage["output_tokens_details"])["reasoning_tokens"])
	}
	request.tracker.ObserveUsage(prompt, cached, completion, reasoning)
}

// finishRequest persists the completed request record.
func (s *Server) finishRequest(request *requestContext, status int, err error) {
	record := request.tracker.Record()
	s.snapshotTransport(request)
	if len(request.trail.all()) > 0 {
		record.Attempts = request.trail.all()
		record.FailoverCount = len(record.Attempts)
	}
	s.History.Append(request.tracker.Finish(status, err))
}

// serve routes one attempt to the protocol handler named by the request path.
func (s *Server) serve(w http.ResponseWriter, r *http.Request, request *requestContext, credential credentials.Record, timeout time.Duration) (int, error, bool) {
	switch r.URL.Path {
	case "/v1/messages":
		return s.serveMessages(w, r, request, credential, timeout)
	case "/v1/responses":
		return s.serveResponses(w, r, request, credential, timeout)
	default:
		return s.serveChatCompletions(w, r, request, credential, timeout)
	}
}

// ---------------------------------------------------------------- errors

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "clinepassproxy_error",
		},
	})
}
