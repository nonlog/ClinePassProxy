// Package upstream speaks Cline's Chat Completions API directly.
//
// This is the inference data plane. Nothing here may route through
// CPA/CLIProxyAPI: the whole point of ClinePassProxy is that a large request
// body travels once, over plain HTTP, to Cline.
package upstream

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/proxy"

	"github.com/nonlog/ClinePassProxy/internal/credentials"
	"github.com/nonlog/ClinePassProxy/internal/version"
)

// Client pools HTTP clients by proxy URL so connections are reused across
// requests instead of being re-handshaked per call.
type Client struct {
	mu      sync.RWMutex
	baseURL string
	pool    sync.Map // proxy URL -> *http.Client
}

// New builds a client for a Cline-compatible API root.
func New(baseURL string) *Client {
	return &Client{baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/")}
}

// SetBaseURL repoints the client at another API root.
func (c *Client) SetBaseURL(baseURL string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
}

// BaseURL returns the configured API root.
func (c *Client) BaseURL() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.baseURL
}

// HTTPClient returns the pooled HTTP client for a credential's proxy URL, so
// other Cline endpoints (the account and quota API) use exactly the same
// transport, connection pool and proxy as the inference data plane.
func (c *Client) HTTPClient(proxyURL string) (*http.Client, error) {
	return c.client(proxyURL)
}

// endpoint joins an API path onto the current root. The root is a directory
// prefix such as https://api.cline.bot/api/v1, so the separator is required.
func (c *Client) endpoint(path string) string {
	return c.BaseURL() + "/" + strings.TrimPrefix(path, "/")
}

// Stream is an open upstream response.
type Stream struct {
	StatusCode int
	Header     http.Header
	Body       io.ReadCloser

	cancel context.CancelFunc
	once   sync.Once
}

// Close releases the response body and its request context. It is safe to call
// multiple times.
func (s *Stream) Close() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		if s.Body != nil {
			_ = s.Body.Close()
		}
		if s.cancel != nil {
			s.cancel()
		}
	})
}

// IsSSE reports whether the upstream declared an SSE content type.
func (s *Stream) IsSSE() bool {
	if s == nil {
		return false
	}
	return strings.Contains(strings.ToLower(s.Header.Get("Content-Type")), "text/event-stream")
}

// ChatRequest describes one upstream Chat Completions call.
type ChatRequest struct {
	Body    []byte
	Stream  bool
	Timeout time.Duration
	Trace   *Trace
}

// ChatCompletions opens a streaming or non-streaming Chat Completions request.
func (c *Client) ChatCompletions(ctx context.Context, credential credentials.Record, req ChatRequest) (*Stream, error) {
	return c.post(ctx, credential.ProxyURL, credential.APIKey, "/chat/completions", req.Body, req.Stream, req.Timeout, req.Trace)
}

// PostJSON sends a bounded JSON request to an arbitrary path on the client's
// configured API root. It is used for the Codex-compatible search adapter,
// whose upstream is CommandCodeProxy rather than Cline Chat Completions.
func (c *Client) PostJSON(ctx context.Context, apiKey, path string, body []byte, timeout time.Duration) (*Stream, error) {
	return c.Post(ctx, apiKey, path, body, false, timeout)
}

// Post sends a JSON request to an arbitrary path and preserves streaming when
// the caller asks for it. It is used for upstream APIs that expose an
// Anthropic-shaped endpoint, such as CommandCodeProxy native web search.
func (c *Client) Post(ctx context.Context, apiKey, path string, body []byte, stream bool, timeout time.Duration) (*Stream, error) {
	return c.post(ctx, "", apiKey, path, body, stream, timeout, nil)
}

func (c *Client) post(ctx context.Context, proxyURL, apiKey, path string, body []byte, stream bool, requestTimeout time.Duration, trace *Trace) (*Stream, error) {
	client, err := c.client(proxyURL)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(apiKey) == "" {
		return nil, errors.New("credential has no api key")
	}

	if requestTimeout <= 0 {
		requestTimeout = 300 * time.Second
	}
	requestCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	if trace != nil {
		requestCtx = httptrace.WithClientTrace(requestCtx, trace.ClientTrace())
	}

	httpReq, err := http.NewRequestWithContext(requestCtx, http.MethodPost, c.endpoint(path), bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", acceptHeader(stream))
	httpReq.Header.Set("User-Agent", version.Name+"/"+version.Version)

	resp, err := client.Do(httpReq)
	if err != nil {
		cancel()
		return nil, wrapTransportError(err)
	}
	if trace != nil {
		trace.Response(resp.Proto, resp.StatusCode)
	}
	return &Stream{StatusCode: resp.StatusCode, Header: resp.Header.Clone(), Body: resp.Body, cancel: cancel}, nil
}

func acceptHeader(stream bool) string {
	if stream {
		return "text/event-stream"
	}
	return "application/json"
}

func wrapTransportError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("Cline upstream request timed out: %w", err)
	}
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("Cline upstream request canceled: %w", err)
	}
	return fmt.Errorf("Cline upstream transport failed: %w", err)
}

