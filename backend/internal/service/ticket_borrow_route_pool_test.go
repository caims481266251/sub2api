package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type routeOutcomeRepo struct {
	*borrowTestRepo
	BorrowRoutePoolRepository
	outcomes []BorrowRouteOutcome
}

func (r *routeOutcomeRepo) ObserveRouteOutcome(_ context.Context, o BorrowRouteOutcome) error {
	r.outcomes = append(r.outcomes, o)
	return nil
}

func TestBorrowRoutePoolResponseClassification(t *testing.T) {
	for _, tc := range []struct {
		name, body, reason string
		status             int
		success            bool
	}{
		{"complete", `{"type":"response.completed","response":{"status":"completed","error":null}}`, "", 200, true},
		{"malformed_completed", `{"type":"response.completed","response":{"status":"completed"`, "route_incomplete_response", 200, false},
		{"missing_response", `{"type":"response.completed"}`, "route_incomplete_response", 200, false},
		{"http429", "", "route_rate_limited", 429, false},
		{"http503", "", "route_upstream_error", 503, false},
		{"bad_input", `{"type":"error","error":{"code":"invalid_prompt"}}`, "", 200, false},
		{"sse429", `{"type":"response.failed","response":{"error":{"code":"rate_limit_exceeded"}}}`, "route_rate_limited", 200, false},
		{"cancelled", `{"type":"response.cancelled"}`, "", 200, false},
		{"large_output", strings.Repeat("x", 66000), "", 200, false},
		{"only_delta", `{"type":"response.output_text.delta","delta":"21"}`, "route_incomplete_response", 200, false},
		{"incomplete", `{"type":"response.incomplete","response":{"status":"incomplete"}}`, "", 200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, base := borrowFixture()
			r := &routeOutcomeRepo{borrowTestRepo: base}
			s.repo = r
			o := &borrowRouteResponseObserver{owner: s, route: base.route, lease: "one", ctx: t.Context()}
			o.headers(&http.Response{StatusCode: tc.status, Header: http.Header{}})
			b := &borrowObservedBody{ReadCloser: io.NopCloser(strings.NewReader("data: " + tc.body + "\n\n")), owner: s, outcome: o}
			data, err := io.ReadAll(b)
			require.NoError(t, err)
			require.Contains(t, string(data), tc.body)
			require.Len(t, r.outcomes, 1)
			require.Equal(t, tc.success, r.outcomes[0].Success)
			require.Equal(t, tc.reason, r.outcomes[0].Reason)
		})
	}
}
func TestBorrowRoutePoolPreservesExplicitWaitAndSingleOutcome(t *testing.T) {
	s, base := borrowFixture()
	r := &routeOutcomeRepo{borrowTestRepo: base}
	s.repo = r
	o := &borrowRouteResponseObserver{owner: s, route: base.route, lease: "one", ctx: t.Context()}
	o.headers(&http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"300"}}})
	o.event([]byte(`{"type":"response.completed"}`))
	require.Len(t, r.outcomes, 1)
	require.False(t, r.outcomes[0].Success)
	require.NotNil(t, r.outcomes[0].RetryAt)
	require.WithinDuration(t, time.Now().Add(300*time.Second), *r.outcomes[0].RetryAt, time.Second)
}
