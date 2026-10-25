package service

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync"
	"syscall"
	"time"
)

// Only allowlisted codes and relative times are persisted. No error strings,
// URLs, addresses, headers, credentials or SSE payloads enter diagnostics.
type BorrowHTTPAttempt struct {
	Phase             string           `json:"phase"`
	Number            int              `json:"number"`
	StartedAt         time.Time        `json:"started_at"`
	DurationMS        int64            `json:"duration_ms"`
	Status            int              `json:"status"`
	Events            map[string]int64 `json:"events"`
	ErrorClass        string           `json:"error_class,omitempty"`
	FailureStage      string           `json:"failure_stage,omitempty"`
	TimeoutStage      string           `json:"timeout_stage,omitempty"`
	CancellationCause string           `json:"cancellation_cause,omitempty"`
	Terminal          string           `json:"terminal,omitempty"`
	ConnectionReused  *bool            `json:"connection_reused,omitempty"`
}

type BorrowProbePhase struct {
	DurationMS int64               `json:"duration_ms"` // Includes admission and retry backoff.
	RetryCount int                 `json:"retry_count"`
	Attempts   []BorrowHTTPAttempt `json:"attempts"`
}

type borrowCancelReason string

func (r borrowCancelReason) Error() string { return string(r) }

const (
	borrowCancelUserStop           borrowCancelReason = "user_stop"
	borrowCancelBatchSuccess       borrowCancelReason = "batch_success"
	borrowCancelCandidateDeadline  borrowCancelReason = "candidate_deadline"
	borrowCancelValidationDeadline borrowCancelReason = "validation_deadline"
	borrowCancelRequestDeadline    borrowCancelReason = "request_deadline"
	borrowCancelServiceShutdown    borrowCancelReason = "service_shutdown"
	borrowCancelTargetLeaseLost    borrowCancelReason = "target_lease_lost"
)

func borrowCancellation(ctx context.Context, err error) string {
	cause := context.Cause(ctx)
	var reason borrowCancelReason
	if errors.As(cause, &reason) {
		return string(reason)
	}
	if errors.Is(err, ErrBorrowPaused) {
		return string(borrowCancelUserStop)
	}
	if borrowContinueTimedOut(cause) || borrowContinueTimedOut(err) {
		return "continue_deadline"
	}
	if errors.Is(cause, context.DeadlineExceeded) {
		return "unclassified_deadline"
	}
	if errors.Is(cause, context.Canceled) || errors.Is(err, context.Canceled) {
		return "unclassified_cancel"
	}
	return ""
}

type borrowHTTPTraceKey struct{}
type borrowAttemptNumberKey struct{}
type borrowHTTPTrace struct {
	mu       sync.Mutex
	data     BorrowHTTPAttempt
	stage    string
	finished bool
}

