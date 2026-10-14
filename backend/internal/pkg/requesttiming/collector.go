// Package requesttiming records bounded, request-local observations. It never
// retains request/response bodies, headers, URLs, or raw error messages.
package requesttiming

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
)

type key struct{}
type Span struct {
	Name    string  `json:"name"`
	StartMS float64 `json:"start_ms"`
	EndMS   float64 `json:"end_ms"`
	Attempt int     `json:"attempt,omitempty"`
}
type Attempt struct {
	Transport                  TransportInfo      `json:"transport,omitzero"`
	Protocol                   string             `json:"protocol,omitempty"`
	ALPN                       string             `json:"alpn,omitempty"`
	TLSHandshake               string             `json:"tls_handshake,omitempty"`
	BorrowLeaseHash            string             `json:"borrow_lease_hash,omitempty"`
	BorrowLeaseState           string             `json:"borrow_lease_state,omitempty"`
	BorrowLeaseEnd             string             `json:"borrow_lease_end,omitempty"`
	BorrowLeaseReleaseAttempts int                `json:"borrow_lease_release_attempts,omitempty"`
	Model                      string             `json:"model,omitempty"`
	ReasoningEffort            string             `json:"reasoning_effort,omitempty"`
	RetryAfterMS               int64              `json:"retry_after_ms,omitempty"`
	ReasoningEvents            int                `json:"reasoning_events,omitempty"`
	NoTextLongTail             bool               `json:"no_text_long_tail,omitempty"`
	BorrowRouteID              string             `json:"borrow_route_id,omitempty"`
	Terminal                   string             `json:"terminal,omitempty"`
	CleanupCanceled            bool               `json:"cleanup_canceled,omitempty"`
	Kind                       string             `json:"kind"`
	Parent                     int                `json:"parent,omitempty"`
	Number                     int                `json:"number"`
	AccountID                  int64              `json:"account_id"`
	ProxyID                    int64              `json:"proxy_id"`
	StartMS                    float64            `json:"start_ms"`
	EndMS                      *float64           `json:"end_ms"`
	Status                     int                `json:"status"`
	Error                      string             `json:"error,omitempty"`
	Reused                     *bool              `json:"reused"`
	RequestBytes               int64              `json:"request_bytes"`
	ResponseBytes              int64              `json:"response_bytes"`
	BodyEOF                    bool               `json:"body_eof"`
	Events                     map[string]float64 `json:"events"`
}
type BorrowRecoverySnapshot struct {
	TransportAttempts int    `json:"transport_attempts"`
	RequestsSent      int    `json:"requests_sent"`
	Recovering        bool   `json:"recovering"`
	OutputStarted     bool   `json:"output_started"`
	StopReason        string `json:"stop_reason,omitempty"`
}

func BorrowRecovery(ctx context.Context, state BorrowRecoverySnapshot) {
	c := From(ctx)
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.finished {
		c.data.BorrowRecovery = &state
	}
}

type Snapshot struct {
	SessionAffinity   *SessionAffinitySnapshot `json:"session_affinity,omitempty"`
	BorrowRecovery    *BorrowRecoverySnapshot  `json:"borrow_recovery,omitempty"`
	Outcome           string                   `json:"outcome,omitempty"`
	ClientDisconnect  bool                     `json:"client_disconnect"`
	Version           int                      `json:"version"`
	TraceID           string                   `json:"trace_id"`
	StartedAt         time.Time                `json:"started_at"`
	TotalMS           float64                  `json:"total_ms"`
	Status            int                      `json:"status"`
	Canceled          bool                     `json:"canceled"`
	Truncated         bool                     `json:"truncated"`
	BodyBytes         int64                    `json:"body_bytes"`
	BodyExpected      int64                    `json:"body_expected"`
	BodyComplete      bool                     `json:"body_complete"`
	BodyReadMS        float64                  `json:"body_read_ms"`
	DownstreamBytes   int64                    `json:"downstream_bytes"`
	DownstreamWriteMS float64                  `json:"downstream_write_ms"`
	DownstreamError   bool                     `json:"downstream_error"`
	TTFTMode          string                   `json:"ttft_mode,omitempty"`
	Terminal          string                   `json:"terminal,omitempty"`
	Events            map[string]float64       `json:"events"`
	Spans             []Span                   `json:"spans"`
	Attempts          []Attempt                `json:"attempts"`
}
type Collector struct {
	mu        sync.Mutex
	start     time.Time
	data      Snapshot
	finished  bool
	callbacks []func(Snapshot)
}

