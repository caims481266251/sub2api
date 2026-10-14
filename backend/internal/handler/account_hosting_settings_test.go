package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type hostedSettingsRepo struct {
	service.HostingRepository
	updated bool
}

func (*hostedSettingsRepo) GetUserPolicy(context.Context, int64) (service.HostingUserPolicy, error) {
	return service.HostingUserPolicy{}, nil
}

func (r *hostedSettingsRepo) UpdateOwnedSettings(_ context.Context, userID, id int64, input service.HostingAccountSettings) (*service.Account, error) {
	if userID != 7 || id != 17 {
		return nil, service.ErrHostingAccountNotFound
	}
	r.updated = true
	return &service.Account{ID: id, OwnerUserID: &userID, Name: "Codex", Concurrency: 3, Status: service.StatusActive, Schedulable: true, Credentials: map[string]any{"refresh_token": "private-token"}, Extra: input.Extra}, nil
}

func TestHostingAccountSettingsHTTPBoundary(t *testing.T) {
	for _, test := range []struct {
		name, body, id string
		userID         int64
		status         int
		updated        bool
	}{
		{"owned settings", `{"extra":{"openai_compact_mode":"force_on"}}`, "17", 7, http.StatusOK, true},
		{"advanced settings", `{"notes":"My account","load_factor":8,"expires_at":2000000000,"auto_pause_on_expired":true,"credentials":{"account_scheduling_threshold":85,"temp_unschedulable_enabled":true,"temp_unschedulable_rules":[{"error_code":429,"keywords":["limit"],"duration_minutes":30}]},"extra":{"auto_reset_credit_enabled":true,"auto_reset_credit_5h_threshold":0,"auto_reset_credit_7d_threshold":0.9}}`, "17", 7, http.StatusOK, true},
		{"unauthenticated", `{}`, "17", 0, http.StatusUnauthorized, false},
		{"other owner", `{}`, "17", 8, http.StatusNotFound, false},
		{"invalid id", `{}`, "nope", 7, http.StatusBadRequest, false},
		{"unknown root field", `{"owner_user_id":8}`, "17", 7, http.StatusBadRequest, false},
		{"credentials protected", `{"credentials":{"refresh_token":"attack"}}`, "17", 7, http.StatusBadRequest, false},
		{"runtime protected", `{"extra":{"codex_5h_used_percent":0}}`, "17", 7, http.StatusBadRequest, false},
		{"reset credit runtime protected", `{"extra":{"codex_auto_reset_credit_state":{"status":"success"}}}`, "17", 7, http.StatusBadRequest, false},
		{"admin proxy protected", `{"proxy_id":5}`, "17", 7, http.StatusBadRequest, false},
		{"Prism is admin only", `{"extra":{"openai_prism_browser":true}}`, "17", 7, http.StatusBadRequest, false},
		{"BPS network is admin only", `{"extra":{"openai_excel_bps_proxy_source":"ip_pool"}}`, "17", 7, http.StatusBadRequest, false},
		{"trailing JSON", `{} {}`, "17", 7, http.StatusBadRequest, false},
		{"size limit", `{"name":"` + strings.Repeat("x", 128*1024) + `"}`, "17", 7, http.StatusBadRequest, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := &hostedSettingsRepo{}
			h := &HostingHandler{hosting: service.NewHostingService(repo, &hostedGatewaySettings{}, nil, nil, nil, nil)}
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPut, "/api/v1/hosting/accounts/"+test.id+"/settings", strings.NewReader(test.body))
			c.Params = gin.Params{{Key: "id", Value: test.id}}
			if test.userID != 0 {
				c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: test.userID})
			}
			h.UpdateAccountSettings(c)
			require.Equal(t, test.status, w.Code, w.Body.String())
			require.Equal(t, test.updated, repo.updated)
			require.NotContains(t, w.Body.String(), "private-token")
		})
	}
}

func TestHostingCreationSettingsHTTPBoundary(t *testing.T) {
	for _, endpoint := range []string{"import", "oauth/complete"} {
		for _, body := range []string{
			`{"settings":{"owner_user_id":8}}`,
			`{"settings":{"proxy_id":5}}`,
			`{"content":"{}","settings":{"credentials":{"refresh_token":"forged"}}}`,
			`{"content":"{}","settings":{"extra":{"openai_prism_browser":true}}}`,
			`{"content":"{}","settings":{"extra":{"openai_excel_bps_mihomo":true}}}`,
			`{} {}`,
		} {
			t.Run(endpoint+body, func(t *testing.T) {
				h := &HostingHandler{hosting: service.NewHostingService(&hostedSettingsRepo{}, &hostedGatewaySettings{}, nil, nil, nil, nil)}
				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/hosting/"+endpoint, strings.NewReader(body))
				c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 7})
				if endpoint == "import" {
					h.Import(c)
				} else {
					h.OAuthComplete(c)
				}
				require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			})
		}
	}
}
