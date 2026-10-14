package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

type poolCoordinatorRepo struct {
	BorrowCandidatePoolRepository
	queue   []*BorrowPoolCandidate
	claims  int
	demands int
	removed bool
	ended   map[string]string
	mu      sync.Mutex
}

func (r *poolCoordinatorRepo) RegisterCandidatePoolDemand(context.Context, BorrowAcquisition, int) error {
	r.demands++
	return nil
}
func (r *poolCoordinatorRepo) RemoveCandidatePoolDemand(ctx context.Context, _ BorrowAcquisition) error {
	r.removed = ctx.Err() == nil
	return ctx.Err()
}
func (r *poolCoordinatorRepo) ClaimCandidatePool(context.Context, BorrowAcquisition) (*BorrowPoolCandidate, error) {
	if len(r.queue) == 0 {
		return nil, nil
	}
	c := r.queue[0]
	r.queue = r.queue[1:]
	r.claims++
	return c, nil
}
func (r *poolCoordinatorRepo) FinishCandidatePool(ctx context.Context, id, _, reason string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ended == nil {
		r.ended = map[string]string{}
	}
	if _, exists := r.ended[id]; exists {
		return errors.New("duplicate completion")
	}
	r.ended[id] = reason
	return nil
}
func coordinatorCandidate(id string) *BorrowPoolCandidate {
	return &BorrowPoolCandidate{ID: id, SourceID: 1, Cookie: http.Cookie{Name: "__oailb", Value: id}, Expires: time.Now().Add(time.Minute),
		Details: &BorrowCandidateDetails{CandidateID: id}, Environment: &BorrowCandidateEnvironment{ID: id}}
}

func TestBorrowSharedPoolCoordinatorDrainsConcurrentSuccesses(t *testing.T) {
	r := &poolCoordinatorRepo{queue: []*BorrowPoolCandidate{coordinatorCandidate("first"), coordinatorCandidate("second"), coordinatorCandidate("unused")}}
	s := &TicketBorrowService{}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	started := make(chan string, 2)
	first, second := make(chan struct{}), make(chan struct{})
	consumed := make(chan string, 2)
	needed := true // Used only by the coordinator callbacks.
	done := make(chan string, 1)
	go func() {
		done <- s.consumeCandidatePool(ctx, r, BorrowAcquisition{TargetID: 1, Lease: "batch"}, 2,
			func() (bool, error) { return needed, nil },
			func(c borrowProbeCandidate) borrowProbeVerification {
				started <- c.details.CandidateID
				gate := first
				if c.details.CandidateID == "second" {
					gate = second
				}
				select {
				case <-gate:
				case <-ctx.Done():
					return borrowProbeVerification{candidate: c, reason: "unexpected_cancellation"}
				}
				return borrowProbeVerification{candidate: c, result: &OpenAICodexStateProbeResult{Verdict: OpenAICodexStateHealthy}}
			}, func(out borrowProbeVerification) (bool, string) {
				needed = false
				consumed <- out.candidate.details.CandidateID
				if out.result == nil {
					return false, out.reason
				}
				return true, "target_probe_passed"
			})
	}()
	for range 2 {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("verifiers did not start")
		}
	}
	close(first)
	require.Equal(t, "first", <-consumed)
	close(second)
	require.Empty(t, <-done)
	require.NoError(t, ctx.Err(), "satisfying inventory must not cancel another admitted verification")
	require.Equal(t, 2, r.claims)
	require.True(t, r.removed)
	require.Equal(t, map[string]string{"first": "target_probe_passed", "second": "target_probe_passed"}, r.ended)
}

func TestBorrowSharedPoolCoordinatorZeroSuccessAndStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := &poolCoordinatorRepo{}
		for i := range 100 {
			r.queue = append(r.queue, coordinatorCandidate(fmt.Sprint(i)))
		}
		s := &TicketBorrowService{}
		count := 0
		reason := s.consumeCandidatePool(t.Context(), r, BorrowAcquisition{TargetID: 1, Lease: "batch"}, 2,
			func() (bool, error) { return true, nil },
			func(c borrowProbeCandidate) borrowProbeVerification {
				return borrowProbeVerification{candidate: c, reason: "target_ticket_changed"}
			},
			func(out borrowProbeVerification) (bool, string) { count++; return count < 100, out.reason })
		require.Empty(t, reason)
		require.Len(t, r.ended, 100, "zero successes must not permanently stop bounded exploration")
		require.True(t, r.removed)
	})
	t.Run("cancel_and_drain", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r := &poolCoordinatorRepo{queue: []*BorrowPoolCandidate{coordinatorCandidate("a"), coordinatorCandidate("b")}}
			s := &TicketBorrowService{}
			ctx, cancel := context.WithCancel(t.Context())
			started := make(chan struct{}, 2)
			done := make(chan struct{})
			go func() {
				defer close(done)
				s.consumeCandidatePool(ctx, r, BorrowAcquisition{TargetID: 1, Lease: "batch"}, 2,
					func() (bool, error) { return true, nil },
					func(c borrowProbeCandidate) borrowProbeVerification {
						started <- struct{}{}
						<-ctx.Done()
						return borrowProbeVerification{candidate: c}
					},
					func(borrowProbeVerification) (bool, string) { return false, "candidate_unused_after_stop" })
			}()
			<-started
			<-started
			cancel()
			<-done
			require.Len(t, r.ended, 2)
			require.True(t, r.removed, "cleanup must use an independent live context")
		})
	})
}

