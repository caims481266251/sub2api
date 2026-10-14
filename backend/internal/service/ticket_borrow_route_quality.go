package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/tidwall/gjson"
)

// A bounded background check; business streams retain their own lifecycle.
const borrowRouteQualityTimeout = 180 * time.Second

type BorrowRouteQualityConfig struct {
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoning_effort"`
	IntervalSeconds int    `json:"interval_seconds"`
}

func DefaultBorrowRouteQualityConfig() *BorrowRouteQualityConfig {
	return &BorrowRouteQualityConfig{Model: "gpt-6-astra", ReasoningEffort: "medium", IntervalSeconds: 30}
}

func (c BorrowRouteQualityConfig) Validate() error {
	if c.Model != "gpt-6-astra" && c.Model != "gpt-6.1-sol" && c.Model != "gpt-6-sol" && c.Model != "gpt-5.6-sol" {
		return errors.New("borrow_route_quality_settings_invalid")
	}
	switch c.ReasoningEffort {
	case "low", "medium", "high", "xhigh", "max":
	default:
		return errors.New("borrow_route_quality_settings_invalid")
	}
	if c.IntervalSeconds < 5 || c.IntervalSeconds > 3600 {
		return errors.New("borrow_route_quality_settings_invalid")
	}
	return nil
}

type BorrowRouteQualityResult struct {
	CookieDeadline  time.Time `json:"-"`
	Status          string    `json:"status"`
	Model           string    `json:"model"`
	ReasoningEffort string    `json:"reasoning_effort"`
	Answer          string    `json:"answer,omitempty"`
	Reason          string    `json:"reason,omitempty"`
	HTTPStatus      int       `json:"http_status"`
	CheckedAt       time.Time `json:"checked_at"`
	DurationMS      int64     `json:"duration_ms"`
}

func (r *BorrowRouteQualityResult) Passed(c BorrowRouteQualityConfig) bool {
	return r != nil && r.Status == "passed" && r.Reason == "" && r.HTTPStatus == http.StatusOK &&
		r.Model == c.Model && r.ReasoningEffort == c.ReasoningEffort && CandyAnswerCorrect(r.Answer)
}

func borrowRouteQualityBody(c BorrowRouteQualityConfig) []byte {
	payload := createOpenAITestPayload(c.Model, true)
	payload["instructions"] = openai.DefaultInstructions
	payload["input"] = []map[string]any{{"role": "user", "content": []map[string]any{{"type": "input_text", "text": intelligenceTestPrompt(&PelicanTestConfig{Prompt: CandyPrompt, QuestionKind: "candy"})}}}}
	payload["reasoning"] = map[string]string{"effort": c.ReasoningEffort}
	data, _ := json.Marshal(payload)
	return data
}

// The account page's exact candy question and answer matcher define this gate.
// A delta, HTTP 200 or [DONE] alone is not a completed quality result.
func borrowQualityAnswer(data []byte) (string, string) {
	var deltas strings.Builder
	var answer string
	completed := false
	for _, raw := range openAICodexStateStreamEvents(data) {
		if !gjson.ValidBytes(raw) {
			continue
		}
		v := gjson.ParseBytes(raw)
		switch v.Get("type").String() {
		case "response.output_text.delta":
			deltas.WriteString(v.Get("delta").String())
		case "error", "response.failed", "response.incomplete", "response.cancelled", "response.canceled":
			_, _, reason := borrowRouteSSEOutcome(raw)
			if reason == "" {
				reason = "route_quality_incomplete"
			}
			return "", reason
		case "response.completed", "response.done":
			terminal, success, _ := borrowRouteSSEOutcome(raw)
			if !terminal || !success {
				return "", "route_quality_incomplete"
			}
			completed = true
			for _, item := range v.Get("response.output").Array() {
				if item.Get("type").String() != "message" {
					continue
				}
				for _, part := range item.Get("content").Array() {
					if part.Get("type").String() == "refusal" {
						return "", "route_quality_incomplete"
					}
					if part.Get("type").String() == "output_text" {
						answer += part.Get("text").String()
					}
				}
			}
		}
	}
	if !completed {
		return "", "route_quality_incomplete"
	}
	if answer == "" {
		answer = deltas.String()
	} else if deltas.Len() > 0 && strings.TrimSpace(answer) != strings.TrimSpace(deltas.String()) {
		return "", "route_quality_incomplete"
	}
	answer = strings.TrimSpace(answer)
	if !CandyAnswerCorrect(answer) {
		return answer, "route_quality_failed"
	}
	return answer, ""
}

