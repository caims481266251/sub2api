package service

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

func TestBorrowDiagnosticsSourceTraceAndRedaction(t *testing.T) {
	ctx := context.WithValue(t.Context(), borrowInternalKey{}, true)
	secret := "private-user:private-password@private-proxy.invalid private-ticket private-cookie"
	shot, err := fireOpenAICodexStateShotRequest(ctx, http.Header{}, "gpt-6-luna", "", "", func(req *http.Request) (*http.Response, error) {
		tr := httptrace.ContextClientTrace(req.Context())
		tr.GetConn(secret)
		tr.TLSHandshakeStart()
		tr.TLSHandshakeDone(tls.ConnectionState{}, errors.New(secret))
		return nil, errors.New(secret)
	})
	require.Error(t, err)
	require.NotNil(t, shot.diagnostic)
	require.Equal(t, "tls_error", shot.diagnostic.ErrorClass)
	require.Equal(t, "tls", shot.diagnostic.FailureStage)
	require.Contains(t, shot.diagnostic.Events, "tls_start")
	require.Contains(t, shot.diagnostic.Events, "tls_end")
	raw, e := json.Marshal(shot.diagnostic)
	require.NoError(t, e)
	require.NotContains(t, string(raw), "private-")
}

func TestBorrowDiagnosticsSplitSSEAndEOF(t *testing.T) {
	ctx, tr := newBorrowHTTPTrace(t.Context(), "mint")
	resp := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.created\"}\r\n\r\ndata: {\"type\":\r\ndata: \"response.completed\"}\r\n\r\n"))}
	tr.response(resp, ctx)
	var out strings.Builder
	buf := make([]byte, 3)
	for {
		n, err := resp.Body.Read(buf)
		out.Write(buf[:n])
		if err != nil {
			require.ErrorIs(t, err, io.EOF)
			break
		}
	}
	require.NoError(t, resp.Body.Close())
	d := tr.snapshot()
	require.Equal(t, "completed", d.Terminal)
	require.Empty(t, d.ErrorClass)
	for _, name := range []string{"response_headers", "first_sse", "completed", "eof"} {
		require.Contains(t, d.Events, name)
	}
	require.Contains(t, out.String(), "response.completed")
	// Previously returned evidence cannot change after subsequent trace callbacks.
	d.Events["private"] = 123
	require.NotContains(t, tr.snapshot().Events, "private")
}

func TestBorrowDiagnosticsContinueTimeoutStageAndPersistence(t *testing.T) {
	for _, mode := range []string{"headers", "first_sse", "stream", "eof", "late_completed"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, base := borrowFixture()
				r := &diagnosticRequestRepo{borrowTestRepo: base}
				s.repo = r
				ctx := context.WithValue(t.Context(), borrowInternalKey{}, true)
				ctx = context.WithValue(ctx, borrowProbeRequestKey{}, BorrowRequest{Kind: "probe"})
				_, err := fireOpenAICodexStateShotRequest(ctx, http.Header{}, "gpt-6-astra", "ticket", "", func(req *http.Request) (*http.Response, error) {
					return s.trackTargetRequest(req, BorrowRequest{Kind: "probe"}, "", 11, 2, nil, func(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
						tr := httptrace.ContextClientTrace(req.Context())
						tr.GetConn("not-persisted")
						tr.GotConn(httptrace.GotConnInfo{})
						tr.WroteRequest(httptrace.WroteRequestInfo{})
						if mode == "headers" {
							<-req.Context().Done()
							return nil, req.Context().Err()
						}
						sent := false
						body := &continueTestBody{read: func(p []byte) (int, error) {
							if !sent && mode != "first_sse" && mode != "late_completed" {
								sent = true
								event := "response.created"
								if mode == "eof" {
									event = "response.completed"
								}
								return copy(p, "data: {\"type\":\""+event+"\"}\n\n"), nil
							}
							<-req.Context().Done()
							if mode == "late_completed" {
								return copy(p, stateProbeCompletedStream), io.EOF
							}
							return 0, req.Context().Err()
						}}
						return &http.Response{StatusCode: 200, Header: http.Header{}, Body: body}, nil
					})
				})
				require.True(t, borrowContinueTimedOut(err))
				require.NotNil(t, r.ended.Diagnostic)
				d := r.ended.Diagnostic
				stage := mode
				if mode == "late_completed" {
					stage = "first_sse"
				}
				if mode == "headers" {
					stage = "response_headers"
				}
				require.Equal(t, stage, d.TimeoutStage)
				require.Equal(t, "timeout", d.ErrorClass)
				require.Equal(t, "continue_deadline", d.CancellationCause)
				require.GreaterOrEqual(t, d.DurationMS, int64(7000))
				require.Equal(t, "continue", d.Phase)
				if mode == "eof" {
					require.Contains(t, d.Events, "completed")
					require.NotContains(t, d.Events, "eof")
				}
			})
		})
	}
}

