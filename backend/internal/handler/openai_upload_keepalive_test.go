//go:build unit

package handler

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type uploadFailoverSignalWriter struct {
	gin.ResponseWriter
	flushed   chan struct{}
	once      sync.Once
	failWrite bool
}

func (w *uploadFailoverSignalWriter) Write(body []byte) (int, error) {
	if w.failWrite {
		w.once.Do(func() { close(w.flushed) })
		return 0, errors.New("client connection closed")
	}
	return w.ResponseWriter.Write(body)
}

func (w *uploadFailoverSignalWriter) Flush() {
	w.ResponseWriter.Flush()
	w.once.Do(func() { close(w.flushed) })
}

type uploadFailoverUpstream struct {
	service.HTTPUpstream
	flushed <-chan struct{}
	status  int
	calls   []int64
}

func (u *uploadFailoverUpstream) Do(_ *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	u.calls = append(u.calls, accountID)
	status := u.status
	if len(u.calls) == 1 {
		select {
		case <-u.flushed:
		case <-time.After(4 * time.Second):
			return nil, fmt.Errorf("no upload heartbeat before response headers")
		}
		status = http.StatusServiceUnavailable
	}
	return &http.Response{
		StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(`{"error":{"type":"invalid_request_error","message":"synthetic failure"}}`)),
	}, nil
}

func TestOpenAIUploadKeepaliveAllowsSameGroupFailover(t *testing.T) {
	for _, finalStatus := range []int{http.StatusBadRequest, http.StatusServiceUnavailable} {
		t.Run(fmt.Sprint(finalStatus), func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			c, rec := newOpenAIResponsesFailoverTestContext(t, nil)
			body := `{"model":"gpt-5.1","stream":true,"input":"synthetic"}`
			c.Request.Body = io.NopCloser(strings.NewReader(body))
			c.Request.ContentLength = int64(len(body))
			signal := &uploadFailoverSignalWriter{ResponseWriter: c.Writer, flushed: make(chan struct{})}
			c.Writer = signal
			upstream := &uploadFailoverUpstream{flushed: signal.flushed, status: finalStatus}
			handler := newOpenAIResponsesFailoverTestHandler(t, upstream, func(cfg *config.Config, accounts []service.Account) {
				cfg.Gateway.StreamKeepaliveInterval = 1
				for i := range accounts {
					accounts[i].Type = service.AccountTypeAPIKey
					accounts[i].Credentials = map[string]any{"api_key": "synthetic"}
					// Exercise passthrough -> standard response error handover too.
					accounts[i].Extra = map[string]any{"openai_passthrough": i == 0}
				}
			})
			handler.maxAccountSwitches = 1
			handler.Responses(c)
			require.Equal(t, []int64{1, 2}, upstream.calls, "heartbeat comments must not consume the failover opportunity")
			require.Equal(t, http.StatusOK, rec.Code, "the heartbeat has already committed HTTP 200")
			require.Equal(t, "text/event-stream", rec.Result().Header.Get("Content-Type"))
			require.Contains(t, rec.Body.String(), ": keepalive\n\n")
			require.Equal(t, 1, strings.Count(rec.Body.String(), "event: response.failed\n"))
			for _, frame := range strings.Split(strings.TrimSpace(rec.Body.String()), "\n\n") {
				if strings.HasPrefix(frame, ":") {
					continue
				}
				parts := strings.SplitN(frame, "\ndata: ", 2)
				require.Len(t, parts, 2, "no raw JSON may follow an SSE heartbeat")
				require.True(t, gjson.Valid(parts[1]))
				require.Equal(t, "failed", gjson.Get(parts[1], "response.status").String())
			}
		})
	}
}

func TestOpenAIUploadKeepaliveWriteFailureStopsFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, rec := newOpenAIResponsesFailoverTestContext(t, nil)
	body := `{"model":"gpt-5.1","stream":true,"input":"synthetic"}`
	c.Request.Body = io.NopCloser(strings.NewReader(body))
	c.Request.ContentLength = int64(len(body))
	signal := &uploadFailoverSignalWriter{ResponseWriter: c.Writer, flushed: make(chan struct{}), failWrite: true}
	c.Writer = signal
	upstream := &uploadFailoverUpstream{flushed: signal.flushed, status: http.StatusServiceUnavailable}
	handler := newOpenAIResponsesFailoverTestHandler(t, upstream, func(cfg *config.Config, accounts []service.Account) {
		cfg.Gateway.StreamKeepaliveInterval = 1
		for i := range accounts {
			accounts[i].Type = service.AccountTypeAPIKey
			accounts[i].Credentials = map[string]any{"api_key": "synthetic"}
			accounts[i].Extra = map[string]any{"openai_passthrough": true}
		}
	})
	handler.Responses(c)
	require.NoError(t, c.Request.Context().Err(), "write failure may precede HTTP request cancellation")
	require.True(t, service.OpenAIDownstreamDisconnected(c))
	require.Equal(t, []int64{1}, upstream.calls, "never start a second attempt after a confirmed client write failure")
	require.Zero(t, rec.Body.Len())
}
