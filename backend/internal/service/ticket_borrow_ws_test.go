package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type borrowWSCaptureDialer struct {
	mu             sync.Mutex
	headers        []http.Header
	proxies        []string
	conns          []*openAIWSCaptureConn
	responseHeader http.Header
	events         [][]byte
	dialErr        error
	status         int
}

func (d *borrowWSCaptureDialer) Dial(_ context.Context, _ string, h http.Header, proxy string) (openAIWSClientConn, int, http.Header, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	events := d.events
	if events == nil {
		events = [][]byte{[]byte(`{"type":"response.completed","response":{"id":"resp_ws_borrow"}}`)}
	}
	c := &openAIWSCaptureConn{events: append([][]byte(nil), events...)}
	d.headers = append(d.headers, h.Clone())
	d.proxies = append(d.proxies, proxy)
	d.conns = append(d.conns, c)
	if d.dialErr != nil {
		return nil, d.status, d.responseHeader, d.dialErr
	}
	return c, 101, d.responseHeader, nil
}
func borrowWSPoolFixture(t *testing.T) (*openAIWSConnPool, *TicketBorrowService, *borrowTestRepo, *borrowWSCaptureDialer, openAIWSAcquireRequest) {
	t.Helper()
	service, repo := borrowFixture()
	cfg := newOpenAIWSV2TestConfig()
	cfg.Gateway.OpenAIWS.MinIdlePerAccount = 0
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 4
	cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 4
	p := newOpenAIWSConnPool(cfg)
	p.borrowRoutes = service
	t.Cleanup(p.Close)
	d := &borrowWSCaptureDialer{}
	p.setClientDialerForTest(d)
	account := service.owner.accountRepo.(borrowTestAccounts).account
	account.Concurrency = 4
	req := openAIWSAcquireRequest{Account: account, Model: "gpt-6-sol", WSURL: strings.Replace(chatgptCodexURL, "https:", "wss:", 1), Headers: http.Header{"Authorization": {"Bearer target-secret"}, "User-Agent": {"normal-business-agent"}, "Cookie": {"other=keep"}}}
	return p, service, repo, d, req
}
func TestBorrowWSUsesNormalProtocolChoice(t *testing.T) {
	borrow, _ := borrowFixture()
	account := wsSSETestAccount()
	cfg := newOpenAIWSV2TestConfig()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
	svc := &OpenAIGatewayService{cfg: cfg}
	for _, enabled := range []bool{false, true} {
		account.Extra["openai_oauth_ws_sse_acceleration"] = enabled
		decision := NewOpenAIWSProtocolResolver(cfg).Resolve(account)
		normal := svc.resolveOpenAIHTTPWSSSEDecision(c, account, []byte(wsSSETestRequest), decision)
		svc.borrowRoutes = borrow
		borrowed := svc.resolveOpenAIHTTPWSSSEDecision(c, account, []byte(wsSSETestRequest), decision)
		require.Equal(t, normal, borrowed)
		svc.borrowRoutes = nil
	}
}
func TestBorrowWSPreservesPayloadAndDrainsExpiredRoute(t *testing.T) {
	p, _, repo, d, req := borrowWSPoolFixture(t)
	req.HeadersFactory = func(_ context.Context, h http.Header) (http.Header, error) {
		h.Set("Cookie", "other=keep; __oailb=old")
		return h, nil
	}
	lease, err := p.Acquire(t.Context(), req)
	require.NoError(t, err)
	defer lease.Release()
	require.Contains(t, d.headers[0].Get("Cookie"), "other=keep")
	require.Contains(t, d.headers[0].Get("Cookie"), "__oailb=donor-cookie")
	require.NotContains(t, d.headers[0].Get("Cookie"), "__oailb=old")
	require.Equal(t, req.Headers.Get("Authorization"), d.headers[0].Get("Authorization"))
	require.Equal(t, repo.route.Secret.Fingerprint.UserAgent, d.headers[0].Get("User-Agent"))
	require.Empty(t, d.proxies[0])
	payload := json.RawMessage(`{"type":"response.create","model":"gpt-6-sol","reasoning":{"effort":"medium"},"input":"candy","stream":true,"prompt_cache_key":"preserve"}`)
	require.NoError(t, lease.WriteJSONContext(t.Context(), payload))
	got, _ := json.Marshal(d.conns[0].lastWrite)
	require.JSONEq(t, string(payload), string(got))
	repo.mu.Lock()
	repo.now = repo.route.ExpiresAt
	repo.mu.Unlock()
	raw, err := lease.ReadMessageContext(t.Context())
	require.NoError(t, err)
	require.Contains(t, string(raw), "response.completed")
	require.ErrorIs(t, lease.WriteJSONContext(t.Context(), payload), ErrBorrowUnavailable)
	lease.Release()
	require.Zero(t, activeBorrowLeases(repo))
	_, err = p.Acquire(t.Context(), req)
	require.ErrorIs(t, err, ErrBorrowUnavailable)
}
func TestBorrowWSReusesOnlyTheSameRoute(t *testing.T) {
	p, _, repo, d, req := borrowWSPoolFixture(t)
	req.Headers.Set("session_id", "parity-ws-session")
	first, err := p.Acquire(t.Context(), req)
	require.NoError(t, err)
	id := first.ConnID()
	first.Release()
	second, err := p.Acquire(t.Context(), req)
	require.NoError(t, err)
	require.True(t, second.Reused())
	require.Equal(t, id, second.ConnID())
	second.Release()
	repo.mu.Lock()
	route := *repo.route
	route.ID = "route-b"
	route.Secret.Cookie.Value = "second-cookie"
	repo.route = &route
	repo.sessions = map[string]string{borrowSessionKey(req.Headers): "route-a"}
	repo.mu.Unlock()
	third, err := p.Acquire(t.Context(), req)
	require.NoError(t, err)
	require.False(t, third.Reused())
	require.NotEqual(t, id, third.ConnID())
	third.Release()
	require.Len(t, d.headers, 2)
	require.Contains(t, d.headers[1].Get("Cookie"), "second-cookie")
	repo.mu.Lock()
	repo.enabled = false
	repo.mu.Unlock()
	normal, err := p.Acquire(t.Context(), req)
	require.NoError(t, err)
	require.False(t, normal.Reused())
	normal.Release()
	require.Equal(t, "other=keep", d.headers[2].Get("Cookie"))
	require.Zero(t, activeBorrowLeases(repo))
}
func TestBorrowWSNativeContinuationRetainsPreviousResponse(t *testing.T) {
	p, _, repo, d, req := borrowWSPoolFixture(t)
	lease, err := p.Acquire(t.Context(), req)
	require.NoError(t, err)
	defer lease.Release()
	_, err = lease.ReadMessageContext(t.Context())
	require.NoError(t, err)
	payload := json.RawMessage(`{"type":"response.create","model":"gpt-6-astra","previous_response_id":"resp_ws_borrow","input":"continue"}`)
	require.NoError(t, lease.WriteJSONWithContextTimeout(t.Context(), payload, time.Second))
	require.Equal(t, "resp_ws_borrow", d.conns[0].lastWrite["previous_response_id"])
	require.ErrorIs(t, lease.WriteJSONContext(t.Context(), json.RawMessage(`{"type":"response.create","model":"gpt-6-astra","previous_response_id":"resp_unknown"}`)), ErrBorrowUnavailable)
	lease.Release()
	require.Zero(t, activeBorrowLeases(repo))
}
func TestBorrowWSAcquisitionDoesNotOwnBusinessLifetime(t *testing.T) {
	p, _, repo, d, req := borrowWSPoolFixture(t)
	business, cancelBusiness := context.WithCancel(t.Context())
	defer cancelBusiness()
	acquire, cancelAcquire := context.WithTimeout(business, time.Second)
	req.LifetimeContext = business
	lease, err := p.Acquire(acquire, req)
	require.NoError(t, err)
	defer lease.Release()
	cancelAcquire()
	require.NoError(t, lease.borrow.use.ctx.Err(), "returning a socket ends acquisition, not the business turn")
	require.NoError(t, lease.WriteJSONContext(business, json.RawMessage(`{"type":"response.create","model":"gpt-6-sol","input":"hello"}`)))
	raw, err := lease.ReadMessageContext(business)
	require.NoError(t, err)
	require.Contains(t, string(raw), "response.completed")
	ap := p.getOrCreateAccountPool(req.Account.ID)
	ap.mu.Lock()
	cachedLifetime := ap.lastAcquire.LifetimeContext
	ap.mu.Unlock()
	require.Nil(t, cachedLifetime, "pool/prewarm snapshots must not retain request lifetimes")
	cancelBusiness()
	require.Eventually(t, func() bool { return activeBorrowLeases(repo) == 0 }, time.Second, time.Millisecond)
	d.conns[0].mu.Lock()
	closed := d.conns[0].closed
	d.conns[0].mu.Unlock()
	require.True(t, closed)
}

