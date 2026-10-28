// Package vmkernel implements a single attempt against a private guest kernel.
// It has no knowledge of account selection, credential refresh, or billing.
package vmkernel

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

type Event struct {
	Type string
	Data json.RawMessage
}

// CodexMetadataEvent carries allowlisted transport context, never model output.
const CodexMetadataEvent = "codex.response.metadata"

type Result struct {
	// ProviderHTTPFailure distinguishes a verified HTTP rejection from an in-stream error.
	ProviderHTTPFailure       bool
	DispatchedCredentialsHash string `json:"-"`
	// Settled means the attempt ended at the guest, including a provider rejection.
	// It is deliberately independent of successful completion and usage presence.
	// Cancellation records local transport cleanup or the control response;
	// neither execution-stream closure nor HTTP 2xx proves settlement.
	CancelStatus     string
	CancelHTTPStatus int
	// ExecutionTransportCanceled means this attempt's local HTTP context was
	// canceled and any response body closed. It does not prove upstream stopped.
	ExecutionTransportCanceled                                  bool
	Settled                                                     bool
	RequestID                                                   string
	ErrorEvent                                                  json.RawMessage
	AttemptID, ResponseID, Model, Terminal                      string
	Usage                                                       json.RawMessage
	HasUsage, Completed, Dispatched, Accepted, ClientDisconnect bool
	Headers                                                     http.Header
	FirstTokenMs                                                *int
	Duration                                                    time.Duration
	Response                                                    json.RawMessage
}

// CancellationMode is selected by trusted runtime configuration, never request
// protocol, caller headers, or a previous 404 response.
type CancellationMode string

const (
	// The zero value retains control-route compatibility for existing callers.
	CancellationModeDefault     CancellationMode = ""
	CancellationModeControl     CancellationMode = "control"
	CancellationModeStreamClose CancellationMode = "stream_close"
)

type Binding struct {
	// Only trusted runtime state can supply this; never caller headers.
	InstallationID            string
	CancellationMode          CancellationMode
	Socket, Token, InstanceID string
	Generation                uint64
	AccountID                 int64
	GuestBootID               string
	CredentialRevision        string
	ProxyRevision             string
}

type Request struct {
	// DisablePromptCache suppresses cache routing hints without changing conversation input.
	DisablePromptCache bool
	Protocol           string
	Body               json.RawMessage
	Headers            map[string]string
}

// AttemptError's SafeToRetry is true only when the request was not submitted.
// A socket failure after submission is an unknown execution outcome.
type AttemptError struct {
	Code        string
	SafeToRetry bool
	Cause       error
}

func (e *AttemptError) Error() string { return e.Code }
func (e *AttemptError) Unwrap() error { return e.Cause }

type contextKey uint8

const (
	sessionKey contextKey = iota
	acceptedKey
	bindingKey
	headersKey
	responseHeadersKey
	firstTokenPredicateKey
)

func WithSessionID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, sessionKey, id)
}
func WithOnAccepted(ctx context.Context, callback func()) context.Context {
	return context.WithValue(ctx, acceptedKey, callback)
}

// WithFirstTokenPredicate lets the business adapter apply its existing TTFT
// policy without coupling kernel transport to gateway settings.
func WithFirstTokenPredicate(ctx context.Context, predicate func(Event) bool) context.Context {
	return context.WithValue(ctx, firstTokenPredicateKey, predicate)
}

// SessionBinding belongs to one public WS connection. It is checked on each
// turn and never authorizes a session to move to a replacement guest.
type SessionBinding struct {
	AccountID               int64
	InstanceID, GuestBootID string
	Generation              uint64
}

func WithSessionBinding(ctx context.Context, binding *SessionBinding) context.Context {
	return context.WithValue(ctx, bindingKey, binding)
}
func BindingFromContext(ctx context.Context) *SessionBinding {
	binding, _ := ctx.Value(bindingKey).(*SessionBinding)
	return binding
}
func WithHeaders(ctx context.Context, headers http.Header) context.Context {
	return context.WithValue(ctx, headersKey, headers.Clone())
}

// CodexRequestHeaderNames is the kernel adapter's protocol allowlist.
// Authorization, cookies and arbitrary client headers never enter the envelope.
var CodexRequestHeaderNames = []string{
	"originator", "openai-beta", "session-id", "thread-id", "x-client-request-id",
	"x-codex-beta-features", "x-codex-installation-id", "x-codex-parent-thread-id",
	"x-codex-turn-metadata", "x-codex-turn-state", "x-codex-window-id",
	"x-openai-internal-codex-responses-lite", "x-openai-memgen-request", "x-openai-subagent",
}

// WithOnResponseHeaders observes allowlisted headers before the next public event.
// The caller decides when to commit them. The callback runs synchronously.
func WithOnResponseHeaders(ctx context.Context, callback func(http.Header)) context.Context {
	return context.WithValue(ctx, responseHeadersKey, callback)
}
func notifyResponseHeaders(ctx context.Context, headers http.Header) {
	if callback, ok := ctx.Value(responseHeadersKey).(func(http.Header)); ok && callback != nil {
		callback(headers.Clone())
	}
}
