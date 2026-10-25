package tlsfingerprint

import "slices"

// EffectiveProfile materializes the parameters consumed by the dialer. The
// result owns its slices and contains no connection secrets or TLS randomness.
// Keep this in sync with buildClientHelloSpecFromProfile; equivalence is tested.
func EffectiveProfile(profile *Profile) *Profile {
	p := Profile{}
	if profile != nil {
		p = *profile
	}
	resolve := func(v, fallback []uint16) []uint16 {
		if len(v) == 0 {
			v = fallback
		}
		return slices.Clone(v)
	}
	p.CipherSuites = resolve(p.CipherSuites, defaultCipherSuites)
	curves := make([]uint16, len(defaultCurves))
	for i, v := range defaultCurves {
		curves[i] = uint16(v)
	}
	p.Curves = resolve(p.Curves, curves)
	p.PointFormats = resolve(p.PointFormats, defaultPointFormats)
	signatures := make([]uint16, len(defaultSignatureAlgorithms))
	for i, v := range defaultSignatureAlgorithms {
		signatures[i] = uint16(v)
	}
	p.SignatureAlgorithms = resolve(p.SignatureAlgorithms, signatures)
	p.ALPNProtocols = slices.Clone(p.ALPNProtocols)
	if len(p.ALPNProtocols) == 0 {
		p.ALPNProtocols = []string{"h2", "http/1.1"}
	}
	p.SupportedVersions = resolve(p.SupportedVersions, []uint16{0x0304, 0x0303})
	p.KeyShareGroups = resolve(p.KeyShareGroups, []uint16{0x001d})
	p.PSKModes = resolve(p.PSKModes, []uint16{1})
	defaultExtensions := len(p.Extensions) == 0
	p.Extensions = resolve(p.Extensions, defaultExtensionOrder)
	if p.EnableGREASE && defaultExtensions {
		p.Extensions = append([]uint16{0x0a0a}, p.Extensions...)
		p.Extensions = append(p.Extensions, 0x0a0a)
	}
	// With an explicit extension list this flag is ignored by the dialer.
	p.EnableGREASE = false
	return &p
}
