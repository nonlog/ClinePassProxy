package serving

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/nonlog/ClinePassProxy/internal/admin"
	"github.com/nonlog/ClinePassProxy/internal/credentials"
	"github.com/nonlog/ClinePassProxy/internal/models"
	"github.com/nonlog/ClinePassProxy/internal/translate"
	"github.com/nonlog/ClinePassProxy/internal/upstream"
)

// handleMessages serves POST /v1/messages.
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	request, ok := s.readRequest(w, r, "claude-messages")
	if !ok {
		return
	}
	if hasNativeClaudeWebSearch(request.body) {
		s.handleNativeClaudeWebSearch(w, r, request)
		return
	}
	s.dispatch(w, r, request)
}

// handleResponses serves POST /v1/responses.
func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	request, ok := s.readRequest(w, r, "openai-responses")
	if !ok {
		return
	}
	s.dispatch(w, r, request)
}

// handleChatCompletions serves POST /v1/chat/completions.
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	request, ok := s.readRequest(w, r, "chat-completions")
	if !ok {
		return
	}
	s.dispatch(w, r, request)
}

// handleAlphaSearch forwards the Codex SearchRequest contract to the
// configured CommandCodeProxy. Cline itself does not expose /alpha/search, so
// this adapter deliberately keeps search out of the Cline credential pool.
func (s *Server) handleAlphaSearch(w http.ResponseWriter, r *http.Request) {
	settings := s.Config.Get()
	if strings.TrimSpace(settings.SearchBaseURL) == "" {
		writeError(w, http.StatusServiceUnavailable, "Codex search is not configured")
		return
	}

	limit := s.limitBytes()
	raw, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read search request body: "+err.Error())
		return
	}
	if int64(len(raw)) > limit {
		writeError(w, http.StatusRequestEntityTooLarge, "search request body exceeds the configured limit")
		return
	}
	if _, err := models.DecodeObject(raw); err != nil {
		writeError(w, http.StatusBadRequest, "search request body must be a JSON object")
		return
	}

	searchKey := strings.TrimSpace(settings.SearchAPIKey)
	if searchKey == "" {
		// A shared data-plane key is a useful zero-configuration deployment. A
		// dedicated search_api_key can be set when CCP and CPP use separate keys.
		searchKey = bearerToken(r)
	}
	if searchKey == "" {
		writeError(w, http.StatusUnauthorized, "CommandCodeProxy search API key is not configured")
		return
	}

	s.search.SetBaseURL(settings.SearchBaseURL)
	stream, err := s.search.PostJSON(r.Context(), searchKey, "/v1/alpha/search", raw, time.Duration(settings.TimeoutSeconds)*time.Second)
	if err != nil {
		writeError(w, http.StatusBadGateway, credentials.Sanitize(err.Error()))
		return
	}
	defer stream.Close()
	if stream.StatusCode < 200 || stream.StatusCode >= 300 {
		status, upstreamErr := upstreamError(stream, "CommandCode search")
		writeError(w, status, upstreamErr.Error())
		return
	}

	response, err := readAll(stream, limit)
	if err != nil {
		writeError(w, http.StatusBadGateway, credentials.Sanitize(err.Error()))
		return
	}
	if _, err := models.DecodeObject(response); err != nil {
		writeError(w, http.StatusBadGateway, "CommandCode search returned invalid JSON")
		return
	}
	contentType := stream.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/json"
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(response)
}

