package repository

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestBorrowBusinessRecoveryReusesFingerprintTransport(t *testing.T) {
	var accepted atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, "ok")
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			accepted.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()
	s := NewHTTPUpstream(&config.Config{Gateway: config.GatewayConfig{ConnectionPoolIsolation: config.ConnectionPoolIsolationAccountProxy}}).(*httpUpstreamService)
	profile := &tlsfingerprint.Profile{Name: "stable-target-device"}
	ordinary, err := s.getClientEntryWithTLS("", 11, 10, profile, service.HTTPUpstreamProfileOpenAI, false, false)
	require.NoError(t, err)
	defer ordinary.client.CloseIdleConnections()
	borrow, err := s.getClientEntryWithTLS("", 11, 10, profile, service.HTTPUpstreamProfileOpenAIHarvest, false, false)
	require.NoError(t, err)
	defer borrow.client.CloseIdleConnections()
	require.Same(t, ordinary, borrow)
	require.NotNil(t, borrow.client.Transport.(*http.Transport).DialTLSContext, "fingerprint dialer must remain installed")
	before := accepted.Load()
	reused := []bool{}
	for _, entry := range []*upstreamClientEntry{ordinary, borrow, ordinary} {
		ctx := httptrace.WithClientTrace(t.Context(), &httptrace.ClientTrace{GotConn: func(x httptrace.GotConnInfo) { reused = append(reused, x.Reused) }})
		req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL, strings.NewReader("{}"))
		resp, e := doUpstreamRequest(entry.client, req)
		require.NoError(t, e)
		_, e = io.Copy(io.Discard, resp.Body)
		require.NoError(t, e)
		require.NoError(t, resp.Body.Close())
	}
	require.Equal(t, int64(1), accepted.Load()-before)
	require.Equal(t, []bool{false, true, true}, reused)
}

func TestBorrowBusinessRecoveryPhysicalSendLimit(t *testing.T) {
	var posts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { posts.Add(1); w.WriteHeader(503) }))
	defer srv.Close()
	ctx, r := service.WithBorrowBusinessRecovery(t.Context(), nil)
	defer r.Close()
	require.NoError(t, r.Activate(11, false))
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	send := func(ctx context.Context) error {
		req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL, strings.NewReader("{}"))
		resp, err := doUpstreamRequest(client, req)
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		return err
	}
	require.NoError(t, send(ctx))
	require.True(t, r.ReserveRecovery(ctx, 503))
	require.NoError(t, send(context.WithoutCancel(ctx)))
	require.ErrorIs(t, send(ctx), service.ErrBorrowRecoveryBudget)
	require.Equal(t, int64(2), posts.Load())
}

func TestBorrowBusinessRecoveryDoesNotFollowRedirects(t *testing.T) {
	var posts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		posts.Add(1)
		http.Redirect(w, req, "/redirected", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	ctx, r := service.WithBorrowBusinessRecovery(t.Context(), nil)
	defer r.Close()
	require.NoError(t, r.Activate(11, false))
	req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL, strings.NewReader("{}"))
	resp, err := doUpstreamRequest(srv.Client(), req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusTemporaryRedirect, resp.StatusCode)
	require.Equal(t, int64(1), posts.Load())
}

func TestBorrowBusinessRecovery429ThenSlowHeadersNaturalCompletion(t *testing.T) {
	var posts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if posts.Add(1) == 1 {
			w.WriteHeader(429)
			return
		}
		time.Sleep(1200 * time.Millisecond) // exceeds the pre-send recovery timeout
		_, _ = io.WriteString(w, "natural completion")
	}))
	defer srv.Close()
	ctx, r := service.WithBorrowBusinessRecovery(t.Context(), &config.Config{Gateway: config.GatewayConfig{BorrowBusinessRecoveryTimeoutSeconds: 1}})
	defer r.Close()
	require.NoError(t, r.Activate(11, false))
	send := func() (*http.Response, error) {
		req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL, strings.NewReader("{}"))
		return doUpstreamRequest(srv.Client(), req)
	}
	resp, err := send()
	require.NoError(t, err)
	resp.Body.Close()
	require.True(t, r.ReserveRecovery(ctx, 429))
	resp, err = send()
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.NoError(t, err)
	require.Equal(t, "natural completion", string(body))
	require.NoError(t, r.Err())
	require.EqualValues(t, 2, posts.Load())
	_, err = send()
	require.ErrorIs(t, err, service.ErrBorrowRecoveryBudget)
}
