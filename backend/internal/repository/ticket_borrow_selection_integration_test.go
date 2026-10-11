//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func borrowSelectionFixture(t *testing.T, r service.TicketBorrowRepository) service.BorrowSelection {
	t.Helper()
	s, e := r.Selection(t.Context())
	require.NoError(t, e)
	old := s
	t.Cleanup(func() {
		cur, _ := r.Selection(context.Background())
		old.Revision = cur.Revision
		_ = r.SaveSelection(context.Background(), old)
	})
	s.GroupIDs = []int64{}
	s.AccountIDs = []int64{}
	s.ExcludedAccountIDs = []int64{}
	s.ModelOverrides = map[int64]string{}
	return s
}
func borrowNormalAccountFixture(t *testing.T) *service.Account {
	t.Helper()
	a := mustCreateAccount(t, testEntClient(t), &service.Account{Name: "normal-borrow-" + uuid.NewString(), Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Credentials: map[string]any{"access_token": "normal-token", "chatgpt_account_id": uuid.NewString()}, Status: service.StatusActive, Schedulable: true})
	t.Cleanup(func() {
		_, _ = integrationDB.Exec(`DELETE FROM ticket_borrow_transfers WHERE account_id=$1`, a.ID)
		_, _ = integrationDB.Exec(`DELETE FROM ticket_borrow_sources WHERE account_id=$1`, a.ID)
		_, _ = integrationDB.Exec(`DELETE FROM account_groups WHERE account_id=$1`, a.ID)
		_, _ = integrationDB.Exec(`DELETE FROM accounts WHERE id=$1`, a.ID)
	})
	return a
}
func TestTicketBorrowNormalSelectionMembershipAndRouteSurvival(t *testing.T) {
	_, pool, _ := poolFixture(t)
	_, poolErr := pool.Import(t.Context(), service.ProxyPoolImport{Revision: 1, Mode: "append", Text: "normal-flow.example:80"})
	require.NoError(t, poolErr)
	r, targetID := borrowIntegrationFixture(t)
	ctx := t.Context()
	selection := borrowSelectionFixture(t, r)
	a, b := borrowNormalAccountFixture(t), borrowNormalAccountFixture(t)
	g := mustCreateGroup(t, testEntClient(t), &service.Group{Name: uuid.NewString(), Platform: service.PlatformOpenAI})
	t.Cleanup(func() {
		_, _ = integrationDB.Exec(`DELETE FROM account_groups WHERE group_id=$1`, g.ID)
		_, _ = integrationDB.Exec(`DELETE FROM groups WHERE id=$1`, g.ID)
	})
	_, e := integrationDB.Exec(`INSERT INTO account_groups(account_id,group_id) VALUES($1,$2)`, a.ID, g.ID)
	require.NoError(t, e)
	selection.GroupIDs = []int64{g.ID}
	selection.AccountIDs = []int64{a.ID, targetID}
	selection.ModelOverrides = map[int64]string{a.ID: "gpt-6-sol"}
	require.NoError(t, r.SaveSelection(ctx, selection))
	require.ErrorIs(t, r.SaveSelection(ctx, selection), service.ErrBorrowConflict)
	sources, e := r.Sources(ctx)
	require.NoError(t, e)
	require.Len(t, sources, 2)
	var source service.BorrowSource
	for _, s := range sources {
		if s.AccountID == a.ID {
			source = s
			require.True(t, s.Explicit)
			require.Equal(t, []int64{g.ID}, s.GroupIDs)
			require.Equal(t, "gpt-6-sol", s.Model)
		} else {
			require.False(t, s.Enabled)
			require.Equal(t, "source_is_target", s.UnavailableReason)
		}
	}
	var secret string
	require.NoError(t, integrationDB.QueryRow(`SELECT secret FROM ticket_borrow_sources WHERE id=$1`, source.ID).Scan(&secret))
	require.Empty(t, secret)
	_, e = integrationDB.Exec(`INSERT INTO account_groups(account_id,group_id) VALUES($1,$2)`, b.ID, g.ID)
	require.NoError(t, e)
	sources, e = r.Sources(ctx)
	require.NoError(t, e)
	require.Len(t, sources, 3)
	_, e = integrationDB.Exec(`DELETE FROM account_groups WHERE account_id=$1 AND group_id=$2`, a.ID, g.ID)
	require.NoError(t, e)
	got, e := r.Source(ctx, source.ID)
	require.NoError(t, e)
	require.True(t, got.Explicit)
	require.Empty(t, got.GroupIDs)
	tstate, e := r.Target(ctx, targetID)
	require.NoError(t, e)
	ok, e := r.AcquireJob(ctx, "target", targetID, "normal-route")
	require.NoError(t, e)
	require.True(t, ok)
	route := borrowAdmissionFixture(&service.BorrowRoute{ID: uuid.NewString(), TargetID: targetID, TargetRevision: tstate.Revision, SourceID: source.ID, IdentityHash: "identity", CookieHash: uuid.NewString(), ExpiresAt: time.Now().Add(3 * time.Minute)})
	require.NoError(t, r.Publish(ctx, route, "normal-route"))
	selection, e = r.Selection(ctx)
	require.NoError(t, e)
	selection.ExcludedAccountIDs = []int64{a.ID}
	require.NoError(t, r.SaveSelection(ctx, selection))
	_, e = r.Source(ctx, source.ID)
	require.ErrorIs(t, e, service.ErrBorrowUnavailable)
	_, e = r.Route(ctx, targetID, "identity", route.ID)
	require.NoError(t, e)
	selection, e = r.Selection(ctx)
	require.NoError(t, e)
	selection.ExcludedAccountIDs = []int64{}
	selection.AccountIDs = []int64{}
	require.NoError(t, r.SaveSelection(ctx, selection))
	_, e = r.Source(ctx, source.ID)
	require.ErrorIs(t, e, service.ErrBorrowUnavailable)
	_, e = r.Route(ctx, targetID, "identity", route.ID)
	require.NoError(t, e)
	require.NoError(t, r.ScanTarget(ctx, targetID, true))
	require.NoError(t, r.FinishJob(ctx, "target", targetID, "normal-route", "done", 0))
	require.NoError(t, r.ScanTarget(ctx, targetID, false))
	scan, e := r.Target(ctx, targetID)
	require.NoError(t, e)
	require.Equal(t, 1, scan.ScanTotal)
}
func TestTicketBorrowReturnOwnershipPreservesIDsProxyAndHistory(t *testing.T) {
	r, targetID := borrowIntegrationFixture(t)
	ctx := t.Context()
	_ = borrowSelectionFixture(t, r)
	a := borrowNormalAccountFixture(t)
	source := &service.BorrowSource{Name: a.Name, Enabled: true, Model: "gpt-6-luna", SubjectHash: uuid.NewString(), Secret: service.BorrowSourceSecret{Credentials: a.Credentials, Proxies: []string{"http://borrow-user:private-proxy@example.com:2345"}}}
	require.NoError(t, r.TransferSource(ctx, a, source))
	_, e := r.RestoreSources(ctx)
	require.ErrorIs(t, e, service.ErrBorrowConflict)
	cfg, e := r.Settings(ctx)
	require.NoError(t, e)
	original := cfg
	t.Cleanup(func() {
		cur, _ := r.Settings(context.Background())
		original.Revision = cur.Revision
		_ = r.SaveSettings(context.Background(), original)
	})
	cfg.Running = false
	require.NoError(t, r.SaveSettings(ctx, cfg))
	target, e := r.Target(ctx, targetID)
	require.NoError(t, e)
	// Preserve a pre-existing route across transfer back without granting it new TTL.
	expiry := time.Now().Add(time.Minute)
	_, e = integrationDB.Exec(`INSERT INTO ticket_borrow_routes(id,target_id,target_revision,source_id,identity_hash,cookie_hash,gateway,secret,expires_at,admission_until) VALUES($1,$2,$3,$4,'id','cookie','host','legacy-route',$5,$5)`, uuid.NewString(), targetID, target.Revision, source.ID, expiry)
	require.NoError(t, e)
	ids, e := r.RestoreSources(ctx)
	require.NoError(t, e)
	require.Equal(t, []int64{a.ID}, ids)
	ids, e = r.RestoreSources(ctx)
	require.NoError(t, e)
	require.Empty(t, ids)
	var raw []byte
	var proxyID int64
	var archived bool
	require.NoError(t, integrationDB.QueryRow(`SELECT credentials,proxy_id,deleted_at IS NOT NULL FROM accounts WHERE id=$1`, a.ID).Scan(&raw, &proxyID, &archived))
	require.False(t, archived)
	t.Cleanup(func() {
		_, _ = integrationDB.Exec(`UPDATE accounts SET proxy_id=NULL WHERE id=$1`, a.ID)
		_, _ = integrationDB.Exec(`DELETE FROM proxies WHERE id=$1`, proxyID)
	})
	var credentials map[string]any
	require.NoError(t, json.Unmarshal(raw, &credentials))
	require.Equal(t, a.Credentials, credentials)
	var secret string
	var ref int64
	require.NoError(t, integrationDB.QueryRow(`SELECT account_id,secret FROM ticket_borrow_sources WHERE id=$1`, source.ID).Scan(&ref, &secret))
	require.Equal(t, a.ID, ref)
	require.Empty(t, secret)
	require.ErrorIs(t, r.RefreshSource(ctx, source.ID, source.Revision, "old-worker", source.Secret), service.ErrBorrowConflict)
	_, e = integrationDB.Exec(`UPDATE accounts SET credentials=jsonb_set(credentials,'{access_token}','"refreshed"') WHERE id=$1`, a.ID)
	require.NoError(t, e)
	selected, e := r.Selection(ctx)
	require.NoError(t, e)
	require.Contains(t, selected.AccountIDs, a.ID)
	require.Equal(t, "gpt-6-luna", selected.ModelOverrides[a.ID])
	sources, e := r.Sources(ctx)
	require.NoError(t, e)
	require.Len(t, sources, 1)
	require.Equal(t, source.ID, sources[0].ID)
	var expiryAfter time.Time
	require.NoError(t, integrationDB.QueryRow(`SELECT expires_at FROM ticket_borrow_routes WHERE source_id=$1`, source.ID).Scan(&expiryAfter))
	require.WithinDuration(t, expiry, expiryAfter, time.Microsecond)
}
