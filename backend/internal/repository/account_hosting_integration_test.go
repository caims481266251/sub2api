//go:build integration

package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func hostingIntegrationFixture(t *testing.T) (*ent.Client, *hostingRepository, *accountRepository) {
	t.Helper()
	client := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, integrationDB)))
	aes, err := NewAESEncryptor(&config.Config{Totp: config.TotpConfig{EncryptionKey: strings.Repeat("ab", 32)}})
	require.NoError(t, err)
	enc := &credentialEncryptor{fixed: true, fallback: aes}
	accounts := ProvideAccountRepository(client, integrationDB, nil, enc).(*accountRepository)
	return client, NewHostingRepository(client, integrationDB, nil).(*hostingRepository), accounts
}
func hostingIntegrationAccount(userID int64, identity string) *service.Account {
	hash := sha256.Sum256([]byte(identity + "\x00upstream-user"))
	fingerprint := hex.EncodeToString(hash[:])
	return &service.Account{Name: "Hosted " + identity, OwnerUserID: &userID, HostingIdentity: &fingerprint, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Status: service.StatusActive, Schedulable: true, Concurrency: 3, Credentials: map[string]any{"chatgpt_account_id": identity, "chatgpt_user_id": "upstream-user", "access_token": "access-secret", "refresh_token": "refresh-secret", "id_token": "id-secret"}, Extra: map[string]any{}}
}

func TestHostingAccountsExcludedFromPublicAdminLists(t *testing.T) {
	ctx := context.Background()
	client, hosting, accounts := hostingIntegrationFixture(t)
	owner := mustCreateUser(t, client, &service.User{})
	marker := uuid.NewString()
	t.Cleanup(func() {
		_, err := integrationDB.ExecContext(ctx, "DELETE FROM accounts WHERE owner_user_id=$1 OR name=$2", owner.ID, marker+" public")
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(ctx, "DELETE FROM users WHERE id=$1", owner.ID)
		require.NoError(t, err)
	})
	private := hostingIntegrationAccount(owner.ID, marker)
	private.Name = marker + " hosted"
	saved, err := hosting.SaveOwned(ctx, owner.ID, private, 0, 1)
	require.NoError(t, err)
	public := mustCreateAccount(t, client, &service.Account{Name: marker + " public", Platform: service.PlatformOpenAI})

	for _, groupID := range []int64{0, service.AccountListGroupUngrouped} {
		listed, page, err := accounts.ListWithFilters(ctx, pagination.PaginationParams{Page: 1, PageSize: 1}, service.PlatformOpenAI, service.AccountTypeOAuth, service.StatusActive, marker, groupID, "")
		require.NoError(t, err)
		require.EqualValues(t, 1, page.Total)
		require.Len(t, listed, 1)
		require.Equal(t, public.ID, listed[0].ID)
		all, err := accounts.ListAllWithFilters(ctx, service.PlatformOpenAI, "", "", marker, groupID, "")
		require.NoError(t, err)
		require.Len(t, all, 1)
		require.Equal(t, public.ID, all[0].ID)
	}
	listed, page, err := accounts.ListWithFilters(ctx, pagination.PaginationParams{Page: 1, PageSize: 20}, "", "", "", private.Name, 0, "")
	require.NoError(t, err)
	require.Zero(t, page.Total)
	require.Empty(t, listed)
	owned, err := hosting.ListOwned(ctx, owner.ID)
	require.NoError(t, err)
	require.Len(t, owned, 1)
	require.Equal(t, saved.ID, owned[0].ID)
	adminHosted, total, err := hosting.ListAllHosted(ctx, marker, 1, 20)
	require.NoError(t, err)
	require.Equal(t, 1, total)
	require.Len(t, adminHosted, 1)
	require.Equal(t, saved.ID, adminHosted[0].Account.ID)
}

