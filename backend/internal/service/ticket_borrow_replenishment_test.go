package service

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

type candidateFlowRepo struct {
	*borrowWorkerRepo
	rolling     bool
	sourceReads int
	published   []*BorrowRoute
}

func (r *candidateFlowRepo) Sources(ctx context.Context) ([]BorrowSource, error) {
	r.sourceReads++
	return r.borrowWorkerRepo.Sources(ctx)
}
func (r *candidateFlowRepo) Settings(context.Context) (BorrowSettings, error) {
	w := 80
	return BorrowSettings{Running: true, AdmissionWindowSeconds: &w, Replenishment: &BorrowReplenishmentSettings{Enabled: r.rolling}}, nil
}
func (r *candidateFlowRepo) Publish(_ context.Context, route *BorrowRoute, _ string) error {
	r.published = append(r.published, route)
	return nil
}

type candidateFlowCall struct {
	account int64
	header  http.Header
	proxy   string
	profile *tlsfingerprint.Profile
}
type candidateFlowUpstream struct {
	HTTPUpstream
	mu    sync.Mutex
	calls []candidateFlowCall
}

func (u *candidateFlowUpstream) DoWithTLS(req *http.Request, proxy string, id int64, _ int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	if err := BorrowBeforeSend(req); err != nil {
		return nil, err
	}
	if err := BorrowSourceRequestSent(req); err != nil {
		return nil, err
	}
	u.mu.Lock()
	u.calls = append(u.calls, candidateFlowCall{id, req.Header.Clone(), proxy, profile})
	u.mu.Unlock()
	h := http.Header{}
	if req.Header.Get("Cookie") == "" {
		payload := fmt.Sprintf(`{"host":"chat.gateway.unified-1.api.openai.com","candidate":%q}`, req.Header.Get("X-Codex-Installation-ID"))
		h.Set("Set-Cookie", "__oailb=x."+base64.RawURLEncoding.EncodeToString([]byte(payload))+".x; Max-Age=3600")
	} else if req.Header.Get(openAICodexTurnStateHeader) == "" {
		h.Set(openAICodexTurnStateHeader, "target-ticket")
	}
	return &http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(strings.NewReader(stateProbeCompletedStream))}, nil
}

func TestTicketBorrowFreshSelfCandidatesWithRollingOffAndOn(t *testing.T) {
	for _, rolling := range []bool{false, true} {
		t.Run(fmt.Sprint(rolling), func(t *testing.T) {
			s, base := borrowFixture()
			base.route = nil
			r := &candidateFlowRepo{borrowWorkerRepo: &borrowWorkerRepo{borrowTestRepo: base}, rolling: rolling}
			s.repo = r
			s.owner.openaiGatewayService = &OpenAIGatewayService{accountRepo: s.owner.accountRepo}
			up := &candidateFlowUpstream{}
			s.owner.httpUpstream = up
			settings, _ := r.Settings(t.Context())
			expected := 1
			if rolling {
				expected = 2
			}
			for range 2 {
				s.runTarget(t.Context(), BorrowTarget{AccountID: 11, Revision: 2, ProbePolicy: BorrowProbePolicy{Concurrency: 2}}, settings, "fresh-self")
			}
			require.Zero(t, r.sourceReads, "self acquisition must not depend on source inventory")
			require.Len(t, r.published, expected*2)
			devices := map[string]string{}
			for _, route := range r.published {
				require.Zero(t, route.SourceID)
				require.EqualValues(t, 11, route.AcquisitionAccountID)
				require.Equal(t, BorrowAcquisitionSelf, route.AcquisitionIdentity)
				require.NotNil(t, route.AcquisitionStartedAt)
				require.Equal(t, route.AcquisitionStartedAt.Add(80*1e9), route.AdmissionUntil)
				env := r.environments[route.CandidateID]
				require.NotNil(t, env)
				require.Empty(t, devices[env.Fingerprint.DeviceID])
				devices[env.Fingerprint.DeviceID] = route.CandidateID
			}
			for _, call := range up.calls {
				require.EqualValues(t, 11, call.account)
				require.Equal(t, "Bearer target-secret", call.header.Get("Authorization"))
				require.Equal(t, "http://pool.example:80", call.proxy)
				require.NotNil(t, call.profile)
			}
		})
	}
}
