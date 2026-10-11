package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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

type borrowTestAccounts struct {
	AccountRepository
	account *Account
}

func (a borrowTestAccounts) GetByID(_ context.Context, id int64) (*Account, error) {
	if id == a.account.ID {
		return a.account, nil
	}
	return &Account{ID: id, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Credentials: map[string]any{"access_token": "source-token", "chatgpt_account_id": "source"}}, nil
}

func boundBorrowTestSource(source BorrowSource) BorrowSource {
	if source.AccountID == 0 {
		source.AccountID = 20 + source.ID
	}
	if source.Model == "" {
		source.Model = "gpt-6-luna"
	}
	if source.UseProxyPool {
		return source
	}
	source.UseProxyPool, source.RotateIdentity, source.PoolEntryID = true, false, 7
	f, err := NewBorrowFingerprint()
	if err != nil {
		panic(err)
	}
	source.Secret = BorrowSourceSecret{Proxies: []string{"http://pool.example:80"}, DeviceID: f.DeviceID, Fingerprint: f, Profile: f.Profile}
	return source
}

type borrowTestRepo struct {
	TicketBorrowRepository
	mu           sync.Mutex
	now          time.Time
	minRemaining time.Duration
	enabled      bool
	unavailable  bool
	route        *BorrowRoute
	revoked      []string
	sessions     map[string]string
	oldRoutes    map[string]*BorrowRoute
	events       []BorrowEvent
	leases       map[string]string
	releases     int
	renewals     int
	renewError   error
}

func (r *borrowTestRepo) Targets(ctx context.Context) ([]BorrowTarget, error) {
	t, err := r.Target(ctx, 11)
	if err != nil || !t.Enabled {
		return nil, err
	}
	return []BorrowTarget{t}, nil
}

