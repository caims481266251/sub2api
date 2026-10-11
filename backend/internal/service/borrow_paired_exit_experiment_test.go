//go:build astra_experiment

package service

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBorrowPairedExitCopiesOnlyTransportIdentity(t *testing.T) {
	base, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, chatgptCodexURL, nil)
	base.Header.Set("Authorization", "Bearer target-token")
	base.Header.Set("ChatGPT-Account-ID", "target-subject")
	base.Header.Set("User-Agent", "old-target")
	base.Header.Set("Accept-Language", "old-language")
	source := http.Header{}
	source.Set("Authorization", "Bearer source-token")
	source.Set("ChatGPT-Account-ID", "source-subject")
	source.Set("User-Agent", "source-UA")
	source.Set("X-Codex-Installation-ID", "source-device")
	source.Set("Cookie", "secret-route")
	matched := pairedExitTemplate(base, source)
	require.Equal(t, "Bearer target-token", matched.Header.Get("Authorization"))
	require.Equal(t, "target-subject", matched.Header.Get("ChatGPT-Account-ID"))
	require.Equal(t, "source-UA", matched.Header.Get("User-Agent"))
	require.Equal(t, "source-device", matched.Header.Get("X-Codex-Installation-ID"))
	require.Empty(t, matched.Header.Get("Accept-Language"))
	require.Empty(t, matched.Header.Get("Cookie"))
	require.Equal(t, "old-target", base.Header.Get("User-Agent"))
	require.Equal(t, "old-language", base.Header.Get("Accept-Language"))
}
func TestBorrowPairedExitNeverRefreshesAnExpiringToken(t *testing.T) {
	a := &Account{Type: AccountTypeOAuth, Platform: PlatformOpenAI, Credentials: map[string]any{"access_token": "token", "expires_at": time.Now().Add(time.Minute).Format(time.RFC3339)}}
	require.False(t, diagnosticTokenReady(a))
	a.Credentials["expires_at"] = time.Now().Add(time.Hour).Format(time.RFC3339)
	require.True(t, diagnosticTokenReady(a))
}