type diagnosticRequestRepo struct {
	*borrowTestRepo
	ended BorrowRequest
}

func (r *diagnosticRequestRepo) EndTargetRequest(_ context.Context, q BorrowRequest, _ int, _ string) error {
	r.ended = q
	return nil
}

func TestBorrowDiagnosticsCancellationCauses(t *testing.T) {
	for _, cause := range []borrowCancelReason{borrowCancelUserStop, borrowCancelBatchSuccess, borrowCancelCandidateDeadline, borrowCancelValidationDeadline, borrowCancelServiceShutdown, borrowCancelTargetLeaseLost, "batch_deadline"} {
		t.Run(string(cause), func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(t.Context())
			ctx, tr := newBorrowHTTPTrace(ctx, "mint")
			cancel(cause)
			tr.finish(ctx, ctx.Err())
			require.Equal(t, string(cause), tr.snapshot().CancellationCause)
		})
	}
	require.Equal(t, "user_stop", borrowCancellation(t.Context(), ErrBorrowPaused))
	require.Empty(t, borrowCancellation(t.Context(), errors.New("private-error")))
}

func TestBorrowDiagnosticsPhaseRetriesRemainSeparate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		result := &OpenAICodexStateProbeResult{}
		for _, mint := range []bool{true, false} {
			calls := 0
			_, err := retryOpenAICodexStateRouteShot(t.Context(), result, mint, func(ctx context.Context) (openAICodexStateShot, error) {
				calls++
				time.Sleep(time.Second)
				n, _ := ctx.Value(borrowAttemptNumberKey{}).(int)
				d := &BorrowHTTPAttempt{Number: n, DurationMS: 1000}
				if mint && calls == 1 {
					return openAICodexStateShot{diagnostic: d}, io.ErrUnexpectedEOF
				}
				return openAICodexStateShot{status: 200, state: "ticket", diagnostic: d}, nil
			})
			require.NoError(t, err)
		}
		require.Equal(t, 1, result.RetryCount)
		require.Equal(t, 1, result.Mint.RetryCount)
		require.Len(t, result.Mint.Attempts, 2)
		require.Equal(t, 2, result.Mint.Attempts[1].Number)
		require.Equal(t, int64(2500), result.Mint.DurationMS)
		require.Zero(t, result.Continue.RetryCount)
		require.Len(t, result.Continue.Attempts, 1)
		require.Equal(t, int64(1000), result.Continue.DurationMS)
	})
}

func TestBorrowDiagnosticsDeadlineCausesUseOwningDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		task, taskCancel := context.WithCancelCause(t.Context())
		defer taskCancel(nil)
		candidate, candidateCancel := context.WithTimeoutCause(task, time.Second, borrowCancelCandidateDeadline)
		defer candidateCancel()
		validation, validationCancel := context.WithTimeoutCause(task, BorrowCandidateVerificationLimit, borrowCancelValidationDeadline)
		defer validationCancel()
		time.Sleep(2 * time.Second)
		require.Equal(t, "candidate_deadline", borrowCancellation(candidate, candidate.Err()))
		require.Empty(t, borrowCancellation(validation, nil))
		time.Sleep(89 * time.Second)
		require.Equal(t, "validation_deadline", borrowCancellation(validation, validation.Err()))
		taskCancel(borrowCancelServiceShutdown)
		require.Equal(t, "service_shutdown", borrowCancellation(task, task.Err()))
		require.Equal(t, "candidate_deadline", borrowCancellation(candidate, candidate.Err()))
	})
}
