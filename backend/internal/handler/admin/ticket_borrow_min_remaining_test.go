package admin

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type borrowSettingsAdminRepo struct {
	service.TicketBorrowRepository
	saved *service.BorrowSettings
}

func (r *borrowSettingsAdminRepo) SaveSettings(_ context.Context, settings service.BorrowSettings) error {
	r.saved = &settings
	return nil
}

func (r *borrowSettingsAdminRepo) Settings(context.Context) (service.BorrowSettings, error) {
	if r.saved != nil {
		return *r.saved, nil
	}
	return service.BorrowSettings{}, nil
}

func TestTicketBorrowAdminPoolValidation(t *testing.T) {
	for _, tc := range []struct {
		field, value string
		status       int
	}{
		{"route_pool_size", "4", 200}, {"route_pool_size", "0", 400}, {"route_pool_size", "1.5", 400}, {"route_cooldown_seconds", "120", 200}, {"route_cooldown_seconds", "-1", 400},
	} {
		t.Run(tc.field+tc.value, func(t *testing.T) {
			r := &borrowSettingsAdminRepo{}
			h := &AccountHandler{accountTestService: &service.AccountTestService{TicketBorrow: service.NewTicketBorrowService(r, nil, nil)}}
			router := gin.New()
			router.PUT("/settings", h.BorrowSettings)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest("PUT", "/settings", strings.NewReader(fmt.Sprintf(`{"revision":1,"%s":%s}`, tc.field, tc.value))))
			require.Equal(t, tc.status, w.Code)
		})
	}
}

func TestTicketBorrowAdminGlobalProbeValidation(t *testing.T) {
	for _, tc := range []struct {
		body   string
		status int
	}{
		{`"probe_concurrency":2,"renew_before_seconds":150`, 200},
		{`"probe_concurrency":32,"renew_before_seconds":0`, 200},
		{`"probe_concurrency":0`, 400}, {`"probe_concurrency":33`, 400},
		{`"probe_concurrency":1.5`, 400}, {`"renew_before_seconds":-1`, 200},
		{`"renew_before_seconds":241`, 200}, {`"renew_before_seconds":1.5`, 200},
	} {
		t.Run(tc.body, func(t *testing.T) {
			r := &borrowSettingsAdminRepo{}
			h := &AccountHandler{accountTestService: &service.AccountTestService{TicketBorrow: service.NewTicketBorrowService(r, nil, nil)}}
			router := gin.New()
			router.PUT("/settings", h.BorrowSettings)
			req := httptest.NewRequest("PUT", "/settings", strings.NewReader(`{"revision":1,`+tc.body+`}`))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			require.Equal(t, tc.status, w.Code)
			if tc.status != 200 {
				require.Nil(t, r.saved)
			}
		})
	}
}

func TestTicketBorrowAdminContinueTimeoutValidation(t *testing.T) {
	for _, tc := range []struct {
		value  string
		status int
	}{{"1", 200}, {"7", 200}, {"45", 200}, {"0", 400}, {"-1", 400}, {"46", 400}, {"7.5", 400}, {`"7"`, 400}} {
		t.Run(tc.value, func(t *testing.T) {
			r := &borrowSettingsAdminRepo{}
			h := &AccountHandler{accountTestService: &service.AccountTestService{TicketBorrow: service.NewTicketBorrowService(r, nil, nil)}}
			router := gin.New()
			router.PUT("/settings", h.BorrowSettings)
			req := httptest.NewRequest("PUT", "/settings", strings.NewReader(fmt.Sprintf(`{"revision":1,"continue_timeout_seconds":%s}`, tc.value)))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			require.Equal(t, tc.status, w.Code)
			if tc.status == 200 {
				require.NotNil(t, r.saved.ContinueTimeoutSeconds)
			} else {
				require.Nil(t, r.saved)
			}
		})
	}
}
