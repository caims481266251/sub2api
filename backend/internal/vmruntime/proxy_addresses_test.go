package vmruntime

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestProxyAddressObservationPreservesHealthAndProjection(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ips     []net.IP
		err     error
		changed bool
	}{
		{"unchanged-order-duplicates", []net.IP{net.ParseIP("192.0.2.2"), net.ParseIP("192.0.2.1"), net.ParseIP("192.0.2.1")}, nil, false},
		{"changed", []net.IP{net.ParseIP("192.0.2.3")}, nil, true},
		{"dns-error", nil, errors.New("dns unavailable"), false},
		{"empty", nil, nil, false},
		{"invalid-only", []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1"), net.ParseIP("0.0.0.0")}, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			os.Mkdir(filepath.Join(dir, "credentials"), 0700)
			path := filepath.Join(dir, "credentials", "proxy.json")
			original := []byte(`{"url":"socks5h://proxy.example:1080","ips":["192.0.2.1","192.0.2.2"]}`)
			if err := os.WriteFile(path, original, 0600); err != nil {
				t.Fatal(err)
			}
			calls := 0
			h := &Helper{proxyLookup: func(_ context.Context, host string) ([]net.IP, error) {
				if host != "proxy.example" {
					t.Fatal("wrong host")
				}
				calls++
				return tc.ips, tc.err
			}}
			s := &State{AccountID: 42, Ready: true, Healthy: true, Generation: 7}
			h.checkProxyAddresses(t.Context(), s, dir)
			h.checkProxyAddresses(t.Context(), s, dir)
			if s.ProxyAddressChanged != tc.changed || !s.Ready || !s.Healthy || s.Generation != 7 || calls != 1 {
				t.Fatalf("wrong observation: changed=%v healthy=%v ready=%v calls=%d", s.ProxyAddressChanged, s.Healthy, s.Ready, calls)
			}
			data, _ := os.ReadFile(path)
			if string(data) != string(original) {
				t.Fatal("observation rewrote the active proxy")
			}
			h.proxyChecked[42] = time.Now().Add(-proxyAddressCheckInterval)
			h.checkProxyAddresses(t.Context(), s, dir)
			if calls != 2 {
				t.Fatal("address check did not refresh")
			}
		})
	}
}

func TestProxyLiteralDoesNotResolveDNS(t *testing.T) {
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "credentials"), 0700)
	os.WriteFile(filepath.Join(dir, "credentials", "proxy.json"), []byte(`{"url":"http://192.0.2.1:3128","ips":["192.0.2.1"]}`), 0600)
	h := &Helper{proxyLookup: func(context.Context, string) ([]net.IP, error) { t.Fatal("literal resolved DNS"); return nil, nil }}
	h.checkProxyAddresses(t.Context(), &State{AccountID: 42}, dir)
}