// applyProviderSelection pins Cline Pass routing when a model alias declares
// providers. Planner models consume providerOptions.gateway.only; direct
// OpenRouter models consume provider.only. Unknown pipelines receive both
// shapes, matching the proven switcher behavior without changing requests
// whose model alias has no provider selection.
func applyProviderSelection(payload map[string]any, providers []string) {
	if len(providers) == 0 {
		return
	}
	only := append([]string{}, providers...)
	providerOptions := models.Object(payload["providerOptions"])
	if providerOptions == nil {
		providerOptions = map[string]any{}
	}
	gateway := models.Object(providerOptions["gateway"])
	if gateway == nil {
		gateway = map[string]any{}
	}
	gateway["only"] = only
	providerOptions["gateway"] = gateway
	payload["providerOptions"] = providerOptions

	provider := models.Object(payload["provider"])
	if provider == nil {
		provider = map[string]any{}
	}
	provider["only"] = append([]string{}, providers...)
	payload["provider"] = provider
}

// serveMessages converts Anthropic Messages to Cline Chat Completions and back.
func (s *Server) serveMessages(w http.ResponseWriter, r *http.Request, request *requestContext, credential credentials.Record, timeout time.Duration) (int, error, bool) {
	payload, err := translate.ClaudeMessagesToChatCompletions(request.raw, request.upstream, models.Bool(request.body["stream"]))
	if err != nil {
		return http.StatusBadRequest, err, false
	}
	applyProviderSelection(payload, request.providers)
	request.tracker.Stage(admin.StageTranslateDone)

	streaming := models.Bool(request.body["stream"])
	if !streaming {
		raw, status, err, retryable := s.readNonstreamCompletion(r, request, credential, payload, timeout)
		if err != nil {
			return status, err, retryable
		}
		s.observeChunk(request, raw)
		converted, err := translate.OpenAICompletionToClaude(raw, request.model, request.raw)
		if err != nil {
			return http.StatusBadGateway, err, false
		}
		s.finishRequest(request, http.StatusOK, nil)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(converted)
		return http.StatusOK, nil, false
	}

	stream, err := s.openUpstream(r, request, credential, payload, true, timeout)
	if err != nil {
		return http.StatusBadGateway, err, true
	}
	defer stream.Close()
	request.tracker.Stage(admin.StageUpstreamHeaders)

	if stream.StatusCode < 200 || stream.StatusCode >= 300 {
		status, err := upstreamError(stream, "Cline")
		return status, err, retryableUpstreamStatus(status)
	}

	converter := translate.NewClaudeStreamConverter(request.model, request.raw)
	writer := newSSEWriter(w)
	writer.prepare()
	request.streamed = true

	startFrames, err := converter.Start()
	if err != nil {
		return http.StatusInternalServerError, err, false
	}
	if err := writer.write(request.tracker, startFrames); err != nil {
		s.finishRequest(request, statusClientClosed, err)
		return statusClientClosed, err, false
	}

	_, err = s.consumeUpstream(r, request, stream,
		func(raw []byte) ([][]byte, error) { return converter.Feed(raw) },
		func(frames [][]byte) error { return writer.write(request.tracker, frames) },
		func() ([][]byte, error) { return converter.Done() },
	)
	if err != nil {
		if isTruncatedStream(err) {
			_ = writer.write(request.tracker, [][]byte{converter.ErrorFrame(truncationMessage(err))})
		}
		return s.finishStreamFailure(request, err)
	}
	s.observeClaudeUsage(request, converter.Usage())
	s.finishRequest(request, http.StatusOK, nil)
	return http.StatusOK, nil, false
}

