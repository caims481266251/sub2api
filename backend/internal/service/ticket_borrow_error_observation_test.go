package service

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
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
