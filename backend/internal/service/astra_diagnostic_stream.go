//go:build astra_experiment

package service

import (
	"encoding/json"
	"strings"
	"time"
)

// Only fixed protocol enums are retained, never an arbitrary upstream message.
type AstraDiagnosticStream struct {
	Events            map[string]int `json:"events"`
	ErrorCodes        []string       `json:"error_codes,omitempty"`
	ErrorTypes        []string       `json:"error_types,omitempty"`
	WaitSeconds       int64          `json:"retry_after_seconds,omitempty"`
	IncompleteReasons []string       `json:"incomplete_reasons,omitempty"`
}

func astraDiagnosticStream(data []byte) AstraDiagnosticStream {
	out := AstraDiagnosticStream{Events: map[string]int{}}
	_, wait := codexProbeSignal(data, nil, time.Now())
	out.WaitSeconds = int64((wait + time.Second - 1) / time.Second)
	for _, p := range openAICodexStateStreamEvents(data) {
		var e struct {
			Type  string `json:"type"`
			Code  string `json:"code"`
			Error struct {
				Code string `json:"code"`
				Type string `json:"type"`
			} `json:"error"`
			Response struct {
				Error struct {
					Code string `json:"code"`
					Type string `json:"type"`
				} `json:"error"`
				Incomplete struct {
					Reason string `json:"reason"`
				} `json:"incomplete_details"`
			} `json:"response"`
		}
		if json.Unmarshal(p, &e) != nil {
			out.Events["invalid_json"]++
			continue
		}
		kind := e.Type
		switch kind {
		case "response.created", "response.in_progress", "response.completed", "response.done", "response.failed", "response.incomplete", "error", "response.output_text.delta":
		default:
			kind = "other"
		}
		out.Events[kind]++
		for _, code := range []string{e.Code, e.Error.Code, e.Response.Error.Code} {
			if code == "" {
				continue
			}
			switch code {
			case "server_error", "server_is_overloaded", "rate_limit_exceeded", "insufficient_quota", "usage_limit_reached", "token_expired", "invalid_api_key", "authentication_error", "access_denied", "model_not_found", "unsupported_model", "context_length_exceeded":
			default:
				code = "unrecognized"
			}
			out.ErrorCodes = append(out.ErrorCodes, code)
		}
		for _, kind := range []string{e.Error.Type, e.Response.Error.Type} {
			kind = strings.ToLower(strings.TrimSpace(kind))
			if kind == "" {
				continue
			}
			if failure, _ := codexProbeErrorClass(kind); failure == "" {
				kind = "unrecognized"
			}
			out.ErrorTypes = append(out.ErrorTypes, kind)
		}
		if reason := e.Response.Incomplete.Reason; reason != "" {
			switch reason {
			case "max_output_tokens", "content_filter":
			default:
				reason = "unrecognized"
			}
			out.IncompleteReasons = append(out.IncompleteReasons, reason)
		}
	}
	return out
}

// Auth/quota/limit codes in HTTP200 streams must also stop the diagnostic driver.
func (s AstraDiagnosticStream) HardWait() time.Duration {
	wait := time.Duration(s.WaitSeconds) * time.Second
	for _, values := range [][]string{s.ErrorCodes, s.ErrorTypes} {
		for _, value := range values {
			if _, delay := codexProbeErrorClass(value); delay > wait {
				wait = delay
			}
		}
	}
	return wait
}
