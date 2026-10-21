package securityaudit

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestPersonalSecurityRecordCannotOverrideAuthenticatedOwner(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	h := ProvidePenaltyAdminHandler(nil, service.NewSecurityPenaltyService(db, nil, nil))
	r := gin.New()
	r.GET("/security/events/:id", func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 7})
		c.Next()
	}, h.MyPenaltyEvent)
	// The authenticated user is 7 even when the query asks for owner 999.
	mock.ExpectQuery(`SELECT .*full_prompt.* FROM security_penalty_events WHERE id=\$1 AND \(\$2::bigint=0 OR user_id=\$2\)`).WithArgs(int64(12), int64(7)).WillReturnError(sql.ErrNoRows)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/security/events/12?user_id=999", nil))
	require.Equal(t, http.StatusNotFound, w.Code)
	require.NoError(t, mock.ExpectationsWereMet())
}