type poolClockRepo struct{ *continuePolicyRepo }

func (r *poolClockRepo) Now(context.Context) (time.Time, error) { return time.Now(), nil }

func TestBorrowSharedPoolTimeBudgetUsesRemainingStagesAndCurrentTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, base := borrowFixture()
		r := &poolClockRepo{&continuePolicyRepo{borrowTestRepo: base}}
		s.repo = r
		ctx := context.WithValue(t.Context(), borrowPoolVerificationKey{}, borrowPoolVerificationDeadline{Until: time.Now().Add(59 * time.Second)})
		require.NoError(t, s.checkPoolVerificationTime(ctx, "mint"))
		time.Sleep(time.Millisecond)
		require.ErrorIs(t, s.checkPoolVerificationTime(ctx, "mint"), ErrBorrowCandidateTimeBudget)
		time.Sleep(30 * time.Second)
		require.NoError(t, s.checkPoolVerificationTime(ctx, "continue"), "completed mint is not budgeted a second time")
		seconds := 45
		r.seconds = &seconds
		require.ErrorIs(t, s.checkPoolVerificationTime(ctx, "continue"), ErrBorrowCandidateTimeBudget, "a timeout increase changes remaining-stage admission")
		require.NoError(t, s.checkPoolVerificationTime(t.Context(), "mint"), "self and explicit scan flows retain their original policy")
	})
}

func TestBorrowSharedPoolRechecksTimeAfterSlotAndConnectionWait(t *testing.T) {
	for _, stage := range []string{"admission", "slot", "connection"} {
		t.Run(stage, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, base := borrowFixture()
				r := &poolClockRepo{&continuePolicyRepo{borrowTestRepo: base}}
				s.repo = r
				until := time.Now().Add(60 * time.Second)
				if stage == "admission" {
					until = time.Now().Add(58 * time.Second)
				}
				ctx := withBorrowProbeSlots(t.Context(), 1)
				ctx = context.WithValue(ctx, borrowPoolVerificationKey{}, borrowPoolVerificationDeadline{Until: until})
				if stage == "slot" {
					release, err := acquireBorrowProbeSlot(ctx, 0)
					require.NoError(t, err)
					go func() { time.Sleep(2 * time.Second); release() }()
				}
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.test", nil)
				require.NoError(t, err)
				writes := 0
				_, err = s.trackTargetRequest(req, BorrowRequest{Kind: "probe"}, "", 11, 1, nil,
					func(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
						time.Sleep(2 * time.Second)
						if err := BorrowBeforeSend(req); err != nil {
							return nil, err
						}
						writes++
						return &http.Response{StatusCode: 200, Body: http.NoBody}, nil
					})
				require.ErrorIs(t, err, ErrBorrowCandidateTimeBudget)
				require.Zero(t, writes)
				require.Zero(t, r.active)
				if stage != "connection" {
					require.Empty(t, r.requests, "local rejection before admission consumes no HTTP reservation")
				}
				release, err := acquireBorrowProbeSlot(ctx, 0)
				require.NoError(t, err, "time rejection releases the slot")
				release()
			})
		})
	}
}

func TestBorrowSharedPoolLocalBudgetFailureIsNotRetriedOrUpstreamFailure(t *testing.T) {
	result, attempts := &OpenAICodexStateProbeResult{}, 0
	shot, err := retryOpenAICodexStateRouteShot(t.Context(), result, true, func(context.Context) (openAICodexStateShot, error) {
		attempts++
		return openAICodexStateShot{}, ErrBorrowCandidateTimeBudget
	})
	require.ErrorIs(t, err, ErrBorrowCandidateTimeBudget)
	require.Equal(t, 1, attempts)
	require.False(t, result.shotUsable(t.Context(), "mint", shot, err))
	require.Equal(t, "candidate_time_budget_insufficient", result.Failure)
	require.False(t, BorrowGatewayFeedbackReason(result.Failure))
	_, wait := borrowProbeUpstreamWait(result)
	require.Zero(t, wait)
}

// Exercise the actual source -> persisted payload -> target mint/continue ->
// publication path. The repository concurrency/ownership rules have separate
// database tests; this guards the service wiring and non-serialized callbacks.
type poolFlowRepo struct {
	*borrowWorkerRepo
	BorrowInventoryRepository
	poolCoordinatorRepo
	saved     *BorrowPoolCandidate
	loaded    *BorrowPoolCandidate
	released  []string
	publicRaw []byte
}

