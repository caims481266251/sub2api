//go:build integration

package repository

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func fingerprintCandidate(t *testing.T, profile *tlsfingerprint.Profile) *service.BorrowFingerprint {
	t.Helper()
	f, err := service.NewBorrowFingerprint()
	require.NoError(t, err)
	if profile != nil {
		name := f.Profile.Name
		raw, err := json.Marshal(profile)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(raw, &f.Profile))
		f.Profile.Name = name
	}
	return f
}

func TestTicketBorrowFingerprintGlobalHistoryRejectsStructuralDuplicates(t *testing.T) {
	r, _, sources := rotationSourceFixture(t)
	old, err := r.Source(t.Context(), sources[0].ID)
	require.NoError(t, err)
	require.NotNil(t, old.Secret.Fingerprint)
	tx, err := integrationDB.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer tx.Rollback()
	require.NoError(t, lockBorrowRotation(t.Context(), tx))
	count := 0
	got, err := newBorrowFingerprintWithGenerator(t.Context(), tx, sources[1].AccountID, sources[1].PoolEntryID, func() (*service.BorrowFingerprint, error) {
		count++
		if count == 1 {
			f := fingerprintCandidate(t, old.Secret.Profile)
			// A different full hash, but the same structural fingerprint.
			f.Profile.SignatureAlgorithms[0], f.Profile.SignatureAlgorithms[1] = f.Profile.SignatureAlgorithms[1], f.Profile.SignatureAlgorithms[0]
			return f, nil
		}
		return fingerprintCandidate(t, nil), nil
	})
	require.NoError(t, err)
	require.Equal(t, 2, count)
	var reused bool
	require.NoError(t, tx.QueryRow(`SELECT (snapshot->>'reused')::boolean FROM ticket_borrow_fingerprint_history WHERE fingerprint_hash=encode(sha256(convert_to($1,'UTF8')),'hex')`, got).Scan(&reused))
	require.False(t, reused)
}

func TestTicketBorrowFingerprintBoundedFallbackPrefersUnusedBySource(t *testing.T) {
	r, _, sources := rotationSourceFixture(t)
	own, err := r.Source(t.Context(), sources[0].ID)
	require.NoError(t, err)
	other, err := r.Source(t.Context(), sources[1].ID)
	require.NoError(t, err)
	tx, err := integrationDB.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer tx.Rollback()
	require.NoError(t, lockBorrowRotation(t.Context(), tx))
	count := 0
	got, err := newBorrowFingerprintWithGenerator(t.Context(), tx, own.AccountID, own.PoolEntryID, func() (*service.BorrowFingerprint, error) {
		count++
		profile := own.Secret.Profile
		if count%2 == 0 {
			profile = other.Secret.Profile
		}
		return fingerprintCandidate(t, profile), nil
	})
	require.NoError(t, err)
	require.Equal(t, borrowFingerprintAttempts, count)
	require.NotEqual(t, own.Secret.DeviceID, got)
	var raw []byte
	require.NoError(t, tx.QueryRow(`SELECT snapshot FROM ticket_borrow_fingerprint_history WHERE fingerprint_hash=encode(sha256(convert_to($1,'UTF8')),'hex')`, got).Scan(&raw))
	var saved service.BorrowFingerprint
	require.NoError(t, json.Unmarshal(raw, &saved))
	require.True(t, saved.Reused)
	require.NoError(t, service.ValidateBorrowFingerprint(&saved))
	_, expected := service.BorrowTLSFingerprintHashes(other.Secret.Profile)
	_, actual := service.BorrowTLSFingerprintHashes(saved.Profile)
	require.Equal(t, expected, actual)
}

