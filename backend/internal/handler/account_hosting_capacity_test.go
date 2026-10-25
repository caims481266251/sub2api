package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type hostedCapacityStub struct {
	service.RPMCache
	ids []int64
	err error
}

func (s *hostedCapacityStub) GetAccountConcurrencyBatch(_ context.Context, ids []int64) (map[int64]int, error) {
	s.ids = append([]int64{}, ids...)
	return map[int64]int{17: 2, 99: 5}, s.err
}
func (s *hostedCapacityStub) GetRPMBatch(_ context.Context, ids []int64) (map[int64]int, error) {
	s.ids = append([]int64{}, ids...)
	return map[int64]int{17: 4, 99: 9}, s.err
}
func TestHostingCapacityScopeAndUnavailable(t *testing.T) {
	reader := &hostedCapacityStub{}
	h := &HostingHandler{concurrency: reader, rpm: reader}
	accounts := []*service.Account{{ID: 17}, {ID: 18}}
	result := h.hostedCapacity(context.Background(), accounts)
	require.Equal(t, []int64{17, 18}, reader.ids)
	require.Len(t, result, 2)
	require.Equal(t, 2, *result[17].Concurrency)
	require.Equal(t, 4, *result[17].RPM)
	require.Zero(t, *result[18].Concurrency)
	reader.err = errors.New("cache unavailable")
	result = h.hostedCapacity(context.Background(), accounts)
	require.Nil(t, result[17].Concurrency)
	require.Nil(t, result[17].RPM)
}

type hostedAdminMutationRepo struct {
	hostedSettingsRepo
	owner  int64
	action string
}

func (r *hostedAdminMutationRepo) ChangeOwned(_ context.Context, owner, id int64, action string) error {
	r.owner = owner
	r.action = action
	return nil
}
func TestHostingAdminAccountManagementUsesStoredOwner(t *testing.T) {
	for _, platform := range []bool{false, true} {
		for _, operation := range []string{"settings", "pause", "resume", "delete", "unsupported"} {
			t.Run(operation, func(t *testing.T) {
				account := hostedQuotaFixture()
				if platform {
					account.OwnerUserID = nil
				}
				repo := &hostedAdminMutationRepo{}
				h := &HostingHandler{accounts: &hostingQuotaRepoStub{account: account}, hosting: service.NewHostingService(repo, &hostedGatewaySettings{}, nil, nil, nil, nil)}
				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 1})
				c.Params = gin.Params{{Key: "id", Value: "17"}, {Key: "action", Value: operation}}
				method := http.MethodPost
				if operation == "delete" {
					method = http.MethodDelete
				}
				c.Request = httptest.NewRequest(method, "/admin/hosting/accounts/17/"+operation, strings.NewReader(`{"concurrency":8}`))
				if operation == "settings" {
					h.AdminAccountSettings(c)
				} else {
					h.AdminChangeAccount(c)
				}
				if platform {
					require.Equal(t, http.StatusNotFound, w.Code)
					require.False(t, repo.updated)
					require.Zero(t, repo.owner)
				} else if operation == "unsupported" {
					require.Equal(t, http.StatusBadRequest, w.Code)
					require.Zero(t, repo.owner)
				} else {
					require.Equal(t, http.StatusOK, w.Code, w.Body.String())
					if operation == "settings" {
						require.True(t, repo.updated)
					} else {
						require.Equal(t, int64(7), repo.owner)
						require.Equal(t, operation, repo.action)
					}
				}
				require.NotContains(t, w.Body.String(), "private-token")
			})
		}
	}
}
