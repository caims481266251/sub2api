package service

import (
	"context"
	"errors"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

type dockerSystemPoolProviderFunc func(context.Context) ([]DockerSystemProfile, error)

func (f dockerSystemPoolProviderFunc) ListDockerSystemProfiles(ctx context.Context) ([]DockerSystemProfile, error) {
	return f(ctx)
}

func dockerSystemPoolFixture() DockerSystemPoolProvider {
	return dockerSystemPoolProviderFunc(func(context.Context) ([]DockerSystemProfile, error) {
		profiles := make([]DockerSystemProfile, 0, 5)
		for _, id := range DockerSystemProfileIDs() {
			profiles = append(profiles, DockerSystemProfile{ID: id, Available: true})
		}
		return profiles, nil
	})
}

func TestDockerSystemSelectionInputValidation(t *testing.T) {
	for _, test := range []struct{ mode, requested, want, reason string }{
		{AccountExecutionModeNormal, "", "", ""},
		{AccountExecutionModeNormal, "auto", "", "DOCKER_SYSTEM_PROFILE_UNSUPPORTED"},
		{AccountExecutionModeNormal, "debian-12", "", "DOCKER_SYSTEM_PROFILE_UNSUPPORTED"},
		{AccountExecutionModeDockerKernel, "", "auto", ""},
		{AccountExecutionModeDockerKernel, "auto", "auto", ""},
		{AccountExecutionModeDockerKernel, "debian-12", "debian-12", ""},
		{AccountExecutionModeDockerKernel, "debian", "", "DOCKER_SYSTEM_PROFILE_INVALID"},
		{AccountExecutionModeDockerKernel, " debian-12", "", "DOCKER_SYSTEM_PROFILE_INVALID"},
		{AccountExecutionModeDockerKernel, "sha256:image", "", "DOCKER_SYSTEM_PROFILE_INVALID"},
	} {
		t.Run(test.mode+"/"+test.requested, func(t *testing.T) {
			got, err := normalizeDockerSystemSelection(test.mode, test.requested)
			require.Equal(t, test.want, got)
			if test.reason == "" {
				require.NoError(t, err)
			} else {
				require.Equal(t, test.reason, infraerrors.Reason(err))
			}
		})
	}
}

func TestDockerSystemSelectionUsesAvailableProfilesInFixedOrder(t *testing.T) {
	svc := &adminServiceImpl{dockerSystemPool: dockerSystemPoolProviderFunc(func(context.Context) ([]DockerSystemProfile, error) {
		return []DockerSystemProfile{{ID: "fedora-44", Available: true}, {ID: "ubuntu-24.04", Available: true}, {ID: "debian-12", Available: false}, {ID: "untrusted-template", Available: true}}, nil
	})}
	for _, request := range []string{"", "auto", "fedora-44"} {
		account := &Account{ExecutionMode: AccountExecutionModeDockerKernel}
		require.NoError(t, svc.prepareDockerSystemSelection(t.Context(), &CreateAccountInput{DockerSystemProfile: request}, account))
		require.Equal(t, []string{"ubuntu-24.04", "fedora-44"}, account.DockerSystemSelection.Candidates)
		require.Nil(t, account.DockerSystemProfile, "the transaction owns final allocation")
	}
	err := svc.prepareDockerSystemSelection(t.Context(), &CreateAccountInput{DockerSystemProfile: "debian-12"}, &Account{ExecutionMode: AccountExecutionModeDockerKernel})
	require.Equal(t, "DOCKER_SYSTEM_PROFILE_UNAVAILABLE", infraerrors.Reason(err))
}

func TestDockerSystemSelectionFailsClosedAndNormalDoesNotProbe(t *testing.T) {
	for _, provider := range []DockerSystemPoolProvider{nil, dockerSystemPoolProviderFunc(func(context.Context) ([]DockerSystemProfile, error) { return nil, nil })} {
		svc := &adminServiceImpl{dockerSystemPool: provider}
		require.Error(t, svc.prepareDockerSystemSelection(t.Context(), &CreateAccountInput{}, &Account{ExecutionMode: AccountExecutionModeDockerKernel}))
	}
	wantErr := errors.New("local inventory unavailable")
	calls := 0
	svc := &adminServiceImpl{dockerSystemPool: dockerSystemPoolProviderFunc(func(context.Context) ([]DockerSystemProfile, error) { calls++; return nil, wantErr })}
	require.NoError(t, svc.prepareDockerSystemSelection(t.Context(), &CreateAccountInput{}, &Account{}))
	require.Zero(t, calls)
	require.ErrorIs(t, svc.prepareDockerSystemSelection(t.Context(), &CreateAccountInput{}, &Account{ExecutionMode: AccountExecutionModeDockerKernel}), wantErr)
}

func TestDockerSystemProfileExtraCannotOverrideOrMigrateBinding(t *testing.T) {
	profile := "debian-12"
	proxyID := int64(7)
	for _, binding := range []*string{nil, &profile} {
		account := &Account{ID: 72, Platform: PlatformOpenAI, Type: AccountTypeOAuth, ExecutionMode: AccountExecutionModeDockerKernel, DockerSystemProfile: binding, ProxyID: &proxyID, Status: StatusActive}
		repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{account.ID: account}}
		svc := &adminServiceImpl{accountRepo: repo}
		updated, err := svc.UpdateAccount(t.Context(), account.ID, &UpdateAccountInput{Extra: map[string]any{"docker_system_profile": "fedora-44", "docker_system_template": "arbitrary-image"}})
		require.NoError(t, err)
		require.Equal(t, binding, updated.DockerSystemProfile)
		require.Nil(t, updated.DockerSystemSelection)
	}
}