// serveResponses converts OpenAI Responses to Cline Chat Completions and back.
func (s *Server) serveResponses(w http.ResponseWriter, r *http.Request, request *requestContext, credential credentials.Record, timeout time.Duration) (int, error, bool) {
	payload, err := translate.ResponsesToChatCompletions(request.raw, request.upstream, models.Bool(request.body["stream"]))
	if err != nil {
		return http.StatusBadRequest, err, false
	}
	applyProviderSelection(payload, request.providers)
	request.tracker.Stage(admin.StageTranslateDone)

	translated, _ := jsonBytes(payload)
	streaming := models.Bool(request.body["stream"])
	if !streaming {
		raw, status, err, retryable := s.readNonstreamCompletion(r, request, credential, payload, timeout)
		if err != nil {
			return status, err, retryable
		}
		s.observeChunk(request, raw)
		converted, err := translate.OpenAICompletionToResponses(raw, request.model, request.raw, translated)
		if err != nil {
			return http.StatusBadGateway, err, false
		}
		s.finishRequest(request, http.StatusOK, nil)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(converted)
		return http.StatusOK, nil, false
	}

	stream, err := s.openUpstream(r, request, credential, payload, true, timeout)
	if err != nil {
		return http.StatusBadGateway, err, true
	}
	defer stream.Close()
	request.tracker.Stage(admin.StageUpstreamHeaders)

	if stream.StatusCode < 200 || stream.StatusCode >= 300 {
		status, err := upstreamError(stream, "Cline")
		return status, err, retryableUpstreamStatus(status)
	}

	converter := translate.NewResponsesStreamConverter(request.model, request.raw, translated)
	writer := newSSEWriter(w)
	writer.prepare()
	request.streamed = true

	startFrames, err := converter.Start()
	if err != nil {
		return http.StatusInternalServerError, err, false
	}
	if err := writer.write(request.tracker, startFrames); err != nil {
		s.finishRequest(request, statusClientClosed, err)
		return statusClientClosed, err, false
	}

	_, err = s.consumeUpstream(r, request, stream,
		func(raw []byte) ([][]byte, error) { return converter.Feed(raw) },
		func(frames [][]byte) error { return writer.write(request.tracker, frames) },
		func() ([][]byte, error) { return converter.Done() },
	)
	if err != nil {
		if isTruncatedStream(err) {
			_ = writer.write(request.tracker, [][]byte{converter.ErrorFrame(truncationMessage(err))})
		}
		return s.finishStreamFailure(request, err)
	}
	if usage := converter.Usage(); usage != nil {
		request.tracker.ObserveUsage(
			models.Number(usage["input_tokens"]),
			models.Number(models.Object(usage["input_tokens_details"])["cached_tokens"]),
			models.Number(usage["output_tokens"]),
			models.Number(models.Object(usage["output_tokens_details"])["reasoning_tokens"]),
		)
	}
	s.finishRequest(request, http.StatusOK, nil)
	return http.StatusOK, nil, false
}

// serveChatCompletions normalizes a Chat Completions request and forwards the
// upstream SSE or JSON response as-is.
func (s *Server) serveChatCompletions(w http.ResponseWriter, r *http.Request, request *requestContext, credential credentials.Record, timeout time.Duration) (int, error, bool) {
	payload := translate.CloneMap(request.body)
	payload["model"] = request.upstream
	applyProviderSelection(payload, request.providers)
	stream := models.Bool(request.body["stream"])
	request.tracker.Stage(admin.StageTranslateDone)

	if !stream {
		raw, status, err, retryable := s.readNonstreamCompletion(r, request, credential, payload, timeout)
		if err != nil {
			return status, err, retryable
		}
		s.observeChunk(request, raw)
		s.finishRequest(request, http.StatusOK, nil)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(normalizeCompletionModel(raw, request.model))
		return http.StatusOK, nil, false
	}

	upstreamStream, err := s.openUpstream(r, request, credential, payload, true, timeout)
	if err != nil {
		return http.StatusBadGateway, err, true
	}
	defer upstreamStream.Close()
	request.tracker.Stage(admin.StageUpstreamHeaders)

	if upstreamStream.StatusCode < 200 || upstreamStream.StatusCode >= 300 {
		status, err := upstreamError(upstreamStream, "Cline")
		return status, err, retryableUpstreamStatus(status)
	}

	writer := newSSEWriter(w)
	writer.prepare()
	request.streamed = true
	sawFinish := false
	sawDone, err := s.consumeUpstream(r, request, upstreamStream,
		func(raw []byte) ([][]byte, error) {
			if chatChunkFinished(raw) {
				sawFinish = true
			}
			return forwardChatFrames(raw, request.model)
		},
		func(frames [][]byte) error { return writer.write(request.tracker, frames) },
		nil,
	)
	if err != nil {
		if isTruncatedStream(err) || errors.Is(err, errClientGone) {
			// Nothing to add: the client is already gone.
		} else {
			_ = writer.write(request.tracker, [][]byte{translate.ChatStreamError(truncationMessage(err))})
		}
		return s.finishStreamFailure(request, err)
	}
	if !sawDone && !sawFinish {
		err := translate.ErrUpstreamTruncated
		_ = writer.write(request.tracker, [][]byte{translate.ChatStreamError(truncationMessage(err))})
		return s.finishStreamFailure(request, err)
	}
	if err := writer.write(request.tracker, [][]byte{translate.DoneEvent()}); err != nil {
		return s.finishStreamFailure(request, err)
	}
	s.finishRequest(request, http.StatusOK, nil)
	return http.StatusOK, nil, false
}