func (r *borrowTestRepo) Target(_ context.Context, id int64) (BorrowTarget, error) {
	if id != 11 {
		return BorrowTarget{AccountID: id}, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.unavailable {
		return BorrowTarget{}, errors.New("storage down")
	}
	return BorrowTarget{AcquisitionIdentity: BorrowAcquisitionSource, AccountID: 11, Enabled: r.enabled, Revision: 2, ServerNow: r.now}, nil
}
func (r *borrowTestRepo) Route(_ context.Context, id int64, identity, preferred string) (*BorrowRoute, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.routeLocked(id, identity, preferred)
}
func (r *borrowTestRepo) routeLocked(id int64, identity, preferred string) (*BorrowRoute, error) {
	route := r.route
	if preferred != "" && r.oldRoutes[preferred] != nil {
		route = r.oldRoutes[preferred]
	}
	if r.unavailable || !r.enabled || route == nil || route.TargetID != id || route.IdentityHash != identity || !r.now.Add(r.minRemaining).Before(route.ExpiresAt) || (preferred != "" && preferred != route.ID) {
		return nil, ErrBorrowUnavailable
	}
	copy := *route
	copy.UsableUntil = copy.ExpiresAt.Add(-r.minRemaining)
	return &copy, nil
}
func (r *borrowTestRepo) ReserveRoute(_ context.Context, id int64, identity, preferred, session, lease string) (*BorrowRoute, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if bound := r.sessions[session]; session != "" && bound != "" {
		if preferred != "" && preferred != bound {
			return nil, ErrBorrowUnavailable
		}
		preferred = bound
	}
	route, err := r.routeLocked(id, identity, preferred)
	if err != nil {
		return nil, err
	}
	if session != "" {
		if r.sessions == nil {
			r.sessions = map[string]string{}
		}
		r.sessions[session] = route.ID
	}
	if r.leases == nil {
		r.leases = map[string]string{}
	}
	r.leases[lease] = route.ID
	return route, nil
}
func (r *borrowTestRepo) RenewRoute(_ context.Context, lease string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.renewals++
	if r.renewError != nil {
		return r.renewError
	}
	if r.leases[lease] == "" {
		return ErrBorrowUnavailable
	}
	return nil
}
func (r *borrowTestRepo) ReleaseRoute(_ context.Context, lease string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.releases++
	delete(r.leases, lease)
	return nil
}
func (r *borrowTestRepo) RouteStatuses(context.Context, int64, string) ([]BorrowRouteStatus, error) {
	return nil, nil
}
func (r *borrowTestRepo) Revoke(_ context.Context, _ int64, id, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.revoked = append(r.revoked, id)
	if r.route != nil && r.route.ID == id {
		r.route = nil
	}
	return nil
}
func (r *borrowTestRepo) RefreshTarget(context.Context, int64) error { return nil }
func (r *borrowTestRepo) Event(_ context.Context, e BorrowEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
	return nil
}
func (r *borrowTestRepo) LookupSession(_ context.Context, _ int64, key string) (string, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.sessions[key]
	return v, ok, nil
}
func (r *borrowTestRepo) BindSession(_ context.Context, _ int64, key, id string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sessions == nil {
		r.sessions = map[string]string{}
	}
	if old := r.sessions[key]; old != "" {
		return old, nil
	}
	r.sessions[key] = id
	return id, nil
}
func borrowFixture() (*TicketBorrowService, *borrowTestRepo) {
	a := &Account{ID: 11, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Credentials: map[string]any{"access_token": "target-secret", "chatgpt_account_id": "target"}}
	r := &borrowTestRepo{now: time.Now(), enabled: true}
	r.route = &BorrowRoute{ID: "route-a", TargetID: 11, TargetRevision: 2, IdentityHash: borrowAccountIdentity(a), ExpiresAt: r.now.Add(time.Minute), Secret: BorrowRouteSecret{Endpoint: chatgptCodexURL, Cookie: http.Cookie{Name: "__oailb", Value: "donor-cookie"}, Headers: http.Header{"Authorization": []string{"Bearer target-secret"}}}}
	accounts := borrowTestAccounts{account: a}
	return NewTicketBorrowService(r, &AccountTestService{accountRepo: accounts, openaiGatewayService: &OpenAIGatewayService{accountRepo: accounts}}, nil), r
}
func borrowRequest(t *testing.T) *http.Request {
	t.Helper()
	req, e := http.NewRequestWithContext(t.Context(), "POST", chatgptCodexURL, strings.NewReader(`{"model":"gpt-6-astra","input":"hello"}`))
	require.NoError(t, e)
	req.Header.Set("Authorization", "Bearer target-secret")
	req.Header.Set("X-Codex-Turn-State", "conversation-ticket")
	return req
}
func TestTicketBorrowExpiryAllowsLastMillisecondWithZeroFloor(t *testing.T) {
	for _, remaining := range []time.Duration{15 * time.Second, time.Second, time.Millisecond, 0, -time.Millisecond} {
		t.Run(remaining.String(), func(t *testing.T) {
			s, r := borrowFixture()
			r.route.ExpiresAt = r.now.Add(remaining)
			calls := 0
			_, err := s.ForwardHTTP(borrowRequest(t), "", 11, 10, nil, func(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
				if err := BorrowBeforeSend(req); err != nil {
					return nil, err
				}
				calls++
				require.Equal(t, "Bearer target-secret", req.Header.Get("Authorization"))
				require.Equal(t, "conversation-ticket", req.Header.Get("X-Codex-Turn-State"))
				cookie, e := req.Cookie("__oailb")
				require.NoError(t, e)
				require.Equal(t, "donor-cookie", cookie.Value)
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: http.NoBody}, nil
			})
			if remaining > 0 {
				require.NoError(t, err)
				require.Equal(t, 1, calls)
			} else {
				require.ErrorIs(t, err, ErrBorrowUnavailable)
				require.Zero(t, calls)
			}
		})
	}
}
func TestTicketBorrowRechecksAfterQueueAndDoesNotInterruptStartedStream(t *testing.T) {
	s, r := borrowFixture()
	req := borrowRequest(t)
	_, err := s.ForwardHTTP(req, "", 11, 1, nil, func(sent *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
		r.now = r.route.ExpiresAt
		return nil, BorrowBeforeSend(sent)
	})
	require.ErrorIs(t, err, ErrBorrowUnavailable)
	s, r = borrowFixture()
	resp, err := s.ForwardHTTP(req, "", 11, 1, nil, func(sent *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
		require.NoError(t, BorrowBeforeSend(sent))
		r.now = r.route.ExpiresAt
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: http.NoBody}, nil
	})
	require.NoError(t, err)
	require.NotNil(t, resp)
}
func TestTicketBorrowConcurrentConversationsShareRouteNotTickets(t *testing.T) {
	s, _ := borrowFixture()
	var calls atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := borrowRequest(t)
			ticket := time.Now().String()
			req.Header.Set("X-Codex-Turn-State", ticket)
			_, e := s.ForwardHTTP(req, "", 11, 10, nil, func(sent *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
				require.Equal(t, ticket, sent.Header.Get("X-Codex-Turn-State"))
				calls.Add(1)
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: http.NoBody}, nil
			})
			require.NoError(t, e)
		}()
	}
	wg.Wait()
	require.EqualValues(t, 10, calls.Load())
}
func TestTicketBorrowBusinessFailureRevokesOnlyItsBinding(t *testing.T) {
	s, r := borrowFixture()
	_, e := s.ForwardHTTP(borrowRequest(t), "", 11, 1, nil, func(*http.Request, string, int64, int, *tlsfingerprint.Profile) (*http.Response, error) {
		copy := *r.route
		copy.ID = "route-b"
		r.route = &copy
		return &http.Response{StatusCode: 200, Header: http.Header{"X-Codex-Turn-State": []string{"changed"}}, Body: http.NoBody}, nil
	})
	require.ErrorIs(t, e, ErrBorrowResponseRejected)
	require.False(t, IsBorrowRouteUnavailable(e), "a response identity change is not a pre-send route rejection")
	require.Equal(t, []string{"route-a"}, r.revoked)
	require.NotNil(t, r.route)
	require.Equal(t, "route-b", r.route.ID)
}
func TestTicketBorrowFirstTicketAndTransientErrorsDoNotRevoke(t *testing.T) {
	s, r := borrowFixture()
	req := borrowRequest(t)
	req.Header.Del("X-Codex-Turn-State")
	_, e := s.ForwardHTTP(req, "", 11, 1, nil, func(*http.Request, string, int64, int, *tlsfingerprint.Profile) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"X-Codex-Turn-State": []string{"first-ticket"}}, Body: http.NoBody}, nil
	})
	require.NoError(t, e)
	require.Empty(t, r.revoked)
	_, e = s.ForwardHTTP(req, "", 11, 1, nil, func(*http.Request, string, int64, int, *tlsfingerprint.Profile) (*http.Response, error) {
		return nil, errors.New("timeout")
	})
	require.Error(t, e)
	require.Empty(t, r.revoked)
}
func TestTicketBorrowCannotBypassWithProbeOrClientHeader(t *testing.T) {
	s, r := borrowFixture()
	r.route = nil
	req := borrowRequest(t)
	req.Header.Set("X-Borrow-Internal", "true")
	req = req.WithContext(context.WithValue(req.Context(), openAICodexStateProbeContextKey{}, true))
	_, e := s.ForwardHTTP(req, "", 11, 1, nil, func(*http.Request, string, int64, int, *tlsfingerprint.Profile) (*http.Response, error) {
		t.Fatal("must not send")
		return nil, nil
	})
	require.ErrorIs(t, e, ErrBorrowUnavailable)
	r.unavailable = true
	err := s.Check(t.Context(), 11, "gpt-6-astra", "http")
	require.True(t, IsOpenAITurnAdmissionError(err))
	require.False(t, IsBorrowRouteUnavailable(err), "storage failures do not prove a missing route")
}
func TestTicketBorrowRedactsAllStoredSecrets(t *testing.T) {
	s := BorrowSource{Name: "source", Secret: BorrowSourceSecret{Credentials: map[string]any{"access_token": "SECRET"}, Proxies: []string{"http://u:PASSWORD@proxy"}}}
	b, e := json.Marshal(s)
	require.NoError(t, e)
	require.NotContains(t, string(b), "SECRET")
	require.NotContains(t, string(b), "PASSWORD")
}

