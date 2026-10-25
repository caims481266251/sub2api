package service

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

type targetLeaseFlowRepo struct {
	*poolFlowRepo
	leaseMu           sync.Mutex
	token             string
	until             time.Time
	renewals          int
	finished          bool
	finishReason      string
	renewAfterEnd     bool
	failAt            time.Time
	blockRenewal      bool
	transientFailures int
	readyAt           time.Time
	produce           func()
}

func targetLeaseFixture() (*TicketBorrowService, *targetLeaseFlowRepo) {
	s, base := borrowFixture()
	base.route = nil
	r := &targetLeaseFlowRepo{poolFlowRepo: &poolFlowRepo{borrowWorkerRepo: &borrowWorkerRepo{
		borrowTestRepo: base, source: boundBorrowTestSource(BorrowSource{ID: 1, Revision: 3, Enabled: true}),
	}}, token: "target-owner", until: time.Now().Add(BorrowTargetLeaseTTL)}
	s.repo = r
	return s, r
}

func (r *targetLeaseFlowRepo) RenewTargetLease(ctx context.Context, a BorrowAcquisition) error {
	r.leaseMu.Lock()
	if r.renewals > 0 && r.transientFailures > 0 {
		r.transientFailures--
		r.leaseMu.Unlock()
		return context.DeadlineExceeded
	}
	if r.finished {
		r.renewAfterEnd = true
	}
	if r.blockRenewal && r.renewals > 0 {
		r.leaseMu.Unlock()
		<-ctx.Done()
		return ctx.Err()
	}
	defer r.leaseMu.Unlock()
	if !r.failAt.IsZero() && !time.Now().Before(r.failAt) {
		return errors.New("lease storage unavailable")
	}
	if ctx.Err() != nil || r.finished || a.Lease != r.token || !time.Now().Before(r.until) {
		return ErrBorrowUnavailable
	}
	r.until = time.Now().Add(BorrowTargetLeaseTTL)
	r.renewals++
	return nil
}

func (r *targetLeaseFlowRepo) renewalCount() int {
	r.leaseMu.Lock()
	defer r.leaseMu.Unlock()
	return r.renewals
}

func TestBorrowTargetLeaseTransientFailurePausesThenResumes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, r := targetLeaseFixture()
		r.transientFailures = 1
		state := &borrowTargetLeaseState{}
		ctx, cancel := context.WithCancelCause(context.WithValue(t.Context(), borrowTargetLeaseKey{}, state))
		defer cancel(nil)
		stop, err := s.keepTargetLease(ctx, BorrowAcquisition{Lease: r.token}, cancel)
		require.NoError(t, err)
		defer stop()
		time.Sleep(10 * time.Second)
		synctest.Wait()
		state.mu.Lock()
		paused := state.paused
		state.mu.Unlock()
		require.True(t, paused)
		require.NoError(t, ctx.Err(), "a single database timeout must not cancel sent work")
		ready := make(chan error, 1)
		go func() { ready <- waitBorrowTargetLease(ctx) }()
		synctest.Wait()
		require.Empty(t, ready, "new work waits while renewal is uncertain")
		time.Sleep(time.Second)
		synctest.Wait()
		require.NoError(t, <-ready)
		require.NoError(t, ctx.Err())
		require.Equal(t, 2, r.renewalCount())
	})
}

func TestBorrowTargetLeaseWrongOwnerStopsImmediately(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, r := targetLeaseFixture()
		ctx, cancel := context.WithCancelCause(t.Context())
		defer cancel(nil)
		stop, err := s.keepTargetLease(ctx, BorrowAcquisition{Lease: r.token}, cancel)
		require.NoError(t, err)
		defer stop()
		r.leaseMu.Lock()
		r.token = "new-owner"
		r.leaseMu.Unlock()
		time.Sleep(borrowTargetLeaseRenewInterval)
		synctest.Wait()
		require.ErrorIs(t, context.Cause(ctx), borrowCancelTargetLeaseLost)
	})
}

func (r *targetLeaseFlowRepo) CheckAcquisition(ctx context.Context, a BorrowAcquisition) error {
	r.leaseMu.Lock()
	active := !r.finished && a.Lease == r.token && time.Now().Before(r.until)
	r.leaseMu.Unlock()
	if !active {
		return ErrBorrowUnavailable
	}
	return ctx.Err()
}