func TestHostingPublicIdentityCoexistence(t *testing.T) {
	for _, publicFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("public_first_%t", publicFirst), func(t *testing.T) {
			ctx := context.Background()
			client, hosting, accounts := hostingIntegrationFixture(t)
			owner := mustCreateUser(t, client, &service.User{})
			other := mustCreateUser(t, client, &service.User{})
			identity := uuid.NewString()
			public := &service.Account{Name: identity + " public", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Status: service.StatusActive, Schedulable: true, Concurrency: 7, Credentials: hostingIntegrationAccount(owner.ID, identity).Credentials}
			t.Cleanup(func() {
				_, err := integrationDB.ExecContext(ctx, "DELETE FROM accounts WHERE owner_user_id IN ($1,$2) OR name=$3", owner.ID, other.ID, public.Name)
				require.NoError(t, err)
				_, err = integrationDB.ExecContext(ctx, "DELETE FROM users WHERE id IN ($1,$2)", owner.ID, other.ID)
				require.NoError(t, err)
			})
			if publicFirst {
				require.NoError(t, accounts.Create(ctx, public))
			}
			private, err := hosting.SaveOwned(ctx, owner.ID, hostingIntegrationAccount(owner.ID, identity), 0, 2)
			require.NoError(t, err)
			if !publicFirst {
				require.NoError(t, accounts.Create(ctx, public))
			}
			require.NotEqual(t, public.ID, private.ID)
			require.Nil(t, public.OwnerUserID)
			require.Equal(t, owner.ID, *private.OwnerUserID)

			// Both repository validation and direct persistence keep hosted identities exclusive.
			for _, userID := range []int64{owner.ID, other.ID} {
				_, err = hosting.SaveOwned(ctx, userID, hostingIntegrationAccount(userID, identity), 0, 2)
				require.ErrorIs(t, err, service.ErrHostingIdentityConflict)
				duplicate := hostingIntegrationAccount(userID, identity)
				fingerprint := uuid.NewString() // Exercise the trigger, independent of the unique fingerprint index.
				duplicate.HostingIdentity = &fingerprint
				require.ErrorContains(t, accounts.Create(ctx, duplicate), "account identity already hosted")
			}
			unknownUser := hostingIntegrationAccount(other.ID, identity)
			delete(unknownUser.Credentials, "chatgpt_user_id")
			_, err = hosting.SaveOwned(ctx, other.ID, unknownUser, 0, 2)
			require.ErrorIs(t, err, service.ErrHostingIdentityConflict)

			listed, page, err := accounts.ListWithFilters(ctx, pagination.PaginationParams{Page: 1, PageSize: 20}, service.PlatformOpenAI, "", "", identity, 0, "")
			require.NoError(t, err)
			require.EqualValues(t, 1, page.Total)
			require.Len(t, listed, 1)
			require.Equal(t, public.ID, listed[0].ID)
			owned, err := hosting.ListOwned(ctx, owner.ID)
			require.NoError(t, err)
			require.Len(t, owned, 1)
			require.Equal(t, private.ID, owned[0].ID)
			schedulable, err := accounts.ListSchedulableByPlatform(ctx, service.PlatformOpenAI)
			require.NoError(t, err)
			foundPublic := false
			for _, candidate := range schedulable {
				require.NotEqual(t, private.ID, candidate.ID)
				foundPublic = foundPublic || candidate.ID == public.ID
			}
			require.True(t, foundPublic)

			// Reauthorization changes only the selected record, retaining its own settings.
			replacement := hostingIntegrationAccount(owner.ID, identity)
			replacement.Credentials["refresh_token"] = "hosted-rotated-secret"
			private, err = hosting.SaveOwned(ctx, owner.ID, replacement, private.ID, 0)
			require.NoError(t, err)
			require.Equal(t, 3, private.Concurrency)
			publicRead, err := accounts.GetByID(ctx, public.ID)
			require.NoError(t, err)
			require.Equal(t, 7, publicRead.Concurrency)
			require.Equal(t, "refresh-secret", publicRead.GetCredential("refresh_token"))
			publicRead.Credentials["refresh_token"] = "public-rotated-secret"
			require.NoError(t, accounts.UpdateCredentials(ctx, public.ID, publicRead.Credentials))
			privateRead, err := accounts.GetByID(ctx, private.ID)
			require.NoError(t, err)
			require.Equal(t, "hosted-rotated-secret", privateRead.GetCredential("refresh_token"))

			if publicFirst {
				require.NoError(t, hosting.ChangeOwned(ctx, owner.ID, private.ID, "delete"))
				remaining, err := accounts.GetByID(ctx, public.ID)
				require.NoError(t, err)
				require.True(t, remaining.IsSchedulable())
				require.Equal(t, "public-rotated-secret", remaining.GetCredential("refresh_token"))
			} else {
				require.NoError(t, accounts.Delete(ctx, public.ID))
				remaining, err := accounts.GetByID(ctx, private.ID)
				require.NoError(t, err)
				require.True(t, remaining.IsSchedulable())
				require.Equal(t, owner.ID, *remaining.OwnerUserID)
				require.Equal(t, "hosted-rotated-secret", remaining.GetCredential("refresh_token"))
			}
		})
	}
}

