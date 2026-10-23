package vmkernel

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

const MaxEventBytes = 16 << 20

type Client struct {
	ResponseHeaderTimeout time.Duration
	CancelTimeout         time.Duration
}

func NewClient() *Client {
	return &Client{ResponseHeaderTimeout: 30 * time.Second, CancelTimeout: 3 * time.Second}
}

// Execute sends exactly one kernel request. There is no automatic retry here.
func (c *Client) Execute(ctx context.Context, binding Binding, request Request, emit func(Event) error) (*Result, error) {
	started := time.Now()
	if !filepath.IsAbs(binding.Socket) || binding.Token == "" || binding.Generation == 0 {
		return nil, &AttemptError{Code: "kernel_binding_invalid", SafeToRetry: true}
	}
	endpoint := ""
	switch request.Protocol {
	case "messages":
		endpoint = "/internal/v1/messages"
	case "responses":
		endpoint = "/internal/v1/codex/responses"
	default:
		return nil, &AttemptError{Code: "kernel_protocol_unsupported", SafeToRetry: true}
	}
	var body map[string]json.RawMessage
	if json.Unmarshal(request.Body, &body) != nil || body == nil {
		return nil, &AttemptError{Code: "kernel_body_invalid", SafeToRetry: true}
	}
	body["stream"] = json.RawMessage("true")
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, err
	}
	attemptID := hex.EncodeToString(random[:])
	headers := map[string]string{}
	if supplied, ok := ctx.Value(headersKey).(http.Header); ok {
		for _, name := range []string{"anthropic-beta", "anthropic-version", "openai-beta"} {
			if value := supplied.Get(name); value != "" {
				headers[name] = value
			}
		}
	}
	envelope := map[string]any{"request_id": attemptID, "body": body, "stream": true, "headers": headers}
	if request.Protocol == "responses" {
		sessionID, _ := ctx.Value(sessionKey).(string)
		envelope["session"] = map[string]string{"session_id": sessionID}
		envelope["options"] = map[string]string{"upstream_transport": "http"}
	} else {
		envelope["delivery_mode"] = "realtime"
		envelope["preserve_cache_breakpoints"] = true
		envelope["cache_ttl"] = nil
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", binding.Socket)
		},
		ResponseHeaderTimeout: c.ResponseHeaderTimeout, MaxResponseHeaderBytes: 64 << 10,
		DisableKeepAlives: true,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	var dispatched atomic.Bool
	// Once a connection is obtained a write may partially reach the guest even
	// without a WroteHeaders callback. Conservatively forbid replay from here.
	trace := &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) { dispatched.Store(true) }}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodPost, "http://guest"+endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("TE", "trailers")
	req.Header.Set("x-kin-internal-token", binding.Token)
	req.Header.Set("x-internal-token", binding.Token)
	result := &Result{AttemptID: attemptID, Headers: make(http.Header)}
	defer func() { result.Duration = time.Since(started); result.Dispatched = dispatched.Load() }()
	resp, err := client.Do(req)
	if err != nil {
		if dispatched.Load() {
			result.CancelStatus, result.CancelHTTPStatus = c.cancel(binding, attemptID)
		}
		return result, &AttemptError{Code: "kernel_transport_failed", SafeToRetry: !dispatched.Load(), Cause: err}
	}
	defer resp.Body.Close()
	copyPublicHeaders(result.Headers, resp.Header)
	result.RequestID = result.Headers.Get("x-request-id")
	if resp.StatusCode != http.StatusOK {
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, (64<<10)+1))
		if readErr == nil && len(raw) <= 64<<10 {
			// This private socket reaches only the fixed synchronous kernel;
			// the relay does not generate HTTP responses. A fully read failure
			// settles the local handler even when its diagnostic is unrecognized.
			// It says nothing about upstream execution or safe replay.
			result.Settled = true
			event, localCode := settledHTTPFailure(raw, resp.Header.Get("X-Terminal-State"), resp.StatusCode)
			if localCode != "" {
				return result, &AttemptError{Code: localCode}
			}
			if event != nil {
				result.ErrorEvent = event
				result.Settled = true
				// Settlement means the kernel handler returned a complete failure,
				// never that the provider performed no work or retry is safe.
				return result, &AttemptError{Code: "kernel_response_failed"}
			}
		}
		// Truncated/oversized errors remain unknown. Local auth/protocol
		// responses settle the handler without becoming provider auth errors.
		return result, &AttemptError{Code: fmt.Sprintf("kernel_status_%d", resp.StatusCode)}
	}
	usage := map[string]json.RawMessage{}
	observeHeaders(result, usage, resp.Header)
	var terminals []Event
	terminalBytes := 0
	failed := false
	sawJobDone := false
	stopReason := ""
	accept := func() {
		if result.Accepted {
			return
		}
		result.Accepted = true
		if callback, ok := ctx.Value(acceptedKey).(func()); ok && callback != nil {
			callback()
		}
	}
	consume := func(event Event) error {
		var fields map[string]json.RawMessage
		if json.Unmarshal(event.Data, &fields) != nil {
			return errors.New("kernel_event_invalid")
		}
		if event.Type == "" {
			_ = json.Unmarshal(fields["type"], &event.Type)
		}
		for _, nested := range []string{"message", "response"} {
			if raw := fields[nested]; len(raw) > 0 {
				var object map[string]json.RawMessage
				if json.Unmarshal(raw, &object) == nil {
					observeObject(result, usage, object)
					if raw := object["stop_reason"]; len(raw) > 0 {
						var reason string
						_ = json.Unmarshal(raw, &reason)
						if reason != "" {
							stopReason = reason
						}
					}
					var status string
					_ = json.Unmarshal(object["status"], &status)
					if status == "failed" || status == "incomplete" || status == "cancelled" {
						failed = true
					}
					if nested == "response" {
						result.Response = append(json.RawMessage(nil), raw...)
					}
				}
			}
		}
		var delta map[string]json.RawMessage
		if json.Unmarshal(fields["delta"], &delta) == nil {
			var reason string
			_ = json.Unmarshal(delta["stop_reason"], &reason)
			if reason != "" {
				stopReason = reason
			}
		}
		observeObject(result, usage, fields)
		if event.Type == "response_headers" {
			var rawHeaders map[string]json.RawMessage
			if json.Unmarshal(fields["headers"], &rawHeaders) == nil {
				h := make(http.Header)
				for key, raw := range rawHeaders {
					var v string
					if json.Unmarshal(raw, &v) == nil {
						h.Set(key, v)
					}
				}
				copyPublicHeaders(result.Headers, h)
				observeHeaders(result, usage, h)
			}
			return nil
		}
		if event.Type == "job_done" || event.Type == "job_started" || event.Type == "job_accepted" {
			if event.Type == "job_done" {
				sawJobDone = true
			}
			if event.Type == "job_accepted" || event.Type == "job_started" {
				accept()
			}
			if raw := fields["error"]; len(raw) > 0 && string(raw) != "null" {
				failed = true
			}
			return nil
		}
		if !publicEvent(request.Protocol, event.Type) {
			return nil
		}
		accept()
		if result.FirstTokenMs == nil && (strings.Contains(event.Type, "delta") || event.Type == "content_block_start") {
			ms := int(time.Since(started).Milliseconds())
			result.FirstTokenMs = &ms
		}
		if event.Type == "error" || event.Type == "response.failed" || event.Type == "response.incomplete" {
			failed = true
			if len(event.Data) <= 64<<10 {
				result.ErrorEvent = append(json.RawMessage(nil), event.Data...)
			}
			// Failures are presented once by the HTTP/WS business boundary after
			// settlement. A bare error must not commit HTTP 200 or escape raw.
			if event.Type == "error" {
				return nil
			}
		}
		if terminalEvent(event.Type) || (event.Type == "message_delta" && stopReason != "") {
			if terminalEvent(event.Type) {
				result.Terminal = event.Type
			}
			terminalBytes += len(event.Data)
			if terminalBytes > MaxEventBytes || len(terminals) >= 4 {
				return errors.New("kernel_terminal_too_large")
			}
			terminals = append(terminals, event)
			return nil
		}
		if emit != nil {
			return emit(event)
		}
		return nil
	}
	if strings.Contains(resp.Header.Get("Content-Type"), "application/json") {
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, MaxEventBytes+1))
		if readErr != nil {
			err = readErr
		} else if len(raw) > MaxEventBytes {
			err = errors.New("kernel_response_too_large")
		} else {
			result.Response = append(json.RawMessage(nil), raw...)
			var object map[string]json.RawMessage
			if json.Unmarshal(raw, &object) != nil {
				err = errors.New("kernel_json_invalid")
			} else {
				observeObject(result, usage, object)
				var status string
				_ = json.Unmarshal(object["status"], &status)
				if request.Protocol == "responses" && status == "completed" && (len(object["error"]) == 0 || string(object["error"]) == "null") {
					eventRaw, _ := json.Marshal(map[string]any{"type": "response.completed", "response": json.RawMessage(raw)})
					err = consume(Event{Type: "response.completed", Data: eventRaw})
				} else {
					err = errors.New("kernel_json_not_completed")
				}
			}
		}
	} else {
		err = readEvents(resp.Body, consume)
	}
	copyPublicHeaders(result.Headers, resp.Trailer)
	for _, name := range []string{"X-Usage", "X-Kin-Usage"} {
		if value := resp.Trailer.Get(name); value != "" {
			mergeUsage(usage, json.RawMessage(value))
		}
	}
	if value := resp.Trailer.Get("X-Model"); value != "" {
		result.Model = value
	}
	if len(usage) > 0 {
		result.Usage, _ = json.Marshal(usage)
		result.HasUsage = validUsage(usage)
	}
	if result.ResponseID == "" {
		result.ResponseID = result.Headers.Get("x-request-id")
	}
	result.RequestID = result.Headers.Get("x-request-id")
	if err != nil {
		if ctx.Err() != nil {
			result.ClientDisconnect = true
		}
		result.CancelStatus, result.CancelHTTPStatus = c.cancel(binding, attemptID)
		return result, &AttemptError{Code: "kernel_stream_incomplete", Cause: err}
	}
	// A verified terminal plus normal EOF settles this HTTP attempt. A socket
	// failure or best-effort cancellation above does not provide that evidence.
	result.Settled = result.Terminal != "" || sawJobDone || len(result.ErrorEvent) > 0
	if failed || result.Terminal == "response.failed" || result.Terminal == "response.incomplete" {
		return result, &AttemptError{Code: "kernel_response_failed"}
	}
	if result.Terminal == "" || !result.HasUsage || result.ResponseID == "" || (request.Protocol == "messages" && stopReason == "") {
		result.CancelStatus, result.CancelHTTPStatus = c.cancel(binding, attemptID)
		return result, &AttemptError{Code: "kernel_completion_metadata_missing"}
	}
	for _, event := range terminals {
		if emit != nil {
			if err := emit(event); err != nil {
				result.ClientDisconnect = true
				return result, &AttemptError{Code: "kernel_downstream_write_failed", Cause: err}
			}
		}
	}
	result.Completed = true
	return result, nil
}

