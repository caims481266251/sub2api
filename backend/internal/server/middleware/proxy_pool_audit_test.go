package middleware

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestProxyPoolAuditOmitsCredentialsAndPreservesHandlerBody(t *testing.T) {
	for _, test := range []struct{ method, path string }{{"POST", "/api/v1/admin/proxies/pool/preview"}, {"PUT", "/api/v1/admin/proxies/pool"}} {
		t.Run(test.method, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			repo := &auditCaptureRepository{}
			svc := service.NewAuditLogService(repo, nil)
			svc.Start()
			router := gin.New()
			router.Use(gin.HandlerFunc(NewAuditLogMiddleware(svc)))
			body := `{"revision":1,"mode":"append","text":"example.com:80:private-user:pool-secret-canary"}`
			router.Handle(test.method, test.path, func(c *gin.Context) {
				data, err := io.ReadAll(c.Request.Body)
				require.NoError(t, err)
				require.Equal(t, body, string(data))
				c.Status(http.StatusOK)
			})
			req := httptest.NewRequest(test.method, test.path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			require.Equal(t, 200, w.Code)
			svc.Stop()
			repo.mu.Lock()
			defer repo.mu.Unlock()
			require.Len(t, repo.logs, 1)
			require.Equal(t, "<credential-bearing body omitted>", repo.logs[0].RequestBody)
			require.NotContains(t, repo.logs[0].RequestBody, "pool-secret-canary")
		})
	}
}
