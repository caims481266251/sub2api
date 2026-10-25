//go:build integration

package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	_ "github.com/Wei-Shaw/sub2api/ent/runtime"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type accountActivityIntegrationAdmin struct {
	service.AdminService
	accounts service.AccountRepository
}

func (s *accountActivityIntegrationAdmin) GetAccountsByIDs(ctx context.Context, ids []int64) ([]*service.Account, error) {
	return s.accounts.GetByIDs(ctx, ids)
}

// Cover the first-install schema and actual HTTP read path without calling an
// upstream model: only the completed probe is supplied through the repository.
func TestAccountActivityFirstInstallHTTPReads(t *testing.T) {
	previousMode := gin.Mode()
	gin.SetMode(gin.TestMode)
	t.Cleanup(func() { gin.SetMode(previousMode) })
	ctx := t.Context()
	db := newProxyPoolFirstInstallDB(t)
	var tables int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema='public'`).Scan(&tables))
	require.Zero(t, tables, "the handler must be tested after migrating an empty database")
	require.NoError(t, repository.ApplyMigrations(ctx, db))
	assertAllProxyPoolFirstInstallMigrations(t, db)

	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	accounts := repository.NewAccountRepository(client, db, nil)
	monitors := repository.NewAccountQualityMonitorRepository(db)
	monitor := service.NewAccountQualityMonitorService(monitors, accounts, nil)
	t.Cleanup(monitor.Stop)
	h := &AccountHandler{adminService: &accountActivityIntegrationAdmin{accounts: accounts}}
	h.SetAccountActivity(monitor, nil)
	router := gin.New()
	router.Use(h.AuthorizeObserver)
	router.GET("/api/v1/admin/accounts/:id/quality-monitor", h.GetQualityMonitor)
	router.POST("/api/v1/admin/accounts/activity/batch", h.GetAccountActivity)

	var accountID int64
	require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO accounts(name,platform,type,status,schedulable,credentials)
 VALUES('activity-first-install','openai','apikey','active',true,'{"api_key":"activity-fixture-secret"}') RETURNING id`).Scan(&accountID))
	path := fmt.Sprintf("/%d/quality-monitor", accountID)
	batchBody := fmt.Sprintf(`{"account_ids":[%d]}`, accountID)
	readDetail := func() service.AccountQualityMonitorView {
		w := activityCall(router, http.MethodGet, path, "")
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.NotContains(t, w.Body.String(), "activity-fixture-secret")
		var payload struct {
			Code int                               `json:"code"`
			Data service.AccountQualityMonitorView `json:"data"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &payload))
		require.Zero(t, payload.Code)
		return payload.Data
	}
	readBatch := func() service.AccountQualityMonitorView {
		w := activityCall(router, http.MethodPost, "/activity/batch", batchBody)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.NotContains(t, w.Body.String(), "activity-fixture-secret")
		var payload struct {
			Code int `json:"code"`
			Data struct {
				Items []accountActivityItem `json:"items"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &payload))
		require.Zero(t, payload.Code)
		require.Len(t, payload.Data.Items, 1)
		require.Equal(t, accountID, payload.Data.Items[0].AccountID)
		require.NotNil(t, payload.Data.Items[0].Quality)
		return *payload.Data.Items[0].Quality
	}

	initial := readDetail()
	require.Equal(t, accountID, initial.AccountID)
	require.Equal(t, "gpt-6.1-sol", initial.Model)
	require.Equal(t, "untested", initial.State)
	require.False(t, initial.Enabled)
	require.Equal(t, 5, initial.IntervalMinutes)
	require.NotEmpty(t, initial.Scope)
	require.Nil(t, initial.LatestAttempt)
	summary := readBatch()
	require.Equal(t, initial, summary)
	var monitorRows int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM account_quality_monitors`).Scan(&monitorRows))
	require.Zero(t, monitorRows, "opening detail and batch pages must not persist a default configuration")

	now := time.Now().UTC().Truncate(time.Microsecond)
	record, err := monitors.Save(ctx, accountID, initial.AccountQualityMonitorConfig, now)
	require.NoError(t, err)
	claimed, err := monitors.Claim(ctx, record, "first-install-probe", now, now.Add(time.Minute), false)
	require.NoError(t, err)
	require.True(t, claimed)
	attempt := &service.AccountQualityMonitorResult{State: "passed", Answer: "21", Model: initial.Model,
		CheckedAt: now, DurationMS: 1234, Text: "calculation text\n21"}
	finished, err := monitors.Finish(ctx, record, "first-install-probe", attempt, now)
	require.NoError(t, err)
	require.True(t, finished)
	require.NoError(t, monitors.Release(ctx, accountID, "first-install-probe"))

	detail := readDetail()
	require.Equal(t, "passed", detail.State)
	require.NotNil(t, detail.LatestAttempt)
	require.Equal(t, attempt, detail.LatestAttempt)
	require.NotNil(t, detail.LastValid)
	require.Equal(t, "21", detail.LastValid.Answer)
	require.Empty(t, detail.LastValid.Text)
	summary = readBatch()
	require.Equal(t, "passed", summary.State)
	require.NotNil(t, summary.LatestAttempt)
	require.Equal(t, "21", summary.LatestAttempt.Answer)
	require.Empty(t, summary.LatestAttempt.Text, "batch responses must omit full answer text")
	require.Empty(t, summary.LastValid.Text)
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM account_quality_monitors`).Scan(&monitorRows))
	require.Equal(t, 1, monitorRows)
}