func TestTicketBorrowKeepsSessionAndResponseOnOldValidRoute(t *testing.T) {
	s, r := borrowFixture()
	req := borrowRequest(t)
	req.Header.Set("session_id", "session-one")
	send := func(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_one\",\"status\":\"completed\"}}\n\n"))}, nil
	}
	resp, err := s.ForwardHTTP(req, "", 11, 10, nil, send)
	require.NoError(t, err)
	_, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Empty(t, r.events, "business completion does not create borrowing records")
	original := r.route
	r.oldRoutes = map[string]*BorrowRoute{original.ID: original}
	replacement := *original
	replacement.ID = "replacement"
	replacement.Secret.Cookie.Value = "replacement-cookie"
	r.route = &replacement
	req, _ = http.NewRequestWithContext(t.Context(), "POST", chatgptCodexURL, strings.NewReader(`{"model":"gpt-6-astra","previous_response_id":"resp_one"}`))
	_, err = s.ForwardHTTP(req, "", 11, 10, nil, func(sent *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
		c, e := sent.Cookie("__oailb")
		require.NoError(t, e)
		require.Equal(t, "donor-cookie", c.Value)
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: http.NoBody}, nil
	})
	require.NoError(t, err)
	original.ExpiresAt = r.now
	_, err = s.ForwardHTTP(req, "", 11, 10, nil, func(*http.Request, string, int64, int, *tlsfingerprint.Profile) (*http.Response, error) {
		t.Fatal("expired continuation must not move to replacement")
		return nil, nil
	})
	require.ErrorIs(t, err, ErrBorrowUnavailable)
	require.NoError(t, s.Check(t.Context(), 11, "gpt-6-astra", "http"))
}
func TestTicketBorrowCredentialRefreshAndOrdinaryConstraints(t *testing.T) {
	s, _ := borrowFixture()
	a := s.owner.accountRepo.(borrowTestAccounts).account
	a.Credentials["access_token"] = "refreshed"
	require.NoError(t, s.Check(t.Context(), 11, "gpt-6-astra", "http"))
	a.Schedulable = false
	require.ErrorIs(t, s.Check(t.Context(), 11, "gpt-6-astra", "http"), ErrBorrowUnavailable)
	a.Schedulable = true
	require.NoError(t, s.Check(t.Context(), 11, "gpt-6-astra", "ws"))
	require.NoError(t, s.Check(t.Context(), 11, "gpt-6-luna", "http"), "model availability is governed by ordinary account selection")
	a.Credentials["chatgpt_account_id"] = "other"
	require.ErrorIs(t, s.Check(t.Context(), 11, "gpt-6-astra", "http"), ErrBorrowUnavailable)
}
func TestTicketBorrowCookieLifetimeAndRetryAfter(t *testing.T) {
	receipt := time.Now()
	now := receipt.Add(20 * time.Second)
	cookie := &http.Cookie{Expires: receipt.Add(100 * time.Second)}
	require.Equal(t, 80*time.Second, borrowCookieTTL(cookie, receipt, now, 230))
	cookie.MaxAge = 50
	require.Equal(t, 30*time.Second, borrowCookieTTL(cookie, receipt, now, 230))
	require.Equal(t, 120*time.Second, borrowRetryAfter("120", now))
	require.Equal(t, time.Minute, borrowRetryAfter("invalid", now))
}
func TestTicketBorrowInterruptedBodyNeverCountsCompleted(t *testing.T) {
	s, r := borrowFixture()
	body := &borrowObservedBody{ReadCloser: io.NopCloser(strings.NewReader("data: {\"type\":\"response.created\",\"response\":{\"id\":\"abc\"}}\n\n")), owner: s, target: 11, route: "route-a"}
	_, err := io.ReadAll(body)
	require.NoError(t, err)
	require.NoError(t, body.Close())
	require.Empty(t, r.events, "incomplete business streams do not create borrowing records")
}

