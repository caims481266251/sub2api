//go:build unit

package handler

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type borrowUnavailableHTTPUpstream struct {
	service.HTTPUpstream
	calls int
}

func (u *borrowUnavailableHTTPUpstream) Do(*http.Request, string, int64, int) (*http.Response, error) {
	u.calls++
	return nil, fmt.Errorf("transport admission: %w", service.ErrBorrowUnavailable)
}

func TestBorrowUnprovenTransportRejectionDoesNotReplay(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, endpoint := range []string{"responses", "chat/completions", "messages"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", endpoint, stream), func(t *testing.T) {
				repo := &initialAdmissionRepo{}
				upstream := &borrowUnavailableHTTPUpstream{}
				h := newInitialAdmissionHandler(t, repo, upstream)
				for i := range repo.accounts {
					repo.accounts[i].Extra = nil
				}
				c, rec := newExcelBPSFailoverTestContext(context.Background(), stream)
				apiKey, _ := middleware2.GetAPIKeyFromContext(c)
				apiKey.Group.AllowMessagesDispatch = true
				c.Request.URL.Path = "/v1/" + endpoint
				if endpoint != "responses" {
					body := fmt.Sprintf(`{"model":"gpt-6-astra","stream":%t,"max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`, stream)
					c.Request.Body = io.NopCloser(strings.NewReader(body))
					c.Request.ContentLength = int64(len(body))
				}
				switch endpoint {
				case "responses":
					h.Responses(c)
				case "chat/completions":
					h.ChatCompletions(c)
				case "messages":
					h.Messages(c)
				}
				require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
				require.Equal(t, "api_error", gjson.GetBytes(rec.Body.Bytes(), "error.type").String())
				require.Contains(t, gjson.GetBytes(rec.Body.Bytes(), "error.message").String(), "routing")
				require.Equal(t, 1, upstream.calls, "a raw error does not prove that the business request was never sent")
				_, recordedUpstreamError := c.Get(service.OpsUpstreamErrorsKey)
				require.False(t, recordedUpstreamError, "local rejection must not be recorded as upstream rate limiting")
				repo.slots.mu.Lock()
				acquired, released := repo.slots.accountAcquireCalls, repo.slots.accountReleaseCalls
				repo.slots.mu.Unlock()
				require.Equal(t, acquired, released)
			})
		}
	}
}

func TestBorrowUnavailableSelectionHTTPClassification(t *testing.T) {
	fallback := noAccountErrorClassification{Status: http.StatusServiceUnavailable, ErrType: "api_error", Message: "Service temporarily unavailable"}
	err := fmt.Errorf("%w: %w", service.ErrNoAvailableAccounts, service.ErrBorrowUnavailable)
	got := classifySelectionFailureError(err, fallback)
	require.Equal(t, fallback, got)
	mixed := fmt.Errorf("rate_limited=1: %w", err)
	require.Equal(t, fallback, classifySelectionFailureError(mixed, fallback), "one route veto and one limited account do not prove all accounts are rate limited")
	rpm := fmt.Errorf("%w: %w", service.ErrOpenAIRPMExhausted, err)
	require.Equal(t, http.StatusTooManyRequests, classifySelectionFailureError(rpm, fallback).Status)
	require.Equal(t, fallback, classifySelectionFailureError(service.ErrNoAvailableAccounts, fallback))
	modelNotFound := noAccountErrorClassification{Status: http.StatusNotFound, ModelNotFound: true}
	require.Equal(t, modelNotFound, classifySelectionFailureError(err, modelNotFound))
	status, kind, _ := openAIAdmissionClientError(&service.OpenAITurnAdmissionError{Reason: "account_binding_changed"})
	require.Equal(t, http.StatusServiceUnavailable, status)
	require.Equal(t, "admission_unavailable", kind)
}
