package repository

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestTicketBorrowProbeAndBusinessConnectionCachesAreIndependent(t *testing.T) {
	for _, isolation := range []string{config.ConnectionPoolIsolationProxy, config.ConnectionPoolIsolationAccount, config.ConnectionPoolIsolationAccountProxy} {
		for _, tls := range []bool{false, true} {
			t.Run(isolation+map[bool]string{false: "/plain", true: "/tls"}[tls], func(t *testing.T) {
				cfg := &config.Config{Gateway: config.GatewayConfig{ConnectionPoolIsolation: isolation, MaxUpstreamClients: 1}}
				s := NewHTTPUpstream(cfg).(*httpUpstreamService)
				acquire := func(p service.HTTPUpstreamProfile, n int) (*upstreamClientEntry, error) {
					if tls {
						return s.acquireClientWithTLS("", 11, n, &tlsfingerprint.Profile{Name: "same-fingerprint"}, p)
					}
					return s.acquireClientWithProfile("", 11, n, p)
				}
				probe, err := acquire(service.HTTPUpstreamProfileBorrowProbe, 3)
				require.NoError(t, err)
				defer func() { atomic.AddInt64(&probe.inFlight, -1); probe.client.CloseIdleConnections() }()
				// Even the maximum client-cache count cannot let probes exclude business.
				business, err := acquire(service.HTTPUpstreamProfileOpenAIHarvest, 1)
				require.NoError(t, err)
				defer func() { atomic.AddInt64(&business.inFlight, -1); business.client.CloseIdleConnections() }()
				require.NotSame(t, probe, business)
				require.Equal(t, 3, probe.client.Transport.(*http.Transport).MaxConnsPerHost)
				again, err := acquire(service.HTTPUpstreamProfileBorrowProbe, 3)
				require.NoError(t, err)
				require.Same(t, probe, again)
				atomic.AddInt64(&again.inFlight, -1)
				require.Len(t, s.clients, 1)
				require.Len(t, s.borrowProbePool().clients, 1)
				require.False(t, s.useRegionalEgress("", 11, service.HTTPUpstreamProfileBorrowProbe))
			})
		}
	}
}

func TestTicketBorrowSaturatedProbeTransportDoesNotQueueBusiness(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/probe" {
			close(entered)
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()
	defer close(release)
	s := NewHTTPUpstream(&config.Config{Gateway: config.GatewayConfig{ConnectionPoolIsolation: config.ConnectionPoolIsolationAccount, MaxUpstreamClients: 1}}).(*httpUpstreamService)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	probe, _ := http.NewRequestWithContext(service.WithHTTPUpstreamProfile(ctx, service.HTTPUpstreamProfileBorrowProbe), "GET", server.URL+"/probe", nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, err := s.Do(probe, "", 11, 1)
		if err == nil {
			resp.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("probe did not enter transport")
	}
	business, _ := http.NewRequestWithContext(service.WithHTTPUpstreamProfile(ctx, service.HTTPUpstreamProfileOpenAIHarvest), "POST", server.URL+"/responses", strings.NewReader("{}"))
	resp, err := s.Do(business, "", 11, 1)
	require.NoError(t, err, "business must complete before the probe connection is released")
	require.NoError(t, resp.Body.Close())
	cancel()
	<-done
}