// The Claude kernel wraps provider failures with provider_error. The Codex
// kernel also returns usage_limit_reached as HTTP 429 without a terminal header.
// Local scheduler errors settle the attempt but must not become provider quota
// or authentication feedback. Settlement alone never authorizes replay.
func settledHTTPFailure(raw []byte, terminal string, httpStatus int) (json.RawMessage, string) {
	var envelope struct {
		Status int `json:"status"`
		Error  struct {
			Status     int    `json:"status"`
			StatusCode int    `json:"status_code"`
			Code       string `json:"code"`
			Type       string `json:"type"`
			Message    string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return nil, ""
	}
	e := envelope.Error
	// The fixed Codex kernel returns upstream_auth with HTTP 401 and no
	// terminal header. Keep this exact provider contract distinct from local
	// socket authentication, and never retain the embedded upstream body.
	if httpStatus == http.StatusUnauthorized && e.Code == "upstream_auth" {
		out, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]any{
			"code": "upstream_auth", "type": "authentication_error", "status": http.StatusUnauthorized,
			"message": "Upstream authentication failed",
		}})
		return out, ""
	}
	switch e.Code {
	case "no_capacity", "no_credential", "sticky_unavailable":
		return nil, "kernel_" + e.Code
	}
	known := e.Code == "provider_error" || e.Type == "provider_error" || e.Code == "upstream_transport" || e.Code == "upstream_transport_error"
	if httpStatus == http.StatusTooManyRequests && e.Code == "usage_limit_reached" {
		known = true
	}
	if terminal == "rejected" || terminal == "error" {
		switch e.Code {
		case "rate_limit_exceeded", "usage_limit_reached", "model_not_found", "invalid_api_key", "insufficient_quota", "permission_denied", "upstream_transport", "upstream_transport_error":
			known = true
		}
	}
	if !known {
		return nil, ""
	}
	// Do not retain diagnostic extras or proxy URLs from transport failures.
	message := e.Message
	if e.Code == "upstream_transport" || e.Code == "upstream_transport_error" || strings.Contains(message, "://") || len(message) > 4096 {
		message = "Upstream request failed"
	}
	fields := map[string]any{"code": e.Code, "type": e.Type, "message": message}
	for _, status := range []int{e.Status, e.StatusCode, envelope.Status, httpStatus} {
		if status >= 400 && status <= 599 {
			fields["status"] = status
			break
		}
	}
	out, _ := json.Marshal(map[string]any{"type": "error", "error": fields})
	return out, ""
}