func probeBorrowRouteQuality(req *http.Request, cfg BorrowRouteQualityConfig, send func(*http.Request) (*http.Response, error)) (result *BorrowRouteQualityResult, retry *time.Time, err error) {
	started := time.Now()
	result = &BorrowRouteQualityResult{Status: "failed", Model: cfg.Model, ReasoningEffort: cfg.ReasoningEffort}
	defer func() {
		result.CheckedAt = time.Now()
		result.DurationMS = time.Since(started).Milliseconds()
		// Store bounded answer metadata, never arbitrary upstream error bodies.
		if len(result.Answer) > 512 {
			result.Answer = string([]rune(result.Answer)[:min(128, len([]rune(result.Answer)))])
		}
	}()
	resp, err := send(req)
	if err != nil {
		result.Reason = "route_quality_transport_error"
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return result, nil, err
	}
	if resp == nil {
		result.Reason = "route_quality_incomplete"
		return result, nil, nil
	}
	result.HTTPStatus = resp.StatusCode
	if resp.Body != nil {
		defer resp.Body.Close()
	}
	if resp.StatusCode != http.StatusOK {
		result.Reason = borrowRouteHTTPFailure(resp.StatusCode)
		if result.Reason == "" {
			result.Reason = "route_quality_http_error"
		}
		return result, explicitBorrowRetryAt(resp.Header), nil
	}
	if resp.Body == nil {
		result.Reason = "route_quality_incomplete"
		return result, nil, nil
	}
	if pinned, e := req.Cookie("__oailb"); e == nil {
		for _, cookie := range resp.Cookies() {
			if cookie.Name == pinned.Name && cookie.Value == pinned.Value {
				deadline := cookie.Expires
				if cookie.MaxAge > 0 {
					at := time.Now().Add(time.Duration(cookie.MaxAge) * time.Second)
					if deadline.IsZero() || at.Before(deadline) {
						deadline = at
					}
				}
				if !deadline.IsZero() && (result.CookieDeadline.IsZero() || deadline.Before(result.CookieDeadline)) {
					result.CookieDeadline = deadline
				}
			}
			if cookie.Name == pinned.Name && (cookie.Value != pinned.Value || cookie.MaxAge < 0 || (!cookie.Expires.IsZero() && !cookie.Expires.After(time.Now()))) {
				result.Reason = "target_route_changed"
				return result, nil, nil
			}
		}
	}
	data, readErr := readBorrowProbeResponse(resp.Body, false, func() {})
	if readErr != nil || len(data) > openAICodexStateProbeMaxBody || req.Context().Err() != nil {
		result.Reason = "route_quality_incomplete"
		return result, nil, readErr
	}
	result.Answer, result.Reason = borrowQualityAnswer(data)
	if model := openAICodexStateStreamModel(data); model != "" && model != cfg.Model {
		result.Reason = "route_quality_model_mismatch"
	}
	if result.Reason == "" {
		result.Status = "passed"
	}
	return result, nil, nil
}

// A quality check is always a fresh request, without a prior ticket or session.
func borrowQualityRequest(template *http.Request, cfg BorrowRouteQualityConfig, session string) *http.Request {
	body := borrowRouteQualityBody(cfg)
	req := template.Clone(template.Context())
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	req.ContentLength = int64(len(body))
	for _, name := range []string{openAICodexTurnStateHeader, "conversation_id", "x-codex-turn-metadata", "Content-Length", "Content-Encoding", responsesLiteHeaderKey} {
		req.Header.Del(name)
	}
	req.Header.Set("session_id", session)
	return req
}

// A separate key prevents the generic tracker from mistaking a fresh candy
// request (without a turn ticket) for the mint phase.
type borrowQualityRequestKey struct{}

func withBorrowQualityRequest(ctx context.Context) context.Context {
	return context.WithValue(ctx, borrowQualityRequestKey{}, true)
}
