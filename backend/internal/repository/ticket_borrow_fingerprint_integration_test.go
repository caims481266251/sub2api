//go:build integration

package repository

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestTicketBorrowCandidateFingerprintSnapshotsAreFreshAndPersistent(t *testing.T) {
	r, a := candidateFixture(t)
	ctx := service.WithBorrowAcquisition(t.Context(), a)
	seen := map[string]bool{}
	for range 8 {
		env, err := r.AllocateCandidateEnvironment(ctx, uuid.NewString(), "fingerprint-subject")
		require.NoError(t, err)
		require.NoError(t, service.ValidateBorrowFingerprint(env.Fingerprint))
		require.False(t, seen[env.Fingerprint.DeviceID], "every candidate gets a fresh device identity")
		seen[env.Fingerprint.DeviceID] = true
		var raw []byte
		var ciphertext string
		require.NoError(t, integrationDB.QueryRow(`SELECT fingerprint,proxy_ciphertext FROM ticket_borrow_candidate_environments WHERE id=$1`, env.ID).Scan(&raw, &ciphertext))
		var saved service.BorrowFingerprint
		require.NoError(t, json.Unmarshal(raw, &saved))
		require.Equal(t, env.Fingerprint, &saved)
		require.NotEqual(t, env.Proxy, ciphertext)
		restarted := NewTicketBorrowRepository(integrationDB, r.encryptor).(*ticketBorrowRepository)
		proxy, err := restarted.encryptor.Decrypt(ciphertext)
		require.NoError(t, err)
		require.Equal(t, env.Proxy, proxy)
		_, _, err = restarted.StartCandidateEnvironment(ctx, env.ID)
		require.NoError(t, err)
		require.NoError(t, restarted.MarkCandidateEnvironmentSent(ctx, env.ID))
		require.NoError(t, restarted.ReleaseCandidateEnvironment(ctx, env.ID))
	}
	var bindings int
	require.NoError(t, integrationDB.QueryRow(`SELECT count(*) FROM ticket_borrow_proxy_bindings WHERE account_id=$1`, a.TargetID).Scan(&bindings))
	require.Zero(t, bindings)
}
