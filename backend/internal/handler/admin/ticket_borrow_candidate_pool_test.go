package admin

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type poolAdminRepo struct {
	service.TicketBorrowRepository
	saved *service.BorrowCandidatePoolConfig
	query service.BorrowCandidatePoolQuery
}

func (r *poolAdminRepo) SaveCandidatePoolConfig(_ context.Context, c service.BorrowCandidatePoolConfig) (*service.BorrowCandidatePoolConfig, error) {
	if c.Revision != 1 {
		return nil, service.ErrBorrowConflict
	}
	r.saved = &c
	return &c, nil
}
func (r *poolAdminRepo) CandidatePoolPage(_ context.Context, q service.BorrowCandidatePoolQuery) (*service.BorrowCandidatePoolPage, error) {
	r.query = q
	return &service.BorrowCandidatePoolPage{Items: []service.BorrowCandidatePoolItem{}}, nil
}
func (r *poolAdminRepo) CandidatePoolDetail(context.Context, string) (*service.BorrowCandidatePoolDetail, error) {
	return nil, service.ErrBorrowPoolNotFound
}

func TestBorrowPoolAdminHTTPValidation(t *testing.T) {
	for _, tc := range []struct {
		method, path, body string
		status             int
	}{
		{"PUT", "/pool/config", `{"capacity":64,"revision":1}`, 200},
		{"PUT", "/pool/config", `{"capacity":1,"revision":1}`, 200},
		{"PUT", "/pool/config", `{"capacity":1024,"revision":1}`, 200},
		{"PUT", "/pool/config", `{"capacity":0,"revision":1}`, 400},
		{"PUT", "/pool/config", `{"capacity":-1,"revision":1}`, 400},
		{"PUT", "/pool/config", `{"capacity":1025,"revision":1}`, 400},
		{"PUT", "/pool/config", `{"capacity":64.5,"revision":1}`, 400},
		{"PUT", "/pool/config", `{"capacity":"64","revision":1}`, 400},
		{"PUT", "/pool/config", `{"capacity":64}`, 400},
		{"PUT", "/pool/config", `{"capacity":64,"revision":2}`, 409},
		{"GET", "/pool", "", 200},
		{"GET", "/pool?state=finished&source_account_id=13&target_id=63&page=2&page_size=25", "", 200},
		{"GET", "/pool?state=made_up", "", 400},
		{"GET", "/pool?page=-1", "", 400},
		{"GET", "/pool?page=0", "", 400},
		{"GET", "/pool?page=1000001", "", 400},
		{"GET", "/pool?page_size=101", "", 400},
		{"GET", "/pool?source_account_id=-1", "", 400},
		{"GET", "/pool?target_id=oops", "", 400},
		{"GET", "/pool/missing", "", 404},
	} {
		t.Run(tc.method+tc.path+tc.body, func(t *testing.T) {
			r := &poolAdminRepo{}
			h := &AccountHandler{accountTestService: &service.AccountTestService{TicketBorrow: service.NewTicketBorrowService(r, nil, nil)}}
			router := gin.New()
			router.GET("/pool", h.BorrowCandidatePool)
			router.PUT("/pool/config", h.BorrowCandidatePoolConfig)
			router.GET("/pool/:candidateID", h.BorrowCandidatePoolDetail)
			request := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			request.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			router.ServeHTTP(w, request)
			require.Equal(t, tc.status, w.Code, w.Body.String())
			if tc.status != 200 {
				require.Nil(t, r.saved)
			}
			if tc.path == "/pool" {
				require.Equal(t, service.BorrowCandidatePoolQuery{State: "active", Page: 1, PageSize: 50}, r.query)
			}
		})
	}
}
