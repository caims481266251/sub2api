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

type borrowAdminRepo struct {
	service.TicketBorrowRepository
	err error
}

func (r borrowAdminRepo) Sources(context.Context) ([]service.BorrowSource, error) {
	return []service.BorrowSource{{Name: "source", Secret: service.BorrowSourceSecret{Credentials: map[string]any{"access_token": "TOKEN_SECRET"}, Proxies: []string{"http://user:PROXY_SECRET@proxy"}}}}, r.err
}
func TestTicketBorrowAdminSourceDTOAndErrorRedaction(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "dto", true: "error"}[fail], func(t *testing.T) {
			repo := borrowAdminRepo{}
			if fail {
				repo.err = errors.New("TOKEN_SECRET PROXY_SECRET")
			}
			h := &AccountHandler{accountTestService: &service.AccountTestService{TicketBorrow: service.NewTicketBorrowService(repo, nil, nil)}}
			router := gin.New()
			router.GET("/sources", h.BorrowSources)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest("GET", "/sources", nil))
			require.NotContains(t, w.Body.String(), "TOKEN_SECRET")
			require.NotContains(t, w.Body.String(), "PROXY_SECRET")
			if fail {
				require.Equal(t, 503, w.Code)
			} else {
				require.Equal(t, 200, w.Code)
			}
		})
	}
}
func TestTicketBorrowAdminRequiresExplicitRevisionAndTransferConsent(t *testing.T) {
	h := &AccountHandler{accountTestService: &service.AccountTestService{TicketBorrow: service.NewTicketBorrowService(borrowAdminRepo{}, nil, nil)}}
	router := gin.New()
	router.PUT("/targets/:id", h.BorrowTarget)
	router.POST("/transfer", h.BorrowTransfer)
	for _, item := range []struct{ method, path, body string }{{"PUT", "/targets/11", `{"enabled":true}`}, {"POST", "/transfer", `{"account_id":11}`}} {
		req := httptest.NewRequest(item.method, item.path, strings.NewReader(item.body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		require.Equal(t, 400, w.Code)
	}
}
