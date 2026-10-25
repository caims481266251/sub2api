package service

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type routeReprobeRepo struct {
	*continuePolicyRepo
	BorrowRoutePoolRepository
	result   BorrowRouteOutcome
	finished int
	guardErr error
	beginErr error
}

func (r *routeReprobeRepo) CheckRouteHealth(context.Context, *BorrowRouteHealthJob, string) error {
	return r.guardErr
}
func (r *routeReprobeRepo) RenewRouteHealth(context.Context, *BorrowRouteHealthJob) error { return nil }
func (r *routeReprobeRepo) FinishRouteHealth(_ context.Context, _ *BorrowRouteHealthJob, o BorrowRouteOutcome) error {
	r.result = o
	r.finished++
	return nil
}
func (r *routeReprobeRepo) CheckTargetRequest(context.Context, BorrowRequest) error {
	return r.guardErr
}
func (r *routeReprobeRepo) MarkTargetRequestSent(context.Context, BorrowRequest) error { return nil }
func (r *routeReprobeRepo) BeginTargetRequest(ctx context.Context, q BorrowRequest) error {
	if r.beginErr != nil {
		return r.beginErr
	}
	return r.continuePolicyRepo.BeginTargetRequest(ctx, q)
}
func (r *routeReprobeRepo) ObserveCookie(_ context.Context, _ string, d time.Duration) (time.Time, error) {
	return time.Now().Add(d), nil
}

type routeReprobeUpstream struct {
	*stateProbeUpstream
	lastProfile *tlsfingerprint.Profile
}

func (u *routeReprobeUpstream) DoWithTLS(req *http.Request, proxy string, id int64, n int, p *tlsfingerprint.Profile) (*http.Response, error) {
	if err := BorrowBeforeSend(req); err != nil {
		return nil, err
	}
	if err := BorrowTargetRequestConnected(req); err != nil {
		return nil, err
	}
	u.lastProfile = p
	return u.stateProbeUpstream.DoWithTLS(req, proxy, id, n, p)
}
func TestBorrowRouteHealthUsesSingleQualityRequest(t *testing.T) {
	for _, ticket := range []string{"", "a-new-unrelated-ticket"} {
		t.Run("ticket="+ticket, func(t *testing.T) {
			s, base := borrowFixture()
			r := &routeReprobeRepo{continuePolicyRepo: &continuePolicyRepo{borrowTestRepo: base}}
			s.repo = r
			reply := candyQualityReply("21")
			reply.state = ticket
			u := &routeReprobeUpstream{stateProbeUpstream: &stateProbeUpstream{replies: []stateProbeReply{reply}}}
			s.owner.httpUpstream = u
			job := &BorrowRouteHealthJob{Route: base.route, Token: "quality-owner"}
			job.Route.VerificationModel = "gpt-6.1-sol"
			s.runRouteHealth(t.Context(), r, job)
			require.Equal(t, 1, r.finished)
			require.True(t, r.result.Success)
			require.Empty(t, r.result.Reason)
			require.Zero(t, r.active)
			require.Len(t, r.requests, 1, "a pooled route never repeats mint/continue")
			require.Len(t, r.outcomes, 1)
			require.Len(t, u.calls, 1)
			require.Equal(t, job.Token, r.requests[0].ID)
			require.Equal(t, "quality", r.requests[0].Phase)
			require.Equal(t, job.Route.ID, r.requests[0].RouteID)
			call := u.calls[0]
			require.Equal(t, "Bearer target-secret", call.header.Get("Authorization"))
			require.Equal(t, base.route.Secret.Fingerprint.DeviceID, call.header.Get("X-Codex-Installation-ID"))
			require.Equal(t, base.route.Secret.Fingerprint.Profile, u.lastProfile)
			require.Contains(t, call.header.Get("Cookie"), "__oailb=donor-cookie")
			require.Empty(t, call.header.Get(openAICodexTurnStateHeader))
			require.Equal(t, "gpt-6-astra", gjson.Get(call.body, "model").String())
			require.Equal(t, "medium", gjson.Get(call.body, "reasoning.effort").String())
			require.False(t, gjson.Get(call.body, "store").Bool())
			require.False(t, gjson.Get(call.body, "previous_response_id").Exists())
		})
	}
}
func TestBorrowRouteHealthCancelledJobNeverPasses(t *testing.T) {
	s, base := borrowFixture()
	r := &routeReprobeRepo{continuePolicyRepo: &continuePolicyRepo{borrowTestRepo: base}, guardErr: errors.New("ownership lost")}
	s.repo = r
	s.owner.httpUpstream = &stateProbeUpstream{}
	s.runRouteHealth(t.Context(), r, &BorrowRouteHealthJob{Route: base.route, Token: "job"})
	require.False(t, r.result.Success)
	require.Empty(t, r.result.Reason)
	require.Empty(t, r.requests)
	require.Equal(t, 1, r.finished)
}

func TestBorrowRouteHealthLocalAdmissionFailureStaysPending(t *testing.T) {
	for _, err := range []error{ErrBorrowRequestBudget, errors.New("database unavailable")} {
		s, base := borrowFixture()
		r := &routeReprobeRepo{continuePolicyRepo: &continuePolicyRepo{borrowTestRepo: base}, beginErr: err}
		s.repo = r
		u := &routeReprobeUpstream{stateProbeUpstream: &stateProbeUpstream{}}
		s.owner.httpUpstream = u
		s.runRouteHealth(t.Context(), r, &BorrowRouteHealthJob{Route: base.route, Token: "owner"})
		require.False(t, r.result.Success)
		require.Empty(t, r.result.Reason)
		require.Empty(t, u.calls)
		require.Nil(t, r.result.Quality)
		require.Equal(t, 1, r.finished)
		require.Zero(t, r.active)
	}
}
