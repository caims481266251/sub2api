package service

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestBorrowErrorObservationsFollowActualAttempt(t *testing.T) {
	collector := requesttiming.New(time.Now(), 0)
	ctx := requesttiming.With(t.Context(), collector)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil).WithContext(ctx)
	req, tr := requesttiming.StartAttempt(c.Request, 63, 5)
	_ = requesttiming.WithBorrowRequest(requesttiming.WithBorrowRoute(req.Context(), "route-first"), "gpt-6.1-sol", "high")
	tr.Response(&http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"3"}}}, nil)
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{AccountID: 63, UpstreamStatusCode: 429})
	req, tr = requesttiming.StartAttempt(c.Request, 116, 6)
	_ = requesttiming.WithBorrowRoute(req.Context(), "route-second")
	tr.Response(&http.Response{StatusCode: 200}, nil)
	stream := &OpsStreamError{AccountID: 116, IntendedStatus: 502, Message: "stream failed"}
	snapshotOpsStreamErrorContext(c, stream)
	require.Len(t, stream.UpstreamErrors, 2)
	require.Equal(t, "route-first", stream.UpstreamErrors[0].Attempt.BorrowRouteID)
	require.Equal(t, int64(3000), stream.UpstreamErrors[0].Attempt.RetryAfterMS)
	require.Equal(t, "route-second", stream.UpstreamErrors[1].Attempt.BorrowRouteID)
	require.Equal(t, 200, stream.UpstreamErrors[1].UpstreamStatusCode, "HTTP 200 may still end in stream failure")
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{AccountID: 999, UpstreamStatusCode: 503})
	value, _ := c.Get(OpsUpstreamErrorsKey)
	events := value.([]*OpsUpstreamErrorEvent)
	require.Nil(t, events[len(events)-1].Attempt, "selection alone must not claim the previous route")
}

type borrowStreamResetReader struct{ io.Reader }

func (r borrowStreamResetReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if err == io.EOF {
		err = errors.New("stream error: stream ID 9; INTERNAL_ERROR; received from peer")
	}
	return n, err
}

func TestBorrowStreamingResetRecordsActualRouteWithoutHandlerFallback(t *testing.T) {
	collector := requesttiming.New(time.Now(), 0)
	ctx := requesttiming.With(t.Context(), collector)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil).WithContext(ctx)
	req, tr := requesttiming.StartAttempt(c.Request, 63, 5)
	req = req.WithContext(requesttiming.WithBorrowRequest(requesttiming.WithBorrowRoute(req.Context(), "actual-route"), "gpt-6.1-sol", "low"))
	resp := &http.Response{StatusCode: 200, Header: http.Header{}, Request: req,
		Body: io.NopCloser(borrowStreamResetReader{strings.NewReader("data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n")})}
	tr.Response(resp, nil)
	defer resp.Body.Close()
	s := &OpenAIGatewayService{cfg: &config.Config{}}
	_, err := s.handleStreamingResponseWithReasoning(req.Context(), resp, c,
		&Account{ID: 63, Platform: PlatformOpenAI, Type: AccountTypeOAuth}, time.Now(), "gpt-6.1-sol", "gpt-6.1-sol", "low")
	require.Error(t, err)
	require.True(t, IsResponseCommitted(c))
	stream, ok := GetOpsStreamError(c)
	require.True(t, ok, "self-written error must have an ops record even without the handler fallback")
	require.Equal(t, OpenAIUpstreamHTTP2StreamErrorCode, stream.Code)
	require.Equal(t, 502, stream.IntendedStatus)
	require.Equal(t, 200, stream.UpstreamStatus)
	require.Len(t, stream.UpstreamErrors, 1)
	require.Equal(t, "actual-route", stream.UpstreamErrors[0].Attempt.BorrowRouteID)
	require.Equal(t, "low", stream.UpstreamErrors[0].Attempt.ReasoningEffort)
	requesttiming.BorrowLeaseEvent(req.Context(), "released", 1)
	refreshed := GetOpsStreamErrors(c)
	require.Equal(t, "released", refreshed[0].UpstreamErrors[0].Attempt.BorrowLeaseState)
	require.Empty(t, stream.UpstreamErrors[0].Attempt.BorrowLeaseState, "the previously returned error snapshot stays immutable")
}