func (c *Client) cancel(binding Binding, attempt string) (string, int) {
	timeout := c.CancelTimeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", binding.Socket)
	}, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	body, _ := json.Marshal(map[string]string{"request_id": attempt})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://guest/internal/v1/cancel", bytes.NewReader(body))
	req.Header.Set("x-kin-internal-token", binding.Token)
	req.Header.Set("x-internal-token", binding.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		return "transport_failed", 0
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, io.LimitReader(resp.Body, 4096)); err != nil {
		return "response_incomplete", resp.StatusCode
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "rejected", resp.StatusCode
	}
	// Acknowledgment is not proof that the original execution has finished.
	return "acknowledged", resp.StatusCode
}

func readEvents(reader io.Reader, consume func(Event) error) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), MaxEventBytes)
	var event Event
	flush := func() error {
		if len(event.Data) == 0 {
			event = Event{}
			return nil
		}
		if bytes.Equal(event.Data, []byte("[DONE]")) {
			event = Event{}
			return nil
		}
		err := consume(event)
		event = Event{}
		return err
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, "event:") {
			event.Type = strings.TrimSpace(line[6:])
		}
		if strings.HasPrefix(line, "data:") {
			data := strings.TrimPrefix(line[5:], " ")
			if len(event.Data) > 0 {
				event.Data = append(event.Data, '\n')
			}
			if len(event.Data)+len(data) > MaxEventBytes {
				return errors.New("kernel_event_too_large")
			}
			event.Data = append(event.Data, data...)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return flush()
}