// statusClientClosed uses the nginx-style 499 for a client that went away.
const statusClientClosed = 499

// finishStreamFailure records a stream failure. When the response has not been
// committed the caller can still retry on another credential.
func (s *Server) finishStreamFailure(request *requestContext, err error) (int, error, bool) {
	if errors.Is(err, context.Canceled) {
		s.finishRequest(request, statusClientClosed, err)
		return statusClientClosed, err, false
	}
	if errors.Is(err, upstream.ErrStreamLimit) {
		s.finishRequest(request, http.StatusBadGateway, err)
		return http.StatusBadGateway, err, false
	}
	if errors.Is(err, errClientGone) {
		s.finishRequest(request, statusClientClosed, err)
		return statusClientClosed, err, false
	}
	s.finishRequest(request, http.StatusBadGateway, err)
	return http.StatusBadGateway, err, false
}

// isTruncatedStream reports an upstream stream that stopped before the
// response was complete.
func isTruncatedStream(err error) bool {
	return errors.Is(err, translate.ErrUpstreamTruncated) || errors.Is(err, upstream.ErrPartialFrame)
}

// truncationMessage renders an upstream-compatible explanation for a stream
// that ended early.
func truncationMessage(err error) string {
	if errors.Is(err, upstream.ErrPartialFrame) {
		return "Cline upstream stream ended in the middle of an event"
	}
	return "Cline upstream stream ended before the response completed"
}

// chatChunkFinished reports whether an upstream chunk carried a finish reason.
func chatChunkFinished(raw []byte) bool {
	root, err := models.DecodeObject(raw)
	if err != nil {
		return false
	}
	for _, rawChoice := range models.List(root["choices"]) {
		if models.String(models.Object(rawChoice)["finish_reason"]) != "" {
			return true
		}
	}
	return false
}

// errClientGone reports a downstream write that failed.
var errClientGone = errors.New("client disconnected")

// sseWriter tracks the first downstream write and flushes every frame.
type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	wrote   bool
}

func newSSEWriter(w http.ResponseWriter) *sseWriter {
	writer := &sseWriter{w: w}
	if flusher, ok := w.(http.Flusher); ok {
		writer.flusher = flusher
	}
	return writer
}

func (w *sseWriter) prepare() {
	w.w.Header().Set("Content-Type", "text/event-stream")
	w.w.Header().Set("Cache-Control", "no-cache")
	w.w.Header().Set("Connection", "keep-alive")
	w.w.Header().Set("X-Accel-Buffering", "no")
	w.w.WriteHeader(http.StatusOK)
	if w.flusher != nil {
		w.flusher.Flush()
	}
}

func (w *sseWriter) write(tracker *admin.Tracker, frames [][]byte) error {
	for _, frame := range frames {
		n, err := w.w.Write(frame)
		tracker.Record().ResponseBytes += int64(n)
		if err != nil {
			return fmt.Errorf("%w: %v", errClientGone, err)
		}
	}
	if !w.wrote {
		w.wrote = true
		tracker.Stage(admin.StageFirstDownstreamWrite)
	}
	if w.flusher != nil {
		w.flusher.Flush()
	}
	if len(frames) > 0 {
		markFlushed(tracker, frames)
	}
	return nil
}