func TestHostingCreateWithSettingsAtomicPersistence(t *testing.T) {
	ctx := context.Background()
	client, repo, _ := hostingIntegrationFixture(t)
	u := mustCreateUser(t, client, &service.User{})
	other := mustCreateUser(t, client, &service.User{})
	foreign, err := repo.SavePool(ctx, other.ID, 0, "Foreign pool")
	require.NoError(t, err)
	identity := uuid.NewString()
	_, err = repo.SaveOwned(ctx, u.ID, hostingIntegrationAccount(u.ID, identity), 0, 1, &service.HostingAccountSettings{HostingPoolID: &foreign.ID})
	require.Error(t, err)
	accounts, err := repo.ListOwned(ctx, u.ID)
	require.NoError(t, err)
	require.Empty(t, accounts, "failed preferences must roll back account creation and release the seat")
	var input service.HostingAccountSettings
	require.NoError(t, json.Unmarshal([]byte(`{"name":"Configured on creation","notes":"Saved atomically","concurrency":8,"priority":12,"load_factor":6,"expires_at":2000000000,"auto_pause_on_expired":true,"credentials":{"model_mapping":{"client":"gpt-5.4"},"model_mapping_mode":"aliases","account_scheduling_threshold":85},"extra":{"openai_compact_mode":"force_on","codex_fingerprint_mode":"device"}}`), &input))
	a, err := repo.SaveOwned(ctx, u.ID, hostingIntegrationAccount(u.ID, identity), 0, 1, &input)
	require.NoError(t, err)
	require.Equal(t, "Configured on creation", a.Name)
	require.Equal(t, "Saved atomically", *a.Notes)
	require.Equal(t, 8, a.Concurrency)
	require.Equal(t, 12, a.Priority)
	require.Equal(t, 6, *a.LoadFactor)
	require.Equal(t, int64(2000000000), a.ExpiresAt.Unix())
	require.Equal(t, "gpt-5.4", a.GetMappedModel("client"))
	require.Equal(t, "force_on", a.GetOpenAICompactMode())
	require.NotEmpty(t, a.Extra["codex_fingerprint_seed"])
	require.Equal(t, "refresh-secret", a.GetCredential("refresh_token"))
	var stored string
	require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT credentials::text FROM accounts WHERE id=$1", a.ID).Scan(&stored))
	require.NotContains(t, stored, "refresh-secret")
	require.Contains(t, stored, hostedTokenPrefix)
	// Reauthorization and edited preferences share a transaction too.
	replacement := hostingIntegrationAccount(u.ID, identity)
	replacement.Credentials["refresh_token"] = "reauthorized-secret"
	zero := int64(0)
	a, err = repo.SaveOwned(ctx, u.ID, replacement, a.ID, 0, &service.HostingAccountSettings{ExpiresAt: &zero, Extra: map[string]any{"openai_compact_mode": "force_off"}})
	require.NoError(t, err)
	require.Equal(t, "reauthorized-secret", a.GetCredential("refresh_token"))
	require.Equal(t, "force_off", a.GetOpenAICompactMode())
	require.Nil(t, a.ExpiresAt)
	require.Equal(t, 8, a.Concurrency)
	require.Equal(t, "gpt-5.4", a.GetMappedModel("client"))
	accounts, err = repo.ListOwned(ctx, u.ID)
	require.NoError(t, err)
	require.Len(t, accounts, 1)
}

func TestHostingSeatsOwnershipReauthorizationAndEncryption(t *testing.T) {
	ctx := context.Background()
	client, repo, accounts := hostingIntegrationFixture(t)
	u := mustCreateUser(t, client, &service.User{Balance: 10})
	other := mustCreateUser(t, client, &service.User{})
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := repo.SaveOwned(ctx, u.ID, hostingIntegrationAccount(u.ID, uuid.NewString()), 0, 1)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	success, full := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else {
			require.ErrorIs(t, err, service.ErrHostingSeatsFull)
			full++
		}
	}
	require.Equal(t, 1, success)
	require.Equal(t, 1, full)
	owned, err := repo.ListOwned(ctx, u.ID)
	require.NoError(t, err)
	require.Len(t, owned, 1)
	a := owned[0]
	var stored string
	require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT credentials::text FROM accounts WHERE id=$1", a.ID).Scan(&stored))
	require.NotContains(t, stored, "access-secret")
	require.Contains(t, stored, hostedTokenPrefix)
	require.Equal(t, "refresh-secret", a.GetCredential("refresh_token"))
	cache := NewSchedulerCache(integrationRedis)
	require.NoError(t, cache.SetAccount(ctx, &a))
	cached, err := cache.GetAccount(ctx, a.ID)
	require.NoError(t, err)
	require.Nil(t, cached, "hosted credentials do not enter the public scheduler cache")
	public, err := accounts.ListSchedulableByPlatform(ctx, service.PlatformOpenAI)
	require.NoError(t, err)
	for _, p := range public {
		require.NotEqual(t, a.ID, p.ID)
	}
	require.ErrorIs(t, repo.ChangeOwned(ctx, other.ID, a.ID, "delete"), service.ErrHostingAccountNotFound)
	_, err = repo.SaveOwned(ctx, other.ID, hostingIntegrationAccount(other.ID, a.GetChatGPTAccountID()), 0, 1)
	require.ErrorIs(t, err, service.ErrHostingIdentityConflict)
	require.NoError(t, repo.ChangeOwned(ctx, u.ID, a.ID, "pause"))
	_, err = repo.SaveOwned(ctx, u.ID, hostingIntegrationAccount(u.ID, uuid.NewString()), 0, 1)
	require.ErrorIs(t, err, service.ErrHostingSeatsFull)
	replacement := hostingIntegrationAccount(u.ID, a.GetChatGPTAccountID())
	replacement.Credentials["refresh_token"] = "rotated-secret"
	reauthorized, err := repo.SaveOwned(ctx, u.ID, replacement, a.ID, 0)
	require.NoError(t, err)
	require.Equal(t, a.ID, reauthorized.ID)
	require.True(t, reauthorized.IsSchedulable())
	require.Equal(t, "rotated-secret", reauthorized.GetCredential("refresh_token"))
	require.NoError(t, accounts.UpdateCredentials(ctx, a.ID, replacement.Credentials))
	latest, err := accounts.GetByID(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, "rotated-secret", latest.GetCredential("refresh_token"))
	storedSnapshot, err := accountCredentialsSnapshot(ctx, client, accounts.hostingEncryptor, latest)
	require.NoError(t, err)
	require.NotContains(t, string(storedSnapshot), "rotated-secret")
	require.Contains(t, string(storedSnapshot), hostedTokenPrefix)
	oldSnapshot := *latest
	newCredentials := make(map[string]any, len(latest.Credentials))
	for k, v := range latest.Credentials {
		newCredentials[k] = v
	}
	newCredentials["access_token"] = "new-access-secret"
	require.NoError(t, accounts.UpdateCredentials(ctx, a.ID, newCredentials))
	_, err = accountCredentialsSnapshot(ctx, client, accounts.hostingEncryptor, &oldSnapshot)
	require.ErrorIs(t, err, service.ErrUpstreamBillingProbeIdentityChanged)
	group := mustCreateGroup(t, client, &service.Group{Name: "hosting-guard-" + uuid.NewString(), Platform: service.PlatformOpenAI})
	_, err = integrationDB.ExecContext(ctx, "INSERT INTO account_groups (account_id,group_id,priority) VALUES ($1,$2,1)", a.ID, group.ID)
	require.Error(t, err, "all membership paths reject hosted accounts")
	_, err = integrationDB.ExecContext(ctx, "UPDATE accounts SET owner_user_id=NULL WHERE id=$1", a.ID)
	require.Error(t, err)
	platformClone := &service.Account{Name: "platform clone", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Status: service.StatusActive, Schedulable: true, Credentials: replacement.Credentials}
	require.NoError(t, accounts.Create(ctx, platformClone), "public and hosted records may share an upstream identity")
	require.NotEqual(t, a.ID, platformClone.ID)
	require.Nil(t, platformClone.OwnerUserID)
	limit := 0
	require.NoError(t, repo.SetUserPolicy(ctx, u.ID, service.HostingUserPolicy{SeatLimit: &limit}))
	_, err = repo.SaveOwned(ctx, u.ID, hostingIntegrationAccount(u.ID, uuid.NewString()), 0, 99)
	require.ErrorIs(t, err, service.ErrHostingSeatsFull)
	require.NoError(t, repo.ChangeOwned(ctx, u.ID, a.ID, "delete"))
	owned, err = repo.ListOwned(ctx, u.ID)
	require.NoError(t, err)
	require.Empty(t, owned)
	require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT credentials::text FROM accounts WHERE id=$1", a.ID).Scan(&stored))
	require.Equal(t, "{}", stored)
	// A queued credential refresh must not resurrect deleted secrets.
	_ = accounts.UpdateCredentials(ctx, a.ID, replacement.Credentials)
	require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT credentials::text FROM accounts WHERE id=$1", a.ID).Scan(&stored))
	require.Equal(t, "{}", stored)
	require.NoError(t, repo.SetUserPolicy(ctx, u.ID, service.HostingUserPolicy{}))
	_, err = repo.SaveOwned(ctx, u.ID, hostingIntegrationAccount(u.ID, a.GetChatGPTAccountID()), 0, 1)
	require.NoError(t, err, "deleted identity releases the seat and identity reservation")
}

