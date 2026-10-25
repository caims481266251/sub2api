package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

// AccountRecentRequest is a bounded observation, independent of billing and scheduling.
type AccountRecentRequest struct {
	AttemptID    string    `json:"attempt_id"`
	RequestID    string    `json:"request_id,omitempty"`
	AccountID    int64     `json:"account_id"`
	Source       string    `json:"source"`
	Model        string    `json:"model"`
	CompletedAt  time.Time `json:"completed_at"`
	Outcome      string    `json:"outcome"`
	HTTPStatus   int       `json:"http_status"`
	FirstTokenMs *int64    `json:"first_token_ms"`
	DurationMs   int64     `json:"duration_ms"`
	ErrorCode    string    `json:"error_code,omitempty"`
	ErrorMessage string    `json:"error_message,omitempty"`
}

type AccountRecentRequestsCache interface {
	Append(context.Context, string, AccountRecentRequest) error
	Batch(context.Context, map[int64]string) (map[int64][]AccountRecentRequest, error)
	Close() error
}

func (s *OpenAIGatewayService) SetRecentRequests(cache AccountRecentRequestsCache) {
	s.recentRequests = cache
}
func (s *AccountTestService) SetRecentRequests(cache AccountRecentRequestsCache) {
	s.recentRequests = cache
}

type recentRequestContextKey struct{}
type recentRequestModelContextKey struct{}
type recentRequestScopeContextKey struct{}
type recentRequestScope struct {
	accountID int64
	scope     string
}

func withRecentRequestScope(ctx context.Context, account, credentialAccount *Account) context.Context {
	if account == nil || !account.IsShadow() {
		return ctx
	}
	return context.WithValue(ctx, recentRequestScopeContextKey{}, recentRequestScope{account.ID, AccountActivityScopeWithCredentialAccount(account, credentialAccount)})
}

func withRecentRequestModel(ctx context.Context, body []byte) context.Context {
	return context.WithValue(ctx, recentRequestModelContextKey{}, recentRequestText(gjson.GetBytes(body, "model").String(), 256))
}

// An attempt has one owner; parser and cleanup paths can safely finish it together.
type accountRequestAttempt struct {
	mu       sync.Mutex
	once     sync.Once
	cache    AccountRecentRequestsCache
	scope    string
	started  time.Time
	record   AccountRecentRequest
	terminal string
	err      error
	finished bool
}

func beginAccountRequestAttempt(ctx context.Context, cache AccountRecentRequestsCache, account *Account, model string) *accountRequestAttempt {
	if cache == nil || account == nil || account.Platform != PlatformOpenAI {
		return nil
	}
	scope := AccountActivityScope(account)
	if account.IsShadow() {
		if binding, ok := ctx.Value(recentRequestScopeContextKey{}).(recentRequestScope); ok && binding.accountID == account.ID {
			scope = binding.scope
		}
	}
	if scope == "" {
		return nil
	}
	source := "business"
	if probe, _ := ctx.Value(qualityProbeContextKey{}).(bool); probe {
		source = "quality"
	}
	return &accountRequestAttempt{cache: cache, scope: scope, started: time.Now(), record: AccountRecentRequest{
		AttemptID: uuid.NewString(), AccountID: account.ID, Source: source, Model: recentRequestText(model, 256), Outcome: "unknown",
	}}
}

func recentRequestText(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) > limit {
		value = value[:limit]
	}
	return value
}

func recentAttemptFrom(ctx context.Context) *accountRequestAttempt {
	if ctx == nil {
		return nil
	}
	a, _ := ctx.Value(recentRequestContextKey{}).(*accountRequestAttempt)
	return a
}

func recentRequestResponseContext(ctx context.Context, resp *http.Response) context.Context {
	if resp == nil {
		return ctx
	}
	if resp.Request != nil {
		if a := recentAttemptFrom(resp.Request.Context()); a != nil {
			return context.WithValue(ctx, recentRequestContextKey{}, a)
		}
	}
	return recentRequestReaderContext(ctx, resp.Body)
}

func recentRequestReaderContext(ctx context.Context, reader io.Reader) context.Context {
	if body, ok := reader.(*recentRequestBody); ok {
		return context.WithValue(ctx, recentRequestContextKey{}, body.attempt)
	}
	return ctx
}

