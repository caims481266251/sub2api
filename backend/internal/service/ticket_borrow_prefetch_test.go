package service

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

type slowPrefetchUpstream struct {
	HTTPUpstream
	started   chan struct{}
	fallback  chan struct{}
	cancelled atomic.Int32
	degraded  bool
}

func (u *slowPrefetchUpstream) DoWithTLS(req *http.Request, _ string, id int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	h := http.Header{}
	if id == 21 || id == 22 {
		u.started <- struct{}{}
		select {
		case <-req.Context().Done():
			u.cancelled.Add(1)
			return nil, req.Context().Err()
		case <-u.fallback:
			return nil, context.Canceled
		}
	}
	if id == 23 {
		// Ensure two sources really are in-flight before the fast source wins.
		for range 2 {
			select {
			case <-u.started:
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
		}
		cookie := "x." + base64.RawURLEncoding.EncodeToString([]byte(`{"host":"chat.gateway.unified-3.api.openai.com"}`)) + ".x"
		h.Set("Set-Cookie", "__oailb="+cookie+"; Max-Age=230")
	} else if req.Header.Get(openAICodexTurnStateHeader) == "" {
		h.Set(openAICodexTurnStateHeader, "target-ticket")
	} else if u.degraded {
		h.Set(openAICodexTurnStateHeader, "replacement-ticket")
	}
	return &http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(strings.NewReader(stateProbeCompletedStream))}, nil
}

type stoppedPrefetchRepo struct {
	*prefetchReleaseRepo
	paused  bool
	cooling bool
}

func (r *stoppedPrefetchRepo) Settings(context.Context) (BorrowSettings, error) {
	return BorrowSettings{Running: !r.paused}, nil
}
func (r *stoppedPrefetchRepo) RecordProbeOutcome(_ context.Context, _ int64, _, reason string, _ time.Duration) error {
	if reason == "target_ticket_changed" {
		r.cooling = true
	}
	return nil
}
func (r *stoppedPrefetchRepo) Target(ctx context.Context, id int64) (BorrowTarget, error) {
	t, e := r.borrowTestRepo.Target(ctx, id)
	if r.cooling {
		until := t.ServerNow.Add(time.Minute)
		t.CooldownUntil = &until
	}
	return t, e
}

func TestTicketBorrowStoppedRoundCancelsSlowSources(t *testing.T) {
	for _, paused := range []bool{false, true} {
		name := "upstream_wait"
		if paused {
			name = "paused"
		}
		t.Run(name, func(t *testing.T) {
			s, base := borrowFixture()
			base.route = nil
			r := &stoppedPrefetchRepo{prefetchReleaseRepo: &prefetchReleaseRepo{observationWorkerRepo: &observationWorkerRepo{borrowWorkerRepo: &borrowWorkerRepo{borrowTestRepo: base}}}, paused: paused}
			s.repo = r
			u := &slowPrefetchUpstream{started: make(chan struct{}, 2), fallback: make(chan struct{}), degraded: true}
			s.owner.httpUpstream = u
			s.owner.openaiGatewayService = &OpenAIGatewayService{accountRepo: s.owner.accountRepo}
			done := make(chan struct{})
			go func() {
				defer close(done)
				s.runTarget(t.Context(), BorrowTarget{AcquisitionIdentity: BorrowAcquisitionSource, AccountID: 11, Revision: 2, ProbePolicy: BorrowProbePolicy{Concurrency: 3, RenewBeforeSeconds: 15}}, BorrowSettings{Running: true}, "stop-slow-prefetch")
			}()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				close(u.fallback)
				<-done
				t.Fatal("stopped round waited for unusable speculative sources")
			}
			require.EqualValues(t, 2, u.cancelled.Load())
			require.ErrorIs(t, s.Check(t.Context(), 11, "gpt-6-astra", "http"), ErrBorrowUnavailable, "no verified route may be created by stopped work")
			counts := map[string]int{}
			for _, finish := range r.finishes {
				require.True(t, finish.liveContext)
				counts[finish.kind+":"+finish.reason]++
			}
			want := "target:target_upstream_waiting"
			if paused {
				want = "target:borrow_acquisition_paused"
			}
			require.Equal(t, 1, counts[want])
			require.Equal(t, 2, counts["source:source_acquisition_cancelled"])
			if paused {
				for _, event := range base.events {
					if event.Reason == "source_acquisition_cancelled" {
						require.Equal(t, "user_stop", event.Details.CancellationCause)
					}
				}
			}
		})
	}
}

type prefetchReleaseRepo struct {
	*observationWorkerRepo
	finishMu  sync.Mutex
	published chan struct{}
	finishes  []struct {
		kind, reason string
		liveContext  bool
	}
}

func (r *prefetchReleaseRepo) Publish(ctx context.Context, route *BorrowRoute, lease string) error {
	err := r.observationWorkerRepo.Publish(ctx, route, lease)
	if err == nil && r.published != nil {
		close(r.published)
	}
	return err
}

func (r *prefetchReleaseRepo) FinishJob(ctx context.Context, kind string, _ int64, _, reason string, _ time.Duration) error {
	r.finishMu.Lock()
	defer r.finishMu.Unlock()
	r.finishes = append(r.finishes, struct {
		kind, reason string
		liveContext  bool
	}{kind, reason, ctx.Err() == nil})
	return nil
}

func TestTicketBorrowPublicationPreservesPendingSourceWorkUntilItFinishes(t *testing.T) {
	s, base := borrowFixture()
	base.route = nil
	r := &prefetchReleaseRepo{observationWorkerRepo: &observationWorkerRepo{borrowWorkerRepo: &borrowWorkerRepo{borrowTestRepo: base}}, published: make(chan struct{})}
	s.repo = r
	u := &slowPrefetchUpstream{started: make(chan struct{}, 2), fallback: make(chan struct{})}
	s.owner.httpUpstream = u
	s.owner.openaiGatewayService = &OpenAIGatewayService{accountRepo: s.owner.accountRepo}
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.runTarget(t.Context(), BorrowTarget{AcquisitionIdentity: BorrowAcquisitionSource, AccountID: 11, Revision: 2, ProbePolicy: BorrowProbePolicy{Concurrency: 3}}, BorrowSettings{Running: true}, "preserve-pending")
	}()
	select {
	case <-r.published:
	case <-time.After(2 * time.Second):
		close(u.fallback)
		<-done
		t.Fatal("no publication")
	}
	select {
	case <-done:
		t.Fatal("publication alone must not cancel other candidates")
	default:
	}
	close(u.fallback)
	<-done
	require.Zero(t, u.cancelled.Load())
	require.NotNil(t, base.route)
	require.NoError(t, s.Check(t.Context(), 11, "gpt-6-astra", "http"))
}

func (r *stoppedPrefetchRepo) CheckAcquisition(ctx context.Context, a BorrowAcquisition) error {
	if r.cooling {
		return ErrBorrowRecoveryWaiting
	}
	return r.borrowTestRepo.CheckAcquisition(ctx, a)
}