func TestBorrowWSAcquisitionStillHonorsItsOwnDeadline(t *testing.T) {
	p, _, repo, _, req := borrowWSPoolFixture(t)
	req.LifetimeContext = t.Context()
	req.HeadersFactory = func(ctx context.Context, h http.Header) (http.Header, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	acquire, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	lease, err := p.Acquire(acquire, req)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Nil(t, lease)
	require.Zero(t, activeBorrowLeases(repo))
}

func TestBorrowWSEnablingTargetRejectsExistingPlainSocket(t *testing.T) {
	p, _, repo, d, req := borrowWSPoolFixture(t)
	repo.enabled = false
	lease, err := p.Acquire(t.Context(), req)
	require.NoError(t, err)
	defer lease.Release()
	repo.mu.Lock()
	repo.enabled = true
	repo.mu.Unlock()
	require.ErrorIs(t, lease.WriteJSONContext(t.Context(), json.RawMessage(`{"type":"response.create","model":"gpt-6-sol"}`)), ErrBorrowUnavailable)
	require.Empty(t, d.conns[0].writes)
}

func TestBorrowWSUnauthorizedHandshakeRevokesRoute(t *testing.T) {
	p, _, repo, d, req := borrowWSPoolFixture(t)
	d.status, d.dialErr = http.StatusUnauthorized, errors.New("unauthorized handshake")
	_, err := p.Acquire(t.Context(), req)
	var dialErr *openAIWSDialError
	require.ErrorAs(t, err, &dialErr, "ordinary handshake error handling must retain upstream status")
	require.Equal(t, http.StatusUnauthorized, dialErr.StatusCode)
	require.Equal(t, []string{"route-a"}, repo.revoked)
	require.Zero(t, activeBorrowLeases(repo))
}

func TestBorrowWSChangedHandshakeKeepsRouteUntilExpiry(t *testing.T) {
	for _, change := range []string{"cookie", "expired-cookie", "deleted-cookie", "shortened-cookie"} {
		t.Run(change, func(t *testing.T) {
			p, _, repo, dialer, req := borrowWSPoolFixture(t)
			dialer.responseHeader = borrowChangedResponseHeaders(change)
			first, err := p.Acquire(t.Context(), req)
			require.NoError(t, err, "a changed handshake Cookie must not close a usable socket")
			defer first.Release()
			second, err := p.Acquire(t.Context(), req)
			require.NoError(t, err, "another request can still reserve the unexpired route")
			defer second.Release()
			require.Len(t, dialer.headers, 2)
			require.Empty(t, repo.revoked)
			payload := json.RawMessage(`{"type":"response.create","model":"gpt-6-sol","input":"hello"}`)
			require.NoError(t, first.WriteJSONContext(t.Context(), payload))
			require.NoError(t, second.WriteJSONContext(t.Context(), payload))
			repo.mu.Lock()
			repo.now = repo.route.ExpiresAt
			repo.mu.Unlock()
			require.Equal(t, 2, activeBorrowLeases(repo))
			for _, lease := range []*openAIWSConnLease{first, second} {
				raw, err := lease.ReadMessageContext(t.Context())
				require.NoError(t, err)
				require.Contains(t, string(raw), "response.completed")
				require.ErrorIs(t, lease.WriteJSONContext(t.Context(), payload), ErrBorrowUnavailable)
				lease.Release()
			}
			require.Zero(t, activeBorrowLeases(repo))
		})
	}
}

func TestBorrowWSNormalGatewayStreamsAndRecordsUsage(t *testing.T) {
	p, borrow, repo, dialer, req := borrowWSPoolFixture(t)
	req.Account.Extra = map[string]any{
		"openai_oauth_responses_websockets_v2_enabled": true,
		"openai_oauth_ws_sse_acceleration":             true,
	}
	dialer.events = [][]byte{
		[]byte(`{"type":"response.created","response":{"id":"resp_ws_borrow","model":"gpt-6-sol"}}`),
		[]byte(`{"type":"response.output_text.delta","delta":"21"}`),
		[]byte(`{"type":"response.completed","response":{"id":"resp_ws_borrow","model":"gpt-6-sol","usage":{"input_tokens":7,"output_tokens":3}}}`),
	}
	upstream := &httpUpstreamRecorder{}
	gateway := &OpenAIGatewayService{cfg: p.cfg, accountRepo: borrow.owner.accountRepo,
		borrowRoutes: borrow, openaiWSPool: p, httpUpstream: upstream, cache: &stubGatewayCache{},
		openaiWSResolver: NewOpenAIWSProtocolResolver(p.cfg), toolCorrector: NewCodexToolCorrector()}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
	result, err := gateway.Forward(t.Context(), c, req.Account, []byte(`{"model":"gpt-6-sol","reasoning":{"effort":"medium"},"stream":true,"instructions":"test","input":"candy"}`))
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.OpenAIWSMode)
	require.Equal(t, 7, result.Usage.InputTokens)
	require.Equal(t, 3, result.Usage.OutputTokens)
	require.Contains(t, w.Body.String(), "21")
	require.Contains(t, w.Body.String(), "response.completed")
	require.Nil(t, upstream.lastReq, "the ordinary WS acceleration choice must be retained")
	require.Len(t, dialer.headers, 1)
	require.Contains(t, dialer.headers[0].Get("Cookie"), "__oailb=donor-cookie")
	require.Equal(t, "Bearer target-secret", dialer.headers[0].Get("Authorization"))
	require.Zero(t, activeBorrowLeases(repo))
}
