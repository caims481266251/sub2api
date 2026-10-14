package repository

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type borrowBusinessCancelAccounts struct {
	service.AccountRepository
	account *service.Account
}

func (a borrowBusinessCancelAccounts) GetByID(context.Context, int64) (*service.Account, error) {
	return a.account, nil
}

// Admit before connection acquisition, then reject in GotConn on the same
// multiplexed socket while another ordinary response is still streaming.
type borrowBusinessCancelRepo struct {
	service.TicketBorrowRepository
	endpoint string
	checks   atomic.Int32
	releases atomic.Int32
}

func (r *borrowBusinessCancelRepo) Target(context.Context, int64) (service.BorrowTarget, error) {
	return service.BorrowTarget{AccountID: 11, Enabled: true}, nil
}
func (r *borrowBusinessCancelRepo) ReserveRoute(_ context.Context, id int64, identity, _, _, _ string) (*service.BorrowRoute, error) {
	fingerprint, _ := service.NewBorrowFingerprint()
	return &service.BorrowRoute{ID: "route-a", TargetID: id, IdentityHash: identity,
		Secret: service.BorrowRouteSecret{Fingerprint: fingerprint, Endpoint: r.endpoint, Cookie: http.Cookie{Name: "__oailb", Value: "test-ticket"}}}, nil
}
func (r *borrowBusinessCancelRepo) Route(context.Context, int64, string, string) (*service.BorrowRoute, error) {
	if r.checks.Add(1) >= 2 {
		return nil, service.ErrBorrowUnavailable
	}
	return &service.BorrowRoute{ID: "route-a"}, nil
}
func (r *borrowBusinessCancelRepo) RenewRoute(context.Context, string) error { return nil }
func (r *borrowBusinessCancelRepo) RouteStatuses(context.Context, int64, string) ([]service.BorrowRouteStatus, error) {
	return nil, nil
}
func (r *borrowBusinessCancelRepo) ReleaseRoute(context.Context, string) error {
	r.releases.Add(1)
	return nil
}

var _ service.BorrowRouteLeaseRepository = (*borrowBusinessCancelRepo)(nil)

func TestBorrowBusinessCancellationPreservesSharedHTTP2Stream(t *testing.T) {
	for _, profile := range []service.HTTPUpstreamProfile{service.HTTPUpstreamProfileOpenAI, service.HTTPUpstreamProfileOpenAIHarvest, service.HTTPUpstreamProfileBorrowProbe} {
		t.Run(string(profile), func(t *testing.T) { testBorrowCancellationPreservesSharedHTTP2Stream(t, profile) })
	}
}

func testBorrowCancellationPreservesSharedHTTP2Stream(t *testing.T, profile service.HTTPUpstreamProfile) {
	var connections, rejectedPosts atomic.Int32
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			rejectedPosts.Add(1)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = io.WriteString(w, "first\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
			_, _ = io.WriteString(w, "last\n")
		case <-r.Context().Done():
		}
	}))
	server.EnableHTTP2 = true
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.StartTLS()
	defer server.Close()
	defer unblock()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	ctx = service.WithHTTPUpstreamProfile(ctx, profile)
	client := server.Client()
	normal, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	require.NoError(t, err)
	stream, err := doUpstreamRequest(client, normal)
	require.NoError(t, err)
	defer stream.Body.Close()
	require.Equal(t, 2, stream.ProtoMajor)
	reader := bufio.NewReader(stream.Body)
	first, err := reader.ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "first\n", first)
	repo := &borrowBusinessCancelRepo{endpoint: server.URL + "/responses"}
	accounts := borrowBusinessCancelAccounts{account: &service.Account{ID: 11, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Status: service.StatusActive, Schedulable: true, Credentials: map[string]any{}}}
	owner := service.NewAccountTestService(accounts, nil, nil, nil, nil, nil, nil, nil)
	borrow := service.NewTicketBorrowService(repo, owner, nil)
	gotConn := make(chan httptrace.GotConnInfo, 1)
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { gotConn <- info }})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, repo.endpoint, strings.NewReader(`{"model":"gpt-6-sol","input":"test","stream":true}`))
	require.NoError(t, err)
	_, err = borrow.ForwardHTTP(req, "", 11, 10, nil, func(sent *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
		return doUpstreamRequest(client, sent)
	})
	require.ErrorIs(t, err, service.ErrBorrowUnavailable)
	require.False(t, service.IsOpenAIInitialAdmissionRejection(err), "a GotConn rejection must not assert whole-forward replay safety")
	select {
	case info := <-gotConn:
		require.True(t, info.Reused, "the cancellation must occur on the active shared H2 connection")
	case <-ctx.Done():
		t.Fatal("borrowed request never reached the connection check")
	}
	unblock()
	last, err := io.ReadAll(reader)
	require.NoError(t, err, "rejecting one route must not truncate another HTTP/2 stream")
	require.Equal(t, "last\n", string(last))
	require.EqualValues(t, 1, connections.Load())
	require.Zero(t, rejectedPosts.Load(), "rejected request must not reach upstream")
	require.EqualValues(t, 1, repo.releases.Load())
}
