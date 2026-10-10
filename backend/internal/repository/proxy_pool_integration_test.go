//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func poolFixture(t *testing.T) (service.ProxyPoolRepository, *service.ProxyPoolService, service.OpenAICredentialEncryptor) {
	t.Helper()
	_, err := integrationDB.Exec(`TRUNCATE ticket_borrow_proxy_usage,ticket_borrow_fingerprint_history,ticket_borrow_rotation_assignments,ticket_borrow_rotation_accounts,ticket_borrow_rotation_jobs,ticket_borrow_proxy_bindings,proxy_pool_entries; UPDATE proxy_pool_state SET revision=1 WHERE id=1`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = integrationDB.Exec(`TRUNCATE ticket_borrow_proxy_usage,ticket_borrow_fingerprint_history,ticket_borrow_rotation_assignments,ticket_borrow_rotation_accounts,ticket_borrow_rotation_jobs,ticket_borrow_proxy_bindings,proxy_pool_entries; UPDATE proxy_pool_state SET revision=1 WHERE id=1`)
	})
	cfg := &config.Config{Totp: config.TotpConfig{EncryptionKey: strings.Repeat("ab", 32), EncryptionKeyConfigured: true}}
	aes, err := NewAESEncryptor(cfg)
	require.NoError(t, err)
	enc := NewOpenAICredentialEncryptor(cfg, aes, integrationDB)
	r := NewProxyPoolRepository(integrationDB, enc)
	return r, service.NewProxyPoolService(r, enc), enc
}
func TestProxyPoolEncryptedStorageAppendReplaceAndRestart(t *testing.T) {
	r, s, enc := poolFixture(t)
	ctx := t.Context()
	state, err := s.Import(ctx, service.ProxyPoolImport{Revision: 1, Mode: "append", Text: "a.example:80:user:private-password\nb.example:81"})
	require.NoError(t, err)
	require.Len(t, state.Entries, 2)
	first, second := state.Entries[0], state.Entries[1]
	var ciphertext string
	require.NoError(t, integrationDB.QueryRow(`SELECT credential_ciphertext FROM proxy_pool_entries WHERE id=$1`, first.ID).Scan(&ciphertext))
	require.NotContains(t, ciphertext, "private-password")
	plain, err := enc.Decrypt(ciphertext)
	require.NoError(t, err)
	require.Equal(t, "http://user:private-password@a.example:80", plain)
	state, err = s.SetEnabled(ctx, state.Revision, second.ID, false)
	require.NoError(t, err)
	revision := state.Revision
	state, err = s.Import(ctx, service.ProxyPoolImport{Revision: revision, Mode: "append", Text: "b.example:81\nhttp://user:private-password@a.example:80/"})
	require.NoError(t, err)
	require.Equal(t, revision, state.Revision)
	require.Equal(t, first.ID, state.Entries[0].ID)
	require.False(t, state.Entries[1].Enabled)
	state, err = s.Import(ctx, service.ProxyPoolImport{Revision: revision, Mode: "replace", Text: "b.example:81\nc.example:82"})
	require.NoError(t, err)
	require.Len(t, state.Entries, 2)
	require.Equal(t, second.ID, state.Entries[0].ID)
	require.False(t, state.Entries[0].Enabled)
	// A new repository instance observes the same IDs and state after a restart.
	restarted := NewProxyPoolRepository(integrationDB, enc)
	loaded, err := restarted.Load(ctx)
	require.NoError(t, err)
	require.Equal(t, state.Entries, loaded.Entries)
	_, err = r.Apply(ctx, revision, "replace", state.Entries)
	require.ErrorIs(t, err, service.ErrProxyPoolConflict)
	var retired bool
	require.NoError(t, integrationDB.QueryRow(`SELECT retired_at IS NOT NULL FROM proxy_pool_entries WHERE id=$1`, first.ID).Scan(&retired))
	require.True(t, retired)
	encoded, err := json.Marshal(loaded)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "private-password")
	require.NotContains(t, string(encoded), "credential_ciphertext")
	require.NotContains(t, string(encoded), "identity_hash")
}
func TestProxyPoolConcurrentWritersCannotOverwriteEachOther(t *testing.T) {
	_, s, _ := poolFixture(t)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, host := range []string{"one.example:80", "two.example:80"} {
		wg.Add(1)
		go func(text string) {
			defer wg.Done()
			_, err := s.Import(t.Context(), service.ProxyPoolImport{Revision: 1, Mode: "replace", Text: text})
			results <- err
		}(host)
	}
	wg.Wait()
	close(results)
	success, conflict := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else {
			require.ErrorIs(t, err, service.ErrProxyPoolConflict)
			conflict++
		}
	}
	require.Equal(t, 1, success)
	require.Equal(t, 1, conflict)
	state, err := s.State(t.Context())
	require.NoError(t, err)
	require.Len(t, state.Entries, 1)
	require.EqualValues(t, 2, state.Revision)
}

type poolFailEncryption struct {
	service.OpenAICredentialEncryptor
	calls int
}

func (e *poolFailEncryption) Encrypt(text string) (string, error) {
	e.calls++
	if e.calls == 2 {
		return "", errors.New("secret must never reach API")
	}
	return e.OpenAICredentialEncryptor.Encrypt(text)
}
func TestProxyPoolEncryptionFailureRollsBackWholeReplacement(t *testing.T) {
	_, s, enc := poolFixture(t)
	state, err := s.Import(t.Context(), service.ProxyPoolImport{Revision: 1, Mode: "append", Text: "old.example:80"})
	require.NoError(t, err)
	failure := &poolFailEncryption{OpenAICredentialEncryptor: enc}
	r := NewProxyPoolRepository(integrationDB, failure)
	broken := service.NewProxyPoolService(r, failure)
	_, err = broken.Import(t.Context(), service.ProxyPoolImport{Revision: state.Revision, Mode: "replace", Text: "new1.example:80\nnew2.example:80"})
	require.ErrorIs(t, err, service.ErrProxyPoolUnavailable)
	after, err := s.State(context.Background())
	require.NoError(t, err)
	require.Equal(t, state, after)
}
