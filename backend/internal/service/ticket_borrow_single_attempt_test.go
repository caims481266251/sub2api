package service

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

func TestBorrowCandidateValidationDoesNotRetryFailedPhase(t *testing.T) {
	for _, phase := range []string{"mint", "continue"} {
		for _, tc := range []struct {
			name    string
			reply   stateProbeReply
			failure string
		}{
			{"stream_error", stateProbeReply{status: 200, body: stateProbeFailedStream}, OpenAICodexStateFailureStreamError},
			{"incomplete_stream", stateProbeReply{status: 200, body: "data: {\"type\":\"response.created\"}\n\n"}, OpenAICodexStateFailureStreamError},
			{"http_503", stateProbeReply{status: 503}, OpenAICodexStateFailureUpstreamError},
			{"connection_error", stateProbeReply{err: errors.New("connection reset")}, OpenAICodexStateFailureNetworkError},
			{"network_timeout", stateProbeReply{err: context.DeadlineExceeded}, OpenAICodexStateFailureNetworkError},
			{"rate_limited", stateProbeReply{status: 429}, OpenAICodexStateFailureRateLimited},
			{"unauthorized", stateProbeReply{status: 401}, OpenAICodexStateFailureAccountError},
			{"stream_quota", stateProbeReply{status: 200, body: "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"usage_limit_reached\"}}}\n\n"}, OpenAICodexStateFailureRateLimited},
		} {
			t.Run(phase+"/"+tc.name, func(t *testing.T) {
				ctx := context.WithValue(t.Context(), borrowInternalKey{}, true)
				mint := stateProbeMint("target-ticket")
				mint.cookies = nil
				replies := []stateProbeReply{tc.reply}
				if phase == "continue" {
					replies = append([]stateProbeReply{mint}, replies...)
				}
				wantCalls := len(replies)
				// A healthy fallback would hide the failed candidate if retried.
				replies = append(replies, mint, stateProbeReply{status: 200, body: stateProbeCompletedStream})
				up := &stateProbeUpstream{replies: replies}
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, chatgptCodexURL, nil)
				require.NoError(t, err)
				req.Header.Set("Cookie", "__oailb=candidate")
				result := ProbeOpenAICodexStateRoute(ctx, up, req, "candidate-exit", 11, 1, nil)
				require.Equal(t, OpenAICodexStateInconclusive, result.Verdict)
				require.Equal(t, tc.failure, result.Failure)
				require.Len(t, up.calls, wantCalls)
				require.Zero(t, result.RetryCount)
				require.Len(t, result.Mint.Attempts, 1)
				if phase == "continue" {
					require.Len(t, result.Continue.Attempts, 1)
					require.Equal(t, "target-ticket", up.calls[1].header.Get(openAICodexTurnStateHeader))
				} else {
					require.Nil(t, result.Continue)
				}
				require.NoError(t, ctx.Err(), "failure must not cancel the next candidate's task")
			})
		}
	}
}

func TestBorrowCandidateMissingTicketDoesNotRemint(t *testing.T) {
	ctx := context.WithValue(t.Context(), borrowInternalKey{}, true)
	up := &stateProbeUpstream{replies: []stateProbeReply{{status: 200, body: stateProbeCompletedStream}}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, chatgptCodexURL, nil)
	require.NoError(t, err)
	req.Header.Set("Cookie", "__oailb=candidate")
	result := ProbeOpenAICodexStateRoute(ctx, up, req, "exit", 11, 1, nil)
	require.Equal(t, OpenAICodexStateFailureNoTicket, result.Failure)
	require.Len(t, up.calls, 1)
	require.Zero(t, result.RetryCount)
	require.Nil(t, result.Continue)
}

type singleAttemptFlowRepo struct {
	*poolFlowRepo
	releaseMu sync.Mutex
	released  map[string]bool
}

func (r *singleAttemptFlowRepo) BorrowGatewayPriorities(context.Context, BorrowAcquisition) (map[string]time.Time, error) {
	return nil, nil
}
func (r *singleAttemptFlowRepo) RecordBorrowGatewayOutcome(context.Context, BorrowAcquisition, BorrowGatewayOutcome) error {
	return nil
}
func (r *singleAttemptFlowRepo) NeedsCandidate(context.Context, BorrowAcquisition, *time.Time) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.route == nil, nil
}
func (r *singleAttemptFlowRepo) ReleaseCandidateEnvironment(ctx context.Context, id string) error {
	r.releaseMu.Lock()
	defer r.releaseMu.Unlock()
	r.released[id] = true
	return ctx.Err()
}

