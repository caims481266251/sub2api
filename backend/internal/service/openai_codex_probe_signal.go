package service

import (
	"bytes"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Only structured protocol codes/types affect retry policy. Generated text and
// arbitrary error messages must never turn into an account cooldown signal.
type codexProbeErrorInfo struct {
	Code       string          `json:"code"`
	Type       string          `json:"type"`
	ResetsAt   json.RawMessage `json:"resets_at"`
	ResetsIn   json.RawMessage `json:"resets_in_seconds"`
	RetryAfter json.RawMessage `json:"retry_after_seconds"`
}

func codexProbeErrorClass(value string) (failure string, wait time.Duration) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "rate_limit_exceeded", "rate_limit_error", "rate_limited":
		return OpenAICodexStateFailureRateLimited, time.Minute
	case "insufficient_quota", "usage_limit_reached", "usage_limit_exceeded":
		return OpenAICodexStateFailureRateLimited, 5 * time.Minute
	case "token_expired", "invalid_token", "invalid_api_key", "authentication_error", "access_denied", "permission_denied", "permission_error", "account_deactivated":
		return OpenAICodexStateFailureAccountError, 5 * time.Minute
	default:
		return "", 0
	}
}

func codexProbeSignal(data []byte, headers http.Header, now time.Time) (failure string, wait time.Duration) {
	payloads := openAICodexStateStreamEvents(data)
	if trimmed := bytes.TrimSpace(data); len(trimmed) > 0 && trimmed[0] == '{' {
		payloads = [][]byte{trimmed}
	}
	for _, p := range payloads {
		var e struct {
			codexProbeErrorInfo
			Error    *codexProbeErrorInfo `json:"error"`
			Response struct {
				Error *codexProbeErrorInfo `json:"error"`
			} `json:"response"`
		}
		if json.Unmarshal(p, &e) != nil {
			continue
		}
		infos := []*codexProbeErrorInfo{e.Error, e.Response.Error}
		// Top-level code belongs to an error event, never an output delta.
		if e.Type == "error" || e.Type == "" {
			infos = append(infos, &e.codexProbeErrorInfo)
		}
		for _, info := range infos {
			if info == nil {
				continue
			}
			known := false
			for _, value := range []string{info.Code, info.Type} {
				kind, delay := codexProbeErrorClass(value)
				if kind == "" {
					continue
				}
				known = true
				if failure == "" || kind == OpenAICodexStateFailureAccountError {
					failure = kind
				}
				wait = codexProbeMaxWait(wait, delay)
			}
			if known {
				wait = codexProbeMaxWait(wait, codexProbeSignalSeconds(info.ResetsIn, 0), codexProbeSignalSeconds(info.RetryAfter, 0), codexProbeSignalSeconds(info.ResetsAt, float64(now.UnixNano())/1e9))
			}
		}
	}
	if failure != "" {
		wait = codexProbeMaxWait(wait, borrowRetryAfter(headers.Get("Retry-After"), now))
	}
	return failure, wait
}

// Apply the same 24-hour defensive ceiling as HTTP Retry-After, before duration
// conversion. Strings and numeric JSON are both used by upstream reset fields.
func codexProbeSignalSeconds(raw json.RawMessage, subtract float64) time.Duration {
	value := strings.TrimSpace(string(raw))
	if len(value) > 0 && value[0] == '"' {
		if json.Unmarshal(raw, &value) != nil {
			return 0
		}
	}
	n, err := strconv.ParseFloat(value, 64)
	n -= subtract
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n <= 0 {
		return 0
	}
	return time.Duration(math.Min(n, 24*60*60) * float64(time.Second))
}

func codexProbeMaxWait(values ...time.Duration) time.Duration {
	var longest time.Duration
	for _, value := range values {
		if value > longest {
			longest = value
		}
	}
	return longest
}
