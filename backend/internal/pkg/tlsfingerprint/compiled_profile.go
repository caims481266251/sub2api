package tlsfingerprint

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
)

// HTTP2Profile is an immutable transport snapshot. Its source is retained to
// detect callers editing exported Profile fields after copying a prepared value.
type HTTP2Profile struct {
	source     Profile
	effective  Profile
	digest     string
	wireDigest string
}

func CloneProfile(p *Profile) *Profile {
	if p == nil {
		return nil
	}
	copy := *p
	copy.CipherSuites = slices.Clone(p.CipherSuites)
	copy.Curves = slices.Clone(p.Curves)
	copy.PointFormats = slices.Clone(p.PointFormats)
	copy.SignatureAlgorithms = slices.Clone(p.SignatureAlgorithms)
	copy.ALPNProtocols = slices.Clone(p.ALPNProtocols)
	copy.SupportedVersions = slices.Clone(p.SupportedVersions)
	copy.KeyShareGroups = slices.Clone(p.KeyShareGroups)
	copy.PSKModes = slices.Clone(p.PSKModes)
	copy.Extensions = slices.Clone(p.Extensions)
	return &copy
}

// PrepareHTTP2 owns the source slices; neither subsequent database updates nor
// mutation of a returned clone can alter a transport already in use.
func PrepareHTTP2(p *Profile) *Profile {
	if p == nil {
		return nil
	}
	owned := CloneProfile(p)
	if owned.compiledHTTP2 != nil && sameProfile(owned, &owned.compiledHTTP2.source) {
		return owned
	}
	compiled := CompileHTTP2(owned)
	compiled.source = *CloneProfile(owned)
	compiled.source.compiledHTTP2 = nil
	owned.compiledHTTP2 = compiled
	return owned
}

func CompileHTTP2(p *Profile) *HTTP2Profile {
	if p != nil && p.compiledHTTP2 != nil && sameProfile(p, &p.compiledHTTP2.source) {
		return p.compiledHTTP2
	}
	effective := EffectiveProfile(p)
	effective.compiledHTTP2 = nil
	effective.ALPNProtocols = []string{"h2"}
	// Profile has only JSON-safe primitive fields. Preserve the existing key
	// encoding so this optimization does not merge or split existing pools.
	raw, _ := json.Marshal(effective)
	sum := sha256.Sum256(raw)
	wire := *effective
	wire.Name = ""
	wireRaw, _ := json.Marshal(&wire)
	wireSum := sha256.Sum256(wireRaw)
	return &HTTP2Profile{effective: *effective, digest: hex.EncodeToString(sum[:]), wireDigest: hex.EncodeToString(wireSum[:])}
}

func (p *HTTP2Profile) Digest() string { return p.digest }

// FingerprintID compares effective TLS parameters, excluding display/device
// names. This is a configuration digest, not a captured JA3/JA4 fingerprint.
func (p *HTTP2Profile) FingerprintID() string { return p.wireDigest }

// Profile returns an owned copy only when a new transport needs its parameters.
func (p *HTTP2Profile) Profile() *Profile { return CloneProfile(&p.effective) }

func sameProfile(a, b *Profile) bool {
	return a.Name == b.Name && a.EnableGREASE == b.EnableGREASE &&
		slices.Equal(a.CipherSuites, b.CipherSuites) && slices.Equal(a.Curves, b.Curves) &&
		slices.Equal(a.PointFormats, b.PointFormats) && slices.Equal(a.SignatureAlgorithms, b.SignatureAlgorithms) &&
		slices.Equal(a.ALPNProtocols, b.ALPNProtocols) && slices.Equal(a.SupportedVersions, b.SupportedVersions) &&
		slices.Equal(a.KeyShareGroups, b.KeyShareGroups) && slices.Equal(a.PSKModes, b.PSKModes) &&
		slices.Equal(a.Extensions, b.Extensions)
}
