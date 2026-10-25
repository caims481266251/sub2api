package repository

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type borrowTransportAccounts struct {
	service.AccountRepository
	a *service.Account
}

func (r borrowTransportAccounts) GetByID(context.Context, int64) (*service.Account, error) {
	return r.a, nil
}

type borrowTransportRepo struct {
	service.TicketBorrowRepository
	expired  atomic.Bool
	endpoint string
}

func (r *borrowTransportRepo) Target(context.Context, int64) (service.BorrowTarget, error) {
	return service.BorrowTarget{AccountID: 11, Enabled: true}, nil
}
func (r *borrowTransportRepo) Route(context.Context, int64, string, string) (*service.BorrowRoute, error) {
	if r.expired.Load() {
		return nil, service.ErrBorrowUnavailable
	}
	return &service.BorrowRoute{ID: "route", TargetID: 11, ExpiresAt: time.Now().Add(time.Minute), Secret: service.BorrowRouteSecret{Endpoint: r.endpoint, Cookie: http.Cookie{Name: "__oailb", Value: "cookie"}}}, nil
}
func TestTicketBorrowTransportBlocksAfterDialBeforeAnyHeaders(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); _, _ = io.Copy(io.Discard, r.Body) }))
	defer server.Close()
	repo := &borrowTransportRepo{endpoint: server.URL + "/responses"}
	owner := service.NewAccountTestService(borrowTransportAccounts{a: &service.Account{ID: 11, Status: service.StatusActive, Schedulable: true}}, nil, nil, nil, nil, nil, nil, nil)
	svc := service.NewTicketBorrowService(repo, owner, nil)
	transport := &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
		repo.expired.Store(true)
		return conn, err
	}}
	defer transport.CloseIdleConnections()
	req, err := http.NewRequestWithContext(t.Context(), "POST", server.URL+"/responses", strings.NewReader(`{"model":"gpt-6-astra"}`))
	require.NoError(t, err)
	_, err = svc.ForwardHTTP(req, "", 11, 1, nil, func(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
		return doUpstreamRequest(&http.Client{Transport: transport}, req)
	})
	require.ErrorIs(t, err, service.ErrBorrowUnavailable)
	require.Zero(t, requests.Load())
}

func (r *borrowTransportRepo) BeginTargetRequest(context.Context, service.BorrowRequest) error {
	return nil
}
func (r *borrowTransportRepo) CheckTargetRequest(context.Context, service.BorrowRequest) error {
	return nil
}
func (r *borrowTransportRepo) MarkTargetRequestSent(context.Context, service.BorrowRequest) error {
	return nil
}
func (r *borrowTransportRepo) RenewTargetRequest(context.Context, service.BorrowRequest) error {
	return nil
}
func (r *borrowTransportRepo) EndTargetRequest(context.Context, service.BorrowRequest, int, string) error {
	return nil
}
