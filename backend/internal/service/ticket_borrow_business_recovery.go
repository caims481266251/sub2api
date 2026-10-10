package service

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
)

var ErrBorrowRecoveryBudget = errors.New("borrow business recovery budget exhausted")
var ErrBorrowRecoveryTimeout = errors.New("borrow business recovery timed out before output")

type borrowBusinessRecoveryKey struct{}

// BorrowBusinessRecovery belongs to a logical business request, not an account
// or transport. It remains attached when forwarding detaches for usage draining.
type BorrowBusinessRecovery struct {
	closed                                                  bool
	mu                                                      sync.Mutex
	client                                                  context.Context
	active, recovering, committed, written, explicitFailure bool
	writePossible                                           bool
	transports, sends                                       int
	firstStart, firstSend                                   time.Time
	pinnedTarget                                            int64
	window, timeout                                         time.Duration
	stopReason                                              string
	err                                                     error
	recoveryCtx                                             context.Context
	cancel                                                  context.CancelCauseFunc
	timer                                                   *time.Timer
	stopClient                                              func() bool
	cleanup                                                 []func()
	fallback                                                int64
}

func WithBorrowBusinessRecovery(ctx context.Context, cfg *config.Config) (context.Context, *BorrowBusinessRecovery) {
	if cfg != nil && cfg.Gateway.BorrowBusinessRecoveryDisabled {
		return ctx, nil
	}
	if existing := BorrowBusinessRecoveryFromContext(ctx); existing != nil {
		return ctx, existing
	}
	r := &BorrowBusinessRecovery{client: ctx, window: 15 * time.Second, timeout: 30 * time.Second}
	if cfg != nil {
		if cfg.Gateway.BorrowBusinessRetryWindowSeconds > 0 {
			r.window = time.Duration(cfg.Gateway.BorrowBusinessRetryWindowSeconds) * time.Second
		}
		if cfg.Gateway.BorrowBusinessRecoveryTimeoutSeconds > 0 {
			r.timeout = time.Duration(cfg.Gateway.BorrowBusinessRecoveryTimeoutSeconds) * time.Second
		}
	}
	return context.WithValue(ctx, borrowBusinessRecoveryKey{}, r), r
}

func BorrowBusinessRecoveryFromContext(ctx context.Context) *BorrowBusinessRecovery {
	if ctx == nil {
		return nil
	}
	r, _ := ctx.Value(borrowBusinessRecoveryKey{}).(*BorrowBusinessRecovery)
	return r
}

func (r *BorrowBusinessRecovery) Active() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active
}

func (r *BorrowBusinessRecovery) Activate(target int64, bound bool) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.active = true
	if r.pinnedTarget != 0 && r.pinnedTarget != target {
		return ErrBorrowUnavailable
	}
	if bound {
		r.pinnedTarget = target
	}
	return r.err
}

func (r *BorrowBusinessRecovery) Pinned() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pinnedTarget != 0
}

func (r *BorrowBusinessRecovery) Err() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

// BeginTransport limits Client.Do invocations too, so a TLS retry cannot silently
// create an extra recovery. Sends are counted separately when headers are written.
func (r *BorrowBusinessRecovery) BeginTransport(ctx context.Context) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.active {
		return nil
	}
	if r.err != nil {
		return r.err
	}
	if r.committed || r.transports >= 2 || (r.transports > 0 && !r.recovering) {
		r.stopReason = "attempt_limit"
		return ErrBorrowRecoveryBudget
	}
	if r.transports == 0 {
		r.firstStart = time.Now()
	}
	r.transports++
	r.written, r.explicitFailure, r.writePossible = false, false, false
	r.observeLocked(ctx)
	return nil
}

// HTTP may partially write before WroteHeaders fires. Once a connection is
// handed to its writer, require an explicit upstream failure before replaying.
func (r *BorrowBusinessRecovery) WritePossible() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active {
		r.writePossible = true
	}
}

func (r *BorrowBusinessRecovery) WroteHeaders(ctx context.Context) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.active || r.written {
		return
	}
	r.written, r.writePossible = true, true
	r.sends++
	if r.firstSend.IsZero() {
		r.firstSend = time.Now()
	}
	r.observeLocked(ctx)
}

func (r *BorrowBusinessRecovery) ExplicitFailure() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.explicitFailure = true
}

// ReserveRecovery is called only after existing replay-safety and error
// classification checks. 429 never gets the legacy account-wide retry window.
func (r *BorrowBusinessRecovery) ReserveRecovery(ctx context.Context, status int) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.active {
		return false
	}
	start := r.firstSend
	if start.IsZero() {
		start = r.firstStart
	}
	switch {
	case status == http.StatusTooManyRequests:
		r.stopReason = "upstream_429"
	case r.err != nil:
		r.stopReason = "recovery_timeout"
	case r.committed:
		r.stopReason = "output_started"
	case r.recovering || r.transports >= 2:
		r.stopReason = "attempt_limit"
	case start.IsZero() || time.Since(start) >= r.window:
		r.stopReason = "retry_window_elapsed"
	case r.writePossible && !r.explicitFailure:
		r.stopReason = "ambiguous_upstream_execution"
	case r.client.Err() != nil:
		r.stopReason = "client_canceled"
	default:
		r.recovering = true
		r.recoveryCtx, r.cancel = context.WithCancelCause(context.Background())
		r.timer = time.AfterFunc(r.timeout, func() { r.expire(ErrBorrowRecoveryTimeout) })
		r.stopClient = context.AfterFunc(r.client, func() { r.expire(r.client.Err()) })
		r.observeLocked(ctx)
		return true
	}
	r.observeLocked(ctx)
	return false
}

