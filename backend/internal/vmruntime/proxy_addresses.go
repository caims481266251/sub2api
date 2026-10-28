package vmruntime

import (
	"context"
	"encoding/json"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"time"
)

const proxyAddressCheckInterval = 5 * time.Minute

func lookupProxyAddresses(ctx context.Context, host string) ([]net.IP, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return net.DefaultResolver.LookupIP(ctx, "ip4", host)
}

func proxyIPv4Set(addresses []net.IP) []string {
	ips := []string{}
	for _, ip := range addresses {
		if ip.To4() != nil && !ip.IsLoopback() && !ip.IsUnspecified() && !ip.IsMulticast() {
			ips = append(ips, ip.String())
		}
	}
	slices.Sort(ips)
	return slices.Compact(ips)
}

// Observations only detect a changed address set. The account coordinator owns
// draining and applying it, so neither DNS failure nor refresh kills a stream.
func (h *Helper) checkProxyAddresses(ctx context.Context, s *State, dir string) {
	var proxy struct {
		URL string   `json:"url"`
		IPs []string `json:"ips"`
	}
	data, err := os.ReadFile(filepath.Join(dir, "credentials", "proxy.json"))
	if err != nil || json.Unmarshal(data, &proxy) != nil {
		return
	}
	u, err := url.Parse(proxy.URL)
	if err != nil || u.Hostname() == "" || net.ParseIP(u.Hostname()) != nil {
		return
	}
	now := time.Now()
	h.mu.Lock()
	if now.Sub(h.proxyChecked[s.AccountID]) < proxyAddressCheckInterval {
		h.mu.Unlock()
		return
	}
	if h.proxyChecked == nil {
		h.proxyChecked = make(map[int64]time.Time)
	}
	h.proxyChecked[s.AccountID] = now
	h.mu.Unlock()
	lookup := h.proxyLookup
	if lookup == nil {
		lookup = lookupProxyAddresses
	}
	addresses, err := lookup(ctx, u.Hostname())
	if err != nil {
		return // Preserve the known exit and process health on transient DNS failure.
	}
	ips := proxyIPv4Set(addresses)
	if len(ips) == 0 {
		return
	}
	slices.Sort(proxy.IPs)
	s.ProxyAddressChanged = !slices.Equal(slices.Compact(proxy.IPs), ips)
}