func TestHostingUsagePersistenceSummaryAndIdempotentCharge(t *testing.T) {
	ctx := context.Background()
	client, repo, _ := hostingIntegrationFixture(t)
	u := mustCreateUser(t, client, &service.User{Balance: 10})
	a, err := repo.SaveOwned(ctx, u.ID, hostingIntegrationAccount(u.ID, uuid.NewString()), 0, 1)
	require.NoError(t, err)
	key := &service.APIKey{UserID: u.ID, Name: "hosted", Key: "sk-" + uuid.NewString(), RouteMode: service.RouteModeHosted, Quota: 5, RateLimit5h: 5}
	keys := NewAPIKeyRepository(client, integrationDB).(*apiKeyRepository)
	require.NoError(t, keys.Create(ctx, key))
	loaded, err := keys.GetByKey(ctx, key.Key)
	require.NoError(t, err)
	require.Equal(t, service.RouteModeHosted, loaded.RouteMode)
	usage := &usageLogRepository{sql: integrationDB, client: client}
	poolID, poolName := int64(0), ""
	hosted, fallback, reason := "hosted", "fallback", "托管账号额度已用完"
	log := &service.UsageLog{UserID: u.ID, APIKeyID: key.ID, AccountID: a.ID, RequestID: uuid.NewString(), Model: "gpt-5.1", TotalCost: 100, ActualCost: 3, RateMultiplier: .03, HostingRoute: &hosted, HostingPoolID: &poolID, HostingPoolName: &poolName, CreatedAt: time.Now()}
	inserted, err := usage.Create(ctx, log)
	require.NoError(t, err)
	require.True(t, inserted)
	read, err := usage.GetByID(ctx, log.ID)
	require.NoError(t, err)
	require.Equal(t, hosted, *read.HostingRoute)
	require.NotNil(t, read.HostingPoolID)
	require.Zero(t, *read.HostingPoolID)
	require.Equal(t, .03, read.RateMultiplier)
	group := mustCreateGroup(t, client, &service.Group{Name: fmt.Sprintf("hosting-fallback-%d", u.ID), Platform: service.PlatformOpenAI, RateMultiplier: 2})
	pool, err := repo.SavePool(ctx, u.ID, 0, "Original pool")
	require.NoError(t, err)
	log2 := &service.UsageLog{UserID: u.ID, APIKeyID: key.ID, AccountID: a.ID, GroupID: &group.ID, RequestID: uuid.NewString(), Model: "gpt-5.1", TotalCost: 2, ActualCost: 4, HostingRoute: &fallback, HostingPoolID: &pool.ID, HostingPoolName: &pool.Name, HostingFallbackReason: &reason, CreatedAt: time.Now()}
	_, err = usage.Create(ctx, log2)
	require.NoError(t, err)
	_, err = repo.SavePool(ctx, u.ID, pool.ID, "Renamed pool")
	require.NoError(t, err)
	read, err = usage.GetByID(ctx, log2.ID)
	require.NoError(t, err)
	require.Equal(t, reason, *read.HostingFallbackReason)
	require.Equal(t, pool.ID, *read.HostingPoolID)
	require.Equal(t, "Original pool", *read.HostingPoolName)
	require.Equal(t, group.ID, *read.GroupID)
	charge := NewUsageBillingRepository(client, integrationDB)
	cmd := &service.UsageBillingCommand{RequestID: log.RequestID, UserID: u.ID, APIKeyID: key.ID, AccountID: a.ID, AccountType: service.AccountTypeOAuth, BalanceCost: 3, APIKeyQuotaCost: 3, APIKeyRateLimitCost: 3}
	result, err := charge.Apply(ctx, cmd)
	require.NoError(t, err)
	require.True(t, result.Applied)
	result, err = charge.Apply(ctx, cmd)
	require.NoError(t, err)
	require.False(t, result.Applied)
	var balance, quota float64
	require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT u.balance,k.quota_used FROM users u JOIN api_keys k ON k.user_id=u.id WHERE k.id=$1", key.ID).Scan(&balance, &quota))
	require.Equal(t, 7.0, balance)
	require.Equal(t, 3.0, quota)
	require.NoError(t, repo.ChangeOwned(ctx, u.ID, a.ID, "delete"))
	summary, err := repo.Summary(ctx, u.ID)
	require.NoError(t, err)
	require.Equal(t, service.HostingSummary{StandardCost: 100, HostingCost: 3, FallbackCost: 4}, summary)
}

