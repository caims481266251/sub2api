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

type Result struct {
	DispatchedCredentialsHash string `json:"-"`
	// Settled means the attempt ended at the guest, including a provider rejection.
	// It is deliberately independent of successful completion and usage presence.
	// Cancellation only records the control response; HTTP 2xx alone never proves settlement.
	CancelStatus                                                string
	CancelHTTPStatus                                            int
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

type Binding struct {
	Socket, Token, InstanceID string
	Generation                uint64
}

type Request struct {
	Protocol string
	Body     json.RawMessage
	Headers  map[string]string
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
)

func WithSessionID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, sessionKey, id)
}
func WithOnAccepted(ctx context.Context, callback func()) context.Context {
	return context.WithValue(ctx, acceptedKey, callback)
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
