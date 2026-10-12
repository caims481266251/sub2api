package service

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func pipelineCandidate(id, gateway string) borrowProbeCandidate {
	return borrowProbeCandidate{details: &BorrowCandidateDetails{CandidateID: id, Gateway: gateway}}
}

func TestBorrowGatewayPriorityFairnessAndFallback(t *testing.T) {
	now := time.Now()
	priorities := map[string]time.Time{"bad": now.Add(time.Minute), "expired": now.Add(-time.Second)}
	queue := []borrowProbeCandidate{pipelineCandidate("old", "bad")}
	for i := 0; i < 4; i++ {
		queue = append(queue, pipelineCandidate(fmt.Sprint(i), "new"))
	}
	streak := 0
	for _, want := range []string{"0", "1", "2", "old", "3"} {
		i, demoted := selectBorrowCandidate(queue, priorities, now, &streak)
		require.Equal(t, want, queue[i].details.CandidateID)
		require.Equal(t, want == "old", demoted)
		queue = append(queue[:i], queue[i+1:]...)
	}
	for _, gateway := range []string{"bad", "expired", "unknown"} {
		i, demoted := selectBorrowCandidate([]borrowProbeCandidate{pipelineCandidate("only", gateway)}, priorities, now, &streak)
		require.Zero(t, i)
		require.Equal(t, gateway == "bad", demoted, "single gateway must never block")
	}
}

func TestBorrowPipelineRefillsWithoutWaitingForSlowAcquisition(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	var acquired, discarded atomic.Int32
	verified := 0
	slowStarted := make(chan struct{})
	p := borrowCandidatePipeline{capacity: 2, continuous: true, sources: []BorrowSource{{ID: 1}, {ID: 2}},
		needed: func() bool { return true },
		acquire: func(src BorrowSource) (borrowProbeCandidate, error) {
			n := acquired.Add(1)
			if src.ID == 1 {
				close(slowStarted)
				<-ctx.Done()
				return borrowProbeCandidate{}, ctx.Err()
			}
			return pipelineCandidate(fmt.Sprint(n), "g"), nil
		},
		verify:     func(c borrowProbeCandidate) borrowProbeVerification { return borrowProbeVerification{candidate: c} },
		consume:    func(borrowProbeVerification) bool { verified++; return verified < 5 },
		priorities: func() map[string]time.Time { return nil }, selected: func(borrowProbeCandidate, bool, bool) {},
		discard: func(borrowProbeVerification) { discarded.Add(1) },
	}
	done := make(chan struct{})
	go func() { p.run(ctx, cancel); close(done) }()
	select {
	case <-slowStarted:
	case <-ctx.Done():
		t.Fatal("slow source not started")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("free slot was blocked by slow source")
	}
	require.Equal(t, 5, verified)
	require.GreaterOrEqual(t, acquired.Load(), int32(6))
}

func TestBorrowPipelineBoundsPrefetchAndDrainsAllWorkers(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var acquired, active, discarded, consumed atomic.Int32
	p := borrowCandidatePipeline{capacity: 2, continuous: true, sources: []BorrowSource{{}}, needed: func() bool { return true },
		acquire: func(BorrowSource) (borrowProbeCandidate, error) {
			n := acquired.Add(1)
			return pipelineCandidate(fmt.Sprint(n), "g"), nil
		},
		verify: func(c borrowProbeCandidate) borrowProbeVerification {
			active.Add(1)
			defer active.Add(-1)
			<-ctx.Done()
			return borrowProbeVerification{candidate: c}
		},
		consume: func(borrowProbeVerification) bool { consumed.Add(1); return true }, priorities: func() map[string]time.Time { return nil }, selected: func(borrowProbeCandidate, bool, bool) {}, discard: func(borrowProbeVerification) { discarded.Add(1) },
	}
	done := make(chan struct{})
	go func() { p.run(ctx, cancel); close(done) }()
	require.Eventually(t, func() bool { return acquired.Load() == 4 && active.Load() == 2 }, time.Second, time.Millisecond)
	require.Never(t, func() bool { return acquired.Load() > 4 }, 50*time.Millisecond, time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("pipeline failed to drain")
	}
	require.Zero(t, active.Load())
	require.Equal(t, int32(4), discarded.Load()+consumed.Load())
}

func TestBorrowProbeSlotsPrioritizeContinueAndReleaseCancellation(t *testing.T) {
	ctx := withBorrowProbeSlots(t.Context(), 1)
	release, err := acquireBorrowProbeSlot(ctx, 0)
	require.NoError(t, err)
	slots := ctx.Value(borrowProbeSlotsKey{}).(*borrowProbeSlots)
	order := make(chan int, 3)
	var wg sync.WaitGroup
	for _, priority := range []int{0, 1, 2} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, e := acquireBorrowProbeSlot(ctx, priority)
			if e == nil {
				order <- priority
				r()
				r()
			}
		}()
	}
	require.Eventually(t, func() bool { slots.mu.Lock(); defer slots.mu.Unlock(); return len(slots.waiting) == 3 }, time.Second, time.Millisecond)
	cancelled, stop := context.WithCancel(ctx)
	stop()
	_, err = acquireBorrowProbeSlot(cancelled, 2)
	require.ErrorIs(t, err, context.Canceled)
	release()
	wg.Wait()
	close(order)
	require.Equal(t, 2, <-order)
	require.Equal(t, 1, <-order)
	require.Equal(t, 0, <-order)
	require.Zero(t, slots.active)
	require.Empty(t, slots.waiting)
}

