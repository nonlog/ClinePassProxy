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

// serveMessages converts Anthropic Messages to Cline Chat Completions and back.
func (s *Server) serveMessages(w http.ResponseWriter, r *http.Request, request *requestContext, credential credentials.Record, timeout time.Duration) (int, error, bool) {
	payload, err := translate.ClaudeMessagesToChatCompletions(request.raw, request.upstream, models.Bool(request.body["stream"]))
	if err != nil {
		return http.StatusBadRequest, err, false
	}
	request.tracker.Stage(admin.StageTranslateDone)

	stream, err := s.openUpstream(r, request, credential, payload, models.Bool(request.body["stream"]), timeout)
	if err != nil {
		return http.StatusBadGateway, err, true
	}
	defer stream.Close()
	request.tracker.Stage(admin.StageUpstreamHeaders)

	if stream.StatusCode < 200 || stream.StatusCode >= 300 {
		status, err := upstreamError(stream, "Cline")
		return status, err, retryableUpstreamStatus(status)
	}

	if !models.Bool(request.body["stream"]) {
		raw, err := readAll(stream, s.limitBytes())
		if err != nil {
			return http.StatusBadGateway, err, true
		}
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

	converter := translate.NewClaudeStreamConverter(request.model, request.raw)
	writer := newSSEWriter(w)
	writer.prepare()

	startFrames, err := converter.Start()
	if err != nil {
		return http.StatusInternalServerError, err, false
	}
	if err := writer.write(request.tracker, startFrames); err != nil {
		s.finishRequest(request, statusClientClosed, err)
		return statusClientClosed, err, false
	}

	err = s.consumeUpstream(r, request, stream,
		func(raw []byte) ([][]byte, error) { return converter.Feed(raw) },
		func(frames [][]byte) error { return writer.write(request.tracker, frames) },
		func() ([][]byte, error) { return converter.Done() },
	)
	if err != nil {
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
	request.tracker.Stage(admin.StageTranslateDone)

	stream, err := s.openUpstream(r, request, credential, payload, models.Bool(request.body["stream"]), timeout)
	if err != nil {
		return http.StatusBadGateway, err, true
	}
	defer stream.Close()
	request.tracker.Stage(admin.StageUpstreamHeaders)

	if stream.StatusCode < 200 || stream.StatusCode >= 300 {
		status, err := upstreamError(stream, "Cline")
		return status, err, retryableUpstreamStatus(status)
	}

	translated, _ := jsonBytes(payload)
	if !models.Bool(request.body["stream"]) {
		raw, err := readAll(stream, s.limitBytes())
		if err != nil {
			return http.StatusBadGateway, err, true
		}
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

	converter := translate.NewResponsesStreamConverter(request.model, request.raw, translated)
	writer := newSSEWriter(w)
	writer.prepare()

	startFrames, err := converter.Start()
	if err != nil {
		return http.StatusInternalServerError, err, false
	}
	if err := writer.write(request.tracker, startFrames); err != nil {
		s.finishRequest(request, statusClientClosed, err)
		return statusClientClosed, err, false
	}

	err = s.consumeUpstream(r, request, stream,
		func(raw []byte) ([][]byte, error) { return converter.Feed(raw) },
		func(frames [][]byte) error { return writer.write(request.tracker, frames) },
		func() ([][]byte, error) { return converter.Done() },
	)
	if err != nil {
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
	stream := models.Bool(request.body["stream"])
	request.tracker.Stage(admin.StageTranslateDone)

	upstreamStream, err := s.openUpstream(r, request, credential, payload, stream, timeout)
	if err != nil {
		return http.StatusBadGateway, err, true
	}
	defer upstreamStream.Close()
	request.tracker.Stage(admin.StageUpstreamHeaders)

	if upstreamStream.StatusCode < 200 || upstreamStream.StatusCode >= 300 {
		status, err := upstreamError(upstreamStream, "Cline")
		return status, err, retryableUpstreamStatus(status)
	}

	if !stream {
		raw, err := readAll(upstreamStream, s.limitBytes())
		if err != nil {
			return http.StatusBadGateway, err, true
		}
		s.observeChunk(request, raw)
		s.finishRequest(request, http.StatusOK, nil)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(normalizeCompletionModel(raw, request.model))
		return http.StatusOK, nil, false
	}

	writer := newSSEWriter(w)
	writer.prepare()
	err = s.consumeUpstream(r, request, upstreamStream,
		func(raw []byte) ([][]byte, error) { return forwardChatFrames(raw, request.model) },
		func(frames [][]byte) error { return writer.write(request.tracker, frames) },
		func() ([][]byte, error) { return [][]byte{translate.DoneEvent()}, nil },
	)
	if err != nil {
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
	if errors.Is(err, context.Canceled) || errors.Is(err, upstream.ErrPartialFrame) {
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
		if _, err := w.w.Write(frame); err != nil {
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