type dockerSystemAtomicCreateRepo struct {
	AccountRepository
	atomicErr   error
	atomicCalls int
	createCalls int
	bindCalls   int
	account     *Account
	groups      []AccountGroup
}

func (r *dockerSystemAtomicCreateRepo) Create(_ context.Context, account *Account) error {
	r.createCalls++
	account.ID = 8
	return nil
}

func (r *dockerSystemAtomicCreateRepo) BindGroups(context.Context, int64, []int64) error {
	r.bindCalls++
	return nil
}

func (r *dockerSystemAtomicCreateRepo) CreateWithAccountGroups(_ context.Context, account *Account, groups []AccountGroup) error {
	r.atomicCalls++
	r.account, r.groups = account, groups
	if r.atomicErr != nil {
		return r.atomicErr
	}
	account.ID = 8
	return nil
}

func TestDockerSystemCreateWithoutAdmissionUsesAtomicGroupTransaction(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "group failure"}[fail], func(t *testing.T) {
			repo := &dockerSystemAtomicCreateRepo{}
			if fail {
				repo.atomicErr = errors.New("group insert failed")
			}
			svc := &adminServiceImpl{cfg: &config.Config{VMKernel: config.VMKernelConfig{Enabled: true}},
				accountRepo: repo, accountDuplicateRepo: repo, dockerSystemPool: dockerSystemPoolFixture()}
			proxyID := int64(7)
			account, err := svc.CreateAccount(t.Context(), &CreateAccountInput{Name: "Docker", Platform: PlatformOpenAI, Type: AccountTypeOAuth,
				ExecutionMode: AccountExecutionModeDockerKernel, ProxyID: &proxyID, GroupIDs: []int64{5}, SkipMixedChannelCheck: true})
			if fail {
				require.ErrorIs(t, err, repo.atomicErr)
				require.Nil(t, account)
			} else {
				require.NoError(t, err)
				require.Equal(t, int64(8), account.ID)
			}
			require.Equal(t, 1, repo.atomicCalls)
			require.Zero(t, repo.createCalls)
			require.Zero(t, repo.bindCalls)
			require.Nil(t, repo.account.InitialAdmissionPlan)
			require.NotNil(t, repo.account.DockerSystemSelection)
			require.Equal(t, []AccountGroup{{GroupID: 5, Priority: 50}}, repo.groups)
		})
	}
}