func observeRecentRequest(ctx context.Context, first bool, terminal string) {
	if a := recentAttemptFrom(ctx); a != nil {
		a.observe(first, terminal)
	}
}

func recentRequestFailed(ctx context.Context, err error) {
	if a := recentAttemptFrom(ctx); a != nil {
		a.fail(err)
	}
}

func observeRecentTerminal(ctx context.Context, successful bool) {
	terminal := "failed"
	if successful {
		terminal = "completed"
	}
	observeRecentRequest(ctx, false, terminal)
}

func observeRecentProtocolEvent(ctx context.Context, payload []byte, eventType, mode string) {
	if recentAttemptFrom(ctx) == nil {
		return
	}
	first := recentResponseStartsTTFT(string(payload), eventType, mode)
	observeRecentRequest(ctx, first, "")
	if isOpenAICompatResponsesTerminalEvent(eventType) {
		observeRecentTerminal(ctx, recentResponseTerminalSuccessful(payload, eventType))
	}
}

// Existing semantic mode treats lifecycle terminal events as client progress.
// For request latency, only a terminal carrying actual output can be first output.
func recentResponseStartsTTFT(data, eventType, mode string) bool {
	if eventType == "" {
		eventType = gjson.Get(data, "type").String()
	}
	if isOpenAICompatResponsesTerminalEvent(eventType) {
		return (eventType == "response.completed" || eventType == "response.done") && openAIStreamDataStartsVisibleOutput(data, eventType)
	}
	return openAIStreamDataStartsTTFT(data, eventType, false, mode)
}

// Completion events may omit status, but an explicit error/status always wins.
func recentResponseTerminalSuccessful(payload []byte, eventType string) bool {
	if eventType != "response.completed" && eventType != "response.done" {
		return false
	}
	for _, path := range []string{"error", "response.error"} {
		v := gjson.GetBytes(payload, path)
		if v.Exists() && v.Type != gjson.Null {
			return false
		}
	}
	status := gjson.GetBytes(payload, "response.status").String()
	if status == "" {
		status = gjson.GetBytes(payload, "status").String()
	}
	return status == "" || status == "completed"
}

// Basispoints already parses its wire events; this wrapper receives that parser's
// observation before it closes a completed response or begins an internal repair.
type recentOpenAIEventReader struct {
	io.ReadCloser
	ctx  context.Context
	mode string
}

func (r *recentOpenAIEventReader) ObserveOpenAIEvent(payload []byte, eventType string) {
	observeRecentProtocolEvent(r.ctx, payload, eventType, r.mode)
}
func (r *recentOpenAIEventReader) ObserveOpenAIError(err error) { recentRequestFailed(r.ctx, err) }

func (s *OpenAIGatewayService) doRecentHTTP(req *http.Request, proxyURL string, account *Account) (*http.Response, error) {
	req, attempt := beginRecentHTTPRequest(req, s.recentRequests, account)
	resp, err := s.httpUpstream.Do(req, proxyURL, account.ID, account.Concurrency)
	attempt.response(req, resp, err)
	return resp, err
}

// Use the existing parsed response's completion fields; never infer success from HTTP 200.
func observeRecentJSON(ctx context.Context, body []byte) {
	if gjson.GetBytes(body, "error").Exists() && gjson.GetBytes(body, "error").Type != gjson.Null {
		observeRecentTerminal(ctx, false)
		return
	}
	switch gjson.GetBytes(body, "status").String() {
	case "completed":
		observeRecentTerminal(ctx, true)
		return
	case "failed", "incomplete", "cancelled":
		observeRecentTerminal(ctx, false)
		return
	}
	choices := gjson.GetBytes(body, "choices").Array()
	for _, choice := range choices {
		switch choice.Get("finish_reason").String() {
		case "stop", "tool_calls", "function_call":
			observeRecentTerminal(ctx, true)
		case "length", "content_filter":
			observeRecentTerminal(ctx, false)
		}
	}
}

func recentChatChunkHasOutput(payload []byte) bool {
	for _, choice := range gjson.GetBytes(payload, "choices").Array() {
		d := choice.Get("delta")
		if d.Get("content").String() != "" || d.Get("reasoning_content").String() != "" || d.Get("reasoning").String() != "" || d.Get("refusal").String() != "" || len(d.Get("tool_calls").Array()) > 0 {
			return true
		}
	}
	return false
}

