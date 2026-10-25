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

type probePolicyAdminRepo struct {
	service.TicketBorrowRepository
	err    error
	policy service.BorrowProbePolicy
	id     int64
}

func (r *probePolicyAdminRepo) SaveProbePolicy(_ context.Context, id int64, p service.BorrowProbePolicy) error {
	r.policy = p
	r.id = id
	return r.err
}
func (r *probePolicyAdminRepo) Target(_ context.Context, id int64) (service.BorrowTarget, error) {
	p := r.policy
	p.Revision++
	return service.BorrowTarget{AccountID: id, Revision: 5, ProbePolicy: p}, nil
}
func (r *probePolicyAdminRepo) Targets(context.Context) ([]service.BorrowTarget, error) {
	return []service.BorrowTarget{{AccountID: 11, ProbePolicy: service.BorrowProbePolicy{Revision: 1, Concurrency: 3, RenewBeforeSeconds: 15}}}, nil
}

func TestTicketBorrowProbeBatchAPIValidationScopeAndRedaction(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		observer   bool
		err        error
		status     int
	}{
		{name: "valid-zero", body: `{"accounts":[{"account_id":11,"revision":1}],"concurrency":2,"renew_before_seconds":0}`, status: 200},
		{name: "missing-revision", body: `{"accounts":[{"account_id":11}],"concurrency":2}`, status: 400},
		{name: "no-fields", body: `{"accounts":[{"account_id":11,"revision":1}]}`, status: 400},
		{name: "fractional", body: `{"accounts":[{"account_id":11,"revision":1}],"concurrency":1.5}`, status: 400},
		{name: "fractional-window", body: `{"accounts":[{"account_id":11,"revision":1}],"renew_before_seconds":0.5}`, status: 400},
		{name: "observer", observer: true, status: 403},
		{name: "storage-redacted", body: `{"accounts":[{"account_id":11,"revision":1}],"concurrency":2}`, err: errors.New("SECRET_DATABASE"), status: 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &probePolicyAdminRepo{err: tc.err}
			h := &AccountHandler{accountTestService: &service.AccountTestService{TicketBorrow: service.NewTicketBorrowService(repo, nil, nil)}}
			router := gin.New()
			router.PUT("/probe-policies", h.BorrowSaveProbePolicies)
			req := httptest.NewRequest("PUT", "/probe-policies", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			if tc.observer {
				req = req.WithContext(service.WithObserverScope(req.Context(), []int64{1}))
			}
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			require.Equal(t, tc.status, w.Code)
			require.NotContains(t, w.Body.String(), "SECRET_DATABASE")
			if tc.err != nil {
				require.Contains(t, w.Body.String(), `"status":"failed"`)
			}
			if tc.name == "valid-zero" {
				require.Equal(t, service.BorrowProbePolicy{Revision: 1, Concurrency: 2, RenewBeforeSeconds: 15}, repo.policy)
			}
		})
	}
}

func TestTicketBorrowProbePolicyAPIValidationAndScope(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		observer   bool
		err        error
		status     int
	}{
		{name: "valid", body: `{"revision":1,"concurrency":3,"renew_before_seconds":15}`, status: 200},
		{name: "inherit-global", body: `{"revision":1,"use_global":true}`, status: 200},
		{name: "override-requires-values", body: `{"revision":1,"use_global":false}`, status: 400},
		{name: "zero-window", body: `{"revision":1,"concurrency":32,"renew_before_seconds":0}`, status: 200},
		{name: "automatic-window", body: `{"revision":1,"concurrency":3}`, status: 200},
		{name: "fractional", body: `{"revision":1,"concurrency":1.5,"renew_before_seconds":15}`, status: 400},
		{name: "retired-field-ignored", body: `{"revision":1,"concurrency":3,"renew_before_seconds":1.5}`, status: 200},
		{name: "zero-concurrency", body: `{"revision":1,"concurrency":0,"renew_before_seconds":15}`, status: 400},
		{name: "too-high", body: `{"revision":1,"concurrency":33,"renew_before_seconds":15}`, status: 400},
		{name: "retired-negative-ignored", body: `{"revision":1,"concurrency":3,"renew_before_seconds":-1}`, status: 200},
		{name: "observer", observer: true, status: 403},
		{name: "stale", body: `{"revision":1,"concurrency":3,"renew_before_seconds":15}`, err: service.ErrBorrowConflict, status: 409},
		{name: "storage-redacted", body: `{"revision":1,"concurrency":3,"renew_before_seconds":15}`, err: errors.New("SECRET_DATABASE"), status: 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &probePolicyAdminRepo{err: tc.err}
			h := &AccountHandler{accountTestService: &service.AccountTestService{TicketBorrow: service.NewTicketBorrowService(repo, nil, nil)}}
			router := gin.New()
			router.PUT("/:id/probe-policy", h.BorrowSaveProbePolicy)
			req := httptest.NewRequest("PUT", "/11/probe-policy", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			if tc.observer {
				req = req.WithContext(service.WithObserverScope(req.Context(), []int64{1}))
			}
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			require.Equal(t, tc.status, w.Code)
			require.NotContains(t, w.Body.String(), "SECRET_DATABASE")
			if tc.status == 200 {
				require.EqualValues(t, 11, repo.id)
				require.Equal(t, tc.name == "inherit-global", repo.policy.UseGlobal)
				require.Contains(t, w.Body.String(), `"probe_policy"`)
				require.Contains(t, w.Body.String(), `"revision":5`)
			}
		})
	}
}