func (r *BorrowBusinessRecovery) expire(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.committed || r.cancel == nil {
		return
	}
	r.err = err
	r.stopReason = "recovery_timeout"
	if !errors.Is(err, ErrBorrowRecoveryTimeout) {
		r.stopReason = "client_canceled"
	}
	r.cancel(err)
	r.observeLocked(r.client)
}

// Context applies the recovery deadline to selection, admission and transport,
// even after the upstream context has been detached. The timer can be disarmed
// without canceling a successful stream, unlike context.WithTimeout.
func (r *BorrowBusinessRecovery) Context(ctx context.Context) context.Context {
	if r == nil {
		return ctx
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.recovering || r.committed {
		return ctx
	}
	child, cancel := context.WithCancelCause(ctx)
	stop := context.AfterFunc(r.recoveryCtx, func() { cancel(context.Cause(r.recoveryCtx)) })
	if err := context.Cause(r.recoveryCtx); err != nil {
		cancel(err)
	}
	r.cleanup = append(r.cleanup, func() { stop(); cancel(context.Canceled) })
	return child
}

func BorrowBusinessRecoveryContext(ctx context.Context) context.Context {
	return BorrowBusinessRecoveryFromContext(ctx).Context(ctx)
}

// Observing irreversible upstream output is a conservative replay boundary. It
// protects bridge endpoints too, before they convert Responses events downstream.
func (r *BorrowBusinessRecovery) OutputStarted(ctx context.Context) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.active || r.err != nil {
		return
	}
	r.committed = true
	if r.timer != nil {
		r.timer.Stop()
	}
	if r.stopClient != nil {
		r.stopClient()
	}
	r.observeLocked(ctx)
}

func (r *BorrowBusinessRecovery) SetFallback(id int64) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fallback = id
}

func (r *BorrowBusinessRecovery) TakeFallback() int64 {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	id := r.fallback
	r.fallback = 0
	if r.err != nil || r.transports >= 2 || r.committed {
		return 0
	}
	return id
}

func (r *BorrowBusinessRecovery) observeLocked(ctx context.Context) {
	requesttiming.BorrowRecovery(ctx, requesttiming.BorrowRecoverySnapshot{TransportAttempts: r.transports, RequestsSent: r.sends, Recovering: r.recovering, OutputStarted: r.committed, StopReason: r.stopReason})
}

func (r *BorrowBusinessRecovery) Close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	if r.timer != nil {
		r.timer.Stop()
	}
	if r.stopClient != nil {
		r.stopClient()
	}
	if r.cancel != nil {
		r.cancel(context.Canceled)
	}
	if r.active {
		r.observeLocked(r.client)
	}
	for _, stop := range r.cleanup {
		stop()
	}
}

func (r *BorrowBusinessRecovery) FreshConnectionRequired() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active && r.recovering
}

func (r *BorrowBusinessRecovery) CheckTarget(id int64) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active && r.pinnedTarget != 0 && r.pinnedTarget != id {
		return ErrBorrowUnavailable
	}
	return r.err
}

func (r *BorrowBusinessRecovery) MayRetryBeforeWrite() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active && r.transports > 0 && !r.writePossible
}

// ObserveSSE uses upstream semantic events, not downstream pings or role-only
// chat chunks, so recovery stays bounded while an upstream remains silent.
func (r *BorrowBusinessRecovery) ObserveSSE(ctx context.Context, payload []byte, eventType string) {
	if !r.Active() || !gjson.ValidBytes(payload) {
		return
	}
	event := gjson.ParseBytes(payload)
	if eventType == "" {
		eventType = event.Get("type").String()
	}
	eventType = strings.TrimSpace(eventType)
	if eventType == "response.failed" || eventType == "error" || event.Get("error").Exists() {
		r.ExplicitFailure()
		return
	}
	if eventType == "ping" {
		return
	}
	if choices := event.Get("choices"); choices.IsArray() {
		for _, choice := range choices.Array() {
			delta := choice.Get("delta")
			if delta.Get("content").String() != "" || delta.Get("reasoning_content").String() != "" || delta.Get("refusal").String() != "" || len(delta.Get("tool_calls").Array()) > 0 || delta.Get("function_call").Exists() || choice.Get("finish_reason").String() != "" {
				r.OutputStarted(ctx)
				return
			}
		}
		return
	}
	if strings.HasPrefix(eventType, "response.") && openAIStreamDataStartsClientOutput(string(payload), eventType) {
		r.OutputStarted(ctx)
	}
}
