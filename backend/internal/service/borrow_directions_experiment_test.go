//go:build astra_experiment

package service

import (
	"context"
	"fmt"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type directionsStub struct {
	HTTPUpstream
	seen []*http.Request
}

func (s *directionsStub) DoWithTLS(r *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	s.seen = append(s.seen, r)
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\"}\n\n"))}, nil
}
func TestDirectionsSessionOverridePreservesNormalRequest(t *testing.T) {
	stub := &directionsStub{}
	trace := &directionTrace{HTTPUpstream: stub, session: "shared"}
	req, _ := http.NewRequestWithContext(context.Background(), "POST", "https://example.test", strings.NewReader("payload"))
	req.Close = true
	req.Header.Set("Authorization", "target")
	req.Header.Set("Cookie", "__oailb=route")
	req.Header.Set("session_id", "original")
	for i := 0; i < 2; i++ {
		resp, e := trace.DoWithTLS(req, "proxy", 63, 2, nil)
		require.NoError(t, e)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	require.Equal(t, "original", req.Header.Get("session_id"))
	require.Equal(t, trace.shots[0].SessionHash, trace.shots[1].SessionHash)
	for _, r := range stub.seen {
		require.Equal(t, "shared", r.Header.Get("session_id"))
		require.Equal(t, "target", r.Header.Get("Authorization"))
		require.Equal(t, "__oailb=route", r.Header.Get("Cookie"))
		require.True(t, r.Close)
		data, e := r.GetBody()
		require.NoError(t, e)
		b, _ := io.ReadAll(data)
		require.Equal(t, "payload", string(b))
	}
	require.Equal(t, 1, trace.shots[0].Stream.Events["response.completed"])
	trace.session = ""
	req.Header.Set("session_id", "fresh-business")
	resp, e := trace.DoWithTLS(req, "proxy", 63, 2, nil)
	require.NoError(t, e)
	resp.Body.Close()
	require.NotEqual(t, trace.shots[1].SessionHash, trace.shots[2].SessionHash)
}

func TestDirectionsWindowOrderBalancesEarlyAndLate(t *testing.T) {
	require.Equal(t, []string{"serial", "parallel", "parallel", "serial"}, directionWindowOrder(false))
	require.Equal(t, []string{"parallel", "serial", "serial", "parallel"}, directionWindowOrder(true))
}
func TestDirectionsWindowRejectsSourceChanges(t *testing.T) {
	f, e := NewBorrowFingerprint()
	require.NoError(t, e)
	a := &BorrowSource{AccountID: 1, PoolEntryID: 5, Enabled: true, Model: "model", Secret: BorrowSourceSecret{Proxies: []string{"proxy"}, Fingerprint: f, DeviceID: f.DeviceID}}
	b := *a
	require.True(t, directionSameSource(a, &b))
	b.Secret.Proxies = []string{"changed-proxy"}
	require.False(t, directionSameSource(a, &b))
	b = *a
	b.Secret.Fingerprint, e = NewBorrowFingerprint()
	require.NoError(t, e)
	require.False(t, directionSameSource(a, &b))
	b = *a
	b.Enabled = false
	require.False(t, directionSameSource(a, &b))
	b = *a
	b.Model = "changed-model"
	require.False(t, directionSameSource(a, &b))
}

type gatewayMintStub struct {
	HTTPUpstream
	seen  []*http.Request
	mints int
}

func (s *gatewayMintStub) DoWithTLS(r *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	s.seen = append(s.seen, r)
	h := http.Header{}
	if r.Header.Get(openAICodexTurnStateHeader) == "" {
		s.mints++
		h.Set(openAICodexTurnStateHeader, fmt.Sprintf("new-ticket-%d", s.mints))
	}
	return &http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"))}, nil
}

func TestDirectionsPinnedGatewayMintsIndependentTickets(t *testing.T) {
	stub := &gatewayMintStub{}
	base, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, chatgptCodexURL, nil)
	base.Header.Set("Cookie", "__oailb=fixed-gateway-cookie")
	base.Header.Set("Authorization", "Bearer target")
	sessions := map[string]bool{}
	for i := 0; i < 3; i++ {
		trace := &directionTrace{HTTPUpstream: stub}
		got := ProbeOpenAICodexStateRoute(context.Background(), trace, base, "", 64, 2, nil, OpenAICodexStateProbeDefaultModel)
		require.Equal(t, OpenAICodexStateHealthy, got.Verdict)
		require.Empty(t, got.Failure)
		require.Len(t, trace.shots, 2)
		mint, continuation := stub.seen[2*i], stub.seen[2*i+1]
		require.Empty(t, mint.Header.Get(openAICodexTurnStateHeader))
		require.Equal(t, fmt.Sprintf("new-ticket-%d", i+1), continuation.Header.Get(openAICodexTurnStateHeader))
		for _, q := range []*http.Request{mint, continuation} {
			require.Equal(t, "__oailb=fixed-gateway-cookie", q.Header.Get("Cookie"))
			require.Equal(t, "Bearer target", q.Header.Get("Authorization"))
			session := q.Header.Get("session_id")
			require.NotEmpty(t, session)
			require.False(t, sessions[session])
			sessions[session] = true
		}
	}
	require.Equal(t, 3, stub.mints)
}

func TestDirectionsReadyScopeCannotEscapeBatchOrExpiry(t *testing.T) {
	a := BorrowAcquisition{TargetID: 63, Revision: 2, Lease: "directions-test"}
	ctx := WithBorrowAcquisition(context.Background(), a)
	require.False(t, BorrowDirectionsAllowsReady(ctx, a))
	scoped := WithBorrowDirectionsReadyWindow(ctx, a.Lease, time.Now().Add(time.Minute))
	require.True(t, BorrowDirectionsAllowsReady(scoped, a))
	acquiring := a
	acquiring.Lease = ""
	require.True(t, BorrowDirectionsAllowsReady(scoped, acquiring))
	wrong := a
	wrong.Lease = "other"
	require.False(t, BorrowDirectionsAllowsReady(scoped, wrong))
	wrong = a
	wrong.TargetID = 64
	require.False(t, BorrowDirectionsAllowsReady(scoped, wrong))
	wrong = a
	wrong.TargetID = 11
	require.False(t, BorrowDirectionsAllowsReady(scoped, wrong))
	expired := WithBorrowDirectionsReadyWindow(ctx, a.Lease, time.Now().Add(-time.Second))
	require.False(t, BorrowDirectionsAllowsReady(expired, a))
	require.False(t, BorrowDirectionsAllowsReady(WithBorrowDirectionsReadyWindow(context.Background(), a.Lease, time.Now().Add(time.Minute)), a))
}
