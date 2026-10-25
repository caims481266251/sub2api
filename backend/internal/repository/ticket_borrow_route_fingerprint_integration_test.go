//go:build integration

package repository

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestBorrowRouteFingerprintUpgradeRechecksWithoutDroppingLeases(t *testing.T) {
	r, a := candidateFixture(t)
	ctx := service.WithBorrowAcquisition(t.Context(), a)
	env, err := r.AllocateCandidateEnvironment(ctx, uuid.NewString(), "fingerprint-upgrade")
	require.NoError(t, err)
	legacy := *env.Fingerprint
	legacy.Generator = "borrow-h1-v2"
	profile := *legacy.Profile
	profile.Name = "borrow-h1-v2" + profile.Name[len(service.BorrowFingerprintGenerator):]
	profile.ALPNProtocols = []string{"http/1.1"}
	legacy.Profile = &profile
	raw, err := json.Marshal(legacy)
	require.NoError(t, err)
	rotationExec(t, `UPDATE ticket_borrow_candidate_environments SET fingerprint=$2 WHERE id=$1`, env.ID, raw)
	secret, err := r.seal(service.BorrowRouteSecret{})
	require.NoError(t, err)
	id := uuid.NewString()
	rotationExec(t, `INSERT INTO ticket_borrow_routes(id,target_id,target_revision,identity_hash,cookie_hash,gateway,secret,expires_at,admission_until,candidate_id,state,quality_result,quality_valid_until)
 VALUES($1::uuid,$2,$3,$4,$1::text,'gateway.example',$5,clock_timestamp()+INTERVAL '1 hour',clock_timestamp()+INTERVAL '1 hour',$6,'normal','{"status":"passed","answer":"21"}',clock_timestamp()+INTERVAL '30 seconds')`, id, a.TargetID, a.Revision, a.Identity, secret, env.ID)
	lease := uuid.NewString()
	_, err = r.ReserveRoute(ctx, a.TargetID, a.Identity, id, "", lease)
	require.NoError(t, err)
	data, err := migrations.FS.ReadFile("307_ticket_borrow_route_fingerprint.sql")
	require.NoError(t, err)
	sharedConcurrency, err := migrations.FS.ReadFile("309_ticket_borrow_shared_account_concurrency.sql")
	require.NoError(t, err)
	// Recreate the pre-309 schema while testing the historical upgrade, then
	// restore the current schema before exercising the repository below.
	tx, err := integrationDB.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.Exec(`ALTER TABLE ticket_borrow_routes ADD COLUMN dispatch_limit INT NOT NULL DEFAULT 1 CHECK(dispatch_limit>0)`)
	require.NoError(t, err)
	_, err = tx.Exec(string(data))
	require.NoError(t, err)
	_, err = tx.Exec(string(sharedConcurrency))
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	var state, answer string
	var valid *time.Time
	require.NoError(t, integrationDB.QueryRow(`SELECT state,quality_result->>'answer',quality_valid_until FROM ticket_borrow_routes WHERE id=$1`, id).Scan(&state, &answer, &valid))
	require.Equal(t, "pending_quality", state)
	require.Equal(t, "21", answer, "historical evidence remains intact")
	require.Nil(t, valid)
	var count int
	require.NoError(t, integrationDB.QueryRow(`SELECT count(*) FROM ticket_borrow_route_leases WHERE id=$1 AND lease_until>clock_timestamp()`, lease).Scan(&count))
	require.Equal(t, 1, count)
	_, err = r.Route(ctx, a.TargetID, a.Identity, id)
	require.Error(t, err, "old quality pass must not authorize the new environment")
	var restored service.BorrowRouteSecret
	require.NoError(t, r.openRouteSecret(ctx, integrationDB, id, secret, &restored))
	require.NoError(t, service.ValidateBorrowFingerprint(restored.Fingerprint))
	require.Equal(t, legacy.DeviceID, restored.Fingerprint.DeviceID)
	require.Equal(t, legacy.Profile.CipherSuites, restored.Fingerprint.Profile.CipherSuites)
	require.Equal(t, []string{"h2"}, restored.Fingerprint.Profile.ALPNProtocols)
	require.NoError(t, r.ReleaseRoute(ctx, lease))
}

func TestBorrowCandidateClaimUsesPersistedFingerprintAfterUpgrade(t *testing.T) {
	f := newCandidatePoolFixture(t, 1, 1)
	c := f.enqueue(t, f.reserve(t))
	frozen, err := service.NewBorrowFingerprint()
	require.NoError(t, err)
	raw, err := json.Marshal(frozen)
	require.NoError(t, err)
	rotationExec(t, `UPDATE ticket_borrow_candidate_environments SET fingerprint=$2 WHERE id=$1`, c.ID, raw)
	got, err := f.r.ClaimCandidatePool(t.Context(), f.targets[0])
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, frozen, got.Environment.Fingerprint)
	require.Equal(t, c.Environment.Proxy, got.Environment.Proxy)
	require.Equal(t, c.Cookie, got.Cookie)
}
