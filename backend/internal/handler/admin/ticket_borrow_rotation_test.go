package admin

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type rotationAdminRepo struct {
	borrowAdminRepo
	service.BorrowRotationRepository
}

func (r rotationAdminRepo) QueueRotation(context.Context, service.BorrowRotationInput, string) (*service.BorrowRotationJob, error) {
	return nil, r.err
}
func (r rotationAdminRepo) RotationStates(context.Context) ([]service.BorrowRotationState, error) {
	return []service.BorrowRotationState{}, r.err
}
func (r rotationAdminRepo) SaveRotationPolicy(context.Context, int64, service.BorrowRotationPolicy) error {
	return r.err
}
func TestTicketBorrowRotationAdminObserverValidationAndRedaction(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, body string
		observer                 bool
		err                      error
		status                   int
	}{
		{name: "observer-list", method: "GET", path: "/rotations", observer: true, status: 403},
		{name: "observer-rotate", method: "POST", path: "/rotations", observer: true, status: 403},
		{name: "observer-policy", method: "PUT", path: "/11/rotation", observer: true, status: 403},
		{name: "observer-batch", method: "PUT", path: "/rotations", observer: true, status: 403},
		{name: "batch-success", method: "PUT", path: "/rotations", body: `{"accounts":[{"account_id":11,"revision":0}],"automatic":true,"interval_seconds":300}`, status: 410},
		{name: "batch-missing-revision", method: "PUT", path: "/rotations", body: `{"accounts":[{"account_id":11}],"automatic":true}`, status: 410},
		{name: "batch-no-fields", method: "PUT", path: "/rotations", body: `{"accounts":[{"account_id":11,"revision":0}]}`, status: 410},
		{name: "batch-fractional", method: "PUT", path: "/rotations", body: `{"accounts":[{"account_id":11,"revision":0}],"interval_seconds":0.5}`, status: 400},
		{name: "batch-redacted", method: "PUT", path: "/rotations", body: `{"accounts":[{"account_id":11,"revision":0}],"automatic":true}`, err: errors.New("PROXY_SECRET TOKEN_SECRET"), status: 410},
		{name: "invalid-role", method: "POST", path: "/rotations", body: `{"request_id":"507e1b69-9bd0-41b6-9ff7-3103bd473421","role":"all","account_ids":[11]}`, status: 410},
		{name: "target-rejected", method: "POST", path: "/rotations", body: `{"request_id":"507e1b69-9bd0-41b6-9ff7-3103bd473421","role":"target","account_ids":[11]}`, status: 410},
		{name: "fractional-policy", method: "PUT", path: "/11/rotation", body: `{"automatic":true,"interval_seconds":0.5}`, status: 400},
		{name: "no-alternative", method: "POST", path: "/rotations", body: `{"request_id":"507e1b69-9bd0-41b6-9ff7-3103bd473421","role":"source","account_ids":[11]}`, err: service.ErrBorrowRotationNoProxy, status: 410},
		{name: "redacted", method: "GET", path: "/rotations", err: errors.New("PROXY_SECRET TOKEN_SECRET"), status: 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := rotationAdminRepo{borrowAdminRepo: borrowAdminRepo{err: tc.err}}
			h := &AccountHandler{accountTestService: &service.AccountTestService{TicketBorrow: service.NewTicketBorrowService(repo, nil, nil)}}
			router := gin.New()
			router.GET("/rotations", h.BorrowRotations)
			router.POST("/rotations", h.BorrowRotations)
			router.PUT("/rotations", h.BorrowRotationPolicies)
			router.PUT("/:id/rotation", h.BorrowRotationPolicy)
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			if tc.observer {
				req = req.WithContext(service.WithObserverScope(req.Context(), []int64{1}))
			}
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			require.Equal(t, tc.status, w.Code)
			require.NotContains(t, w.Body.String(), "PROXY_SECRET")
			require.NotContains(t, w.Body.String(), "TOKEN_SECRET")
		})
	}
}
