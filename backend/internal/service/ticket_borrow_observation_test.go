package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

func TestTicketBorrowObservationHashesSensitiveDataWithoutChangingRequest(t *testing.T) {
	req, err := http.NewRequest("POST", chatgptCodexURL, strings.NewReader("private-payload"))
	require.NoError(t, err)
	for key, value := range map[string]string{"Authorization": "Bearer private-auth", "Cookie": "private-cookie", "X-Codex-Turn-State": "private-ticket", "X-Codex-Installation-ID": "private-device", "User-Agent": "private-agent", "Version": "0.162.1"} {
		req.Header.Set(key, value)
	}
	before := req.Header.Clone()
	proxy := "http://private-user:private-password@proxy.example:2000"
	env := borrowEnvironment(req, proxy, &tlsfingerprint.Profile{Name: "private-profile"})
	data, err := json.Marshal(env)
	require.NoError(t, err)
	for _, secret := range []string{"private-payload", "private-auth", "private-cookie", "private-ticket", "private-device", "private-agent", "private-user", "private-password", "private-profile", proxy} {
		require.NotContains(t, string(data), secret)
	}
	require.Equal(t, "0.162.1", env.ClientVersion)
	require.Equal(t, borrowHash("private-payload"), env.BodyHash)
	require.Equal(t, before, req.Header)
	original, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	require.Equal(t, "private-payload", string(original))
	req.Header.Set("Authorization", "different-auth")
	req.Header.Set("session_id", "different-session")
	require.Equal(t, env.IdentityHash, borrowEnvironment(req, proxy, nil).IdentityHash)
	req.Header.Set("X-Codex-Installation-ID", "different-device")
	require.NotEqual(t, env.IdentityHash, borrowEnvironment(req, proxy, nil).IdentityHash)
	req.Header.Set("Version", "Bearer private-version")
	require.Empty(t, borrowEnvironment(req, proxy, nil).ClientVersion)
}

type observationRequestRepo struct {
	*borrowTestRepo
	begin, sent BorrowRequest
}

func (r *observationRequestRepo) BeginTargetRequest(_ context.Context, q BorrowRequest) error {
	r.begin = q
	return nil
}
func (r *observationRequestRepo) MarkTargetRequestSent(_ context.Context, q BorrowRequest) error {
	r.sent = q
	return nil
}

func TestTicketBorrowObservationCapturesConnectedProbeAndOmitsBusinessPayload(t *testing.T) {
	for _, kind := range []string{"probe", "business"} {
		t.Run(kind, func(t *testing.T) {
			s, base := borrowFixture()
			r := &observationRequestRepo{borrowTestRepo: base}
			s.repo = r
			req := borrowRequest(t)
			if kind == "business" {
				req.GetBody = func() (io.ReadCloser, error) { t.Fatal("business payload inspected by telemetry"); return nil, nil }
			}
			resp, err := s.trackTargetRequest(req, BorrowRequest{Kind: kind, CandidateID: "candidate-a", RouteID: "route-a"}, "", 11, 10, nil,
				func(out *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
					out.Header.Set("User-Agent", "connected-agent")
					require.NoError(t, BorrowTargetRequestConnected(out))
					return &http.Response{StatusCode: 200, Header: http.Header{}, Body: http.NoBody}, nil
				})
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			if kind == "probe" {
				require.Equal(t, "candidate-a", r.sent.CandidateID)
				require.Equal(t, "route-a", r.sent.RouteID)
				require.NotEmpty(t, r.sent.Environment.BodyHash)
				require.NotEqual(t, r.begin.Environment.IdentityHash, r.sent.Environment.IdentityHash)
			} else {
				require.Empty(t, r.begin.ID)
				require.Empty(t, r.sent.ID)
			}
		})
	}
}

type observationWorkerRepo struct {
	testSources map[int64]BorrowSource
	*borrowWorkerRepo
	sourceReady chan struct{}
	sourceCount atomic.Int32
}