func (c *Client) client(proxyURL string) (*http.Client, error) {
	key := strings.TrimSpace(proxyURL)
	if cached, ok := c.pool.Load(key); ok {
		return cached.(*http.Client), nil
	}
	client, err := newClient(key)
	if err != nil {
		return nil, err
	}
	actual, _ := c.pool.LoadOrStore(key, client)
	return actual.(*http.Client), nil
}

func newClient(proxyURL string) (*http.Client, error) {
	transport := &http.Transport{
		Proxy:                 nil,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 2 * time.Second,
		ResponseHeaderTimeout: 0, // large contexts legitimately take a long time
		DialContext:           (&net.Dialer{Timeout: 20 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	}
	if proxyURL != "" {
		parsed, err := url.Parse(proxyURL)
		if err != nil || parsed.Host == "" {
			return nil, errors.New("invalid credential proxy URL")
		}
		switch strings.ToLower(parsed.Scheme) {
		case "http", "https":
			transport.Proxy = http.ProxyURL(parsed)
		case "socks5", "socks5h":
			auth := (*proxy.Auth)(nil)
			if parsed.User != nil {
				password, _ := parsed.User.Password()
				auth = &proxy.Auth{User: parsed.User.Username(), Password: password}
			}
			dialer, err := proxy.SOCKS5("tcp", parsed.Host, auth, &net.Dialer{Timeout: 20 * time.Second, KeepAlive: 30 * time.Second})
			if err != nil {
				return nil, fmt.Errorf("build socks5 dialer: %w", err)
			}
			transport.Proxy = nil
			transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
				type result struct {
					conn net.Conn
					err  error
				}
				done := make(chan result, 1)
				go func() {
					conn, err := dialer.Dial(network, address)
					done <- result{conn, err}
				}()
				select {
				case r := <-done:
					return r.conn, r.err
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
		default:
			return nil, errors.New("unsupported credential proxy scheme")
		}
	}
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}, nil
}

// SSEDecoder reassembles Server-Sent Events from arbitrary read boundaries.
type SSEDecoder struct {
	reader *bufio.Reader
	limit  int64
	read   int64
}

// NewSSEDecoder wraps a stream body.
func NewSSEDecoder(body io.Reader, limit int64) *SSEDecoder {
	if limit <= 0 {
		limit = 64 << 20
	}
	return &SSEDecoder{reader: bufio.NewReaderSize(body, 64<<10), limit: limit}
}

// ErrStreamLimit reports that the upstream exceeded the configured response size.
var ErrStreamLimit = errors.New("upstream response exceeds the configured limit")

// ErrPartialFrame reports a stream that ended in the middle of an SSE frame.
var ErrPartialFrame = errors.New("upstream stream ended with an incomplete SSE frame")

// Next returns the next SSE event. It returns io.EOF at a clean end of stream
// and ErrPartialFrame when the stream stopped in the middle of an event.
func (d *SSEDecoder) Next() (Event, error) {
	var event Event
	var data []string
	for {
		line, _, err := d.readLine()
		if line != "" {
			switch {
			case strings.HasPrefix(line, "data:"):
				value := strings.TrimPrefix(line, "data:")
				value = strings.TrimPrefix(value, " ")
				data = append(data, value)
			case strings.HasPrefix(line, "event:"):
				event.Name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			}
		}
		if err == nil {
			if line != "" {
				continue
			}
			// Blank line terminates the frame.
			event.Data = []byte(strings.Join(data, "\n"))
			return event, nil
		}
		if !errors.Is(err, io.EOF) {
			return Event{}, err
		}
		// SSE events are terminated by a blank line, not merely by a newline on
		// the final data line. If EOF arrives while an event is buffered, the
		// transport was cut before the event boundary and the frame is partial.
		// Accepting "data: ...\n" at EOF can otherwise turn a truncated
		// finish_reason chunk into a false success.
		if line != "" || len(data) > 0 || event.Name != "" {
			return Event{}, ErrPartialFrame
		}
		return Event{}, io.EOF
	}
}

// Event is one decoded SSE frame. Comment-only frames carry no data and are
// skipped by Next.
type Event struct {
	Name string
	Data []byte
}

// readLine returns one line, whether it ended with a newline, and the read
// error. A final line that ran into EOF carries terminated=false.
func (d *SSEDecoder) readLine() (string, bool, error) {
	line, err := d.reader.ReadString('\n')
	d.read += int64(len(line))
	if d.read > d.limit {
		return "", false, ErrStreamLimit
	}
	terminated := err == nil
	line = strings.TrimSuffix(line, "\n")
	line = strings.TrimSuffix(line, "\r")
	if err != nil && line == "" {
		return "", terminated, err
	}
	if errors.Is(err, io.EOF) {
		// Keep reporting io.EOF so end-of-stream handling stays in one place;
		// terminated carries the difference between a clean and a partial line.
		return line, false, io.EOF
	}
	return line, terminated, err
}
