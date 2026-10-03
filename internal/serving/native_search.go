package serving

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/nonlog/ClinePassProxy/internal/admin"
	"github.com/nonlog/ClinePassProxy/internal/models"
	"github.com/nonlog/ClinePassProxy/internal/translate"
)

// hasNativeClaudeWebSearch identifies Anthropic's server-side web search tool.
// It must stay separate from ordinary client tools named WebSearch or search.
func hasNativeClaudeWebSearch(body map[string]any) bool {
	for _, raw := range models.List(body["tools"]) {
		tool := models.Object(raw)
		if strings.HasPrefix(models.String(tool["type"]), "web_search_20") {
			return true
		}
	}
	return false
}

// handleNativeClaudeWebSearch lets CommandCodeProxy execute Anthropic's
// server-side search tool. Cline's Chat Completions endpoint cannot produce
// server_tool_use/web_search_tool_result blocks, so native search requests
// must bypass the Cline credential pool and preserve the original body.
func (s *Server) handleNativeClaudeWebSearch(w http.ResponseWriter, r *http.Request, request *requestContext) {
	settings := s.Config.Get()
	if strings.TrimSpace(settings.SearchBaseURL) == "" {
		err := errors.New("Claude native web search is not configured")
		s.finishRequest(request, http.StatusServiceUnavailable, err)
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}

	searchKey := strings.TrimSpace(settings.SearchAPIKey)
	if searchKey == "" {
		searchKey = bearerToken(r)
	}
	if searchKey == "" {
		err := errors.New("CommandCodeProxy search API key is not configured")
		s.finishRequest(request, http.StatusUnauthorized, err)
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}

	streaming := models.Bool(request.body["stream"])
	s.search.SetBaseURL(settings.SearchBaseURL)
	request.tracker.Stage(admin.StageUpstreamRequestStart)
	stream, err := s.search.Post(r.Context(), searchKey, "/v1/messages", request.raw, streaming, time.Duration(settings.TimeoutSeconds)*time.Second)
	if err != nil {
		status := http.StatusBadGateway
		if r.Context().Err() != nil || errors.Is(err, context.Canceled) {
			status = statusClientClosed
		}
		s.finishRequest(request, status, err)
		if status != statusClientClosed {
			writeError(w, status, err.Error())
		}
		return
	}
	defer stream.Close()
	request.tracker.Stage(admin.StageUpstreamHeaders)

	if stream.StatusCode < 200 || stream.StatusCode >= 300 {
		status, upstreamErr := upstreamError(stream, "CommandCode native search")
		s.finishRequest(request, status, upstreamErr)
		writeError(w, status, upstreamErr.Error())
		return
	}

	if !streaming {
		response, err := readAll(stream, s.limitBytes())
		if err != nil {
			s.finishRequest(request, http.StatusBadGateway, err)
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		if _, err := models.DecodeObject(response); err != nil {
			err = errors.New("CommandCode native search returned invalid JSON")
			s.finishRequest(request, http.StatusBadGateway, err)
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		contentType := stream.Header.Get("Content-Type")
		if contentType == "" {
			contentType = "application/json"
		}
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(response)
		s.finishRequest(request, http.StatusOK, nil)
		return
	}

	writer := newSSEWriter(w)
	writer.prepare()
	request.streamed = true
	sawTerminal := false
	tail := ""
	buffer := make([]byte, 32<<10)
	for {
		n, readErr := stream.Body.Read(buffer)
		if n > 0 {
			request.tracker.Stage(admin.StageFirstUpstreamEvent)
			chunk := append([]byte(nil), buffer[:n]...)
			if err := writer.write(request.tracker, [][]byte{chunk}); err != nil {
				s.finishStreamFailure(request, err)
				return
			}
			tail += string(chunk)
			if len(tail) > 1024 {
				tail = tail[len(tail)-1024:]
			}
			if strings.Contains(tail, "event: message_stop") || strings.Contains(tail, `"type":"message_stop"`) {
				sawTerminal = true
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			frame, _ := translate.SSEEvent("error", map[string]any{
				"type":  "error",
				"error": map[string]any{"type": "api_error", "message": "CommandCode native search stream failed"},
			})
			_ = writer.write(request.tracker, [][]byte{frame})
			s.finishStreamFailure(request, readErr)
			return
		}
	}
	if !sawTerminal {
		frame, _ := translate.SSEEvent("error", map[string]any{
			"type":  "error",
			"error": map[string]any{"type": "api_error", "message": "CommandCode native search stream ended before message_stop"},
		})
		_ = writer.write(request.tracker, [][]byte{frame})
		s.finishStreamFailure(request, translate.ErrUpstreamTruncated)
		return
	}
	s.finishRequest(request, http.StatusOK, nil)
}
