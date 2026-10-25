package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestBorrowRecoveryLocalStopIsNotAnUpstreamFailure(t *testing.T) {
	for _, stopped := range []error{ErrBorrowRecoveryTimeout, ErrBorrowRecoveryBudget} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		s := &OpenAIGatewayService{cfg: &config.Config{}}
		err := s.handleOpenAIUpstreamTransportError(t.Context(), c, &Account{ID: 2, Platform: PlatformOpenAI}, stopped, true)
		require.ErrorIs(t, err, stopped)
		_, observed := c.Get(OpsUpstreamErrorsKey)
		require.False(t, observed, "an unsent timeout must not be attributed to the selected fallback provider")
	}
}

func TestBorrowRecoveryDelayRechecksNewlyEnabledTarget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, r := recoveryFixture(t)
		r.ExplicitFailure()
		require.True(t, r.ReserveRecovery(ctx, 429))
		from := &Account{ID: 20, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
		selected := &Account{ID: 11, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"base_url": "https://independent.example/v1"}}
		r.SetRecoveryDelay(from, &UpstreamFailoverError{}, 32*time.Second)
		require.NoError(t, r.WaitForAccount(ctx, selected, true))
		borrow, _ := borrowFixture()
		_, err := borrow.ForwardHTTP(borrowRequest(t).WithContext(ctx), "", 11, 1, nil, func(*http.Request, string, int64, int, *tlsfingerprint.Profile) (*http.Response, error) {
			t.Fatal("newly enabled borrowed target must honor the full wait")
			return nil, nil
		})
		require.ErrorIs(t, err, ErrBorrowRecoveryTimeout)
	})
}

func TestBorrowRecoveryDelayFollowsSelectedTarget(t *testing.T) {
	for _, scenario := range []string{"independent", "same-target", "shared-provider", "provider-scope", "request-scope", "request-transient", "stop-switching", "borrowed-target", "pinned", "client-canceled"} {
		t.Run(scenario, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, r := recoveryFixture(t)
				client, cancel := context.WithCancel(r.client)
				defer cancel()
				r.client = client
				r.WroteHeaders(ctx)
				r.ExplicitFailure()
				require.True(t, r.ReserveRecovery(ctx, 429))
				from := &Account{ID: 11, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
				to := &Account{ID: 12, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"base_url": "https://independent.example/v1"}}
				failure := &UpstreamFailoverError{StatusCode: 429}
				ordinary := true
				switch scenario {
				case "same-target":
					to = from
				case "shared-provider":
					to.Credentials["base_url"] = "https://api.openai.com/v1"
				case "provider-scope":
					failure.Scope = GatewayFailureScopeProvider
				case "request-scope":
					failure.Scope = GatewayFailureScopeRequest
				case "request-transient":
					failure.RequestScopedTransient = true
				case "stop-switching":
					failure.NextAccountAction = NextAccountStop
				case "borrowed-target":
					ordinary = false
				case "pinned":
					require.NoError(t, r.Activate(from.ID, true))
				}
				r.SetRecoveryDelay(from, failure, 32*time.Second)
				if scenario == "client-canceled" {
					cancel()
				}
				start := time.Now()
				err := r.WaitForAccount(context.WithoutCancel(ctx), to, ordinary)
				if scenario == "client-canceled" {
					require.ErrorIs(t, err, context.Canceled)
				} else if scenario == "independent" {
					require.NoError(t, err)
					require.Less(t, time.Since(start), time.Second)
				} else if scenario == "pinned" {
					require.ErrorIs(t, err, ErrBorrowUnavailable)
				} else {
					require.ErrorIs(t, err, ErrBorrowRecoveryTimeout)
					require.Equal(t, 30*time.Second, time.Since(start))
				}
				require.Equal(t, 1, r.transports, "waiting never spends a send attempt")
			})
		})
	}
}

func TestBorrowRecoveryDelaySameTargetHonorsRetryAfter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, r := recoveryFixture(t)
		r.ExplicitFailure()
		require.True(t, r.ReserveRecovery(ctx, http.StatusTooManyRequests))
		a := &Account{ID: 11, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
		r.SetRecoveryDelay(a, &UpstreamFailoverError{}, 8*time.Second)
		r.SetFallback(a.ID)
		require.Equal(t, a.ID, r.TakeFallback())
		start := time.Now()
		require.NoError(t, r.WaitForAccount(ctx, a, false))
		require.Equal(t, 8*time.Second, time.Since(start))
		require.NoError(t, r.BeginTransport(ctx))
		r.WroteHeaders(ctx)
		time.Sleep(40 * time.Second)
		require.NoError(t, r.Err(), "a sent recovery is no longer governed by the wait deadline")
	})
}

func TestBorrowRecoveryLocalCooldownDoesNotBlockIndependentOAuthIdentity(t *testing.T) {
	for _, scenario := range []string{"independent", "same_identity", "explicit_wait", "unknown_identity"} {
		t.Run(scenario, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, r := recoveryFixture(t)
				r.ExplicitFailure()
				require.True(t, r.ReserveRecovery(ctx, 429))
				a := &Account{ID: 11, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"chatgpt_account_id": "subject-a"}}
				b := &Account{ID: 12, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"chatgpt_account_id": "subject-b"}}
				f := &UpstreamFailoverError{StatusCode: 429}
				switch scenario {
				case "same_identity":
					b.Credentials = a.Credentials
				case "explicit_wait":
					f.ResponseHeaders = http.Header{"Retry-After": []string{"32"}}
				case "unknown_identity":
					b.Credentials = nil
				}
				r.SetRecoveryDelay(a, f, 32*time.Second)
				err := r.WaitForAccount(ctx, b, false)
				if scenario == "independent" {
					require.NoError(t, err)
				} else {
					require.ErrorIs(t, err, ErrBorrowRecoveryTimeout)
				}
			})
		})
	}
}