func (r *targetLeaseFlowRepo) FinishJob(ctx context.Context, kind string, _ int64, _, reason string, _ time.Duration) error {
	if kind == "target" {
		r.leaseMu.Lock()
		defer r.leaseMu.Unlock()
		r.finished, r.finishReason, r.until = true, reason, time.Time{}
	}
	return ctx.Err()
}

func (r *targetLeaseFlowRepo) Now(context.Context) (time.Time, error) { return time.Now(), nil }
func (r *targetLeaseFlowRepo) ObserveCookie(_ context.Context, _ string, ttl time.Duration) (time.Time, error) {
	return time.Now().Add(ttl), nil
}
func (r *targetLeaseFlowRepo) StartCandidateEnvironment(_ context.Context, id string) (time.Time, time.Time, error) {
	r.borrowTestRepo.mu.Lock()
	defer r.borrowTestRepo.mu.Unlock()
	e := r.environments[id]
	e.StartedAt = time.Now()
	e.AdmissionUntil = e.StartedAt.Add(230 * time.Second)
	return e.StartedAt, e.AdmissionUntil, nil
}
func (r *targetLeaseFlowRepo) ClaimCandidatePool(ctx context.Context, a BorrowAcquisition) (*BorrowPoolCandidate, error) {
	if time.Now().Before(r.readyAt) {
		return nil, nil
	}
	if r.produce != nil {
		r.produce()
		r.produce = nil
	}
	return r.poolCoordinatorRepo.ClaimCandidatePool(ctx, a)
}
func (r *targetLeaseFlowRepo) GrantSourceAttempt(context.Context, int64, string) (bool, error) {
	return !time.Now().Before(r.readyAt), nil
}
func (r *targetLeaseFlowRepo) NeedsCandidate(ctx context.Context, a BorrowAcquisition, _ *time.Time) (bool, error) {
	inv, err := r.Inventory(ctx, a.TargetID, a.Identity)
	return err == nil && inv.Count < inv.Desired, err
}
func (r *targetLeaseFlowRepo) Publish(ctx context.Context, route *BorrowRoute, lease string) error {
	if err := r.CheckAcquisition(ctx, BorrowAcquisition{Lease: lease}); err != nil {
		return err
	}
	if !time.Now().Before(route.AdmissionUntil) {
		return ErrBorrowCandidateExpiry
	}
	return r.borrowWorkerRepo.Publish(ctx, route, lease)
}

type targetLeaseUpstream struct {
	*stateProbeUpstream
	s *TicketBorrowService
}

func (u *targetLeaseUpstream) DoWithTLS(req *http.Request, proxy string, id int64, n int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	if req.Header.Get("Cookie") == "" {
		return u.stateProbeUpstream.DoWithTLS(req, proxy, id, n, profile)
	}
	q, _ := req.Context().Value(borrowProbeRequestKey{}).(BorrowRequest)
	return u.s.trackTargetRequest(req, q, proxy, id, n, profile, func(req *http.Request, proxy string, id int64, n int, profile *tlsfingerprint.Profile) (*http.Response, error) {
		if err := BorrowBeforeSend(req); err != nil {
			return nil, err
		}
		delay := 35 * time.Second
		if req.Header.Get(openAICodexTurnStateHeader) != "" {
			delay = 5 * time.Second
		}
		select {
		case <-req.Context().Done():
			return nil, req.Context().Err()
		case <-time.After(delay):
		}
		return u.stateProbeUpstream.DoWithTLS(req, proxy, id, n, profile)
	})
}

