package upstream

import (
	"crypto/tls"
	"net/http/httptrace"
	"sync"
	"time"
)

const maxTraceEvents = 64

// TraceEvent contains timing only; connection addresses, headers and errors are
// deliberately excluded from diagnostics.
type TraceEvent struct {
	Name    string  `json:"name"`
	AtMS    float64 `json:"at_ms"`
	Success *bool   `json:"success,omitempty"`
}

// TraceSnapshot describes one upstream attempt on the request's time origin.
type TraceSnapshot struct {
	Events     []TraceEvent `json:"events"`
	ConnReused bool         `json:"conn_reused"`
	WasIdle    bool         `json:"was_idle"`
	IdleTimeMS float64      `json:"idle_time_ms"`
	Protocol   string       `json:"protocol,omitempty"`
	StatusCode int          `json:"status_code,omitempty"`
}

// Trace records bounded HTTP transport timings. HTTP trace hooks may run
// concurrently with each other and with management reads.
type Trace struct {
	mu        sync.Mutex
	startedAt time.Time
	snapshot  TraceSnapshot
}

func NewTrace(startedAt time.Time) *Trace {
	if startedAt.IsZero() {
		startedAt = time.Now()
	}
	return &Trace{startedAt: startedAt}
}

func (t *Trace) recordLocked(name string, success *bool) {
	if len(t.snapshot.Events) < maxTraceEvents {
		t.snapshot.Events = append(t.snapshot.Events, TraceEvent{
			Name: name, AtMS: float64(time.Since(t.startedAt)) / float64(time.Millisecond), Success: success,
		})
	}
}

func (t *Trace) record(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.recordLocked(name, nil)
}

func (t *Trace) completed(name string, err error) {
	success := err == nil
	t.mu.Lock()
	defer t.mu.Unlock()
	t.recordLocked(name, &success)
}

// ClientTrace returns the standard library hooks for this attempt. Callback
// arguments containing private transport data are never retained.
func (t *Trace) ClientTrace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		GetConn: func(string) { t.record("get_conn") },
		GotConn: func(info httptrace.GotConnInfo) {
			t.mu.Lock()
			defer t.mu.Unlock()
			t.snapshot.ConnReused = info.Reused
			t.snapshot.WasIdle = info.WasIdle
			t.snapshot.IdleTimeMS = float64(info.IdleTime) / float64(time.Millisecond)
			t.recordLocked("got_conn", nil)
		},
		DNSStart:          func(httptrace.DNSStartInfo) { t.record("dns_start") },
		DNSDone:           func(info httptrace.DNSDoneInfo) { t.completed("dns_done", info.Err) },
		ConnectStart:      func(string, string) { t.record("connect_start") },
		ConnectDone:       func(_, _ string, err error) { t.completed("connect_done", err) },
		TLSHandshakeStart: func() { t.record("tls_start") },
		TLSHandshakeDone:  func(_ tls.ConnectionState, err error) { t.completed("tls_done", err) },
		WroteHeaders:      func() { t.record("wrote_headers") },
		WroteRequest:      func(info httptrace.WroteRequestInfo) { t.completed("wrote_request", info.Err) },
		GotFirstResponseByte: func() {
			t.record("first_response_byte")
		},
	}
}

// Response records when the complete final response headers are available.
func (t *Trace) Response(protocol string, statusCode int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.snapshot.Protocol = protocol
	t.snapshot.StatusCode = statusCode
	t.recordLocked("response_headers", nil)
}

func (t *Trace) Snapshot() TraceSnapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	snapshot := t.snapshot
	snapshot.Events = append([]TraceEvent(nil), t.snapshot.Events...)
	for i := range snapshot.Events {
		if success := snapshot.Events[i].Success; success != nil {
			copied := *success
			snapshot.Events[i].Success = &copied
		}
	}
	return snapshot
}
