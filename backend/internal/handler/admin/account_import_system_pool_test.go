package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// The fixture returns a resolved binding, so the handler must not echo the
// requested "auto" selection as if it were the persisted system.
type resolvedSystemImportAdmin struct {
	*codexImportMemoryAdminService
	resolvedProfile string
}

func (s *resolvedSystemImportAdmin) CreateAccount(ctx context.Context, input *service.CreateAccountInput) (*service.Account, error) {
	account, err := s.codexImportMemoryAdminService.CreateAccount(ctx, input)
	if err != nil {
		return nil, err
	}
	account.ExecutionMode = input.ExecutionMode
	if input.ExecutionMode == service.AccountExecutionModeDockerKernel {
		profile := s.resolvedProfile
		account.DockerSystemProfile = &profile
	}
	for i := range s.accounts {
		if s.accounts[i].ID == account.ID {
			s.accounts[i] = *account
		}
	}
	return account, nil
}

func TestSystemPoolSessionImportForwardsSelectionAndReturnsResolvedBinding(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, test := range []struct {
		name      string
		mode      string
		selection string
		resolved  string
		twoFA     bool
	}{
		{"auto session", service.AccountExecutionModeDockerKernel, "auto", "debian-12", false},
		{"manual session", service.AccountExecutionModeDockerKernel, "fedora-44", "fedora-44", false},
		{"auto 2FA", service.AccountExecutionModeDockerKernel, "auto", "ubuntu-24.04", true},
		{"manual 2FA", service.AccountExecutionModeDockerKernel, "centos-stream-9", "centos-stream-9", true},
		{"normal 2FA", service.AccountExecutionModeNormal, "", "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			svc := &resolvedSystemImportAdmin{codexImportMemoryAdminService: newCodexImportMemoryAdminService(nil), resolvedProfile: test.resolved}
			h := &AccountHandler{adminService: svc}
			router := gin.New()
			router.POST("/import", h.ImportCodexSession)
			content, err := json.Marshal(buildCodexRefreshImportValue(t, "pool-workspace", "pool-user", "pool-test-refresh"))
			require.NoError(t, err)
			body, err := json.Marshal(CodexSessionImportRequest{
				Content: string(content), ExecutionMode: &test.mode, DockerSystemProfile: test.selection,
				InitializeOpenAITwoFA: test.twoFA, SkipDefaultGroupBind: boolPtr(true),
			})
			require.NoError(t, err)
			req := httptest.NewRequest(http.MethodPost, "/import", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, req)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			require.Len(t, svc.createdAccounts, 1)
			require.Equal(t, test.selection, svc.createdAccounts[0].DockerSystemProfile)
			require.Equal(t, test.twoFA, svc.createdAccounts[0].InitialOpenAITwoFA)
			var out struct {
				Data CodexSessionImportResult `json:"data"`
			}
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &out))
			require.Equal(t, 1, out.Data.Created)
			require.Zero(t, out.Data.Failed)
			require.Len(t, out.Data.Items, 1)
			if test.resolved == "" {
				require.Nil(t, out.Data.Items[0].DockerSystemProfile)
			} else {
				require.NotNil(t, out.Data.Items[0].DockerSystemProfile)
				require.Equal(t, test.resolved, *out.Data.Items[0].DockerSystemProfile)
			}
		})
	}
}