func TestHostingAccountSettingsIsolationPersistenceAndReauthorization(t *testing.T) {
	ctx := context.Background()
	client, repo, _ := hostingIntegrationFixture(t)
	u := mustCreateUser(t, client, &service.User{})
	other := mustCreateUser(t, client, &service.User{})
	a, err := repo.SaveOwned(ctx, u.ID, hostingIntegrationAccount(u.ID, uuid.NewString()), 0, 1)
	require.NoError(t, err)
	_, err = repo.UpdateOwnedSettings(ctx, other.ID, a.ID, service.HostingAccountSettings{Name: &other.Email})
	require.ErrorIs(t, err, service.ErrHostingAccountNotFound)
	_, err = client.Account.UpdateOneID(a.ID).SetExtra(map[string]any{"codex_5h_used_percent": 83, service.ExcelBPS403DisabledAtKey: "server-shutdown", service.ExcelBPS403LastProbeAtKey: "server-probe", service.OpenAIAutoResetCreditStateExtraKey: map[string]any{"status": "server-owned"}}).Save(ctx)
	require.NoError(t, err)
	name, concurrency, priority := "Configured Codex", 8, 12
	notes, loadFactor, expiresAt, pauseOnExpiry := "Account preferences", 6, int64(2000000000), true
	a, err = repo.UpdateOwnedSettings(ctx, u.ID, a.ID, service.HostingAccountSettings{
		Name: &name, Concurrency: &concurrency, Priority: &priority,
		Notes: &notes, LoadFactor: &loadFactor, ExpiresAt: &expiresAt, AutoPauseOnExpired: &pauseOnExpiry,
		Credentials: map[string]any{"model_mapping": map[string]any{"client-model": "gpt-5.4"}, "model_mapping_mode": "aliases", "account_scheduling_threshold": 85, "temp_unschedulable_enabled": true, "temp_unschedulable_rules": []any{map[string]any{"error_code": 429, "keywords": []any{"limit"}, "duration_minutes": 30}}},
		Extra:       map[string]any{"openai_oauth_responses_websockets_v2_mode": "ctx_pool", "codex_fingerprint_mode": "device", service.OpenAIAutoResetCreditEnabledExtraKey: true, service.OpenAIAutoResetCredit5hThresholdExtraKey: 0, service.OpenAIAutoResetCredit7dThresholdExtraKey: .9, service.AccountExtraUpstreamRequestIDHeader: "x-id"},
	})
	require.NoError(t, err)
	require.Equal(t, name, a.Name)
	require.Equal(t, concurrency, a.Concurrency)
	require.Equal(t, priority, a.Priority)
	require.Equal(t, notes, *a.Notes)
	require.Equal(t, loadFactor, *a.LoadFactor)
	require.Equal(t, expiresAt, a.ExpiresAt.Unix())
	require.True(t, a.AutoPauseOnExpired)
	require.Len(t, a.GetTempUnschedulableRules(), 1)
	require.Equal(t, .9, service.ResolveOpenAIAutoResetCreditConfig(a).Threshold7d)
	require.Equal(t, "x-id", service.UpstreamRequestIDHeaderName(a))
	require.Equal(t, "server-owned", a.Extra[service.OpenAIAutoResetCreditStateExtraKey].(map[string]any)["status"])
	require.Equal(t, "refresh-secret", a.GetCredential("refresh_token"))
	require.EqualValues(t, 83, a.Extra["codex_5h_used_percent"])
	require.Equal(t, "server-shutdown", a.Extra[service.ExcelBPS403DisabledAtKey])
	require.NotEmpty(t, a.Extra["codex_fingerprint_seed"])
	require.Equal(t, "gpt-5.4", a.GetMappedModel("client-model"))
	var stored string
	require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT credentials::text FROM accounts WHERE id=$1", a.ID).Scan(&stored))
	require.NotContains(t, stored, "refresh-secret")
	require.Contains(t, stored, hostedTokenPrefix)
	// Independent preference updates retain each other's changes and runtime state.
	var wg sync.WaitGroup
	errors := make(chan error, 2)
	for _, patch := range []service.HostingAccountSettings{
		{Credentials: map[string]any{"compact_model_mapping": map[string]any{"client-model": "gpt-5.4"}}},
		{Extra: map[string]any{"openai_passthrough": true, "openai_excel_bps": true}},
	} {
		wg.Add(1)
		go func(input service.HostingAccountSettings) {
			defer wg.Done()
			_, err := repo.UpdateOwnedSettings(ctx, u.ID, a.ID, input)
			errors <- err
		}(patch)
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	replacement := hostingIntegrationAccount(u.ID, a.GetChatGPTAccountID())
	replacement.Credentials["refresh_token"] = "new-reauth-secret"
	a, err = repo.SaveOwned(ctx, u.ID, replacement, a.ID, 1)
	require.NoError(t, err)
	require.Equal(t, "new-reauth-secret", a.GetCredential("refresh_token"))
	require.Equal(t, "aliases", a.GetCredential("model_mapping_mode"))
	require.Equal(t, "gpt-5.4", a.GetMappedModel("client-model"))
	require.Equal(t, "gpt-5.4", a.GetCompactModelMapping()["client-model"])
	require.True(t, a.IsOpenAIPassthroughEnabled())
	require.NotContains(t, a.Extra, service.ExcelBPS403DisabledAtKey)
	require.NotContains(t, a.Extra, service.ExcelBPS403LastProbeAtKey)
	require.Equal(t, "ctx_pool", a.Extra["openai_oauth_responses_websockets_v2_mode"])
	require.Equal(t, concurrency, a.Concurrency)
	require.Equal(t, priority, a.Priority)
	require.Equal(t, notes, *a.Notes)
	require.Equal(t, loadFactor, *a.LoadFactor)
	require.Equal(t, expiresAt, a.ExpiresAt.Unix())
	require.True(t, a.AutoPauseOnExpired)
	require.True(t, service.ResolveOpenAIAutoResetCreditConfig(a).Enabled)
	require.Len(t, a.GetTempUnschedulableRules(), 1)
	zeroLoad, zeroExpiry := 0, int64(0)
	a, err = repo.UpdateOwnedSettings(ctx, u.ID, a.ID, service.HostingAccountSettings{LoadFactor: &zeroLoad, ExpiresAt: &zeroExpiry, Credentials: map[string]any{"model_mapping": nil, "account_scheduling_threshold": nil}, Extra: map[string]any{"openai_passthrough": nil, service.AccountExtraUpstreamRequestIDHeader: nil}})
	require.NoError(t, err)
	require.NotContains(t, a.Credentials, "model_mapping")
	require.NotContains(t, a.Extra, "openai_passthrough")
	require.Nil(t, a.LoadFactor)
	require.Nil(t, a.ExpiresAt)
	require.NotContains(t, a.Credentials, "account_scheduling_threshold")
	require.NotContains(t, a.Extra, service.AccountExtraUpstreamRequestIDHeader)
	require.Equal(t, "new-reauth-secret", a.GetCredential("refresh_token"))
	require.EqualValues(t, 83, a.Extra["codex_5h_used_percent"])
	require.NoError(t, repo.ChangeOwned(ctx, u.ID, a.ID, "delete"))
	_, err = repo.UpdateOwnedSettings(ctx, u.ID, a.ID, service.HostingAccountSettings{Name: &name})
	require.ErrorIs(t, err, service.ErrHostingAccountNotFound)
}

func TestHostingPoolsOwnershipSeatsBindingsAndDeletion(t *testing.T) {
	ctx := context.Background()
	client, repo, _ := hostingIntegrationFixture(t)
	u := mustCreateUser(t, client, &service.User{})
	other := mustCreateUser(t, client, &service.User{})
	pool, err := repo.SavePool(ctx, u.ID, 0, "工作池")
	require.NoError(t, err)
	foreign, err := repo.SavePool(ctx, other.ID, 0, "工作池")
	require.NoError(t, err)
	_, err = repo.SavePool(ctx, u.ID, 0, "工作池")
	require.ErrorIs(t, err, service.ErrHostingPoolNameExists)
	_, err = repo.GetOwnedPool(ctx, other.ID, pool.ID)
	require.ErrorIs(t, err, service.ErrHostingPoolNotFound)
	_, err = repo.SavePool(ctx, other.ID, pool.ID, "覆盖名称")
	require.ErrorIs(t, err, service.ErrHostingPoolNotFound)
	require.ErrorIs(t, repo.DeletePool(ctx, other.ID, pool.ID), service.ErrHostingPoolNotFound)
	pool, err = repo.SavePool(ctx, u.ID, pool.ID, "开发池")
	require.NoError(t, err)
	input := hostingIntegrationAccount(u.ID, uuid.NewString())
	input.HostingPoolID = &foreign.ID
	_, err = repo.SaveOwned(ctx, u.ID, input, 0, 1)
	require.Error(t, err, "database rejects foreign pool ownership")
	input.HostingPoolID = &pool.ID
	a, err := repo.SaveOwned(ctx, u.ID, input, 0, 1)
	require.NoError(t, err)
	require.Equal(t, pool.ID, *a.HostingPoolID)
	_, err = repo.SaveOwned(ctx, u.ID, hostingIntegrationAccount(u.ID, uuid.NewString()), 0, 1)
	require.ErrorIs(t, err, service.ErrHostingSeatsFull, "seat count spans all pools")
	require.ErrorIs(t, repo.DeletePool(ctx, u.ID, pool.ID), service.ErrHostingPoolInUse)
	keys := NewAPIKeyRepository(client, integrationDB).(*apiKeyRepository)
	key := &service.APIKey{UserID: u.ID, Name: "pool", Key: "sk-" + uuid.NewString(), RouteMode: service.RouteModeHosted, HostingPoolID: &foreign.ID}
	require.Error(t, keys.Create(ctx, key), "key composite FK rejects other users' pools")
	key.HostingPoolID = &pool.ID
	require.NoError(t, keys.Create(ctx, key))
	loaded, err := keys.GetByKey(ctx, key.Key)
	require.NoError(t, err)
	require.Equal(t, pool.ID, *loaded.HostingPoolID)
	pools, err := repo.ListPools(ctx, u.ID)
	require.NoError(t, err)
	require.Len(t, pools, 2)
	require.Zero(t, pools[0].ID)
	require.Equal(t, 1, pools[1].AccountCount)
	require.Equal(t, 1, pools[1].KeyCount)
	_, err = repo.UpdateOwnedSettings(ctx, u.ID, a.ID, service.HostingAccountSettings{HostingPoolID: &foreign.ID})
	require.Error(t, err, "moving an account cannot cross user ownership")
	// Reauthorization changes credentials but retains pool assignment.
	a, err = repo.SaveOwned(ctx, u.ID, hostingIntegrationAccount(u.ID, a.GetChatGPTAccountID()), a.ID, 1)
	require.NoError(t, err)
	require.Equal(t, pool.ID, *a.HostingPoolID)
	zero := int64(0)
	a, err = repo.UpdateOwnedSettings(ctx, u.ID, a.ID, service.HostingAccountSettings{HostingPoolID: &zero})
	require.NoError(t, err)
	require.Nil(t, a.HostingPoolID)
	require.ErrorIs(t, repo.DeletePool(ctx, u.ID, pool.ID), service.ErrHostingPoolInUse, "a Key still references this empty pool")
	key.HostingPoolID = nil
	require.NoError(t, keys.Update(ctx, key, service.APIKeyUpdateFields{HostingRoute: true}))
	require.NoError(t, repo.DeletePool(ctx, u.ID, pool.ID))
	_, err = repo.GetOwnedPool(ctx, u.ID, pool.ID)
	require.ErrorIs(t, err, service.ErrHostingPoolNotFound)
	_, err = repo.GetOwnedPool(ctx, other.ID, foreign.ID)
	require.NoError(t, err)
}

func TestHostingAdminListAllUsersAndPagination(t *testing.T) {
	ctx := context.Background()
	client, repo, _ := hostingIntegrationFixture(t)
	prefix := "hosting-admin-" + uuid.NewString()
	first := mustCreateUser(t, client, &service.User{Email: prefix + "@example.com"})
	second := mustCreateUser(t, client, &service.User{Email: "second-" + prefix + "@example.com"})
	pool, err := repo.SavePool(ctx, first.ID, 0, "Shared name")
	require.NoError(t, err)
	a := hostingIntegrationAccount(first.ID, uuid.NewString())
	a.Name = "First " + prefix
	a.HostingPoolID = &pool.ID
	_, err = repo.SaveOwned(ctx, first.ID, a, 0, 1)
	require.NoError(t, err)
	b := hostingIntegrationAccount(second.ID, uuid.NewString())
	b.Name = "Second " + prefix
	_, err = repo.SaveOwned(ctx, second.ID, b, 0, 1)
	require.NoError(t, err)
	items, total, err := repo.ListAllHosted(ctx, prefix, 1, 1)
	require.NoError(t, err)
	require.Equal(t, 2, total)
	require.Len(t, items, 1)
	require.Equal(t, second.ID, items[0].UserID)
	require.Equal(t, second.Email, items[0].UserEmail)
	require.Empty(t, items[0].PoolName)
	items, total, err = repo.ListAllHosted(ctx, prefix, 2, 1)
	require.NoError(t, err)
	require.Equal(t, 2, total)
	require.Len(t, items, 1)
	require.Equal(t, first.ID, items[0].UserID)
	require.Equal(t, pool.Name, items[0].PoolName)
	items, total, err = repo.ListAllHosted(ctx, fmt.Sprint(first.ID), 1, 20)
	require.NoError(t, err)
	require.Equal(t, 1, total)
	require.Len(t, items, 1)
	require.NoError(t, repo.ChangeOwned(ctx, first.ID, a.ID, "delete"))
	items, total, err = repo.ListAllHosted(ctx, prefix, 1, 20)
	require.NoError(t, err)
	require.Equal(t, 1, total)
	require.Len(t, items, 1)
	require.Equal(t, second.ID, items[0].UserID)
}

func TestHostingSchedulingSwitchPreservesAccountHealthAndOwnership(t *testing.T) {
	ctx := context.Background()
	client, repo, accounts := hostingIntegrationFixture(t)
	owner := mustCreateUser(t, client, &service.User{})
	other := mustCreateUser(t, client, &service.User{})
	a, err := repo.SaveOwned(ctx, owner.ID, hostingIntegrationAccount(owner.ID, uuid.NewString()), 0, 1)
	require.NoError(t, err)
	reset := time.Now().Add(time.Hour).Truncate(time.Second)
	_, err = client.Account.UpdateOneID(a.ID).SetStatus(service.StatusError).SetErrorMessage("invalid_grant").SetRateLimitResetAt(reset).Save(ctx)
	require.NoError(t, err)
	for _, action := range []string{"schedule-disable", "schedule-enable"} {
		require.ErrorIs(t, repo.ChangeOwned(ctx, other.ID, a.ID, action), service.ErrHostingAccountNotFound)
		require.NoError(t, repo.ChangeOwned(ctx, owner.ID, a.ID, action))
		updated, err := accounts.GetByID(ctx, a.ID)
		require.NoError(t, err)
		require.Equal(t, action == "schedule-enable", updated.Schedulable)
		require.Equal(t, service.StatusError, updated.Status)
		require.Equal(t, "invalid_grant", updated.ErrorMessage)
		require.WithinDuration(t, reset, *updated.RateLimitResetAt, time.Second)
		require.False(t, updated.IsSchedulable(), "a manual switch must not bypass authentication failures")
	}
}

func TestHostingBillingPrecisionAndReconciliation(t *testing.T) {
	ctx := context.Background()
	client, repo, _ := hostingIntegrationFixture(t)
	owner := mustCreateUser(t, client, &service.User{})
	other := mustCreateUser(t, client, &service.User{})
	a, err := repo.SaveOwned(ctx, owner.ID, hostingIntegrationAccount(owner.ID, uuid.NewString()), 0, 1)
	require.NoError(t, err)
	key := mustCreateApiKey(t, client, &service.APIKey{UserID: owner.ID, Key: uuid.NewString()})
	otherKey := mustCreateApiKey(t, client, &service.APIKey{UserID: other.ID, Key: uuid.NewString()})
	for _, row := range []struct {
		userID, keyID      int64
		route              string
		base, rate, actual float64
	}{
		{owner.ID, key.ID, "hosted", .004115, .03, .00012345},
		{owner.ID, key.ID, "hosted", .004115, .06, .0002469},
		{owner.ID, key.ID, "fallback", .00123456, 1, .00123456},
		{other.ID, otherKey.ID, "hosted", 100, 1, 100},
	} {
		_, err := client.UsageLog.Create().SetUserID(row.userID).SetAPIKeyID(row.keyID).SetAccountID(a.ID).SetModel("gpt-5").SetRequestID(uuid.NewString()).SetHostingRoute(row.route).SetTotalCost(row.base).SetRateMultiplier(row.rate).SetActualCost(row.actual).SetInputCost(row.base).SetInputTokens(100).Save(ctx)
		require.NoError(t, err)
	}
	all, err := repo.Billing(ctx, owner.ID, 0, "", 1, 20)
	require.NoError(t, err)
	require.EqualValues(t, 3, all.Requests)
	require.InDelta(t, .00160491, all.ActualCost, 1e-10)
	require.InDelta(t, .00037035, all.HostingCost, 1e-10)
	require.InDelta(t, .00123456, all.FallbackCost, 1e-10)
	var sum float64
	for _, row := range all.Items {
		sum += row.ActualCost
		if row.Route == "hosted" {
			require.Equal(t, a.Name, row.AccountName, "owners can see their own hosted account names")
		} else {
			require.Empty(t, row.AccountName, "fallback account names must not appear in user bills")
		}
		require.InDelta(t, row.StandardCost*row.Rate, row.ActualCost, 1e-10)
		require.Equal(t, row.StandardCost, row.InputCost)
		require.NotEmpty(t, row.RequestID)
	}
	require.InDelta(t, all.ActualCost, sum, 1e-10)
	hosted, err := repo.Billing(ctx, owner.ID, a.ID, "", 1, 1)
	require.NoError(t, err)
	require.EqualValues(t, 2, hosted.Requests)
	require.Len(t, hosted.Items, 1)
	require.InDelta(t, .00037035, hosted.ActualCost, 1e-10, "summary includes all pages and historical rates")
	require.Zero(t, hosted.FallbackCost)
}
