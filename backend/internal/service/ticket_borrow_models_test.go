package service

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestBorrowHTTPModelsPreserveRequestAndAdmission(t *testing.T) {
	for _, model := range []string{"gpt-6-astra", "gpt-6.1-sol", "gpt-5.6-sol", "gpt-6-sol", "gpt-6-luna", "custom-future-model"} {
		t.Run(model, func(t *testing.T) {
			s, repo := borrowFixture()
			body := `{"model":"` + model + `","reasoning":{"effort":"high"},"input":"question","stream":true}`
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, chatgptCodexURL, strings.NewReader(body))
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer target-secret")
			require.NoError(t, s.Check(t.Context(), 11, model, "http"))
			require.NoError(t, s.Check(t.Context(), 11, model, "ws"))
			calls := 0
			send := func(sent *http.Request, proxy string, id int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
				calls++
				require.EqualValues(t, 11, id)
				require.Empty(t, proxy)
				require.Equal(t, "Bearer target-secret", sent.Header.Get("Authorization"))
				cookie, e := sent.Cookie("__oailb")
				require.NoError(t, e)
				require.Equal(t, "donor-cookie", cookie.Value)
				actual, e := io.ReadAll(sent.Body)
				require.NoError(t, e)
				require.Equal(t, model, gjson.GetBytes(actual, "model").String())
				require.Equal(t, "high", gjson.GetBytes(actual, "reasoning.effort").String())
				require.False(t, gjson.GetBytes(actual, "store").Exists())
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: http.NoBody}, nil
			}
			resp, err := s.ForwardHTTP(req, "", 11, 10, nil, send)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			require.Empty(t, repo.leases)
			repo.route.ExpiresAt = repo.now
			require.ErrorIs(t, s.Check(t.Context(), 11, model, "http"), ErrBorrowUnavailable)
			_, err = s.ForwardHTTP(req, "", 11, 10, nil, send)
			require.ErrorIs(t, err, ErrBorrowUnavailable)
			require.Equal(t, 1, calls, "expired routes must never reach upstream")
		})
	}
}

func TestBorrowHTTPMissingModelNeverSends(t *testing.T) {
	for _, model := range []string{"", " "} {
		t.Run(model, func(t *testing.T) {
			s, _ := borrowFixture()
			require.ErrorIs(t, s.Check(t.Context(), 11, model, "http"), ErrBorrowUnavailable)
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, chatgptCodexURL, strings.NewReader(`{"model":"`+model+`"}`))
			require.NoError(t, err)
			_, err = s.ForwardHTTP(req, "", 11, 10, nil, func(*http.Request, string, int64, int, *tlsfingerprint.Profile) (*http.Response, error) {
				t.Fatal("missing model reached upstream")
				return nil, nil
			})
			require.ErrorIs(t, err, ErrBorrowUnavailable)
		})
	}
}

func TestBorrowHTTPModelsPrepareOwnedContinuation(t *testing.T) {
	for _, model := range []string{"gpt-6-astra", "gpt-6.1-sol", "gpt-5.6-sol", "gpt-6-sol", "gpt-6-luna", "custom-future-model"} {
		t.Run(model, func(t *testing.T) {
			svc, _ := borrowReplayFixture(t)
			body := []byte(`{"model":"` + model + `","reasoning":{"effort":"medium"},"previous_response_id":"resp_proven","input":"portable context"}`)
			ctx := svc.PrepareBorrowHTTPReplay(t.Context(), nil, 1, 2, "resp_proven", body)
			ctx, effective, converted, err := svc.PrepareBorrowHTTPContinuation(ctx, nil, "resp_proven", model, http.Header{}, body)
			require.NoError(t, err)
			require.True(t, converted)
			require.False(t, gjson.GetBytes(effective, "previous_response_id").Exists())
			require.Equal(t, model, gjson.GetBytes(effective, "model").String())
			require.Equal(t, "medium", gjson.GetBytes(effective, "reasoning.effort").String())
			require.True(t, borrowHTTPContinuationAllowsAccount(ctx, 11))
			require.False(t, borrowHTTPContinuationAllowsAccount(ctx, 12))
		})
	}
}