func TestBorrowTargetLeaseLateCandidatesCompleteWithoutBatchDeadline(t *testing.T) {
	for _, mode := range []string{BorrowAcquisitionSource, BorrowAcquisitionSelf} {
		for _, arrival := range []time.Duration{149 * time.Second, 200 * time.Second} {
			t.Run(mode+"/"+arrival.String(), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					s, r := targetLeaseFixture()
					start := time.Now()
					r.readyAt = start.Add(arrival)
					cookie := "x." + base64.RawURLEncoding.EncodeToString([]byte(`{"host":"chat.gateway.unified-1.api.openai.com"}`)) + ".x"
					mint := stateProbeMint("target-ticket")
					mint.cookies = nil
					up := &stateProbeUpstream{replies: []stateProbeReply{
						{status: 200, cookies: []string{"__oailb=" + cookie + "; Max-Age=230"}, body: stateProbeCompletedStream},
						mint, {status: 200, body: stateProbeCompletedStream},
					}}
					s.owner.httpUpstream = &targetLeaseUpstream{stateProbeUpstream: up, s: s}
					r.produce = func() {
						s.producePoolCandidate(t.Context(), r, BorrowPoolAcquisition{ID: "late-candidate", Lease: "source-owner", SourceID: 1})
					}
					ctx, stop := context.WithTimeout(t.Context(), 5*time.Minute)
					defer stop()
					s.runTarget(ctx, BorrowTarget{AccountID: 11, Revision: 2, AcquisitionIdentity: mode, ProbePolicy: BorrowProbePolicy{Concurrency: 1}}, BorrowSettings{Running: true}, r.token)
					require.NoError(t, ctx.Err())
					require.Len(t, up.calls, 3, "source, target mint and continue must all complete")
					require.NotNil(t, r.route)
					require.WithinDuration(t, start.Add(arrival+40*time.Second), time.Now(), time.Second)
					require.Equal(t, "target_probe_passed", r.finishReason)
					require.GreaterOrEqual(t, r.renewalCount(), 19, "ownership must be renewed during the wait and slow validation")
					require.Equal(t, r.route.AcquisitionStartedAt.Add(230*time.Second), r.route.AdmissionUntil, "renewal must not stretch the original candidate cutoff")
					require.True(t, r.finished)
					require.True(t, r.until.IsZero(), "normal completion releases immediately")
					if mode == BorrowAcquisitionSource {
						require.True(t, r.removed)
						require.Equal(t, map[string]string{"late-candidate": "target_probe_passed"}, r.ended)
					}
					count := r.renewalCount()
					time.Sleep(time.Minute)
					require.Equal(t, count, r.renewalCount())
					require.False(t, r.renewAfterEnd, "renewal must be joined before FinishJob")
				})
			})
		}
	}
}

func TestBorrowTargetLeaseFailureStopsWaitingConsumer(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		t.Run(map[bool]string{false: "rejected", true: "database_timeout"}[blocked], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, r := targetLeaseFixture()
				start := time.Now()
				r.failAt, r.blockRenewal = start.Add(time.Second), blocked
				s.runTarget(t.Context(), BorrowTarget{AccountID: 11, Revision: 2, AcquisitionIdentity: BorrowAcquisitionSource}, BorrowSettings{Running: true}, r.token)
				require.Equal(t, "target_lease_lost", r.finishReason)
				require.WithinDuration(t, start.Add(BorrowTargetLeaseTTL), time.Now(), time.Millisecond)
				require.Nil(t, r.route)
				require.Zero(t, r.claims)
				require.True(t, r.removed)
				require.True(t, r.finished)
				require.False(t, r.renewAfterEnd)
			})
		})
	}
}

func TestBorrowTargetLeaseContinuesWhileDemandDrains(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, r := targetLeaseFixture()
		ctx, cancel := context.WithCancelCause(t.Context())
		defer cancel(nil)
		a := BorrowAcquisition{TargetID: 11, Revision: 2, Lease: r.token}
		stop, err := s.keepTargetLease(ctx, a, cancel)
		require.NoError(t, err)
		pool := &poolCoordinatorRepo{queue: []*BorrowPoolCandidate{coordinatorCandidate("first"), coordinatorCandidate("draining"), coordinatorCandidate("unused")}}
		needed, published := true, 0
		s.consumeCandidatePool(ctx, pool, a, 2,
			func() (bool, error) { return needed, nil },
			func(c borrowProbeCandidate) borrowProbeVerification {
				if c.details.CandidateID == "first" {
					time.Sleep(time.Second)
				} else {
					time.Sleep(40 * time.Second)
				}
				return borrowProbeVerification{candidate: c}
			}, func(out borrowProbeVerification) (bool, string) {
				needed = false
				require.NoError(t, r.CheckAcquisition(ctx, a), "demand removal must not stop ownership renewal before publication")
				published++
				return true, "target_probe_passed"
			})
		require.Equal(t, 2, published)
		require.Equal(t, 2, pool.claims)
		require.True(t, pool.removed)
		// Parent cancellation also leaves the owner alive during cleanup.
		cancel(borrowCancelServiceShutdown)
		synctest.Wait()
		count := r.renewalCount()
		time.Sleep(15 * time.Second)
		synctest.Wait()
		require.Greater(t, r.renewalCount(), count)
		stop()
		require.NoError(t, r.FinishJob(t.Context(), "target", a.TargetID, a.Lease, "done", 0))
		count = r.renewalCount()
		time.Sleep(time.Minute)
		require.Equal(t, count, r.renewalCount())
		require.False(t, r.renewAfterEnd)
	})
}
