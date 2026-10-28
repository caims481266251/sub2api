package dto

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAccountListItemFromAccount_CarriesDockerSystemProfile(t *testing.T) {
	for _, profile := range []string{"centos-stream-9", "ubuntu-24.04", "debian-12", "archlinux", "fedora-44"} {
		t.Run(profile, func(t *testing.T) {
			full := AccountFromServiceShallow(&service.Account{
				ID: 7, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
				ExecutionMode: "docker_kernel", DockerSystemProfile: &profile,
			})
			item := AccountListItemFromAccount(full)
			require.Equal(t, &profile, item.DockerSystemProfile)
			for _, response := range []any{full, item} {
				raw, err := json.Marshal(response)
				require.NoError(t, err)
				var fields map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(raw, &fields))
				require.JSONEq(t, `"`+profile+`"`, string(fields["docker_system_profile"]))
			}
		})
	}
}

func TestAccountListItemFromAccount_PreservesUnboundSystemProfile(t *testing.T) {
	for _, mode := range []string{"normal", "docker_kernel"} {
		t.Run(mode, func(t *testing.T) {
			full := AccountFromServiceShallow(&service.Account{
				ID: 8, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, ExecutionMode: mode,
			})
			item := AccountListItemFromAccount(full)
			require.Nil(t, item.DockerSystemProfile)
			for _, response := range []any{full, item} {
				raw, err := json.Marshal(response)
				require.NoError(t, err)
				var fields map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(raw, &fields))
				require.Equal(t, json.RawMessage("null"), fields["docker_system_profile"])
			}
		})
	}
	require.Nil(t, AccountListItemFromAccount(nil))
}
