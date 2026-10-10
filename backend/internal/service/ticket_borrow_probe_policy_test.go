package service

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

func TestTicketBorrowRenewalWindowKeepsServingUntilExpiry(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		remaining time.Duration
		window    int
		want      bool
	}{
		{16 * time.Second, 15, false}, {15 * time.Second, 15, true}, {time.Millisecond, 15, true},
		{time.Second, 0, false}, {0, 0, true}, {-time.Second, 15, true}, {45 * time.Second, 60, true},
	} {
		expires := now.Add(tc.remaining)
		target := BorrowTarget{Ready: true, ExpiresAt: &expires, ServerNow: now, ProbePolicy: BorrowProbePolicy{Concurrency: 3, RenewBeforeSeconds: tc.window}}
		require.Equal(t, tc.want, borrowTargetNeedsProbe(target))
		s, r := borrowFixture()
		r.route.ExpiresAt = r.now.Add(tc.remaining)
		err := s.Check(t.Context(), 11, "gpt-6-astra", "http")
		if tc.remaining > 0 {
			require.NoError(t, err)
		} else {
			require.ErrorIs(t, err, ErrBorrowUnavailable)
		}
	}
	require.True(t, borrowTargetNeedsProbe(BorrowTarget{}))
	require.Equal(t, DefaultBorrowProbePolicy(), (BorrowProbePolicy{}).effective())
}

type parallelBorrowUpstream struct {
	HTTPUpstream
	started chan struct{}
	release chan struct{}
}

func (u *parallelBorrowUpstream) DoWithTLS(req *http.Request, _ string, id int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	h := http.Header{}
	if id != 11 {
		cookie := "x." + base64.RawURLEncoding.EncodeToString([]byte(`{"host":"chat.gateway.unified-`+strconv.FormatInt(id, 10)+`.api.openai.com"}`)) + ".x"
		h.Set("Set-Cookie", "__oailb="+cookie+"; Max-Age=230")
	} else if req.Header.Get(openAICodexTurnStateHeader) == "" {
		u.started <- struct{}{}
		select {
		case <-u.release:
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
		h.Set(openAICodexTurnStateHeader, "verified-ticket")
	}
	return &http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(strings.NewReader(stateProbeCompletedStream))}, nil
}

func TestTicketBorrowParallelProbesDoNotBlockBusiness(t *testing.T) {
	for _, concurrency := range []int{0, 1, 2, 3} {
		t.Run(strconv.Itoa(concurrency), func(t *testing.T) {
			s, base := borrowFixture()
			base.route.ExpiresAt = base.now.Add(10 * time.Second)
			oldID := base.route.ID
			repo := &observationWorkerRepo{borrowWorkerRepo: &borrowWorkerRepo{borrowTestRepo: base}}
			s.repo = repo
			slots := &normalBorrowSlot{allow: false}
			s.owner.openaiGatewayService = &OpenAIGatewayService{accountRepo: s.owner.accountRepo, concurrencyService: &ConcurrencyService{cache: slots}}
			s.owner.accountRepo.(borrowTestAccounts).account.Concurrency = 1
			up := &parallelBorrowUpstream{started: make(chan struct{}, 3), release: make(chan struct{})}
			s.owner.httpUpstream = up
			done := make(chan struct{})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			defer func() { cancel(); <-done }()
			go func() {
				defer close(done)
				s.runTarget(ctx, BorrowTarget{AccountID: 11, Revision: 2, ProbePolicy: BorrowProbePolicy{Concurrency: concurrency, RenewBeforeSeconds: 15}}, BorrowSettings{Running: true, TTLSeconds: 230}, "parallel")
			}()
			expectedProbes := concurrency
			if expectedProbes == 0 {
				expectedProbes = 2
			}
			for range expectedProbes {
				select {
				case <-up.started:
				case <-time.After(3 * time.Second):
					t.Fatal("configured probes did not start concurrently")
				}
			}
			// All probe slots are occupied, and the business slot limit is only one.
			resp, err := s.ForwardHTTP(borrowRequest(t), "", 11, 1, nil, func(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
				require.NoError(t, BorrowBeforeSend(req))
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: http.NoBody}, nil
			})
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			require.Zero(t, slots.acquired, "probes must never reserve business slots")
			close(up.release)
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("probe batch failed to drain")
			}
			require.NotEqual(t, oldID, base.route.ID)
			require.Empty(t, base.revoked, "renewal must not revoke the serving route")
			for _, event := range base.events {
				require.NotEqual(t, "business", event.Stage)
			}
		})
	}
}

func TestTicketBorrowBusinessUsesConfiguredTargetProxyOrDirect(t *testing.T) {
	for _, configured := range []bool{false, true} {
		t.Run(strconv.FormatBool(configured), func(t *testing.T) {
			s, r := borrowFixture()
			a := s.owner.accountRepo.(borrowTestAccounts).account
			expected := ""
			if configured {
				proxyID := int64(42)
				a.ProxyID = &proxyID
				a.Proxy = &Proxy{Protocol: "http", Host: "target.proxy.test", Port: 8080}
				expected = a.Proxy.URL()
			}
			r.route.IdentityHash = s.targetIdentity(a)
			r.route.Secret.Proxy = "http://borrow-only.proxy.test:9999"
			resp, err := s.ForwardHTTP(borrowRequest(t), "http://incoming.proxy.test:7777", 11, 1, nil, func(req *http.Request, proxy string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
				require.Equal(t, expected, proxy)
				require.Equal(t, HTTPUpstreamProfileOpenAIHarvest, HTTPUpstreamProfileFromContext(req.Context()))
				require.NoError(t, BorrowTargetRequestConnected(req))
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(stateProbeCompletedStream))}, nil
			})
			require.NoError(t, err)
			_, err = io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			require.Empty(t, r.events)
		})
	}
}
