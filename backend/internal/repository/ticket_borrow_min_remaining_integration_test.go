//go:build integration

package repository

import (
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestTicketBorrowAdmissionWindowCompatibilityAndLiveSettings(t *testing.T) {
	r, id, q := borrowRecoveryFixture(t)
	ctx := t.Context()
	settings, err := r.Settings(ctx)
	require.NoError(t, err)
	require.NoError(t, r.SaveSettings(ctx, service.BorrowSettings{Revision: settings.Revision, Running: true, TTLSeconds: 230, MinRouteRemainingSeconds: new(120)}))
	settings, err = r.Settings(ctx)
	require.NoError(t, err)
	require.Equal(t, 110, settings.AdmissionSeconds(), "legacy TTL minus floor translates at the settings boundary")
	settings.AdmissionWindowSeconds, settings.TTLSeconds, settings.MinRouteRemainingSeconds = new(80), 0, nil
	require.NoError(t, r.SaveSettings(ctx, settings))
	renewBorrowTestJob(t, r, q)
	now, err := r.Now(ctx)
	require.NoError(t, err)
	start := now.Add(-12 * time.Second)
	route := &service.BorrowRoute{ID: uuid.NewString(), TargetID: id, TargetRevision: q.Revision,
		AcquisitionIdentity: service.BorrowAcquisitionSelf, AcquisitionAccountID: id, AcquisitionStartedAt: &start, AdmissionUntil: now.Add(time.Hour),
		IdentityHash: "identity", CookieHash: uuid.NewString(), ExpiresAt: now.Add(time.Hour)}
	require.NoError(t, r.Publish(ctx, route, q.BatchID))
	for _, preferred := range []string{"", route.ID} {
		got, e := r.Route(ctx, id, "identity", preferred)
		require.NoError(t, e)
		require.WithinDuration(t, now.Add(80*time.Second), got.UsableUntil, time.Second, "acquisition and verification do not consume the business admission window")
	}
	settings, err = r.Settings(ctx)
	require.NoError(t, err)
	_, err = integrationDB.Exec(`UPDATE ticket_borrow_routes SET admission_started_at=clock_timestamp()-interval '12 seconds' WHERE id=$1`, route.ID)
	require.NoError(t, err)
	settings.AdmissionWindowSeconds, settings.TTLSeconds, settings.MinRouteRemainingSeconds = new(10), 0, nil
	require.NoError(t, r.SaveSettings(ctx, settings))
	fresh := NewTicketBorrowRepository(integrationDB, r.(*ticketBorrowRepository).encryptor)
	_, err = fresh.Route(ctx, id, "identity", route.ID)
	require.ErrorIs(t, err, service.ErrBorrowUnavailable, "shrinking affects old routes without restart")
	settings, err = r.Settings(ctx)
	require.NoError(t, err)
	settings.AdmissionWindowSeconds, settings.TTLSeconds, settings.MinRouteRemainingSeconds = new(240), 0, nil
	require.NoError(t, r.SaveSettings(ctx, settings))
	_, err = fresh.Route(ctx, id, "identity", route.ID)
	require.ErrorIs(t, err, service.ErrBorrowUnavailable, "a larger window cannot revive an old route")
	var unrevoked bool
	require.NoError(t, integrationDB.QueryRow(`SELECT revoked_at IS NULL FROM ticket_borrow_routes WHERE id=$1`, route.ID).Scan(&unrevoked))
	require.True(t, unrevoked, "cutoff does not destroy active response/session state")
	settings, err = r.Settings(ctx)
	require.NoError(t, err)
	settings.AdmissionWindowSeconds = new(80)
	require.EqualError(t, r.SaveSettings(ctx, settings), "borrow_admission_window_invalid", "conflicting modern and legacy inputs are rejected")
}

func TestTicketBorrowAdmissionWindowBoundRouteAndCookieDeadline(t *testing.T) {
	r, id, q := borrowRecoveryFixture(t)
	ctx := t.Context()
	now, err := r.Now(ctx)
	require.NoError(t, err)
	old := borrowAdmissionFixture(&service.BorrowRoute{ID: uuid.NewString(), TargetID: id, TargetRevision: q.Revision,
		IdentityHash: "identity", CookieHash: uuid.NewString(), ExpiresAt: now.Add(time.Hour)})
	require.NoError(t, r.Publish(ctx, old, q.BatchID))
	_, err = r.BindSession(ctx, id, "existing-session", old.ID)
	require.NoError(t, err)
	deadline, err := r.ObserveCookie(ctx, old.CookieHash, 30*time.Second)
	require.NoError(t, err)
	got, err := r.Route(ctx, id, "identity", old.ID)
	require.NoError(t, err)
	require.WithinDuration(t, deadline, got.UsableUntil, time.Millisecond)
	_, err = r.ObserveCookie(ctx, old.CookieHash, -time.Second)
	require.NoError(t, err)
	_, err = r.Route(ctx, id, "identity", old.ID)
	require.ErrorIs(t, err, service.ErrBorrowUnavailable)
	replacement := *old
	replacement.ID, replacement.CookieHash = uuid.NewString(), uuid.NewString()
	require.NoError(t, r.Publish(ctx, &replacement, q.BatchID))
	got, err = r.Route(ctx, id, "identity", "")
	require.NoError(t, err)
	require.Equal(t, replacement.ID, got.ID)
	bound, found, err := r.LookupSession(ctx, id, "existing-session")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, old.ID, bound)
	_, err = r.Route(ctx, id, "identity", bound)
	require.ErrorIs(t, err, service.ErrBorrowUnavailable, "continuations cannot silently switch routes")
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
