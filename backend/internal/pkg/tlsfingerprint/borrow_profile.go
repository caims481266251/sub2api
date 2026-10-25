package tlsfingerprint

import (
	"crypto/rand"
	"errors"
	"math/big"
	"slices"
)

// Groups preserve TLS 1.3, forward-secret AEAD, forward-secret compatibility,
// then RSA compatibility priorities. Randomness is confined to each group.
var borrowCipherGroups = [][]uint16{
	{0x1301, 0x1302, 0x1303},
	{0xc02b, 0xc02f, 0xc02c, 0xc030, 0xcca9, 0xcca8},
	{0xc009, 0xc013, 0xc00a, 0xc014},
	{0x009c, 0x009d},
	{0x002f, 0x0035},
}

// These extensions have no mutual ordering dependency. SNI, ECH, group/point
// declarations, ALPN, signature algorithms and TLS 1.3 extensions keep their
// template positions. No new/unknown extension payloads are synthesized.
var borrowIndependentExtensionSlots = []int{2, 3, 6, 8, 10}

func borrowFingerprintTemplate() *Profile {
	// Versioned literals deliberately do not track mutable dialer defaults.
	return &Profile{
		CipherSuites: []uint16{0x1301, 0x1302, 0x1303, 0xc02b, 0xc02f, 0xc02c, 0xc030, 0xcca9, 0xcca8, 0xc009, 0xc013, 0xc00a, 0xc014, 0x009c, 0x009d, 0x002f, 0x0035},
		Curves:       []uint16{0x001d, 0x0017, 0x0018}, PointFormats: []uint16{0},
		SignatureAlgorithms: []uint16{0x0403, 0x0804, 0x0401, 0x0503, 0x0805, 0x0501, 0x0806, 0x0601, 0x0201},
		ALPNProtocols:       []string{"h2"}, SupportedVersions: []uint16{0x0304, 0x0303},
		KeyShareGroups: []uint16{0x001d}, PSKModes: []uint16{1},
		Extensions: []uint16{0, 65037, 23, 65281, 10, 11, 35, 16, 5, 13, 18, 51, 45, 43},
	}
}

func shuffleBorrowFingerprint(v []uint16) error {
	for i := len(v) - 1; i > 0; i-- {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		if err != nil {
			return err
		}
		j := int(n.Int64())
		v[i], v[j] = v[j], v[i]
	}
	return nil
}

// NewBorrowProfile generates a complete HTTP/2 profile from a constrained,
// versioned family. It does not consume mutable runtime defaults.
func NewBorrowProfile() (*Profile, error) {
	p := borrowFingerprintTemplate()
	start := 0
	for _, group := range borrowCipherGroups {
		if err := shuffleBorrowFingerprint(p.CipherSuites[start : start+len(group)]); err != nil {
			return nil, err
		}
		start += len(group)
	}
	for _, values := range [][]uint16{p.Curves[1:], p.SignatureAlgorithms[:3], p.SignatureAlgorithms[3:6], p.SignatureAlgorithms[6:8]} {
		if err := shuffleBorrowFingerprint(values); err != nil {
			return nil, err
		}
	}
	extensions := make([]uint16, len(borrowIndependentExtensionSlots))
	for i, slot := range borrowIndependentExtensionSlots {
		extensions[i] = p.Extensions[slot]
	}
	if err := shuffleBorrowFingerprint(extensions); err != nil {
		return nil, err
	}
	for i, slot := range borrowIndependentExtensionSlots {
		p.Extensions[slot] = extensions[i]
	}
	return p, ValidateBorrowProfile(p)
}

func sameBorrowValues(a, b []uint16) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

// ValidateBorrowProfile rejects parameters outside the tested template family.
func ValidateBorrowProfile(p *Profile) error {
	if p == nil {
		return errors.New("borrow_tls_profile_invalid")
	}
	base := borrowFingerprintTemplate()
	if p.EnableGREASE || len(p.CipherSuites) != len(base.CipherSuites) ||
		len(p.Curves) != 3 || p.Curves[0] != 0x001d || !sameBorrowValues(p.Curves, base.Curves) ||
		!slices.Equal(p.PointFormats, base.PointFormats) || !slices.Equal(p.ALPNProtocols, base.ALPNProtocols) ||
		!slices.Equal(p.SupportedVersions, base.SupportedVersions) || !slices.Equal(p.KeyShareGroups, base.KeyShareGroups) ||
		!slices.Equal(p.PSKModes, base.PSKModes) || len(p.SignatureAlgorithms) != 9 || len(p.Extensions) != len(base.Extensions) {
		return errors.New("borrow_tls_profile_invalid")
	}
	start := 0
	for _, group := range borrowCipherGroups {
		if !sameBorrowValues(p.CipherSuites[start:start+len(group)], group) {
			return errors.New("borrow_tls_profile_invalid")
		}
		start += len(group)
	}
	for _, bounds := range [][2]int{{0, 3}, {3, 6}, {6, 8}, {8, 9}} {
		if !sameBorrowValues(p.SignatureAlgorithms[bounds[0]:bounds[1]], base.SignatureAlgorithms[bounds[0]:bounds[1]]) {
			return errors.New("borrow_tls_profile_invalid")
		}
	}
	if !sameBorrowValues(p.Extensions, base.Extensions) {
		return errors.New("borrow_tls_profile_invalid")
	}
	for i, ext := range base.Extensions {
		if !slices.Contains(borrowIndependentExtensionSlots, i) && p.Extensions[i] != ext {
			return errors.New("borrow_tls_profile_invalid")
		}
	}
	return nil
}
