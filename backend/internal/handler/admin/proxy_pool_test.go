package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type poolHandlerRepo struct {
	service.ProxyPoolRepository
	writes int
}

func (r *poolHandlerRepo) Load(context.Context) (service.ProxyPoolState, error) {
	return service.ProxyPoolState{Revision: 1, Entries: []service.ProxyPoolEntry{{ID: 1, Host: "example.com", Port: 80, Authenticated: true, URL: "http://private-user:private-password@example.com:80", Identity: "private-hash"}}}, nil
}
func (r *poolHandlerRepo) Apply(context.Context, int64, string, []service.ProxyPoolEntry) (service.ProxyPoolState, error) {
	r.writes++
	return service.ProxyPoolState{}, nil
}

type poolHandlerEncryption struct {
	service.OpenAICredentialEncryptor
}

func (poolHandlerEncryption) EncryptionStatus() (service.CredentialEncryptionStatus, error) {
	return service.CredentialEncryptionStatus{Configured: true}, nil
}
func TestProxyPoolHandlerRedactsStateAndRejectsInvalidOrObserverWrites(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := &poolHandlerRepo{}
	h := &ProxyHandler{pool: service.NewProxyPoolService(r, poolHandlerEncryption{})}
	t.Run("state", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/pool", nil)
		h.PoolState(c)
		require.Equal(t, 200, w.Code)
		require.NotContains(t, w.Body.String(), "private-")
		require.Contains(t, w.Body.String(), "example.com")
	})
	for _, test := range []struct {
		name, body string
		observer   bool
		status     int
	}{
		{"malformed", `{"text":"private-password"`, false, 400},
		{"invalid batch", `{"revision":1,"mode":"append","text":"http://private-user:private-password@%zz:80"}`, false, 400},
		{"observer", `{"revision":1,"mode":"append","text":"example.com:80"}`, true, 403},
	} {
		t.Run(test.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPut, "/pool", strings.NewReader(test.body))
			c.Request.Header.Set("Content-Type", "application/json")
			if test.observer {
				c.Request = c.Request.WithContext(service.WithObserverScope(c.Request.Context(), []int64{1}))
			}
			h.PoolImport(c)
			require.Equal(t, test.status, w.Code)
			require.NotContains(t, w.Body.String(), "private-")
			require.Zero(t, r.writes)
		})
	}
}
