package service

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBorrowFingerprintBatchQualityAndDiversity(t *testing.T) {
	devices, configurations, structures := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for range 10000 {
		f, err := NewBorrowFingerprint()
		require.NoError(t, err)
		require.NoError(t, ValidateBorrowFingerprint(f))
		require.False(t, devices[f.DeviceID])
		devices[f.DeviceID] = true
		full, structure := BorrowTLSFingerprintHashes(f.Profile)
		configurations[full], structures[structure] = true, true
	}
	// Random generation can collide; repository history selection is responsible
	// for rejecting collisions. This only catches collapsed/ineffective entropy.
	require.Greater(t, len(configurations), 9900)
	require.Greater(t, len(structures), 9900)
	t.Logf("generated=10000 devices=%d full_tls=%d client_hello_structures=%d", len(devices), len(configurations), len(structures))
}

func TestBorrowFingerprintSnapshotAndComparisonKeys(t *testing.T) {
	f, err := NewBorrowFingerprint()
	require.NoError(t, err)
	raw, err := json.Marshal(f)
	require.NoError(t, err)
	var restored BorrowFingerprint
	require.NoError(t, json.Unmarshal(raw, &restored))
	require.NoError(t, ValidateBorrowFingerprint(&restored))
	require.True(t, sameBorrowFingerprint(f, &restored))
	full, structure := BorrowTLSFingerprintHashes(f.Profile)
	restored.Profile.Name = "new-device-display-name"
	otherFull, otherStructure := BorrowTLSFingerprintHashes(restored.Profile)
	require.Equal(t, full, otherFull)
	require.Equal(t, structure, otherStructure)
	restored.Profile.SignatureAlgorithms[0], restored.Profile.SignatureAlgorithms[1] = restored.Profile.SignatureAlgorithms[1], restored.Profile.SignatureAlgorithms[0]
	otherFull, otherStructure = BorrowTLSFingerprintHashes(restored.Profile)
	require.NotEqual(t, full, otherFull)
	require.Equal(t, structure, otherStructure, "changing only signatures must not count as a new JA3-style structure")
	restored.Profile.CipherSuites[0], restored.Profile.CipherSuites[1] = restored.Profile.CipherSuites[1], restored.Profile.CipherSuites[0]
	_, otherStructure = BorrowTLSFingerprintHashes(restored.Profile)
	require.NotEqual(t, structure, otherStructure)
}

func TestBorrowFingerprintRejectsIncompleteOrIncoherentSnapshots(t *testing.T) {
	cases := map[string]func(*BorrowFingerprint){
		"unknown generator":   func(f *BorrowFingerprint) { f.Generator = "unknown" },
		"wrong device":        func(f *BorrowFingerprint) { f.DeviceID = "invalid" },
		"version mismatch":    func(f *BorrowFingerprint) { f.Version = "999.0.0" },
		"header injection":    func(f *BorrowFingerprint) { f.UserAgent += "\r\nX-Test: injected" },
		"incomplete":          func(f *BorrowFingerprint) { f.Profile.Curves = nil },
		"wrong alpn":          func(f *BorrowFingerprint) { f.Profile.ALPNProtocols = []string{"http/1.1"} },
		"wrong key share":     func(f *BorrowFingerprint) { f.Profile.KeyShareGroups = []uint16{23} },
		"duplicate extension": func(f *BorrowFingerprint) { f.Profile.Extensions[3] = f.Profile.Extensions[2] },
		"weak priority": func(f *BorrowFingerprint) {
			f.Profile.CipherSuites[3], f.Profile.CipherSuites[16] = f.Profile.CipherSuites[16], f.Profile.CipherSuites[3]
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f, err := NewBorrowFingerprint()
			require.NoError(t, err)
			mutate(f)
			require.ErrorIs(t, ValidateBorrowFingerprint(f), ErrBorrowFingerprintInvalid)
		})
	}
}

func TestBorrowFingerprintHeadersStableAndMinimumVersionPreserved(t *testing.T) {
	f, err := NewBorrowFingerprint()
	require.NoError(t, err)
	first := make(http.Header)
	f.Apply(first, "gpt-6-luna")
	// A changed canonical resolver only affects newly generated environments.
	codexCanonicalUAMu.RLock()
	previous := codexCanonicalUAResolver
	codexCanonicalUAMu.RUnlock()
	SetCodexCanonicalUserAgentResolver(func() string { return buildCodexCLIUserAgent("999.0.0") })
	defer SetCodexCanonicalUserAgentResolver(previous)
	next := make(http.Header)
	f.Apply(next, "gpt-6-luna")
	require.Equal(t, first, next)
	f.Version, f.UserAgent = "0.1.0", buildCodexCLIUserAgent("0.1.0")
	f.Apply(next, "gpt-6-luna")
	require.GreaterOrEqual(t, CompareVersions(next.Get("Version"), openAICodexAstraMinVersion), 0)
	require.NotEqual(t, "0.1.0", next.Get("Version"))
}
