//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func borrowIntegrationFixture(t *testing.T) (service.TicketBorrowRepository, int64) {
	t.Helper()
	cfg := &config.Config{Totp: config.TotpConfig{EncryptionKey: strings.Repeat("ab", 32), EncryptionKeyConfigured: true}}
	aes, e := NewAESEncryptor(cfg)
	require.NoError(t, e)
	r := NewTicketBorrowRepository(integrationDB, NewOpenAICredentialEncryptor(cfg, aes, integrationDB))
	a := mustCreateAccount(t, testEntClient(t), &service.Account{Name: "borrow-fixture", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Credentials: map[string]any{"access_token": "target-token"}, Status: service.StatusActive, Schedulable: true})
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM ticket_borrow_events WHERE target_id=$1`, a.ID)
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM ticket_borrow_source_attempts WHERE target_id=$1`, a.ID)
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM ticket_borrow_routes WHERE target_id=$1`, a.ID)
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM ticket_borrow_targets WHERE account_id=$1`, a.ID)
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM accounts WHERE id=$1`, a.ID)
	})
	require.NoError(t, r.SetTarget(t.Context(), a.ID, true, 0))
	return r, a.ID
}

// Admission metadata shared by older persistence/session fixtures. Deadline
// boundary tests set their own start and cutoff explicitly.
func borrowAdmissionFixture(route *service.BorrowRoute) *service.BorrowRoute {
	start := time.Now()
	route.AcquisitionStartedAt = &start
	route.AdmissionUntil = start.Add(80 * time.Second)
	if route.SourceID > 0 {
		route.AcquisitionIdentity = service.BorrowAcquisitionSource
	} else {
		route.AcquisitionIdentity = service.BorrowAcquisitionSelf
		route.AcquisitionAccountID = route.TargetID
	}
	return route
}

func TestTicketBorrowPersistenceExpiryAndFencing(t *testing.T) {
	r, id := borrowIntegrationFixture(t)
	ctx := t.Context()
	target, e := r.Target(ctx, id)
	require.NoError(t, e)
	ok, e := r.AcquireJob(ctx, "target", id, "lease-one")
	require.NoError(t, e)
	require.True(t, ok)
	now, e := r.Now(ctx)
	require.NoError(t, e)
	route := borrowAdmissionFixture(&service.BorrowRoute{ID: uuid.NewString(), TargetID: id, TargetRevision: target.Revision, IdentityHash: "identity", CookieHash: uuid.NewString(), Gateway: "chat.gateway.unified-1.api.openai.com", ExpiresAt: now.Add(3 * time.Minute)})
	require.NoError(t, r.Publish(ctx, route, "lease-one"))
	got, e := r.Route(ctx, id, "identity", "")
	require.NoError(t, e)
	require.Equal(t, route.ID, got.ID)
	// Pausing acquisition does not invalidate a live route.
	settings, e := r.Settings(ctx)
	require.NoError(t, e)
	t.Cleanup(func() {
		current, _ := r.Settings(context.Background())
		settings.Revision = current.Revision
		_ = r.SaveSettings(context.Background(), settings)
	})
	paused := settings
	paused.Running = false
	require.NoError(t, r.SaveSettings(ctx, paused))
	_, e = r.Route(ctx, id, "identity", route.ID)
	require.NoError(t, e)
	// Expiry is enforced by authoritative time even before storage cleanup.
	_, e = integrationDB.ExecContext(ctx, `UPDATE ticket_borrow_routes SET expires_at=NOW() WHERE id=$1`, route.ID)
	require.NoError(t, e)
	_, e = r.Route(ctx, id, "identity", route.ID)
	require.ErrorIs(t, e, service.ErrBorrowUnavailable)
	route.ID = uuid.NewString()
	route.CookieHash = uuid.NewString()
	route.ExpiresAt = now.Add(time.Minute)
	require.NoError(t, r.SetTarget(ctx, id, false, target.Revision))
	require.ErrorIs(t, r.Publish(ctx, route, "lease-one"), service.ErrBorrowConflict)
}
func TestTicketBorrowOnlyOneWorkerAcrossConcurrentRequests(t *testing.T) {
	r, id := borrowIntegrationFixture(t)
	var successes atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, e := r.AcquireJob(t.Context(), "target", id, uuid.NewString())
			require.NoError(t, e)
			if ok {
				successes.Add(1)
			}
		}()
	}
	wg.Wait()
	require.EqualValues(t, 1, successes.Load())
}
func TestTicketBorrowCookieCannotBeRenewedByAnotherSource(t *testing.T) {
	r, _ := borrowIntegrationFixture(t)
	hash := uuid.NewString()
	t.Cleanup(func() {
		_, _ = integrationDB.Exec(`DELETE FROM ticket_borrow_cookie_deadlines WHERE cookie_hash=$1`, hash)
	})
	first, e := r.ObserveCookie(t.Context(), hash, time.Second)
	require.NoError(t, e)
	next, e := r.ObserveCookie(t.Context(), hash, 230*time.Second)
	require.NoError(t, e)
	require.Equal(t, first, next)
}

func TestTicketBorrowTransferFencesOldCredentialsAndDuplicateOwners(t *testing.T) {
	r, id := borrowIntegrationFixture(t)
	ctx := t.Context()
	target, e := r.Target(ctx, id)
	require.NoError(t, e)
	require.NoError(t, r.SetTarget(ctx, id, false, target.Revision))
	subject := uuid.NewString()
	creds := map[string]any{"access_token": "private-token", "refresh_token": "private-refresh", "chatgpt_account_id": subject}
	raw, e := json.Marshal(creds)
	require.NoError(t, e)
	_, e = integrationDB.ExecContext(ctx, `UPDATE accounts SET credentials=$2::jsonb WHERE id=$1`, id, string(raw))
	require.NoError(t, e)
	source := &service.BorrowSource{Name: "transferred-source", Model: "gpt-6-luna", Enabled: true, SubjectHash: uuid.NewString(), Secret: service.BorrowSourceSecret{Credentials: creds}}
	require.NoError(t, r.TransferSource(ctx, &service.Account{ID: id, Credentials: creds}, source))
	t.Cleanup(func() {
		_, _ = integrationDB.Exec(`DELETE FROM ticket_borrow_transfers WHERE account_id=$1`, id)
		_, _ = integrationDB.Exec(`DELETE FROM ticket_borrow_sources WHERE id=$1`, source.ID)
	})
	saved, e := r.Source(ctx, source.ID)
	require.NoError(t, e)
	require.Equal(t, creds, saved.Secret.Credentials)
	var archived bool
	var stored string
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT deleted_at IS NOT NULL,credentials::text FROM accounts WHERE id=$1`, id).Scan(&archived, &stored))
	require.True(t, archived)
	require.Equal(t, "{}", stored)
	_, e = integrationDB.ExecContext(ctx, `UPDATE accounts SET credentials=$2::jsonb WHERE id=$1`, id, string(raw))
	require.Error(t, e)
	_, e = integrationDB.ExecContext(ctx, `UPDATE accounts SET deleted_at=NULL WHERE id=$1`, id)
	require.Error(t, e)
	// A later normal-account import cannot create a second refresh owner.
	_, other := borrowIntegrationFixture(t)
	_, e = integrationDB.ExecContext(ctx, `UPDATE accounts SET credentials=$2::jsonb WHERE id=$1`, other, string(raw))
	require.Error(t, e)
	ok, e := r.AcquireJob(ctx, "source", source.ID, "refresh-owner")
	require.NoError(t, e)
	require.True(t, ok)
	require.ErrorIs(t, r.RefreshSource(ctx, source.ID, source.Revision, "old-owner", source.Secret), service.ErrBorrowConflict)
	require.NoError(t, r.RefreshSource(ctx, source.ID, source.Revision, "refresh-owner", source.Secret))
}
func TestTicketBorrowSessionsReplacementSourceDeleteAndFreshRepository(t *testing.T) {
	r, id := borrowIntegrationFixture(t)
	ctx := t.Context()
	target, e := r.Target(ctx, id)
	require.NoError(t, e)
	source := &service.BorrowSource{Name: "isolated", Model: "gpt-6-luna", Enabled: true, SubjectHash: uuid.NewString(), Secret: service.BorrowSourceSecret{Credentials: map[string]any{"chatgpt_account_id": uuid.NewString(), "access_token": "private"}}}
	require.NoError(t, r.SaveSource(ctx, source))
	t.Cleanup(func() { _, _ = integrationDB.Exec(`DELETE FROM ticket_borrow_sources WHERE id=$1`, source.ID) })
	ok, e := r.AcquireJob(ctx, "target", id, "worker")
	require.NoError(t, e)
	require.True(t, ok)
	now, e := r.Now(ctx)
	require.NoError(t, e)
	old := borrowAdmissionFixture(&service.BorrowRoute{ID: uuid.NewString(), TargetID: id, TargetRevision: target.Revision, SourceID: source.ID, IdentityHash: "identity", CookieHash: uuid.NewString(), ExpiresAt: now.Add(220 * time.Second)})
	require.NoError(t, r.Publish(ctx, old, "worker"))
	bound, e := r.BindSession(ctx, id, "conversation", old.ID)
	require.NoError(t, e)
	require.Equal(t, old.ID, bound)
	replacement := *old
	replacement.ID = uuid.NewString()
	replacement.CookieHash = uuid.NewString()
	replacement.ExpiresAt = now.Add(4 * time.Minute)
	// A completed valid candidate is publishable independently of new-batch readiness.
	require.NoError(t, r.Publish(ctx, borrowAdmissionFixture(&replacement), "worker"))
	bound, e = r.BindSession(ctx, id, "conversation", replacement.ID)
	require.NoError(t, e)
	require.Equal(t, old.ID, bound)
	require.NoError(t, r.DeleteSource(ctx, source.ID, source.Revision))
	original := r.(*ticketBorrowRepository)
	fresh := NewTicketBorrowRepository(original.db, original.encryptor)
	route, e := fresh.Route(ctx, id, "identity", old.ID)
	require.NoError(t, e)
	require.Equal(t, old.ID, route.ID)
	route, e = fresh.Route(ctx, id, "identity", "")
	require.NoError(t, e)
	require.Equal(t, replacement.ID, route.ID)
	require.NoError(t, r.Revoke(ctx, id, old.ID, "target_ticket_changed"))
	route, e = fresh.Route(ctx, id, "identity", "")
	require.NoError(t, e)
	require.Equal(t, replacement.ID, route.ID)
}
func TestTicketBorrowScanRefreshPauseAndSettingsCAS(t *testing.T) {
	_, pool, _ := poolFixture(t)
	_, poolErr := pool.Import(t.Context(), service.ProxyPoolImport{Revision: 1, Mode: "append", Text: "normal-flow.example:80"})
	require.NoError(t, poolErr)
	r, id := borrowIntegrationFixture(t)
	ctx := t.Context()
	selection := borrowSelectionFixture(t, r)
	for i := 0; i < 5; i++ {
		a := borrowNormalAccountFixture(t)
		selection.AccountIDs = append(selection.AccountIDs, a.ID)
	}
	require.NoError(t, r.SaveSelection(ctx, selection))
	require.NoError(t, r.ScanTarget(ctx, id, false))
	target, e := r.Target(ctx, id)
	require.NoError(t, e)
	require.Equal(t, 5, target.ScanTotal)
	ok, e := r.AcquireJob(ctx, "target", id, "scan-worker")
	require.NoError(t, e)
	require.True(t, ok)
	require.ErrorIs(t, r.ScanTarget(ctx, id, false), service.ErrBorrowConflict)
	require.NoError(t, r.AdvanceScan(ctx, id, "scan-worker", 3))
	require.NoError(t, r.FinishJob(ctx, "target", id, "scan-worker", "no_candidate", 0))
	target, e = r.Target(ctx, id)
	require.NoError(t, e)
	require.Equal(t, 3, target.ScanOffset)
	require.NoError(t, r.RefreshTarget(ctx, id))
	target, e = r.Target(ctx, id)
	require.NoError(t, e)
	require.True(t, target.RefreshRequested)
	require.NoError(t, r.ScanTarget(ctx, id, true))
	target, e = r.Target(ctx, id)
	require.NoError(t, e)
	require.Zero(t, target.ScanTotal)
	cfg, e := r.Settings(ctx)
	require.NoError(t, e)
	restore := cfg
	t.Cleanup(func() {
		current, _ := r.Settings(context.Background())
		restore.Revision = current.Revision
		_ = r.SaveSettings(context.Background(), restore)
	})
	cfg.Running = false
	require.NoError(t, r.SaveSettings(ctx, cfg))
	require.ErrorIs(t, r.SaveSettings(ctx, cfg), service.ErrBorrowConflict)
	ok, e = r.AcquireJob(ctx, "target", id, "paused")
	require.NoError(t, e)
	require.False(t, ok)
}