type borrowWorkerRepo struct {
	environmentError error
	environmentCheck func() error
	*borrowTestRepo
	source       BorrowSource
	environments map[string]*BorrowCandidateEnvironment
}

func (r *borrowWorkerRepo) Sources(context.Context) ([]BorrowSource, error) {
	return []BorrowSource{r.source}, nil
}
func (r *borrowWorkerRepo) Source(context.Context, int64) (*BorrowSource, error) {
	copy := r.source
	copy.Cursor = 1
	return &copy, nil
}
func (r *borrowWorkerRepo) AcquireJob(context.Context, string, int64, string) (bool, error) {
	return true, nil
}
func (r *borrowWorkerRepo) FinishJob(context.Context, string, int64, string, string, time.Duration) error {
	return nil
}
func (r *borrowWorkerRepo) SetPhase(context.Context, int64, string, string) error { return nil }
func (r *borrowWorkerRepo) ObserveCookie(_ context.Context, _ string, ttl time.Duration) (time.Time, error) {
	return r.now.Add(ttl), nil
}
func (r *borrowWorkerRepo) Now(context.Context) (time.Time, error) { return r.now, nil }
func (r *borrowWorkerRepo) CandidateKnown(context.Context, int64, string, string) (bool, error) {
	return false, nil
}
func (r *borrowWorkerRepo) RejectCandidate(context.Context, int64, string, string, time.Time) error {
	return nil
}
func (r *borrowWorkerRepo) Publish(_ context.Context, route *BorrowRoute, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.route = route
	return nil
}
func TestTicketBorrowWorkerPublishesOnlyTargetVerifiedCandidates(t *testing.T) {
	s, base := borrowFixture()
	base.route = nil
	cookie := "x." + base64.RawURLEncoding.EncodeToString([]byte(`{"host":"chat.gateway.unified-1.api.openai.com"}`)) + ".x"
	r := &borrowWorkerRepo{borrowTestRepo: base, source: boundBorrowTestSource(BorrowSource{ID: 1, Enabled: true, Model: "gpt-6-luna", Secret: BorrowSourceSecret{Credentials: map[string]any{"access_token": "source-token", "chatgpt_account_id": "source"}, DeviceID: "source-device"}})}
	s.repo = r
	mint := stateProbeMint("target-state")
	mint.cookies = nil
	up := &stateProbeUpstream{replies: []stateProbeReply{{status: 200, cookies: []string{"__oailb=" + cookie + "; Max-Age=230; Secure"}, body: stateProbeCompletedStream}, mint, {status: 200, body: stateProbeCompletedStream}}}
	s.owner.httpUpstream = up
	s.owner.openaiGatewayService = &OpenAIGatewayService{accountRepo: s.owner.accountRepo}
	require.ErrorIs(t, s.Check(t.Context(), 11, "gpt-6-astra", "http"), ErrBorrowUnavailable)
	s.runTarget(t.Context(), BorrowTarget{AcquisitionIdentity: BorrowAcquisitionSource, AccountID: 11, Revision: 2}, BorrowSettings{Running: true, TTLSeconds: 230}, "worker")
	require.Len(t, up.calls, 3)
	require.Contains(t, up.calls[0].body, "gpt-6-luna")
	require.Equal(t, "Bearer source-token", up.calls[0].header.Get("Authorization"))
	require.Equal(t, "Bearer target-secret", up.calls[1].header.Get("Authorization"))
	require.Equal(t, "target-state", up.calls[2].header.Get("X-Codex-Turn-State"))
	require.NoError(t, s.Check(t.Context(), 11, "gpt-6-astra", "http"))
	oldID := base.route.ID
	// A later degraded candidate must not remove the already admitted route.
	up.replies = []stateProbeReply{{status: 200, cookies: []string{"__oailb=" + cookie + "; Max-Age=230; Secure"}, body: stateProbeCompletedStream}, mint, {status: 200, state: "changed", body: stateProbeCompletedStream}}
	up.calls = nil
	s.runTarget(t.Context(), BorrowTarget{AcquisitionIdentity: BorrowAcquisitionSource, AccountID: 11, Revision: 2}, BorrowSettings{Running: true, TTLSeconds: 230}, "worker")
	require.Equal(t, oldID, base.route.ID)
	require.NoError(t, s.Check(t.Context(), 11, "gpt-6-astra", "http"))
}