type singleAttemptFlowUpstream struct {
	HTTPUpstream
	mu          sync.Mutex
	calls       []candidateFlowCall
	sources     int
	badCookie   string
	failedPhase string
}

func (u *singleAttemptFlowUpstream) DoWithTLS(req *http.Request, proxy string, account int64, _ int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.calls = append(u.calls, candidateFlowCall{account, req.Header.Clone(), proxy, profile})
	headers, body := http.Header{}, stateProbeCompletedStream
	cookie, err := req.Cookie("__oailb")
	if err != nil {
		u.sources++
		value := "x." + base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"host":"chat.gateway.unified-%d.api.openai.com"}`, u.sources))) + ".x"
		if u.sources == 1 {
			u.badCookie = value
		}
		headers.Add("Set-Cookie", "__oailb="+value+"; Max-Age=230; Secure")
	} else {
		phase := "continue"
		if req.Header.Get(openAICodexTurnStateHeader) == "" {
			phase = "mint"
			headers.Set(openAICodexTurnStateHeader, "target-ticket")
		}
		if cookie.Value == u.badCookie && phase == u.failedPhase {
			body = stateProbeFailedStream
		}
	}
	return &http.Response{StatusCode: 200, Header: headers, Body: io.NopCloser(strings.NewReader(body))}, nil
}

func TestBorrowCandidateFailureAdvancesToFreshCandidate(t *testing.T) {
	for _, mode := range []string{BorrowAcquisitionSource, BorrowAcquisitionSelf} {
		for _, phase := range []string{"mint", "continue"} {
			t.Run(mode+"/"+phase, func(t *testing.T) {
				s, base := borrowFixture()
				base.route = nil
				r := &singleAttemptFlowRepo{poolFlowRepo: &poolFlowRepo{borrowWorkerRepo: &borrowWorkerRepo{
					borrowTestRepo: base, source: boundBorrowTestSource(BorrowSource{ID: 1, Revision: 3, Enabled: true}),
				}}, released: map[string]bool{}}
				s.repo = r
				up := &singleAttemptFlowUpstream{failedPhase: phase}
				s.owner.httpUpstream = up
				ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
				defer cancel()
				if mode == BorrowAcquisitionSource {
					for i := range 2 {
						s.producePoolCandidate(ctx, r, BorrowPoolAcquisition{ID: fmt.Sprintf("candidate-%d", i), Lease: "source-owner", SourceID: 1})
					}
					require.Len(t, r.queue, 2)
				}
				s.runTarget(ctx, BorrowTarget{AcquisitionIdentity: mode, AccountID: 11, Revision: 2, ProbePolicy: BorrowProbePolicy{Concurrency: 1}}, BorrowSettings{Running: true}, "target-owner")
				require.NoError(t, ctx.Err())
				require.NotNil(t, base.route, "a failed candidate must not stall the worker")
				require.Equal(t, mode, base.route.AcquisitionIdentity)
				require.NotEqual(t, up.badCookie, base.route.Secret.Cookie.Value)
				require.NoError(t, s.Check(ctx, 11, "gpt-6-astra", "http"))
				var failed *BorrowCandidateDetails
				for _, event := range base.events {
					if event.Stage == "target" && event.Reason == "target_probe_inconclusive" {
						require.Nil(t, failed, "one terminal verdict per failed candidate")
						failed = event.Details
					}
				}
				require.NotNil(t, failed)
				require.Equal(t, OpenAICodexStateFailureStreamError, failed.ProbeFailure)
				require.Zero(t, failed.ProbeRetries)
				require.True(t, r.released[failed.CandidateID])
				require.True(t, r.released[base.route.CandidateID])
				require.NotEqual(t, failed.CandidateID, base.route.CandidateID)
				require.NotEqual(t, r.environments[failed.CandidateID].Fingerprint.DeviceID, r.environments[base.route.CandidateID].Fingerprint.DeviceID)
				badCalls := 0
				for _, call := range up.calls {
					if strings.Contains(call.header.Get("Cookie"), up.badCookie) {
						badCalls++
					}
				}
				wantBadCalls := 1
				if phase == "continue" {
					wantBadCalls = 2
				}
				require.Equal(t, wantBadCalls, badCalls)
				if mode == BorrowAcquisitionSource {
					require.Equal(t, map[string]string{"candidate-0": "target_probe_inconclusive", "candidate-1": "target_probe_passed"}, r.ended)
					require.Equal(t, 2, r.claims)
					require.True(t, r.removed)
				}
			})
		}
	}
}
