//go:build unit

package service

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestParseOpenAIRateLimitResetTime_StreamErrors(t *testing.T) {
	reset := time.Now().Add(4 * time.Hour).Truncate(time.Second)
	for _, tc := range []struct {
		name, body string
		wantReset  bool
	}{
		{"top-level quota type", fmt.Sprintf(`{"type":"error","error":{"type":"usage_limit_reached","resets_at":%d}}`, reset.Unix()), true},
		{"nested quota type", fmt.Sprintf(`{"type":"response.failed","response":{"error":{"type":"usage_limit_reached","resets_at":%d}}}`, reset.Unix()), true},
		{"top-level quota code", fmt.Sprintf(`{"type":"error","error":{"type":"api_error","code":"usage_limit_reached","resets_at":"%d"}}`, reset.Unix()), true},
		{"nested quota code", `{"type":"response.failed","response":{"error":{"code":"rate_limit_exceeded","resets_in_seconds":"14400"}}}`, true},
		{"transient rate limit", `{"type":"error","error":{"code":"rate_limit_exceeded"}}`, false},
		{"local capacity", `{"type":"error","error":{"code":"no_capacity","resets_in_seconds":14400}}`, false},
		{"unknown nested error", `{"type":"response.failed","response":{"error":{"type":"api_error","resets_in_seconds":14400}}}`, false},
		{"arbitrary message", `{"type":"error","error":{"type":"api_error","message":"usage_limit_reached resets in 4 hours"}}`, false},
		{"unverified message wrapper", `{"type":"error","error":{"code":"usage_limit_reached","message":"upstream status 429: {\"error\":{\"type\":\"usage_limit_reached\",\"resets_in_seconds\":14400}}"}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			actual := parseOpenAIRateLimitResetTime([]byte(tc.body))
			if !tc.wantReset {
				require.Nil(t, actual)
				return
			}
			require.NotNil(t, actual)
			require.WithinDuration(t, reset, time.Unix(*actual, 0), 2*time.Second)
		})
	}
}