func New(start time.Time, expected int64) *Collector {
	return &Collector{start: start, data: Snapshot{Version: 1, TraceID: uuid.NewString(), StartedAt: start, BodyExpected: expected, Events: map[string]float64{}, Spans: []Span{}, Attempts: []Attempt{}}}
}
func With(ctx context.Context, c *Collector) context.Context {
	if c == nil {
		return ctx
	}
	return context.WithValue(ctx, key{}, c)
}
func From(ctx context.Context) *Collector {
	if ctx == nil {
		return nil
	}
	c, _ := ctx.Value(key{}).(*Collector)
	return c
}

// TraceID is safe to include in lifecycle logs, including after HTTP completion.
func TraceID(ctx context.Context) string {
	if c := From(ctx); c != nil {
		return c.data.TraceID // Immutable after New.
	}
	return ""
}
func (c *Collector) offset(t time.Time) float64 {
	return float64(t.Sub(c.start)) / float64(time.Millisecond)
}
func (c *Collector) event(name string, t time.Time) {
	if _, ok := c.data.Events[name]; !ok {
		c.data.Events[name] = c.offset(t)
	}
}
func Mark(ctx context.Context, name string) {
	c := From(ctx)
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.finished {
		c.event(name, time.Now())
	}
}
func Observe(ctx context.Context, name string) func() {
	c := From(ctx)
	if c == nil {
		return func() {}
	}
	start := time.Now()
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			if c.finished {
				return
			}
			if len(c.data.Spans) >= 256 {
				c.data.Truncated = true
				return
			}
			c.data.Spans = append(c.data.Spans, Span{Name: name, StartMS: c.offset(start), EndMS: c.offset(time.Now())})
		})
	}
}
func Output(ctx context.Context, semantic, visible bool, terminal string) {
	c := From(ctx)
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.finished {
		return
	}
	now := time.Now()
	if tr, ok := ctx.Value(attemptKey{}).(*Trace); ok && tr.c == c {
		for index := tr.index; index >= 0; {
			a := &c.data.Attempts[index]
			if semantic {
				if _, exists := a.Events["first_semantic"]; !exists {
					a.Events["first_semantic"] = c.offset(now)
				}
			}
			if visible {
				if _, exists := a.Events["first_visible"]; !exists {
					a.Events["first_visible"] = c.offset(now)
				}
			}
			if terminal != "" {
				a.Events["terminal"] = c.offset(now)
				a.Terminal = terminal
			}
			index = a.Parent - 1
		}
	}
	if semantic {
		c.event("first_semantic", now)
	}
	if visible {
		c.event("first_visible", now)
	}
	if terminal != "" {
		c.data.Terminal = terminal
		c.event("terminal", now)
	}
}
func Mode(ctx context.Context, mode string) {
	c := From(ctx)
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.finished {
		c.data.TTFTMode = mode
	}
}
func (c *Collector) snapshot() Snapshot {
	d := c.data
	if d.SessionAffinity != nil {
		v := *d.SessionAffinity
		d.SessionAffinity = &v
	}
	if d.BorrowRecovery != nil {
		v := *d.BorrowRecovery
		d.BorrowRecovery = &v
	}
	d.Events = cloneEvents(d.Events)
	d.Spans = append([]Span{}, d.Spans...)
	d.Attempts = append([]Attempt{}, d.Attempts...)
	for i := range d.Attempts {
		d.Attempts[i].Events = cloneEvents(d.Attempts[i].Events)
	}
	return d
}
func cloneEvents(src map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

// WhenFinished works in either order: the billing worker can bind a persisted
// usage record before or after the HTTP middleware has completed.
func (c *Collector) WhenFinished(fn func(Snapshot)) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if !c.finished {
		if len(c.callbacks) < 8 {
			c.callbacks = append(c.callbacks, fn)
		} else {
			c.data.Truncated = true
		}
		c.mu.Unlock()
		return
	}
	d := c.snapshot()
	c.mu.Unlock()
	fn(d)
}
func (c *Collector) Finish(status int, canceled bool) {
	c.mu.Lock()
	if c.finished {
		c.mu.Unlock()
		return
	}
	c.finished = true
	c.data.TotalMS = c.offset(time.Now())
	c.data.Status = status
	c.data.Canceled = canceled
	callbacks := c.callbacks
	c.callbacks = nil
	d := c.snapshot()
	c.mu.Unlock()
	for _, fn := range callbacks {
		fn(d)
	}
}