func (r *poolFlowRepo) CheckCandidatePoolAcquisition(ctx context.Context, _ BorrowPoolAcquisition) error {
	if _, targetBound := BorrowAcquisitionFromContext(ctx); targetBound {
		return errors.New("shared source borrowed a target lease")
	}
	return ctx.Err()
}

func (r *poolFlowRepo) EnqueueCandidatePool(_ context.Context, _ BorrowPoolAcquisition, c *BorrowPoolCandidate) error {
	r.saved = c
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	var restored BorrowPoolCandidate
	if err = json.Unmarshal(raw, &restored); err != nil {
		return err
	}
	r.publicRaw, err = json.Marshal(c.Details)
	r.loaded = &restored
	r.queue = append(r.queue, r.loaded)
	return err
}

func (r *poolFlowRepo) Inventory(context.Context, int64, string) (*BorrowInventory, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.route != nil {
		return &BorrowInventory{Count: 1, Desired: 1, State: "healthy"}, nil
	}
	return &BorrowInventory{Desired: 1, State: "search"}, nil
}

func (r *poolFlowRepo) ReleaseCandidateEnvironment(_ context.Context, id string) error {
	r.released = append(r.released, id)
	return nil
}

func TestBorrowSharedPoolSourceThroughNormalPublication(t *testing.T) {
	s, base := borrowFixture()
	base.route = nil
	r := &poolFlowRepo{borrowWorkerRepo: &borrowWorkerRepo{
		borrowTestRepo: base,
		source:         boundBorrowTestSource(BorrowSource{ID: 1, Revision: 3, Enabled: true, Model: "gpt-6-luna"}),
	}}
	s.repo = r
	cookie := "x." + base64.RawURLEncoding.EncodeToString([]byte(`{"host":"chat.gateway.unified-1.api.openai.com"}`)) + ".x"
	mint := stateProbeMint("shared-target-state")
	mint.cookies = nil
	up := &stateProbeUpstream{replies: []stateProbeReply{
		{status: 200, cookies: []string{"__oailb=" + cookie + "; Max-Age=230; Secure"}, body: stateProbeCompletedStream},
		mint, {status: 200, body: stateProbeCompletedStream},
	}}
	s.owner.httpUpstream = up
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	s.producePoolCandidate(ctx, r, BorrowPoolAcquisition{ID: "neutral-candidate", Lease: "source-owner", SourceID: 1})
	require.NotNil(t, r.loaded)
	require.Len(t, up.calls, 1, "source acquisition cannot itself publish a usable route")
	require.Nil(t, base.route)
	require.NotSame(t, r.saved.Environment, r.loaded.Environment)
	require.Nil(t, r.loaded.Details.Environment, "public details must not serialize transport secrets")
	require.Nil(t, r.loaded.Details.checkSource, "source qualification must be rebuilt after persistence")
	require.NotEmpty(t, r.loaded.SourceCredentialHash)
	require.NotContains(t, string(r.publicRaw), "source-token")
	require.NotContains(t, string(r.publicRaw), cookie)
	require.NotContains(t, string(r.publicRaw), "http://pool.example")
	require.Empty(t, r.released, "queued candidate retains its environment")

	s.runTarget(ctx, BorrowTarget{AcquisitionIdentity: BorrowAcquisitionSource, AccountID: 11, Revision: 2}, BorrowSettings{Running: true}, "target-owner")
	require.NoError(t, ctx.Err())
	require.Len(t, up.calls, 3, "both normal target validation phases are required")
	require.Equal(t, "Bearer source-token", up.calls[0].header.Get("Authorization"))
	require.Equal(t, "Bearer target-secret", up.calls[1].header.Get("Authorization"))
	require.Equal(t, "shared-target-state", up.calls[2].header.Get(openAICodexTurnStateHeader))
	for _, call := range up.calls[1:] {
		require.Equal(t, up.calls[0].proxy, call.proxy)
		require.Equal(t, up.calls[0].header.Get("User-Agent"), call.header.Get("User-Agent"))
		require.Equal(t, up.calls[0].header.Get("Oai-Device-Id"), call.header.Get("Oai-Device-Id"))
		require.Contains(t, call.header.Get("Cookie"), cookie)
	}
	require.NotNil(t, base.route)
	require.Equal(t, "neutral-candidate", base.route.CandidateID)
	require.Equal(t, r.saved.SourceCredentialHash, base.route.SourceCredentialHash)
	require.Equal(t, r.saved.SourceSubjectHash, base.route.SourceSubjectHash)
	require.False(t, base.route.AdmissionUntil.After(r.saved.Expires), "claiming and publication cannot extend the original cutoff")
	require.Equal(t, map[string]string{"neutral-candidate": "target_probe_passed"}, r.ended)
	require.Equal(t, []string{"neutral-candidate"}, r.released)
	require.True(t, r.removed)
	require.NoError(t, s.Check(ctx, 11, "gpt-6-astra", "http"), "verified result enters the normal usable inventory directly")
}