func TestTicketBorrowFingerprintRejectsInvalidCandidateAndDeviceReuse(t *testing.T) {
	r, _, sources := rotationSourceFixture(t)
	source, err := r.Source(t.Context(), sources[0].ID)
	require.NoError(t, err)
	tx, err := integrationDB.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer tx.Rollback()
	require.NoError(t, lockBorrowRotation(t.Context(), tx))
	_, err = newBorrowFingerprintWithGenerator(t.Context(), tx, source.AccountID, source.PoolEntryID, func() (*service.BorrowFingerprint, error) {
		return source.Secret.Fingerprint, nil
	})
	require.ErrorIs(t, err, service.ErrBorrowRotationNoFingerprint)
	_, err = newBorrowFingerprintWithGenerator(t.Context(), tx, source.AccountID, source.PoolEntryID, func() (*service.BorrowFingerprint, error) {
		f := fingerprintCandidate(t, nil)
		f.Profile.Extensions = nil
		return f, nil
	})
	require.ErrorIs(t, err, service.ErrBorrowFingerprintInvalid)
}

func TestTicketBorrowFingerprintSnapshotsPersistRotateAndFailClosed(t *testing.T) {
	r, _, sources := rotationSourceFixture(t)
	first, err := r.Source(t.Context(), sources[0].ID)
	require.NoError(t, err)
	restarted := NewTicketBorrowRepository(integrationDB, r.encryptor)
	again, err := restarted.Source(t.Context(), first.ID)
	require.NoError(t, err)
	require.Equal(t, first.Secret.Fingerprint, again.Secret.Fingerprint)
	job, err := r.QueueRotation(t.Context(), rotationInput("source", sources[0].AccountID, sources[1].AccountID), "manual")
	require.NoError(t, err)
	require.NoError(t, r.processRotation(t.Context(), job.ID))
	changed, err := r.Source(t.Context(), first.ID)
	require.NoError(t, err)
	require.NotEqual(t, first.Secret.DeviceID, changed.Secret.DeviceID)
	_, oldStructure := service.BorrowTLSFingerprintHashes(first.Secret.Profile)
	_, newStructure := service.BorrowTLSFingerprintHashes(changed.Secret.Profile)
	require.NotEqual(t, oldStructure, newStructure)
	public, err := json.Marshal(changed)
	require.NoError(t, err)
	require.NotContains(t, string(public), changed.Secret.DeviceID)
	require.NotContains(t, string(public), "user_agent")
	rotationExec(t, `UPDATE ticket_borrow_fingerprint_history SET snapshot=NULL WHERE snapshot->>'device_id'=$1`, changed.Secret.DeviceID)
	_, err = r.Source(t.Context(), first.ID)
	require.ErrorIs(t, err, service.ErrBorrowUnavailable)
	// A bad source snapshot does not hide other sources or revert to v1.
	_, err = r.Source(t.Context(), sources[1].ID)
	require.NoError(t, err)
	rotationExec(t, `UPDATE ticket_borrow_proxy_bindings SET fingerprint_version=1 WHERE account_id=$1`, first.AccountID)
	_, err = r.Source(t.Context(), first.ID)
	require.ErrorIs(t, err, service.ErrBorrowUnavailable, "legacy bindings also require a complete persisted fingerprint")
}

func TestTicketBorrowFingerprintBrokenPlanPreservesCurrentBatch(t *testing.T) {
	r, _, sources := rotationSourceFixture(t)
	before := map[int64]string{}
	for _, source := range sources {
		full, err := r.Source(t.Context(), source.ID)
		require.NoError(t, err)
		before[source.ID] = full.Secret.DeviceID
	}
	job, err := r.QueueRotation(t.Context(), rotationInput("source", sources[0].AccountID, sources[1].AccountID), "manual")
	require.NoError(t, err)
	rotationExec(t, `UPDATE ticket_borrow_fingerprint_history SET snapshot=NULL WHERE snapshot->>'device_id' IN (SELECT device_id FROM ticket_borrow_rotation_assignments WHERE job_id=$1)`, job.ID)
	require.NoError(t, r.processRotation(t.Context(), job.ID))
	require.Equal(t, "failed", rotationPhase(t, job.ID))
	for _, source := range sources {
		full, err := r.Source(t.Context(), source.ID)
		require.NoError(t, err)
		require.Equal(t, before[source.ID], full.Secret.DeviceID)
	}
}