func TestBorrowProbeSlotsCancelGrantRaceDoesNotLeak(t *testing.T) {
	ctx := withBorrowProbeSlots(t.Context(), 1)
	for range 100 {
		first, err := acquireBorrowProbeSlot(ctx, 0)
		require.NoError(t, err)
		waiting, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			release, err := acquireBorrowProbeSlot(waiting, 1)
			if err == nil {
				release()
			}
		}()
		cancel()
		first()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("cancelled slot leaked")
		}
	}
	slots := ctx.Value(borrowProbeSlotsKey{}).(*borrowProbeSlots)
	slots.mu.Lock()
	defer slots.mu.Unlock()
	require.Zero(t, slots.active)
	require.Empty(t, slots.waiting)
}

func TestBorrowGatewayFeedbackOnlyUsesDefiniteVerdicts(t *testing.T) {
	for _, reason := range []string{"target_continue_timeout", "target_probe_inconclusive", "candidate_publish_failed", "source_failed", "candidate_unused_after_stop", "candidate_expired"} {
		require.False(t, BorrowGatewayFeedbackReason(reason), reason)
	}
	for _, reason := range []string{"target_probe_passed", "target_ticket_changed", "target_route_changed"} {
		require.True(t, BorrowGatewayFeedbackReason(reason), reason)
	}
}

// Exercise the production continuous path, including self C=1 and the original
// three HTTP steps. The stock worker fake deliberately only supports batches.
type adaptiveFlowRepo struct {
	*candidateFlowRepo
	outcomes []BorrowGatewayOutcome
}

func (r *adaptiveFlowRepo) BorrowGatewayPriorities(context.Context, BorrowAcquisition) (map[string]time.Time, error) {
	return nil, nil
}
func (r *adaptiveFlowRepo) RecordBorrowGatewayOutcome(_ context.Context, _ BorrowAcquisition, o BorrowGatewayOutcome) error {
	r.outcomes = append(r.outcomes, o)
	return nil
}
func (r *adaptiveFlowRepo) Inventory(context.Context, int64, string) (*BorrowInventory, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	state := "refill"
	if len(r.published) >= 2 {
		state = "healthy"
	}
	return &BorrowInventory{Count: len(r.published), Desired: 2, State: state}, nil
}
func (r *adaptiveFlowRepo) NeedsCandidate(context.Context, BorrowAcquisition, *time.Time) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.published) < 2, nil
}
func (r *adaptiveFlowRepo) Publish(_ context.Context, route *BorrowRoute, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.published = append(r.published, route)
	return nil
}
func TestBorrowAdaptiveWorkerPreservesIdentityAndRefillsAtConcurrencyOne(t *testing.T) {
	for _, mode := range []string{BorrowAcquisitionSelf, BorrowAcquisitionSource} {
		t.Run(mode, func(t *testing.T) {
			s, base := borrowFixture()
			base.route = nil
			source := boundBorrowTestSource(BorrowSource{ID: 1, Enabled: true, Model: "gpt-6-luna"})
			r := &adaptiveFlowRepo{candidateFlowRepo: &candidateFlowRepo{borrowWorkerRepo: &borrowWorkerRepo{borrowTestRepo: base, source: source}, rolling: true}}
			s.repo = r
			s.owner.openaiGatewayService = &OpenAIGatewayService{accountRepo: s.owner.accountRepo}
			up := &candidateFlowUpstream{}
			s.owner.httpUpstream = up
			settings, _ := r.Settings(t.Context())
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			s.runTarget(ctx, BorrowTarget{AccountID: 11, Revision: 2, AcquisitionIdentity: mode, ProbePolicy: BorrowProbePolicy{Concurrency: 1}}, settings, "continuous")
			require.NoError(t, ctx.Err(), "C=1 must finish instead of waiting for batch deadline")
			require.Len(t, r.published, 2, "more than one candidate verified within same batch")
			require.Len(t, r.outcomes, 2)
			phases := map[string]int{}
			for _, call := range up.calls {
				if call.header.Get("Cookie") == "" {
					phases["source"]++
					if mode == BorrowAcquisitionSelf {
						require.EqualValues(t, 11, call.account)
					} else {
						require.Equal(t, source.AccountID, call.account)
					}
				} else {
					require.EqualValues(t, 11, call.account)
					phases["target"]++
				}
			}
			require.Equal(t, 4, phases["target"], "each published candidate still has mint and continue")
			require.GreaterOrEqual(t, phases["source"], 2)
			for _, route := range r.published {
				require.Equal(t, mode, route.AcquisitionIdentity)
			}
			if mode == BorrowAcquisitionSelf {
				require.Zero(t, r.sourceReads)
			}
		})
	}
}
