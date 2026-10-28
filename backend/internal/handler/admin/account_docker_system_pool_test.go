package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type poolAdminStub struct {
	*stubAdminService
	snapshot *service.DockerSystemPoolSnapshot
	err      error
	calls    int
	input    *service.CreateAccountInput
}

func (s *poolAdminStub) GetDockerSystemPool(context.Context) (*service.DockerSystemPoolSnapshot, error) {
	s.calls++
	return s.snapshot, s.err
}
func (s *poolAdminStub) CreateAccount(_ context.Context, input *service.CreateAccountInput) (*service.Account, error) {
	s.input = input
	profile := "ubuntu-24.04"
	return &service.Account{ID: 1, Name: input.Name, Platform: input.Platform, Type: input.Type, ExecutionMode: input.ExecutionMode, DockerSystemProfile: &profile, Credentials: input.Credentials}, nil
}
func TestDockerSystemPoolHandlerSnapshotOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &poolAdminStub{stubAdminService: newStubAdminService(), snapshot: &service.DockerSystemPoolSnapshot{Profiles: []service.DockerSystemPoolItem{{ProfileID: "ubuntu-24.04", DisplayName: "Ubuntu 24.04 LTS", Available: true, BoundAccountCount: 12}}, LegacyBoundAccountCount: 5, CountScope: "all", CheckedAt: time.Now().UTC()}}
	handler := &AccountHandler{adminService: svc}
	r := gin.New()
	r.GET("/pool", handler.GetDockerSystemPool)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/pool", nil))
	require.Equal(t, 200, w.Code)
	require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	require.Equal(t, 1, svc.calls)
	var out struct {
		Data service.DockerSystemPoolSnapshot `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	require.Equal(t, int64(12), out.Data.Profiles[0].BoundAccountCount)
	require.Equal(t, int64(5), out.Data.LegacyBoundAccountCount)
	svc.err = errors.New("fixture unavailable")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/pool", nil))
	require.NotEqual(t, 200, w.Code)
	require.NotContains(t, w.Body.String(), "bound_account_count")
}
func TestDockerSystemProfileCreateRequestPropagation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &poolAdminStub{stubAdminService: newStubAdminService()}
	handler := &AccountHandler{adminService: svc}
	r := gin.New()
	r.POST("/accounts", handler.Create)
	body := `{"name":"pool fixture","platform":"openai","type":"oauth","execution_mode":"docker_kernel","docker_system_profile":"ubuntu-24.04","credentials":{"access_token":"fixture"},"concurrency":15}`
	req := httptest.NewRequest(http.MethodPost, "/accounts", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, 200, w.Code, w.Body.String())
	require.Equal(t, "ubuntu-24.04", svc.input.DockerSystemProfile)
	require.Equal(t, 15, svc.input.Concurrency)
	require.Contains(t, w.Body.String(), `"docker_system_profile":"ubuntu-24.04"`)
}
