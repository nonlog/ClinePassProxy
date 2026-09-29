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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
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
	baseURL string
	pool    sync.Map // proxy URL -> *http.Client
}

// New builds a client for a Cline-compatible API root.
func New(baseURL string) *Client {
	return &Client{baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/")}
}

// SetBaseURL repoints the client at another API root.
func (c *Client) SetBaseURL(baseURL string) {
	c.baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
}

// BaseURL returns the configured API root.
func (c *Client) BaseURL() string { return c.baseURL }

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
}

// ChatCompletions opens a streaming or non-streaming Chat Completions request.
func (c *Client) ChatCompletions(ctx context.Context, credential credentials.Record, req ChatRequest) (*Stream, error) {
	client, err := c.client(credential.ProxyURL)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(credential.APIKey) == "" {
		return nil, errors.New("credential has no api key")
	}

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = 300 * time.Second
	}
	requestCtx, cancel := context.WithTimeout(ctx, timeout)

	httpReq, err := http.NewRequestWithContext(requestCtx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(req.Body))
	if err != nil {
		cancel()
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+credential.APIKey)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", acceptHeader(req.Stream))
	httpReq.Header.Set("User-Agent", version.Name+"/"+version.Version)

	resp, err := client.Do(httpReq)
	if err != nil {
		cancel()
		return nil, wrapTransportError(err)
	}
	return &Stream{StatusCode: resp.StatusCode, Header: resp.Header.Clone(), Body: resp.Body, cancel: cancel}, nil
}

// JSON performs a non-streaming GET and returns the raw body.
func (c *Client) JSON(ctx context.Context, credential credentials.Record, path string, timeout time.Duration) (int, []byte, error) {
	client, err := c.client(credential.ProxyURL)
	if err != nil {
		return 0, nil, err
	}
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(requestCtx, http.MethodGet, c.baseURL+strings.TrimPrefix(path, "/"), nil)
	if err != nil {
		return 0, nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+credential.APIKey)
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("User-Agent", version.Name+"/"+version.Version)

	resp, err := client.Do(httpReq)
	if err != nil {
		return 0, nil, wrapTransportError(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, body, nil
}

// PlanUsage fetches the Cline quota snapshot for a credential.
func (c *Client) PlanUsage(ctx context.Context, credential credentials.Record) (credentials.Usage, error) {
	usage := credentials.Usage{Status: "ok", CheckedAt: time.Now().UTC()}

	status, body, err := c.JSON(ctx, credential, "/users/me/plan/usage-limits", 20*time.Second)
	if err != nil {
		return credentials.Usage{Status: classifyUsageError(err, 0), Error: err.Error(), CheckedAt: time.Now().UTC()}, nil
	}
	if status < 200 || status >= 300 {
		return credentials.Usage{Status: classifyUsageError(nil, status), Error: fmt.Sprintf("Cline returned HTTP %d for the usage limit query", status), CheckedAt: time.Now().UTC()}, nil
	}

	if data, ok := unwrapEnvelope(body); ok {
		body = data
	}
	var limits struct {
		Limits []struct {
			Type        string   `json:"type"`
			PercentUsed *float64 `json:"percentUsed"`
			ResetsAt    string   `json:"resetsAt"`
		} `json:"limits"`
	}
	if err := json.Unmarshal(body, &limits); err != nil {
		usage.Status = "unavailable"
		usage.Error = "Cline returned an unrecognized usage limit payload"
		return usage, nil
	}
	for _, limit := range limits.Limits {
		if strings.TrimSpace(limit.Type) == "" {
			continue
		}
		value := limit.PercentUsed
		if value != nil && *value < 0 {
			value = nil
		}
		usage.Limits = append(usage.Limits, credentials.UsageLimit{Type: limit.Type, PercentUsed: value, ResetsAt: limit.ResetsAt})
	}
	usage.Raw = append(json.RawMessage(nil), body...)

	if status, body, err := c.JSON(ctx, credential, "/users/me/plan", 20*time.Second); err == nil && status >= 200 && status < 300 {
		if data, ok := unwrapEnvelope(body); ok {
			body = data
		}
		var plan struct {
			CurrentPeriodEnd string `json:"currentPeriodEnd"`
			Plan             *struct {
				DisplayName string `json:"displayName"`
			} `json:"plan"`
		}
		if json.Unmarshal(body, &plan) == nil {
			if plan.Plan != nil {
				usage.PlanName = plan.Plan.DisplayName
			}
			usage.PeriodEnd = plan.CurrentPeriodEnd
		}
	}
	return usage, nil
}

func classifyUsageError(err error, status int) string {
	switch {
	case status == 401 || status == 403:
		return "unauthorized"
	case status == 429:
		return "rate_limited"
	case status >= 500:
		return "unavailable"
	case err != nil && errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case err != nil:
		return "unavailable"
	default:
		return "unavailable"
	}
}

// unwrapEnvelope handles Cline's `{"success":true,"data":{...}}` wrapper.
func unwrapEnvelope(body []byte) ([]byte, bool) {
	var envelope struct {
		Success bool            `json:"success"`
		Data    json.RawMessage `json:"data"`
	}
	if json.Unmarshal(body, &envelope) != nil || !envelope.Success || len(envelope.Data) == 0 {
		return nil, false
	}
	return envelope.Data, true
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

// Next returns the next SSE event. It returns io.EOF at a clean end of stream.
func (d *SSEDecoder) Next() (Event, error) {
	var event Event
	var data []string
	for {
		line, err := d.readLine()
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
		// End of stream. Flush a final frame that was not blank-line terminated.
		if len(data) > 0 || event.Name != "" {
			event.Data = []byte(strings.Join(data, "\n"))
			return event, nil
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

func (d *SSEDecoder) readLine() (string, error) {
	line, err := d.reader.ReadString('\n')
	d.read += int64(len(line))
	if d.read > d.limit {
		return "", ErrStreamLimit
	}
	line = strings.TrimSuffix(line, "\n")
	line = strings.TrimSuffix(line, "\r")
	if err != nil && line == "" {
		return "", err
	}
	return line, err
}
