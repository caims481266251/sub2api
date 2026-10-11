package service

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/google/uuid"
)

const BorrowFingerprintGenerator = "borrow-h1-v2"

var ErrBorrowFingerprintInvalid = errors.New("borrow_fingerprint_invalid")

// BorrowFingerprint is an immutable snapshot for one acquisition candidate.
// Acquisition and target verification share it; business never inherits it.
type BorrowFingerprint struct {
	Generator  string                  `json:"generator"`
	DeviceID   string                  `json:"device_id"`
	UserAgent  string                  `json:"user_agent"`
	Originator string                  `json:"originator"`
	Version    string                  `json:"version"`
	Profile    *tlsfingerprint.Profile `json:"profile"`
	CreatedAt  time.Time               `json:"created_at"`
	Reused     bool                    `json:"reused"`
}

func NewBorrowFingerprint() (*BorrowFingerprint, error) {
	id, err := uuid.NewRandom()
	if err != nil {
		return nil, err
	}
	p, err := tlsfingerprint.NewBorrowProfile()
	if err != nil {
		return nil, err
	}
	p.Name = BorrowFingerprintGenerator + "-" + borrowHash(id.String())[:12]
	h := make(http.Header)
	// This minimum-compatible identity also works for lower-version models.
	applyOpenAICodexTicketHarvestIdentity(h, OpenAICodexStateProbeDefaultModel)
	f := &BorrowFingerprint{Generator: BorrowFingerprintGenerator, DeviceID: id.String(),
		UserAgent: h.Get("User-Agent"), Originator: h.Get("Originator"), Version: h.Get("Version"),
		Profile: p, CreatedAt: time.Now().UTC()}
	return f, ValidateBorrowFingerprint(f)
}

// Validate the entire permitted template family, including preference tiers.
// Unknown versions and incomplete snapshots fail closed rather than silently
// taking new process defaults or downgrading to the legacy generator.
func ValidateBorrowFingerprint(f *BorrowFingerprint) error {
	if f == nil || f.Generator != BorrowFingerprintGenerator || f.Profile == nil || f.CreatedAt.IsZero() {
		return ErrBorrowFingerprintInvalid
	}
	if _, err := uuid.Parse(f.DeviceID); err != nil {
		return ErrBorrowFingerprintInvalid
	}
	originator, ua, ok := openai.PairCodexClientIdentity(f.UserAgent)
	if !ok || ua != f.UserAgent || originator != f.Originator || NormalizeCodexClientVersion(f.Version) != f.Version || f.Version == "" ||
		openai.CodexUserAgentVersion(f.UserAgent) != f.Version || strings.ContainsAny(f.UserAgent+f.Originator+f.Version, "\r\n") {
		return ErrBorrowFingerprintInvalid
	}
	if f.Profile.Name != BorrowFingerprintGenerator+"-"+borrowHash(f.DeviceID)[:12] || tlsfingerprint.ValidateBorrowProfile(f.Profile) != nil {
		return ErrBorrowFingerprintInvalid
	}
	return nil
}

// Hash both the full effective TLS configuration and its JA3-style structural
// subset. Neither includes the device, display name, headers or connection
// randomness. The latter is a comparison key, not a captured/claimed JA3 hash.
func BorrowTLSFingerprintHashes(profile *tlsfingerprint.Profile) (string, string) {
	p := tlsfingerprint.EffectiveProfile(profile)
	p.Name = ""
	raw, _ := json.Marshal(p)
	structure, _ := json.Marshal([]any{p.CipherSuites, p.Extensions, p.Curves, p.PointFormats, p.SupportedVersions, p.ALPNProtocols})
	return borrowHash(string(raw)), borrowHash(string(structure))
}

// Compare new configurations against pre-snapshot audit records as well.
func BorrowLegacyProfileHash(profile *tlsfingerprint.Profile) string {
	raw, _ := json.Marshal(&tlsfingerprint.Profile{CipherSuites: profile.CipherSuites})
	return borrowHash(string(raw))
}

func BorrowLegacyClientHelloHash(device string) string {
	_, structure := BorrowTLSFingerprintHashes(borrowSourceProfile(device))
	return structure
}

func sameBorrowFingerprint(a, b *BorrowFingerprint) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return string(left) == string(right)
}

func (f *BorrowFingerprint) Apply(h http.Header, model string) {
	h.Set("User-Agent", f.UserAgent)
	h.Set("Originator", f.Originator)
	h.Set("Version", f.Version)
	h.Set("X-Codex-Installation-ID", f.DeviceID)
	h.Set("X-Codex-Window-ID", f.DeviceID)
	// Retain upstream minimum-version safeguards on a future server upgrade.
	minimum := codexUpstreamMinVersion
	if needsOpenAICodexAstraVersion(model) && CompareVersions(openAICodexAstraMinVersion, minimum) > 0 {
		minimum = openAICodexAstraMinVersion
	}
	if CompareVersions(f.Version, minimum) < 0 {
		h.Set("User-Agent", buildCodexCLIUserAgent(minimum))
		h.Set("Originator", openai.CodexDefaultOriginator)
		h.Set("Version", minimum)
	}
}