func (a *accountRequestAttempt) observe(first bool, terminal string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.finished {
		return
	}
	if first && a.record.FirstTokenMs == nil {
		ms := time.Since(a.started).Milliseconds()
		a.record.FirstTokenMs = &ms
	}
	// An observed failure is sticky even when a provider emits a trailing DONE.
	if terminal != "" && a.terminal != "failed" && a.terminal != "incomplete" {
		a.terminal = terminal
	}
}

func (a *accountRequestAttempt) fail(err error) {
	if a == nil || err == nil || errors.Is(err, io.EOF) {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.finished && !(a.terminal == "completed" && errors.Is(err, context.Canceled)) {
		a.err = err
	}
}

func (a *accountRequestAttempt) finish(err error) {
	if a == nil {
		return
	}
	a.fail(err)
	a.once.Do(func() {
		a.mu.Lock()
		a.finished = true
		r := a.record
		r.CompletedAt = time.Now().UTC()
		r.DurationMs = time.Since(a.started).Milliseconds()
		switch {
		case a.terminal == "failed" || a.terminal == "incomplete" || r.HTTPStatus >= 400:
			r.Outcome = "failure"
			r.ErrorCode = "upstream_error"
		case a.err != nil:
			r.Outcome, r.ErrorCode = "failure", "transport_error"
			if errors.Is(a.err, context.Canceled) {
				r.Outcome, r.ErrorCode = "cancelled", "client_cancelled"
			}
			if errors.Is(a.err, context.DeadlineExceeded) {
				r.ErrorCode = "timeout"
			}
		case a.terminal == "completed":
			r.Outcome = "success"
		}
		if r.ErrorCode != "" {
			r.ErrorMessage = map[string]string{"upstream_error": "Upstream request did not complete successfully", "transport_error": "Upstream connection or response failed", "client_cancelled": "Request cancelled", "timeout": "Upstream request timed out"}[r.ErrorCode]
		}
		a.mu.Unlock()
		// Cancellation of the business request must not erase its final observation.
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		_ = a.cache.Append(ctx, a.scope, r)
	})
}

func beginRecentHTTPRequest(req *http.Request, cache AccountRecentRequestsCache, account *Account) (*http.Request, *accountRequestAttempt) {
	if cache == nil || account == nil || account.Platform != PlatformOpenAI {
		return req, nil
	}
	model, _ := req.Context().Value(recentRequestModelContextKey{}).(string)
	// GetBody is the existing immutable request replay; only inspect the model field.
	if model == "" && req.GetBody != nil {
		if body, err := req.GetBody(); err == nil {
			prefix, _ := io.ReadAll(io.LimitReader(body, 8192))
			_ = body.Close()
			model = gjson.GetBytes(prefix, "model").String()
		}
	}
	a := beginAccountRequestAttempt(req.Context(), cache, account, model)
	if a == nil {
		return req, nil
	}
	return req.WithContext(context.WithValue(req.Context(), recentRequestContextKey{}, a)), a
}

func (a *accountRequestAttempt) response(req *http.Request, resp *http.Response, err error) {
	if a == nil {
		return
	}
	if resp == nil {
		a.finish(err)
		return
	}
	a.mu.Lock()
	a.record.HTTPStatus = resp.StatusCode
	a.record.RequestID = recentRequestText(resp.Header.Get("x-request-id"), 128)
	a.mu.Unlock()
	if resp.Request == nil {
		resp.Request = req
	} else {
		resp.Request = resp.Request.WithContext(context.WithValue(resp.Request.Context(), recentRequestContextKey{}, a))
	}
	if err != nil || resp.Body == nil {
		a.finish(err)
		return
	}
	resp.Body = &recentRequestBody{ReadCloser: resp.Body, attempt: a}
}

type recentRequestBody struct {
	io.ReadCloser
	attempt *accountRequestAttempt
}

func (b *recentRequestBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.attempt.fail(err)
	return n, err
}
func (b *recentRequestBody) Close() error {
	err := b.ReadCloser.Close()
	b.attempt.finish(nil)
	return err
}
