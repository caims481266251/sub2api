package service

import (
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

func TestTLSProfileSnapshotRefreshAndOwnedReturns(t *testing.T) {
	s := &TLSFingerprintProfileService{}
	row := &model.TLSFingerprintProfile{ID: 1, Name: "one", CipherSuites: []uint16{0x1301, 0x1302}}
	s.setLocalCache([]*model.TLSFingerprintProfile{nil, row})
	first := s.GetProfileByID(1)
	compiled := tlsfingerprint.CompileHTTP2(first)
	first.CipherSuites[0] = 0x1303
	row.CipherSuites[0] = 0x1303
	require.Equal(t, uint16(0x1301), s.GetProfileByID(1).CipherSuites[0])
	require.Same(t, compiled, tlsfingerprint.CompileHTTP2(s.GetProfileByID(1)))
	s.setLocalCache([]*model.TLSFingerprintProfile{row})
	require.NotEqual(t, compiled.Digest(), tlsfingerprint.CompileHTTP2(s.GetProfileByID(1)).Digest())
	s.setLocalCache(nil)
	require.Nil(t, s.GetProfileByID(1))
	require.Nil(t, s.getRandomProfile())
}

func TestTLSProfileSnapshotsConcurrentRefresh(t *testing.T) {
	s := &TLSFingerprintProfileService{}
	rows := []*model.TLSFingerprintProfile{{ID: 1, Name: "a"}, {ID: 2, Name: "b"}}
	s.setLocalCache(rows)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 100 {
				p := s.getRandomProfile()
				if p != nil {
					_ = tlsfingerprint.CompileHTTP2(p).Digest()
					p.Name = "caller edit"
				}
			}
		})
	}
	for range 100 {
		s.setLocalCache(rows)
	}
	wg.Wait()
	require.Equal(t, "a", s.GetProfileByID(1).Name)
	require.Equal(t, "b", s.GetProfileByID(2).Name)
}
