package requesttiming

import (
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestBorrowAttemptObservationIsolation(t *testing.T) {
	c := New(time.Now(), 0)
	ctx := With(t.Context(), c)
	req, tr := StartAttempt(httptest.NewRequest("POST", "https://example.test", nil).WithContext(ctx), 63, 12)
	req = req.WithContext(WithBorrowRequest(WithBorrowRoute(req.Context(), "route-A"), "gpt-6.1-sol", "medium"))
	tr.Response(&http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"2"}}}, nil)
	first := LatestAttempt(ctx)
	require.Equal(t, "route-A", first.BorrowRouteID)
	require.Equal(t, int64(2000), first.RetryAfterMS)
	first.Events["bad"] = 1
	require.NotContains(t, LatestAttempt(ctx).Events, "bad")
	req, _ = StartAttempt(httptest.NewRequest("POST", "https://example.test", nil).WithContext(ctx), 116, 13)
	req = req.WithContext(WithBorrowRoute(req.Context(), "route-B"))
	require.Equal(t, "route-B", LatestAttempt(ctx).BorrowRouteID)
	require.Equal(t, "route-A", first.BorrowRouteID)
}
func TestBorrowReasoningIsNotAnswerText(t *testing.T) {
	c := New(time.Now().Add(-time.Minute), 0)
	ctx := With(t.Context(), c)
	req, tr := StartAttempt(httptest.NewRequest("POST", "https://example.test", nil).WithContext(ctx), 63, 12)
	tr.update(func(a *Attempt) { a.StartMS = 0 })
	StreamProgress(req.Context(), true, false)
	a := LatestAttempt(ctx)
	require.True(t, a.NoTextLongTail)
	require.Equal(t, 1, a.ReasoningEvents)
	require.NotContains(t, a.Events, "first_text")
	StreamProgress(req.Context(), false, true)
	a = LatestAttempt(ctx)
	require.True(t, a.NoTextLongTail)
	require.Contains(t, a.Events, "first_text")
	BorrowRecovery(ctx, BorrowRecoverySnapshot{RequestsSent: 2})
	snapshot := RecoverySnapshot(ctx)
	snapshot.RequestsSent = 9
	require.Equal(t, 2, RecoverySnapshot(ctx).RequestsSent)
}
