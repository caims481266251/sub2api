package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type activityMonitorStub struct {
	accountQualityMonitor
	batchCalls int
	runCalls   int
	saved      *service.AccountQualityMonitorConfig
}

func (s *activityMonitorStub) BatchForAccounts(_ context.Context, accounts []*service.Account) (map[int64]*service.AccountQualityMonitorView, error) {
	s.batchCalls++
	result := make(map[int64]*service.AccountQualityMonitorView, len(accounts))
	for _, account := range accounts {
		result[account.ID] = &service.AccountQualityMonitorView{AccountID: account.ID, State: "untested"}
	}
	return result, nil
}

func (s *activityMonitorStub) Get(_ context.Context, id int64, _ bool) (*service.AccountQualityMonitorView, error) {
	return &service.AccountQualityMonitorView{AccountID: id, State: "untested"}, nil
}

func (s *activityMonitorStub) Save(_ context.Context, id int64, config service.AccountQualityMonitorConfig) (*service.AccountQualityMonitorView, error) {
	s.saved = &config
	return &service.AccountQualityMonitorView{AccountID: id, State: "untested"}, nil
}

func (s *activityMonitorStub) Start(_ context.Context, id int64, _ string) (*service.AccountQualityMonitorView, error) {
	s.runCalls++
	return &service.AccountQualityMonitorView{AccountID: id, State: "running"}, nil
}

type activityCacheStub struct {
	service.AccountRecentRequestsCache
	err    error
	calls  int
	scopes map[int64]string
}

func (s *activityCacheStub) Batch(_ context.Context, scopes map[int64]string) (map[int64][]service.AccountRecentRequest, error) {
	s.calls++
	s.scopes = scopes
	return map[int64][]service.AccountRecentRequest{}, s.err
}

func activityRouter(monitor *activityMonitorStub, cache *activityCacheStub, observer bool) *gin.Engine {
	gin.SetMode(gin.TestMode)
	h := &AccountHandler{qualityMonitor: monitor, recentRequests: cache, adminService: &observerAccountService{accounts: map[int64]*service.Account{
		1: {ID: 1, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Credentials: map[string]any{"api_key": "synthetic-a"}, GroupIDs: []int64{10}},
		2: {ID: 2, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Credentials: map[string]any{"api_key": "synthetic-b"}, GroupIDs: []int64{20}},
		3: {ID: 3, Platform: service.PlatformAnthropic, GroupIDs: []int64{10}},
	}}}
	r := gin.New()
	if observer {
		r.Use(func(c *gin.Context) {
			c.Request = c.Request.WithContext(service.WithObserverScope(c.Request.Context(), []int64{10}))
		})
	}
	r.Use(h.AuthorizeObserver)
	r.POST("/api/v1/admin/accounts/activity/batch", h.GetAccountActivity)
	r.GET("/api/v1/admin/accounts/:id/quality-monitor", h.GetQualityMonitor)
	r.PUT("/api/v1/admin/accounts/:id/quality-monitor", h.SaveQualityMonitor)
	r.POST("/api/v1/admin/accounts/:id/quality-monitor/run", h.RunQualityMonitor)
	return r
}

func activityCall(r *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, "/api/v1/admin/accounts"+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	return w
}

func TestAccountActivityBatchCacheUnavailableIsNotEmptyHistory(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		monitor, cache := &activityMonitorStub{}, &activityCacheStub{}
		if unavailable {
			cache.err = errors.New("private-cache-connection-details")
		}
		w := activityCall(activityRouter(monitor, cache, false), http.MethodPost, "/activity/batch", `{"account_ids":[1,1,2]}`)
		require.Equal(t, 200, w.Code)
		var payload struct {
			Data struct {
				Items []struct {
					AccountID int64           `json:"account_id"`
					Recent    json.RawMessage `json:"recent_requests"`
				} `json:"items"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &payload))
		require.Len(t, payload.Data.Items, 2)
		require.Equal(t, int64(1), payload.Data.Items[0].AccountID)
		want := "[]"
		if unavailable {
			want = "null"
		}
		require.JSONEq(t, want, string(payload.Data.Items[0].Recent))
		require.Equal(t, 1, monitor.batchCalls)
		require.Equal(t, 1, cache.calls)
		require.Len(t, cache.scopes, 2)
		require.NotContains(t, w.Body.String(), "private-cache")
		require.NotContains(t, w.Body.String(), "synthetic-a")
	}
}

func TestAccountActivityRejectsInvalidAndUnsupportedBatches(t *testing.T) {
	for _, body := range []string{`{"account_ids":[0]}`, `{"account_ids":[3]}`, `{"account_ids":[999]}`, `{"account_ids":[` + strings.Repeat("1,", 200) + `1]}`} {
		monitor, cache := &activityMonitorStub{}, &activityCacheStub{}
		w := activityCall(activityRouter(monitor, cache, false), http.MethodPost, "/activity/batch", body)
		require.True(t, w.Code == 400 || w.Code == 404, w.Body.String())
		require.Zero(t, monitor.batchCalls)
		require.Zero(t, cache.calls)
	}
}

func TestAccountActivityObserverCapabilitiesRemainScoped(t *testing.T) {
	for _, tc := range []struct {
		method, path, body string
		status             int
	}{
		{http.MethodPost, "/activity/batch", `{"account_ids":[1]}`, 200},
		{http.MethodPost, "/activity/batch", `{"account_ids":[1,2]}`, 403},
		{http.MethodGet, "/1/quality-monitor", "", 200},
		{http.MethodGet, "/2/quality-monitor", "", 403},
		{http.MethodPut, "/1/quality-monitor", `{"scope":"test","enabled":false,"model":"test-model","interval_minutes":5}`, 200},
		{http.MethodPut, "/2/quality-monitor", `{"scope":"test","enabled":false,"model":"test-model","interval_minutes":5}`, 403},
		{http.MethodPost, "/1/quality-monitor/run", `{"scope":"test"}`, 202},
		{http.MethodPost, "/2/quality-monitor/run", `{"scope":"test"}`, 403},
	} {
		monitor, cache := &activityMonitorStub{}, &activityCacheStub{}
		w := activityCall(activityRouter(monitor, cache, true), tc.method, tc.path, tc.body)
		require.Equal(t, tc.status, w.Code, w.Body.String())
		if tc.status == 403 {
			require.Zero(t, monitor.batchCalls+monitor.runCalls+cache.calls)
			require.Nil(t, monitor.saved)
		}
	}
}

func TestAccountQualityRunRequiresCurrentScope(t *testing.T) {
	monitor := &activityMonitorStub{}
	r := activityRouter(monitor, &activityCacheStub{}, false)
	for _, body := range []string{"", `{}`, `{"scope":""}`} {
		require.Equal(t, 400, activityCall(r, http.MethodPost, "/1/quality-monitor/run", body).Code)
	}
	require.Zero(t, monitor.runCalls)
}
