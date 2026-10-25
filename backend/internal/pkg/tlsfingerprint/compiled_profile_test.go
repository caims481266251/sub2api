package tlsfingerprint

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func legacyHTTP2Digest(p *Profile) string {
	effective := EffectiveProfile(p)
	effective.ALPNProtocols = []string{"h2"}
	raw, _ := json.Marshal(effective)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func TestCompiledHTTP2PreservesPoolKeyAndWireProfile(t *testing.T) {
	borrow, err := NewBorrowProfile()
	require.NoError(t, err)
	for _, p := range []*Profile{{}, {Name: "default", EnableGREASE: true}, borrow} {
		prepared := PrepareHTTP2(p)
		compiled := CompileHTTP2(prepared)
		require.Equal(t, legacyHTTP2Digest(p), compiled.Digest())
		expected := EffectiveProfile(p)
		expected.ALPNProtocols = []string{"h2"}
		require.Equal(t, buildClientHelloSpecFromProfile(expected), buildClientHelloSpecFromProfile(compiled.Profile()))
		require.Same(t, compiled, CompileHTTP2(CloneProfile(prepared)))
	}
}

func TestCompiledHTTP2DoesNotTrustEditedCopies(t *testing.T) {
	p, err := NewBorrowProfile()
	require.NoError(t, err)
	prepared := PrepareHTTP2(p)
	original := CompileHTTP2(prepared)
	mutations := map[string]func(*Profile){
		"name":       func(p *Profile) { p.Name += " edited" },
		"cipher":     func(p *Profile) { p.CipherSuites[0] ^= 1 },
		"curves":     func(p *Profile) { p.Curves[0] ^= 1 },
		"points":     func(p *Profile) { p.PointFormats[0] ^= 1 },
		"signatures": func(p *Profile) { p.SignatureAlgorithms[0] ^= 1 },
		"versions":   func(p *Profile) { p.SupportedVersions[0] ^= 1 },
		"shares":     func(p *Profile) { p.KeyShareGroups[0] ^= 1 },
		"psk":        func(p *Profile) { p.PSKModes[0] ^= 1 },
		"extensions": func(p *Profile) { p.Extensions[0] ^= 1 },
		"alpn":       func(p *Profile) { p.ALPNProtocols[0] = "http/1.1" },
		"grease":     func(p *Profile) { p.EnableGREASE = !p.EnableGREASE },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			copy := CloneProfile(prepared)
			mutate(copy)
			changed := CompileHTTP2(copy)
			require.NotSame(t, original, changed)
			require.Equal(t, legacyHTTP2Digest(copy), changed.Digest())
			require.Same(t, original, CompileHTTP2(prepared))
		})
	}
	p.CipherSuites[0] ^= 1
	require.Same(t, original, CompileHTTP2(prepared), "input mutation cannot alter published snapshot")
	copy := original.Profile()
	copy.CipherSuites[0] ^= 1
	require.NotEqual(t, copy.CipherSuites, original.Profile().CipherSuites)
}

var benchmarkProfileDigest string

func BenchmarkBorrowRouteProfileResolution(b *testing.B) {
	p, err := NewBorrowProfile()
	if err != nil {
		b.Fatal(err)
	}
	p.Name = "borrow-h2-v3-benchmark"
	b.Run("previous", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			benchmarkProfileDigest = legacyHTTP2Digest(p)
		}
	})
	b.Run("cached", func(b *testing.B) {
		var cache HTTP2ProfileCache
		_ = cache.Compile(p)
		b.ReportAllocs()
		for b.Loop() {
			benchmarkProfileDigest = cache.Compile(p).Digest()
		}
	})
}

func BenchmarkHTTP2ProfileResolution(b *testing.B) {
	p := PrepareHTTP2(&Profile{Name: "benchmark", CipherSuites: []uint16{0x1301, 0x1302, 0x1303}, Curves: []uint16{29, 23}})
	b.Run("previous", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			benchmarkProfileDigest = legacyHTTP2Digest(p)
		}
	})
	b.Run("prepared", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			benchmarkProfileDigest = CompileHTTP2(p).Digest()
		}
	})
	b.Run("prepared_owned_lookup", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			benchmarkProfileDigest = CompileHTTP2(CloneProfile(p)).Digest()
		}
	})
	b.Run("route_snapshot_cache", func(b *testing.B) {
		var cache HTTP2ProfileCache
		raw := CloneProfile(p)
		raw.compiledHTTP2 = nil
		_ = cache.Compile(raw)
		b.ReportAllocs()
		for b.Loop() {
			benchmarkProfileDigest = cache.Compile(raw).Digest()
		}
	})
}

func TestHTTP2ProfileCacheVerifiesContentAndBounds(t *testing.T) {
	var cache HTTP2ProfileCache
	p := &Profile{Name: "route", CipherSuites: []uint16{0x1301, 0x1302}}
	first := cache.Compile(p)
	require.Same(t, first, cache.Compile(CloneProfile(p)))
	p.CipherSuites[0] = 0x1303
	second := cache.Compile(p)
	require.NotSame(t, first, second)
	require.NotEqual(t, first.Digest(), second.Digest())
	require.Equal(t, uint16(0x1301), first.Profile().CipherSuites[0])
	for i := range maxHTTP2ProfileSnapshots * 2 {
		cache.Compile(&Profile{Name: fmt.Sprint(i)})
	}
	require.Len(t, cache.entries, maxHTTP2ProfileSnapshots)
	require.Nil(t, cache.entries["route"])
	require.Equal(t, second.Digest(), cache.Compile(p).Digest())
}

func TestHTTP2FingerprintIDExcludesNamesButPoolKeyRemainsCompatible(t *testing.T) {
	a := CompileHTTP2(&Profile{Name: "device-a"})
	b := CompileHTTP2(&Profile{Name: "device-b", ALPNProtocols: []string{"http/1.1"}})
	require.Equal(t, a.FingerprintID(), b.FingerprintID(), "effective H2 wire parameters are identical")
	require.NotEqual(t, a.Digest(), b.Digest(), "preserve existing pool isolation semantics")
}

func TestHTTP2ProfileCacheConcurrentReadsAndChanges(t *testing.T) {
	var cache HTTP2ProfileCache
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			p := &Profile{Name: "shared", CipherSuites: []uint16{uint16(0x1301 + i%3)}}
			want := legacyHTTP2Digest(p)
			for range 100 {
				require.Equal(t, want, cache.Compile(p).Digest())
			}
		})
	}
	wg.Wait()
}