func TestSystemPoolSessionExistingBindingIsPreservedForUpdateAndSkip(t *testing.T) {
	profile := "debian-12"
	for _, binding := range []*string{nil, &profile} {
		for _, skip := range []bool{false, true} {
			name := "legacy"
			if binding != nil {
				name = *binding
			}
			if skip {
				name += "/skip"
			} else {
				name += "/update"
			}
			t.Run(name, func(t *testing.T) {
				svc := newCodexImportMemoryAdminService(nil)
				h := &AccountHandler{adminService: svc}
				entries := []codexImportEntry{{Index: 1, Value: buildCodexRefreshImportValue(t, "pool-existing-workspace", "pool-existing-user", "old-refresh")}}
				first, err := h.importCodexSessions(t.Context(), CodexSessionImportRequest{SkipDefaultGroupBind: boolPtr(true)}, entries)
				require.NoError(t, err)
				require.Equal(t, 1, first.Created)
				svc.accounts[0].ExecutionMode = service.AccountExecutionModeDockerKernel
				svc.accounts[0].DockerSystemProfile = binding
				entries[0].Value = buildCodexRefreshImportValue(t, "pool-existing-workspace", "pool-existing-user", "new-refresh")
				mode := service.AccountExecutionModeDockerKernel
				result, err := h.importCodexSessions(t.Context(), CodexSessionImportRequest{
					ExecutionMode: &mode, DockerSystemProfile: "fedora-44", InitializeOpenAITwoFA: true,
					SkipExisting: skip, SkipDefaultGroupBind: boolPtr(true),
				}, entries)
				require.NoError(t, err)
				require.Zero(t, result.Created)
				require.Zero(t, result.Failed)
				require.Len(t, svc.createdAccounts, 1, "reauthorization must not allocate another system")
				require.Equal(t, binding, svc.accounts[0].DockerSystemProfile)
				require.Len(t, result.Items, 1)
				require.Equal(t, binding, result.Items[0].DockerSystemProfile)
				if skip {
					require.Equal(t, 1, result.Skipped)
					require.Empty(t, svc.updatedAccounts)
					require.Equal(t, "old-refresh", svc.accounts[0].Credentials["refresh_token"])
				} else {
					require.Equal(t, 1, result.Updated)
					require.Len(t, svc.updatedAccounts, 1)
					require.Equal(t, "new-refresh", svc.accounts[0].Credentials["refresh_token"])
				}
			})
		}
	}
}

func TestSystemPoolDataExportPreservesBindingAndExcludesAllocationState(t *testing.T) {
	router, svc := setupAccountDataRouter()
	profile := "archlinux"
	svc.accounts = []service.Account{
		{ID: 1, Name: "bound", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, ExecutionMode: service.AccountExecutionModeDockerKernel, DockerSystemProfile: &profile, DockerSystemSelection: &service.DockerSystemSelection{Requested: "auto", Candidates: service.DockerSystemProfileIDs()}},
		{ID: 2, Name: "legacy", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, ExecutionMode: service.AccountExecutionModeDockerKernel},
		{ID: 3, Name: "normal", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, ExecutionMode: service.AccountExecutionModeNormal},
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts/data?include_proxies=false", nil))
	require.Equal(t, http.StatusOK, response.Code)
	var out struct {
		Data DataPayload `json:"data"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &out))
	require.Len(t, out.Data.Accounts, 3)
	accounts := make(map[string]DataAccount, 3)
	for _, account := range out.Data.Accounts {
		accounts[account.Name] = account
	}
	require.Equal(t, &profile, accounts["bound"].DockerSystemProfile)
	require.Nil(t, accounts["legacy"].DockerSystemProfile)
	require.Nil(t, accounts["normal"].DockerSystemProfile)
	require.NotContains(t, response.Body.String(), "DockerSystemSelection")
	require.NotContains(t, response.Body.String(), "Candidates")
}

func TestSystemPoolDataImportForwardsExportedBindingAndLeavesLegacyForNewAllocation(t *testing.T) {
	router, svc := setupAccountDataRouter()
	profile := "archlinux"
	payload := struct {
		Data                 DataPayload `json:"data"`
		SkipDefaultGroupBind bool        `json:"skip_default_group_bind"`
	}{Data: DataPayload{Type: dataType, Version: dataVersion, Proxies: []DataProxy{}, Accounts: []DataAccount{
		{Name: "bound", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, ExecutionMode: service.AccountExecutionModeDockerKernel, DockerSystemProfile: &profile, Credentials: map[string]any{"access_token": "export-fixture"}},
		{Name: "legacy", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, ExecutionMode: service.AccountExecutionModeDockerKernel, Credentials: map[string]any{"access_token": "legacy-fixture"}},
		{Name: "normal", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, ExecutionMode: service.AccountExecutionModeNormal, Credentials: map[string]any{"access_token": "normal-fixture"}},
	}}, SkipDefaultGroupBind: true}
	body, err := json.Marshal(payload)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/data", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Len(t, svc.createdAccounts, 3)
	require.Equal(t, profile, svc.createdAccounts[0].DockerSystemProfile)
	require.Empty(t, svc.createdAccounts[1].DockerSystemProfile, "a newly imported legacy record uses normal create-time allocation")
	require.Equal(t, service.AccountExecutionModeDockerKernel, svc.createdAccounts[1].ExecutionMode)
	require.Empty(t, svc.createdAccounts[2].DockerSystemProfile)
	require.Equal(t, service.AccountExecutionModeNormal, svc.createdAccounts[2].ExecutionMode)
}