// forwardChatFrames passes an upstream Chat Completions chunk through, restoring
// the client-facing model ID.
func forwardChatFrames(raw []byte, model string) ([][]byte, error) {
	payload := normalizeCompletionModel(raw, model)
	body := make([]byte, 0, len(payload)+8)
	body = append(body, "data: "...)
	body = append(body, payload...)
	body = append(body, '\n', '\n')
	return [][]byte{body}, nil
}

// normalizeCompletionModel rewrites the model field to the client-facing ID.
func normalizeCompletionModel(raw []byte, model string) []byte {
	root, err := models.DecodeObject(raw)
	if err != nil {
		return raw
	}
	if strings.TrimSpace(models.String(root["model"])) == "" && model == "" {
		return raw
	}
	root["model"] = model
	encoded, err := jsonBytes(root)
	if err != nil {
		return raw
	}
	return encoded
}

// upstreamError builds an error from a non-2xx upstream response.
func upstreamError(stream *upstream.Stream, label string) (int, error) {
	status := stream.StatusCode
	body, err := readAll(stream, 1<<20)
	message := strings.TrimSpace(string(body))
	if decoded, decodeErr := models.DecodeObject(body); decodeErr == nil {
		if text := models.String(models.Object(decoded["error"])["message"]); text != "" {
			message = text
		} else if text := models.String(decoded["message"]); text != "" {
			message = text
		}
	}
	if message == "" && err != nil {
		message = err.Error()
	}
	if message == "" {
		message = http.StatusText(status)
	}
	if len(message) > 400 {
		message = message[:400]
	}
	return status, fmt.Errorf("%s returned HTTP %d: %s", label, status, credentials.Sanitize(message))
}

// readAll reads a bounded body.
func readAll(stream *upstream.Stream, limit int64) ([]byte, error) {
	if limit <= 0 {
		limit = 64 << 20
	}
	raw, err := io.ReadAll(io.LimitReader(stream.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, errors.New("upstream response exceeds the configured limit")
	}
	return raw, nil
}

// unwrapCompletionEnvelope removes Cline's `{"success":true,"data":{...}}`
// wrapper from a blocking completion. Streaming chunks are unwrapped per frame.
func unwrapCompletionEnvelope(raw []byte) []byte {
	root, err := models.DecodeObject(raw)
	if err != nil {
		return raw
	}
	if models.List(root["choices"]) != nil {
		return raw
	}
	data := models.Object(root["data"])
	if data == nil || models.List(data["choices"]) == nil {
		return raw
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return raw
	}
	return encoded
}

// upstreamMessage extracts a Cline error message from a JSON body.
func upstreamMessage(body []byte, status int) string {
	if root, err := models.DecodeObject(body); err == nil {
		if text := models.String(models.Object(root["error"])["message"]); text != "" {
			return credentials.Sanitize(text)
		}
		if text := models.String(root["message"]); text != "" {
			return credentials.Sanitize(text)
		}
	}
	return fmt.Sprintf("Cline returned HTTP %d: %s", status, http.StatusText(status))
}

// observeClaudeUsage copies the Anthropic usage block into the request record.
func (s *Server) observeClaudeUsage(request *requestContext, usage map[string]any) {
	if usage == nil {
		return
	}
	input := models.Number(usage["input_tokens"])
	cached := models.Number(usage["cache_read_input_tokens"])
	created := models.Number(usage["cache_creation_input_tokens"])
	request.tracker.ObserveUsage(input+cached+created, cached, models.Number(usage["output_tokens"]), 0)
}

func jsonBytes(value any) ([]byte, error) {
	return json.Marshal(value)
}
