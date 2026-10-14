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

func TestTicketBorrowPoolSettingsDoNotChangePublishedDeadline(t *testing.T) {
	r, id, q := borrowRecoveryFixture(t)
	ctx := t.Context()
	now, err := r.Now(ctx)
	require.NoError(t, err)
	route := borrowAdmissionFixture(&service.BorrowRoute{ID: uuid.NewString(), TargetID: id, TargetRevision: q.Revision, IdentityHash: "identity", CookieHash: uuid.NewString(), ExpiresAt: now.Add(time.Hour)})
	require.NoError(t, r.Publish(ctx, route, q.BatchID))
	passBorrowFixtureQuality(t)
	got, err := r.Route(ctx, id, "identity", "")
	require.NoError(t, err)
	require.Equal(t, route.ExpiresAt, got.UsableUntil)
	settings, err := r.Settings(ctx)
	require.NoError(t, err)
	settings.RoutePoolSize = new(1)
	settings.RouteCooldownSeconds = new(10)
	require.NoError(t, r.SaveSettings(ctx, settings))
	got, err = r.Route(ctx, id, "identity", route.ID)
	require.NoError(t, err)
	require.Equal(t, route.ExpiresAt, got.UsableUntil)
	rotationExec(t, `UPDATE ticket_borrow_routes SET admission_until=clock_timestamp()-INTERVAL '1 second' WHERE id=$1`, route.ID)
	settings, err = r.Settings(ctx)
	require.NoError(t, err)
	settings.RoutePoolSize = new(4)
	settings.RouteCooldownSeconds = new(120)
	require.NoError(t, r.SaveSettings(ctx, settings))
	_, err = r.Route(ctx, id, "identity", route.ID)
	require.ErrorIs(t, err, service.ErrBorrowUnavailable)
}

func TestTicketBorrowAdmissionWindowBoundRouteAndCookieDeadline(t *testing.T) {
	r, id, q := borrowRecoveryFixture(t)
	ctx := t.Context()
	now, err := r.Now(ctx)
	require.NoError(t, err)
	old := borrowAdmissionFixture(&service.BorrowRoute{ID: uuid.NewString(), TargetID: id, TargetRevision: q.Revision,
		IdentityHash: "identity", CookieHash: uuid.NewString(), ExpiresAt: now.Add(time.Hour)})
	require.NoError(t, r.Publish(ctx, old, q.BatchID))
	passBorrowFixtureQuality(t)
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
	passBorrowFixtureQuality(t)
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