func Outcome(ctx context.Context, result string, disconnected bool) {
	c := From(ctx)
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.finished {
		c.data.Outcome = result
		c.data.ClientDisconnect = disconnected
	}
}

// AttemptByNumber refreshes exactly one retained error's lifecycle observations.
func AttemptByNumber(ctx context.Context, number int) *Attempt {
	c := From(ctx)
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if number <= 0 || number > len(c.data.Attempts) {
		return nil
	}
	a := c.data.Attempts[number-1]
	a.Events = cloneEvents(a.Events)
	return &a
}

// LatestAttempt is a detached, credential-free copy, safe to retain in error logs.
func LatestAttempt(ctx context.Context) *Attempt {
	c := From(ctx)
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.data.Attempts) - 1; i >= 0; i-- {
		a := c.data.Attempts[i]
		if a.Kind != "egress" {
			continue
		}
		if _, ok := a.Events["first_text"]; !ok && c.offset(time.Now())-a.StartMS >= 60000 && a.ReasoningEvents > 0 {
			a.NoTextLongTail = true
		}
		a.Events = cloneEvents(a.Events)
		if a.EndMS != nil {
			v := *a.EndMS
			a.EndMS = &v
		}
		if a.Reused != nil {
			v := *a.Reused
			a.Reused = &v
		}
		return &a
	}
	return nil
}
func RecoverySnapshot(ctx context.Context) *BorrowRecoverySnapshot {
	c := From(ctx)
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.data.BorrowRecovery == nil {
		return nil
	}
	v := *c.data.BorrowRecovery
	return &v
}

// StreamProgress observes structural progress independently of answer text.
// It never cancels, retries, or retains any model output.
func StreamProgress(ctx context.Context, reasoning, text bool) {
	c := From(ctx)
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.finished {
		return
	}
	now := c.offset(time.Now())
	if reasoning {
		c.event("first_reasoning", time.Now())
	}
	if text {
		c.event("first_text", time.Now())
	}
	if tr, ok := ctx.Value(attemptKey{}).(*Trace); ok && tr.c == c {
		for i := tr.index; i >= 0; {
			a := &c.data.Attempts[i]
			if reasoning {
				a.ReasoningEvents++
				if _, ok := a.Events["first_reasoning"]; !ok {
					a.Events["first_reasoning"] = now
				}
			}
			if _, ok := a.Events["first_text"]; !ok && now-a.StartMS >= 60000 && a.ReasoningEvents > 0 {
				a.NoTextLongTail = true
			}
			if text {
				if _, ok := a.Events["first_text"]; !ok {
					a.Events["first_text"] = now
				}
			}

			i = a.Parent - 1
		}
	}
}
