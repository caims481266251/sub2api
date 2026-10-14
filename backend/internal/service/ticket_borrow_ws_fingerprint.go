package service

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/proxyurl"
	"github.com/Wei-Shaw/sub2api/internal/pkg/proxyutil"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
)

// WS handshakes use the same frozen ClientHello as HTTP routed requests. The
// existing bounded client cache isolates proxy/fingerprint pairs and owns cleanup.
func (d *coderOpenAIWSClientDialer) borrowFingerprintHTTPClient(proxy string, profile *tlsfingerprint.Profile) (*http.Client, error) {
	if err := tlsfingerprint.ValidateBorrowProfile(profile); err != nil {
		return nil, err
	}
	var parsed *url.URL
	proxy = strings.TrimSpace(proxy)
	if proxy != "" {
		var err error
		proxy, parsed, err = proxyurl.Parse(proxy)
		if err != nil {
			return nil, fmt.Errorf("invalid websocket proxy URL")
		}
	}
	// RFC 6455 upgrades need HTTP/1.1. Keep all other route fingerprint fields;
	// HTTP/SSE traffic negotiates h2, while this socket remains an Upgrade.
	profile = tlsfingerprint.EffectiveProfile(profile)
	profile.ALPNProtocols = []string{"http/1.1"}
	raw, err := json.Marshal(profile)
	if err != nil {
		return nil, err
	}
	key := fmt.Sprintf("borrow-fingerprint:%x:%s", sha256.Sum256(raw), proxy)
	now := time.Now().UnixNano()
	d.proxyMu.Lock()
	defer d.proxyMu.Unlock()
	if entry := d.proxyClients[key]; entry != nil {
		entry.lastUsedUnixNano = now
		d.proxyHits.Add(1)
		return entry.client, nil
	}
	d.cleanupProxyClientsLocked(now)
	transport := &http.Transport{
		DialContext:  (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns: openAIWSProxyTransportMaxIdleConns, MaxIdleConnsPerHost: openAIWSProxyTransportMaxIdleConnsPerHost,
		IdleConnTimeout: openAIWSProxyTransportIdleConnTimeout, TLSHandshakeTimeout: 10 * time.Second,
		ForceAttemptHTTP2: false,
	}
	if parsed == nil {
		transport.DialTLSContext = tlsfingerprint.NewDialer(profile, nil).DialTLSContext
	} else {
		if err := proxyutil.ConfigureTransportProxy(transport, parsed); err != nil {
			return nil, err
		}
		plainProxy := transport.Proxy
		transport.Proxy = func(req *http.Request) (*url.URL, error) {
			// HTTPS uses the fingerprint dialer's own CONNECT/SOCKS tunnel.
			if req.URL.Scheme == "https" || plainProxy == nil {
				return nil, nil
			}
			return plainProxy(req)
		}
		switch parsed.Scheme {
		case "http", "https":
			transport.DialTLSContext = tlsfingerprint.NewHTTPProxyDialer(profile, parsed).DialTLSContext
		case "socks5", "socks5h":
			transport.DialTLSContext = tlsfingerprint.NewSOCKS5ProxyDialer(profile, parsed).DialTLSContext
		default:
			return nil, fmt.Errorf("unsupported websocket proxy scheme")
		}
	}
	client := &http.Client{Transport: transport}
	if d.proxyClients == nil {
		d.proxyClients = make(map[string]*openAIWSProxyClientEntry)
	}
	d.proxyClients[key] = &openAIWSProxyClientEntry{client: client, lastUsedUnixNano: now}
	d.ensureProxyClientCapacityLocked()
	d.proxyMisses.Add(1)
	return client, nil
}
