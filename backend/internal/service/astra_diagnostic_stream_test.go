//go:build astra_experiment

package service

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAstraDiagnosticStreamKeepsEnumsAndDetectsInStreamLimits(t *testing.T) {
	data := []byte("data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"rate_limit_exceeded\",\"message\":\"private-message\"}}}\n\n" + "data: {\"type\":\"private-event\",\"code\":\"private-code\"}\n\n")
	out := astraDiagnosticStream(data)
	require.Equal(t, 1, out.Events["response.failed"])
	require.Equal(t, []string{"rate_limit_exceeded", "unrecognized"}, out.ErrorCodes)
	require.Equal(t, time.Minute, out.HardWait())
	b, err := json.Marshal(out)
	require.NoError(t, err)
	require.NotContains(t, string(b), "private")
	require.Equal(t, 5*time.Minute, (AstraDiagnosticStream{ErrorCodes: []string{"usage_limit_reached"}}).HardWait())
	require.Zero(t, (AstraDiagnosticStream{ErrorCodes: []string{"server_error"}}).HardWait())
}

func TestAstraDiagnosticStreamTypeOnlyQuotaHonorsReset(t *testing.T) {
	out := astraDiagnosticStream([]byte("data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"type\":\"usage_limit_reached\",\"resets_in_seconds\":700,\"message\":\"private-message\"}}}\n\n"))
	require.Equal(t, []string{"usage_limit_reached"}, out.ErrorTypes)
	require.Equal(t, 700*time.Second, out.HardWait())
	raw, err := json.Marshal(out)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "private-message")
}