func newBorrowHTTPTrace(ctx context.Context, phase string) (context.Context, *borrowHTTPTrace) {
	number, _ := ctx.Value(borrowAttemptNumberKey{}).(int)
	if number < 1 {
		number = 1
	}
	tr := &borrowHTTPTrace{data: BorrowHTTPAttempt{Phase: phase, Number: number, StartedAt: time.Now(), Events: map[string]int64{}}, stage: "admission"}
	ctx = context.WithValue(ctx, borrowHTTPTraceKey{}, tr)
	trace := &httptrace.ClientTrace{
		GetConn:           func(string) { tr.mark("connection_start", "connection") },
		DNSStart:          func(httptrace.DNSStartInfo) { tr.mark("dns_start", "dns") },
		DNSDone:           func(i httptrace.DNSDoneInfo) { tr.mark("dns_end", "connection"); tr.fail(i.Err, "dns") },
		ConnectStart:      func(string, string) { tr.mark("tcp_start", "connection") },
		ConnectDone:       func(_, _ string, err error) { tr.mark("tcp_end", "connection"); tr.fail(err, "connection") },
		TLSHandshakeStart: func() { tr.mark("tls_start", "tls") },
		TLSHandshakeDone:  func(_ tls.ConnectionState, err error) { tr.mark("tls_end", "connection"); tr.fail(err, "tls") },
		GotConn: func(i httptrace.GotConnInfo) {
			tr.mark("connection_ready", "write")
			tr.mu.Lock()
			v := i.Reused
			if !tr.finished {
				tr.data.ConnectionReused = &v
				tr.data.ErrorClass = ""
				tr.data.FailureStage = ""
			}
			tr.mu.Unlock()
		},
		WroteHeaders: func() { tr.mark("headers_written", "write") },
		WroteRequest: func(i httptrace.WroteRequestInfo) {
			if i.Err == nil {
				tr.mark("request_written", "response_headers")
			}
			tr.fail(i.Err, "write")
		},
		GotFirstResponseByte: func() { tr.mark("first_byte", "response_headers") },
	}
	return httptrace.WithClientTrace(ctx, trace), tr
}
func borrowTrace(ctx context.Context) *borrowHTTPTrace {
	tr, _ := ctx.Value(borrowHTTPTraceKey{}).(*borrowHTTPTrace)
	return tr
}
func (tr *borrowHTTPTrace) mark(event, stage string) {
	if tr == nil {
		return
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if tr.finished {
		return
	}
	if _, ok := tr.data.Events[event]; !ok {
		tr.data.Events[event] = time.Since(tr.data.StartedAt).Milliseconds()
	}
	if tr.data.TimeoutStage == "" {
		tr.stage = stage
	}
}
func borrowErrorClass(err error, stage string) string {
	if err == nil {
		return ""
	}
	switch {
	case borrowContinueTimedOut(err), errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, ErrBorrowPaused):
		return "paused"
	case errors.Is(err, ErrBorrowCandidateExpiry):
		return "candidate_expired"
	case errors.Is(err, ErrBorrowCandidateTimeBudget):
		return "candidate_time_budget_insufficient"
	case errors.Is(err, ErrBorrowProbeBusy), errors.Is(err, ErrBorrowRecoveryWaiting), errors.Is(err, ErrBorrowRequestBudget), errors.Is(err, ErrBorrowRouteReady):
		return "admission_rejected"
	case errors.Is(err, ErrBorrowUnavailable), errors.Is(err, errBorrowSourceDeferred):
		return "configuration_unavailable"
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "unexpected_eof"
	case errors.Is(err, io.EOF):
		return "connection_eof"
	case errors.Is(err, syscall.ECONNRESET):
		return "connection_reset"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection_refused"
	case errors.Is(err, syscall.EPIPE):
		return "broken_pipe"
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "dns_error"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "timeout"
	}
	if stage == "tls" {
		return "tls_error"
	}
	return "transport_error"
}
func (tr *borrowHTTPTrace) fail(err error, stage string) {
	if tr == nil || err == nil {
		return
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if tr.finished {
		return
	}
	tr.data.ErrorClass = borrowErrorClass(err, stage)
	tr.data.FailureStage = stage
	if tr.data.ErrorClass == "timeout" {
		tr.data.TimeoutStage = stage
	}
}
func (tr *borrowHTTPTrace) response(resp *http.Response, ctx context.Context) {
	if tr == nil || resp == nil {
		return
	}
	tr.mark("response_headers", "first_sse")
	tr.mu.Lock()
	tr.data.Status = resp.StatusCode
	tr.mu.Unlock()
	if resp.Body != nil {
		resp.Body = &borrowProbeDiagnosticBody{ReadCloser: resp.Body, trace: tr, ctx: ctx}
	}
}
func (tr *borrowHTTPTrace) finish(ctx context.Context, err error) {
	if tr == nil {
		return
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if tr.finished {
		return
	}
	tr.finished = true
	tr.data.DurationMS = time.Since(tr.data.StartedAt).Milliseconds()
	cause := borrowCancellation(ctx, err)
	if err != nil || cause != "" {
		if tr.data.ErrorClass == "" {
			tr.data.ErrorClass = borrowErrorClass(err, tr.stage)
		}
		if cause != "" {
			tr.data.CancellationCause = cause
		}
		if cause == "continue_deadline" || cause == "candidate_deadline" || cause == "validation_deadline" || cause == "batch_deadline" || cause == "request_deadline" || cause == "unclassified_deadline" || tr.data.ErrorClass == "timeout" {
			if tr.data.TimeoutStage == "" {
				tr.data.TimeoutStage = tr.stage
			}
			tr.data.ErrorClass = "timeout"
		}
		if tr.data.ErrorClass == "" && cause != "" {
			tr.data.ErrorClass = "cancelled"
		}
		if tr.data.FailureStage == "" {
			tr.data.FailureStage = tr.stage
		}
	}
	if tr.data.Status >= 400 && tr.data.ErrorClass == "" {
		tr.data.ErrorClass = "upstream_http_error"
		tr.data.FailureStage = "response_headers"
	}
	if err == nil && tr.data.Status == 200 && tr.data.Terminal == "" && tr.data.ErrorClass == "" {
		tr.data.ErrorClass = "incomplete_stream"
		tr.data.FailureStage = "stream"
	}
}
func (tr *borrowHTTPTrace) interrupted(ctx context.Context) {
	if tr == nil {
		return
	}
	cause := borrowCancellation(ctx, nil)
	if cause == "" {
		return
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if tr.finished {
		return
	}
	tr.data.CancellationCause = cause
	switch cause {
	case "continue_deadline", "candidate_deadline", "validation_deadline", "batch_deadline", "request_deadline", "unclassified_deadline":
		tr.data.TimeoutStage = tr.stage
		tr.data.ErrorClass = "timeout"
	default:
		tr.data.ErrorClass = "cancelled"
	}
	tr.data.FailureStage = tr.stage
}

func (tr *borrowHTTPTrace) snapshot() *BorrowHTTPAttempt {
	if tr == nil {
		return nil
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	data := tr.data
	data.Events = make(map[string]int64, len(tr.data.Events))
	for k, v := range tr.data.Events {
		data.Events[k] = v
	}
	return &data
}

// Parsing is incremental, supports split/multiline SSE events and is bounded.
// It observes only event types; the normal probe still owns parsing/verdicts.
type borrowProbeDiagnosticBody struct {
	io.ReadCloser
	trace         *borrowHTTPTrace
	ctx           context.Context
	line, payload []byte
	oversized     bool
}

const borrowSSEDiagnosticLimit = 64 << 10

func (b *borrowProbeDiagnosticBody) event() {
	if len(b.payload) == 0 {
		return
	}
	b.trace.mark("first_sse", "stream")
	var e struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(b.payload, &e) == nil {
		switch e.Type {
		case "response.completed":
			b.trace.mark("completed", "eof")
			b.trace.mu.Lock()
			if !b.trace.finished {
				b.trace.data.Terminal = "completed"
			}
			b.trace.mu.Unlock()
		case "response.failed", "response.incomplete", "error":
			b.trace.mark("failed", "eof")
			b.trace.mu.Lock()
			if !b.trace.finished {
				b.trace.data.Terminal = "failed"
				b.trace.data.ErrorClass = "upstream_stream_error"
				b.trace.data.FailureStage = "stream"
			}
			b.trace.mu.Unlock()
		}
	}
	b.payload = nil
}
func (b *borrowProbeDiagnosticBody) observe(data []byte) {
	for _, c := range data {
		if c != '\n' {
			if len(b.line) < borrowSSEDiagnosticLimit {
				b.line = append(b.line, c)
			} else {
				b.oversized = true
			}
			continue
		}
		line := bytes.TrimSuffix(b.line, []byte{'\r'})
		if !b.oversized {
			if len(line) == 0 {
				b.event()
			} else if bytes.HasPrefix(line, []byte("data:")) {
				p := bytes.TrimPrefix(line, []byte("data:"))
				p = bytes.TrimPrefix(p, []byte{' '})
				if len(b.payload)+len(p)+1 <= borrowSSEDiagnosticLimit {
					b.payload = append(b.payload, p...)
					b.payload = append(b.payload, '\n')
				}
			}
		}
		b.line = nil
		b.oversized = false
	}
}
func (b *borrowProbeDiagnosticBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if b.ctx.Err() != nil {
		b.trace.interrupted(b.ctx)
	}
	if n > 0 {
		b.observe(p[:n])
	}
	if err == io.EOF {
		if len(b.line) > 0 {
			b.observe([]byte{'\n'})
		}
		b.event()
		b.trace.mark("eof", "done")
		b.trace.finish(b.ctx, nil)
	} else if err != nil {
		b.trace.finish(b.ctx, err)
	}
	return n, err
}
func (b *borrowProbeDiagnosticBody) Close() error { return b.ReadCloser.Close() }