func observeObject(result *Result, usage map[string]json.RawMessage, object map[string]json.RawMessage) {
	mergeUsage(usage, object["usage"])
	var id string
	_ = json.Unmarshal(object["id"], &id)
	if strings.HasPrefix(id, "resp_") || strings.HasPrefix(id, "msg_") {
		result.ResponseID = id
	}
	var model string
	_ = json.Unmarshal(object["model"], &model)
	if model != "" {
		result.Model = model
	}
}
func mergeUsage(dst map[string]json.RawMessage, raw json.RawMessage) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return
	}
	for key, value := range fields {
		if string(value) == "null" {
			continue
		}
		if len(value) > 0 && value[0] == '{' {
			nested := map[string]json.RawMessage{}
			_ = json.Unmarshal(dst[key], &nested)
			if nested == nil {
				nested = map[string]json.RawMessage{}
			}
			mergeUsage(nested, value)
			dst[key], _ = json.Marshal(nested)
		} else {
			dst[key] = append(json.RawMessage(nil), value...)
		}
	}
}
func terminalEvent(kind string) bool {
	return kind == "message_stop" || kind == "response.completed" || kind == "response.done" || kind == "response.failed" || kind == "response.incomplete"
}
func publicEvent(protocol, kind string) bool {
	if kind == "error" {
		return true
	}
	if protocol == "responses" {
		return strings.HasPrefix(kind, "response.")
	}
	switch kind {
	case "message_start", "message_delta", "message_stop", "content_block_start", "content_block_delta", "content_block_stop", "ping":
		return true
	}
	return false
}
func copyPublicHeaders(dst, src http.Header) {
	for key, values := range src {
		lower := strings.ToLower(key)
		if lower == "x-request-id" || lower == "retry-after" || (strings.HasPrefix(lower, "x-codex-primary-") || strings.HasPrefix(lower, "x-codex-secondary-")) || strings.HasPrefix(lower, "anthropic-ratelimit-") || strings.HasPrefix(lower, "x-ratelimit-") {
			dst[key] = append([]string(nil), values...)
		}
	}
}

func observeHeaders(result *Result, usage map[string]json.RawMessage, headers http.Header) {
	for _, name := range []string{"X-Usage", "X-Kin-Usage"} {
		if raw := headers.Get(name); raw != "" {
			mergeUsage(usage, json.RawMessage(raw))
		}
	}
	if model := headers.Get("X-Model"); model != "" {
		result.Model = model
	}
}
func validUsage(usage map[string]json.RawMessage) bool {
	for _, key := range []string{"input_tokens", "output_tokens"} {
		var n int64
		raw, ok := usage[key]
		if !ok || json.Unmarshal(raw, &n) != nil || n < 0 {
			return false
		}
	}
	for _, key := range []string{"cache_read_input_tokens", "cache_creation_input_tokens", "total_tokens"} {
		if raw, ok := usage[key]; ok {
			var n int64
			if json.Unmarshal(raw, &n) != nil || n < 0 {
				return false
			}
		}
	}
	for parent, keys := range map[string][]string{"input_tokens_details": {"cached_tokens"}, "cache_creation": {"ephemeral_5m_input_tokens", "ephemeral_1h_input_tokens"}, "output_tokens_details": {"reasoning_tokens"}} {
		if raw, ok := usage[parent]; ok {
			var fields map[string]json.RawMessage
			if json.Unmarshal(raw, &fields) != nil {
				return false
			}
			for _, key := range keys {
				if value, ok := fields[key]; ok {
					var n int64
					if json.Unmarshal(value, &n) != nil || n < 0 {
						return false
					}
				}
			}
		}
	}

	return true
}
