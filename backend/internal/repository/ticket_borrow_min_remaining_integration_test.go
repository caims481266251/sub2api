//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func borrowSetAdmissionFloor(t *testing.T, r service.TicketBorrowRepository, seconds int) {
	t.Helper()
	original, err := r.Settings(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() {
		current, e := r.Settings(context.Background())
		if e == nil {
			current.MinRouteRemainingSeconds = original.MinRouteRemainingSeconds
			_ = r.SaveSettings(context.Background(), current)
		}
	})
	updated := original
	updated.MinRouteRemainingSeconds = &seconds
	require.NoError(t, r.SaveSettings(t.Context(), updated))
}

func TestTicketBorrowMinimumRemainingAdmissionAndLiveSettings(t *testing.T) {
	r, id, q := borrowRecoveryFixture(t)
	ctx := t.Context()
	settings, err := r.Settings(ctx)
	require.NoError(t, err)
	require.NotNil(t, settings.MinRouteRemainingSeconds)
	require.Equal(t, 120, *settings.MinRouteRemainingSeconds)
	now, err := r.Now(ctx)
	require.NoError(t, err)
	route := &service.BorrowRoute{ID: uuid.NewString(), TargetID: id, TargetRevision: q.Revision, SourceID: 1,
		IdentityHash: "identity", CookieHash: uuid.NewString(), ExpiresAt: now.Add(230 * time.Second)}
	require.NoError(t, r.Publish(ctx, route, q.BatchID))
	for _, remaining := range []int{121, 120, 119, 1} {
		_, err = integrationDB.ExecContext(ctx, `UPDATE ticket_borrow_routes SET expires_at=clock_timestamp()+$2*INTERVAL '1 second' WHERE id=$1`, route.ID, remaining)
		require.NoError(t, err)
		for _, preferred := range []string{"", route.ID} {
			got, e := r.Route(ctx, id, "identity", preferred)
			if remaining > 120 {
				require.NoError(t, e)
				require.Equal(t, 120*time.Second, got.ExpiresAt.Sub(got.UsableUntil))
			} else {
				require.ErrorIs(t, e, service.ErrBorrowUnavailable)
			}
		}
	}
	_, err = integrationDB.ExecContext(ctx, `UPDATE ticket_borrow_routes SET expires_at=clock_timestamp()+INTERVAL '90 seconds' WHERE id=$1`, route.ID)
	require.NoError(t, err)
	borrowSetAdmissionFloor(t, r, 60)
	fresh := NewTicketBorrowRepository(integrationDB, r.(*ticketBorrowRepository).encryptor)
	got, err := fresh.Route(ctx, id, "identity", route.ID)
	require.NoError(t, err, "settings take effect without restart or route republication")
	require.Equal(t, 60*time.Second, got.ExpiresAt.Sub(got.UsableUntil))
	settings, err = fresh.Settings(ctx)
	require.NoError(t, err)
	settings.MinRouteRemainingSeconds = nil
	require.NoError(t, fresh.SaveSettings(ctx, settings), "older clients may omit the new field")
	settings, err = fresh.Settings(ctx)
	require.NoError(t, err)
	require.Equal(t, 60, *settings.MinRouteRemainingSeconds)
	borrowSetAdmissionFloor(t, r, 120)
	_, err = fresh.Route(ctx, id, "identity", route.ID)
	require.ErrorIs(t, err, service.ErrBorrowUnavailable)
	borrowSetAdmissionFloor(t, r, 0)
	_, err = fresh.Route(ctx, id, "identity", route.ID)
	require.NoError(t, err, "zero explicitly restores expiry-only admission")
	var unrevoked bool
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT revoked_at IS NULL FROM ticket_borrow_routes WHERE id=$1`, route.ID).Scan(&unrevoked))
	require.True(t, unrevoked, "admission does not destroy routes or in-flight bindings")
}

func TestTicketBorrowMinimumRemainingBoundRouteAndCookieDeadline(t *testing.T) {
	r, id, q := borrowRecoveryFixture(t)
	ctx := t.Context()
	now, err := r.Now(ctx)
	require.NoError(t, err)
	old := &service.BorrowRoute{ID: uuid.NewString(), TargetID: id, TargetRevision: q.Revision, SourceID: 1,
		IdentityHash: "identity", CookieHash: uuid.NewString(), ExpiresAt: now.Add(230 * time.Second)}
	require.NoError(t, r.Publish(ctx, old, q.BatchID))
	_, err = r.BindSession(ctx, id, "existing-session", old.ID)
	require.NoError(t, err)
	_, err = r.ObserveCookie(ctx, old.CookieHash, 119*time.Second)
	require.NoError(t, err)
	_, err = r.Route(ctx, id, "identity", old.ID)
	require.ErrorIs(t, err, service.ErrBorrowUnavailable, "shorter cookie deadline governs admission")
	replacement := *old
	replacement.ID, replacement.CookieHash = uuid.NewString(), uuid.NewString()
	require.NoError(t, r.Publish(ctx, &replacement, q.BatchID), "ineligible route must not block replenishment at the smaller 15s probe window")
	got, err := r.Route(ctx, id, "identity", "")
	require.NoError(t, err)
	require.Equal(t, replacement.ID, got.ID)
	bound, found, err := r.LookupSession(ctx, id, "existing-session")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, old.ID, bound)
	_, err = r.Route(ctx, id, "identity", bound)
	require.ErrorIs(t, err, service.ErrBorrowUnavailable, "continuations cannot silently switch routes")
	require.NoError(t, r.CheckAcquisition(ctx, service.BorrowAcquisition{TargetID: id, Revision: q.Revision, Lease: q.BatchID, Identity: "identity"}), "live batch is separate from new-batch admission")
}

func TestTicketBorrowMinimumRemainingMigration(t *testing.T) {
	tx, err := integrationDB.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.Exec(`CREATE SCHEMA min_remaining_migration_fixture; SET LOCAL search_path TO min_remaining_migration_fixture;
 CREATE TABLE ticket_borrow_settings(id INTEGER PRIMARY KEY, ttl_seconds INTEGER);
 INSERT INTO ticket_borrow_settings VALUES(1,230);`)
	require.NoError(t, err)
	raw, err := migrations.FS.ReadFile("281_ticket_borrow_min_route_remaining.sql")
	require.NoError(t, err)
	_, err = tx.Exec(string(raw))
	require.NoError(t, err)
	var ttl, floor int
	require.NoError(t, tx.QueryRow(`SELECT ttl_seconds,min_route_remaining_seconds FROM ticket_borrow_settings WHERE id=1`).Scan(&ttl, &floor))
	require.Equal(t, 230, ttl)
	require.Equal(t, 120, floor)
	_, err = tx.Exec(`UPDATE ticket_borrow_settings SET min_route_remaining_seconds=60 WHERE id=1`)
	require.NoError(t, err)
	_, err = tx.Exec(string(raw))
	require.NoError(t, err)
	require.NoError(t, tx.QueryRow(`SELECT min_route_remaining_seconds FROM ticket_borrow_settings WHERE id=1`).Scan(&floor))
	require.Equal(t, 60, floor, "migration reruns preserve configured values")
}