func (r *observationWorkerRepo) Event(ctx context.Context, event BorrowEvent) error {
	err := r.borrowWorkerRepo.Event(ctx, event)
	if r.sourceReady != nil && event.Stage == "source" && r.sourceCount.Add(1) == 3 {
		close(r.sourceReady)
	}
	return err
}
func (r *observationWorkerRepo) SetPhase(ctx context.Context, _ int64, _, _ string) error {
	if r.sourceReady != nil {
		select {
		case <-r.sourceReady:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (r *observationWorkerRepo) Sources(ctx context.Context) ([]BorrowSource, error) {
	sources := []BorrowSource{}
	for id := int64(1); id <= 3; id++ {
		source, _ := r.Source(ctx, id)
		sources = append(sources, *source)
	}
	return sources, nil
}
func (r *observationWorkerRepo) Source(_ context.Context, id int64) (*BorrowSource, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.testSources == nil {
		r.testSources = map[int64]BorrowSource{}
	}
	source, ok := r.testSources[id]
	if !ok {
		source = boundBorrowTestSource(BorrowSource{ID: id, Revision: 1, Enabled: true, Cursor: 1, Model: "gpt-6-luna"})
		r.testSources[id] = source
	}
	return &source, nil
}

type observationWorkerUpstream struct {
	HTTPUpstream
	mu                       sync.Mutex
	sourceCalls, targetCalls int
	degraded                 bool
}

func (u *observationWorkerUpstream) DoWithTLS(req *http.Request, _ string, id int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	headers := http.Header{}
	if id != 11 {
		u.sourceCalls++
		cookie := "x." + base64.RawURLEncoding.EncodeToString([]byte(`{"host":"chat.gateway.unified-`+strconv.FormatInt(id, 10)+`.api.openai.com"}`)) + ".x"
		headers.Add("Set-Cookie", "__oailb="+cookie+"; Max-Age=230")
	} else {
		u.targetCalls++
		if req.Header.Get(openAICodexTurnStateHeader) == "" {
			headers.Set(openAICodexTurnStateHeader, "target-private-ticket")
		} else if u.degraded {
			headers.Set(openAICodexTurnStateHeader, "replacement-private-ticket")
		}
	}
	return &http.Response{StatusCode: 200, Header: headers, Body: io.NopCloser(strings.NewReader(stateProbeCompletedStream))}, nil
}

func TestTicketBorrowObservationCorrelatesCandidatesAndRecordsDiscardedPrefetch(t *testing.T) {
	for _, degraded := range []bool{false, true} {
		t.Run(strconv.FormatBool(degraded), func(t *testing.T) {
			s, base := borrowFixture()
			base.route = nil
			r := &observationWorkerRepo{borrowWorkerRepo: &borrowWorkerRepo{borrowTestRepo: base}, sourceReady: make(chan struct{})}
			s.repo = r
			up := &observationWorkerUpstream{degraded: degraded}
			s.owner.httpUpstream = up
			s.owner.openaiGatewayService = &OpenAIGatewayService{accountRepo: s.owner.accountRepo}
			s.runTarget(t.Context(), BorrowTarget{AccountID: 11, Revision: 2, ProbePolicy: BorrowProbePolicy{Concurrency: 3, RenewBeforeSeconds: 15}}, BorrowSettings{Running: true, TTLSeconds: 230}, "batch-a")
			sources := map[string]*BorrowCandidateDetails{}
			targets, discarded := 0, 0
			for _, event := range base.events {
				require.NotNil(t, event.Details)
				d := event.Details
				require.Equal(t, "batch-a", d.BatchID)
				require.Len(t, d.CookieHash, 64)
				require.NotEmpty(t, d.Source.IdentityHash)
				require.Equal(t, borrowHash(string(openAICodexStateProbeBody("gpt-6-luna"))), d.Source.BodyHash)
				if event.Stage == "source" {
					sources[d.CandidateID] = d
					continue
				}
				require.NotNil(t, sources[d.CandidateID])
				require.Equal(t, sources[d.CandidateID].Gateway, d.Gateway)
				require.NotEmpty(t, d.Target.IdentityHash)
				if event.Stage == "target" {
					targets++
					require.NotNil(t, d.CookieAgeAtProbeMS)
					require.Positive(t, *d.RemainingAtProbeMS)
					require.Equal(t, 200, d.ContinueStatus)
					require.Contains(t, []string{"target_probe_passed", "target_ticket_changed"}, event.Reason)
					if event.Reason == "target_probe_passed" {
						require.NotEmpty(t, event.RouteID)
					}
				} else {
					discarded++
					require.Equal(t, "candidate_unused_after_success", event.Reason)
				}
			}
			require.Len(t, sources, 3)
			require.Equal(t, 3, up.sourceCalls)
			if degraded {
				require.Equal(t, 3, targets)
				require.Zero(t, discarded)
				require.Equal(t, 6, up.targetCalls)
			} else {
				require.Equal(t, 1, targets)
				require.Equal(t, 2, discarded)
				require.GreaterOrEqual(t, up.targetCalls, 2)
				require.LessOrEqual(t, up.targetCalls, 6)
			}
			serialized, err := json.Marshal(base.events)
			require.NoError(t, err)
			require.NotContains(t, string(serialized), "private-token")
			require.NotContains(t, string(serialized), "private-ticket")
		})
	}
}