func TestTicketBorrowRejectsUnverifiedEndpointAndPreservesConversation(t *testing.T) {
	s, _ := borrowFixture()
	req := borrowRequest(t)
	req.URL.Host = "other.example"
	_, err := s.ForwardHTTP(req, "", 11, 1, nil, func(*http.Request, string, int64, int, *tlsfingerprint.Profile) (*http.Response, error) {
		t.Fatal("unverified endpoint")
		return nil, nil
	})
	require.ErrorIs(t, err, ErrBorrowUnavailable)
	req = borrowRequest(t)
	req.Header.Set("X-Codex-Installation-ID", "unverified-client-device")
	req.Header.Set("Cookie", "foreign=value")
	_, err = s.ForwardHTTP(req, "", 11, 1, nil, func(sent *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
		require.Equal(t, "unverified-client-device", sent.Header.Get("X-Codex-Installation-ID"))
		require.Contains(t, sent.Header.Get("Cookie"), "foreign=value")
		require.Equal(t, "conversation-ticket", sent.Header.Get("X-Codex-Turn-State"))
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: http.NoBody}, nil
	})
	require.NoError(t, err)
}

func (r *borrowTestRepo) BeginTargetRequest(context.Context, BorrowRequest) error    { return nil }
func (r *borrowTestRepo) CheckTargetRequest(context.Context, BorrowRequest) error    { return nil }
func (r *borrowTestRepo) MarkTargetRequestSent(context.Context, BorrowRequest) error { return nil }
func (r *borrowTestRepo) RenewTargetRequest(context.Context, BorrowRequest) error    { return nil }
func (r *borrowTestRepo) EndTargetRequest(context.Context, BorrowRequest, int, string) error {
	return nil
}

func (r *borrowTestRepo) Settings(context.Context) (BorrowSettings, error) {
	return BorrowSettings{Running: true, TTLSeconds: 230}, nil
}
func (r *borrowTestRepo) RecordProbeOutcome(context.Context, int64, string, string, time.Duration) error {
	return nil
}
func (r *borrowTestRepo) AdvanceScan(context.Context, int64, string, int) error { return nil }

func (r *borrowTestRepo) CheckAcquisition(ctx context.Context, a BorrowAcquisition) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.unavailable {
		return errors.New("storage down")
	}
	if !r.enabled {
		return ErrBorrowUnavailable
	}
	if a.Lease == "" && r.route != nil && r.now.Add(15*time.Second).Before(r.route.ExpiresAt) && r.route.IdentityHash == a.Identity {
		return ErrBorrowRouteReady
	}
	return nil
}
func (r *borrowTestRepo) ReserveSourceAttempt(context.Context, int64, string) error { return nil }
